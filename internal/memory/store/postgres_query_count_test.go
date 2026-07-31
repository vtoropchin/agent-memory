package store

// postgres_query_count_test.go — регрессионный тест на число SQL round-trip'ов
// AllForReindex/AllForExport: демонстрирует ФАКТОМ (а не заявлением в комментарии),
// что батч-загрузка Tags/Links (loadTagsBatch/loadLinksBatch, postgres_crud.go)
// действительно устранила построчный N+1 — число запросов не растёт вместе с числом
// записей. AllForReindex — самый горячий из трёх переведённых на батч мест: он
// вызывается на ВЕСЬ набор живых записей при каждом фоновом reindex-джобе (смена
// модели эмбеддера, DESIGN.md §9), то есть до фикса — 2N лишних round-trip'ов на
// прогон.
//
// Счётчик реализован как отдельный database/sql-драйвер "pgx_counting",
// оборачивающий stdlib.GetDefaultDriver() (тот же самый *pgx.Driver, под которым
// зарегистрирован "pgx" в postgres.go) — считает вызовы QueryContext на уровне
// driver.Conn, то есть каждый физический SQL-запрос независимо от того, пришёл ли
// он через db.Query/QueryRow (оба в database/sql идут через один и тот же путь
// driver.QueryerContext.QueryContext). Exec не считается — Put/Supersede/Touch эту
// метрику не интересуют.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"

	"agentmemory/internal/memory"
)

// queryCounter — потокобезопасный счётчик QueryContext-вызовов countingConn.
type queryCounter struct {
	n int64
}

func (c *queryCounter) reset()     { atomic.StoreInt64(&c.n, 0) }
func (c *queryCounter) get() int64 { return atomic.LoadInt64(&c.n) }
func (c *queryCounter) inc()       { atomic.AddInt64(&c.n, 1) }

// countingConn оборачивает driver.Conn, инкрементируя queryCounter на каждый
// QueryContext. Остальные опциональные интерфейсы driver.Conn (ExecerContext,
// ConnPrepareContext, ConnBeginTx, Pinger, SessionResetter, NamedValueChecker) явно
// форвардятся в исходный conn без изменений — без них database/sql откатился бы на
// медленные пути (Prepare+Exec вместо *Context) или потерял бы поддержку транзакций
// с контекстом (BeginTx), что исказило бы сам факт измерения (пул тестовых
// go-методов store использует BeginTx — см. List/Put/AllForExport и т.д. в других
// тестах, хоть измеряемые здесь AllForReindex/AllForExport транзакцию и не открывают).
type countingConn struct {
	driver.Conn
	counter *queryCounter
}

func (c countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	qc, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	c.counter.inc()
	return qc.QueryContext(ctx, query, args)
}

func (c countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	ec, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return ec.ExecContext(ctx, query, args)
}

func (c countingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if pc, ok := c.Conn.(driver.ConnPrepareContext); ok {
		return pc.PrepareContext(ctx, query)
	}
	return c.Conn.Prepare(query)
}

func (c countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bc, ok := c.Conn.(driver.ConnBeginTx); ok {
		return bc.BeginTx(ctx, opts)
	}
	return c.Conn.Begin() //nolint:staticcheck // фолбэк для conn без ConnBeginTx
}

func (c countingConn) Ping(ctx context.Context) error {
	if p, ok := c.Conn.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c countingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.Conn.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func (c countingConn) CheckNamedValue(nv *driver.NamedValue) error {
	if chk, ok := c.Conn.(driver.NamedValueChecker); ok {
		return chk.CheckNamedValue(nv)
	}
	return driver.ErrSkip
}

// countingDriver — driver.Driver, оборачивающий parent (реальный *stdlib.Driver) и
// возвращающий countingConn вместо его "голого" driver.Conn.
type countingDriver struct {
	parent  driver.Driver
	counter *queryCounter
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.parent.Open(name)
	if err != nil {
		return nil, err
	}
	return countingConn{Conn: conn, counter: d.counter}, nil
}

var (
	pgQueryCounter       = &queryCounter{}
	pgCountingDriverOnce sync.Once
)

// registerCountingDriver регистрирует "pgx_counting" под sql.Register РОВНО ОДИН
// РАЗ на весь прогон тестового бинаря (sql.Register паникует при повторной
// регистрации того же имени, а go test может вызвать эту функцию из нескольких
// тестов).
func registerCountingDriver() {
	pgCountingDriverOnce.Do(func() {
		sql.Register("pgx_counting", countingDriver{parent: stdlib.GetDefaultDriver(), counter: pgQueryCounter})
	})
}

// TestAllForReindexAndAllForExportQueryCount — находка ревью: AllForReindex/
// AllForExport (postgres_crud.go) раньше грузили Tags/Links построчно (loadTags/
// loadLinks в цикле по каждой записи) — 1 базовый SELECT + 2*N round-trip'ов на N
// записей. После перевода на loadTagsAndLinksBatch (loadTagsBatch+loadLinksBatch)
// число запросов должно оставаться константным (3: базовый SELECT + батч tags +
// батч links) независимо от N.
func TestAllForReindexAndAllForExportQueryCount(t *testing.T) {
	// newTestPgStore (ниже) сам пропускает тест через t.Skip, если pgTestDSNEnv не
	// задана — но baseDSN нужен ЗДЕСЬ ещё до её вызова (строим DSN counting-
	// соединения), поэтому та же проверка продублирована в её же формулировке.
	baseDSN := os.Getenv(pgTestDSNEnv)
	if baseDSN == "" {
		t.Skipf("%s не задан — пропуск postgres-тестов store. Пример:\n  %s=postgres://memo:memo@postgres.lan:5432/memo?sslmode=disable make test", pgTestDSNEnv, pgTestDSNEnv)
	}
	registerCountingDriver()

	s := newTestPgStore(t)
	ctx := context.Background()

	const n = 10
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		id := newID(t)
		ids[i] = id
		var links []memory.Link
		if i > 0 {
			links = []memory.Link{{Dst: ids[i-1], Rel: memory.RelRelated}}
		}
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindSemantic, Scope: "project:qcount", Content: "запись под счётчик запросов",
			CreatedAt: int64(i + 1), UpdatedAt: int64(i + 1),
			Tags: []string{"a", "b"}, Links: links,
		}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}

	// Отдельное соединение через "pgx_counting" на ТОТ ЖЕ search_path (s.schema,
	// зафиксированная в s.init через current_schema()) — считаем запросы именно
	// AllForReindex/AllForExport ниже, а не setup Put выше (тот шёл через обычное
	// соединение s.db под драйвером "pgx").
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("разбор %s: %v", pgTestDSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", s.schema)
	u.RawQuery = q.Encode()
	countingDB, err := sql.Open("pgx_counting", u.String())
	if err != nil {
		t.Fatalf("открытие counting-соединения: %v", err)
	}
	t.Cleanup(func() { countingDB.Close() })
	counting := &pgStore{db: countingDB, now: defaultNow, schema: s.schema}

	pgQueryCounter.reset()
	all, err := counting.AllForReindex(ctx)
	if err != nil {
		t.Fatalf("AllForReindex: %v", err)
	}
	if len(all) != n {
		t.Fatalf("AllForReindex вернул %d записей, want %d", len(all), n)
	}
	reindexQueries := pgQueryCounter.get()

	pgQueryCounter.reset()
	allExp, err := counting.AllForExport(ctx, nil)
	if err != nil {
		t.Fatalf("AllForExport: %v", err)
	}
	if len(allExp) != n {
		t.Fatalf("AllForExport вернул %d записей, want %d", len(allExp), n)
	}
	exportQueries := pgQueryCounter.get()

	t.Logf("AllForReindex на %d записях: %d SQL-запросов (построчно было бы 1+2*%d=%d)", n, reindexQueries, n, 1+2*n)
	t.Logf("AllForExport на %d записях: %d SQL-запросов (построчно было бы 1+2*%d=%d)", n, exportQueries, n, 1+2*n)

	// 3 = базовый SELECT + батч tags (loadTagsBatch) + батч links (loadLinksBatch) —
	// константа, не зависящая от N. Строгое равенство (не "<=") — так регрессия
	// (кто-то вернёт построчную загрузку) ловится немедленно, а не маскируется
	// послаблением порога.
	if reindexQueries != 3 {
		t.Errorf("AllForReindex сделал %d запросов на %d записей, want ровно 3 (батч, не построчно)", reindexQueries, n)
	}
	if exportQueries != 3 {
		t.Errorf("AllForExport сделал %d запросов на %d записей, want ровно 3 (батч, не построчно)", exportQueries, n)
	}
}
