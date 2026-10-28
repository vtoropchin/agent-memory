package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
)

// Тесты общей базы синхронизации (postgres_hub.go, шаг migrateChangeXIDPG) — на
// живой PostgreSQL, как остальные postgres-тесты; без MEMO_TEST_POSTGRES_DSN
// пропускаются. Изоляция — схема на тест (newTestPgSchemaDB).
//
// Утверждения о курсоре не опираются на то, что окно [xmin, xmax) пусто: горизонт
// снимка общий на кластер, и чужая транзакция на том же сервере (другой прогон
// тестов, боевой memo) законно заставляет перечитать строки. Проверяется то, что
// гарантирует алгоритм: в пределах прогона каждая строка ровно один раз, между
// прогонами — ни одной потерянной.

// newTestHub — Hub на пустой схеме, без init.
func newTestHub(t *testing.T) *Hub {
	t.Helper()
	return newHub(newTestPgSchemaDB(t, false))
}

// newTestHubApplied — Hub на схеме, созданной явным ApplySchema.
func newTestHubApplied(t *testing.T) *Hub {
	t.Helper()
	h := newTestHub(t)
	if err := h.ApplySchema(context.Background()); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	return h
}

// hubInsert вставляет запись сырым SQL в транзакции q (или в пуле) — как прямой
// писатель, мимо Put.
func hubInsert(t *testing.T, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, scope string, kind memory.Kind) string {
	t.Helper()
	id := ulid.Make().String()
	if _, err := q.ExecContext(context.Background(), `
		INSERT INTO memories (id, kind, scope, content, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 1, 1)`, id, string(kind), scope, "запись "+id); err != nil {
		t.Fatalf("вставка %s: %v", id, err)
	}
	return id
}

func changeXID(t *testing.T, h *Hub, id string) uint64 {
	t.Helper()
	var text string
	if err := h.pg.db.QueryRowContext(context.Background(), `SELECT change_xid::text FROM memories WHERE id = $1`, id).Scan(&text); err != nil {
		t.Fatalf("чтение change_xid %s: %v", id, err)
	}
	x, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		t.Fatalf("разбор change_xid %q: %v", text, err)
	}
	return x
}

// pullAll — один прогон Changes; возвращает страницы.
func pullAll(t *testing.T, h *Hub, scope string, kinds []memory.Kind, from HubCursor, limit int) []HubPage {
	t.Helper()
	var pages []HubPage
	if err := h.Changes(context.Background(), scope, kinds, from, limit, func(p HubPage) error {
		pages = append(pages, p)
		return nil
	}); err != nil {
		t.Fatalf("Changes: %v", err)
	}
	return pages
}

func pageIDs(pages []HubPage) []string {
	var ids []string
	for _, p := range pages {
		for _, r := range p.Records {
			ids = append(ids, r.Memory.Id)
		}
	}
	return ids
}

func TestHub_OpenDoesNoDDL(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t)

	st, err := h.Schema(ctx)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if !st.Empty || st.Version != 0 || st.Supported != pgSchemaVersion || st.Current() || st.Ahead() {
		t.Fatalf("пустая база: Schema = %+v, ожидалась пустая схема", st)
	}

	var objects int
	if err := h.pg.db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema()`).Scan(&objects); err != nil {
		t.Fatalf("подсчёт объектов схемы: %v", err)
	}
	if objects != 0 {
		t.Fatalf("открытие и Schema создали %d объектов, ожидалось 0 (DDL без явного ApplySchema недопустим)", objects)
	}
}

func TestHub_ApplySchema(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)

	st, err := h.Schema(ctx)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if !st.Current() || st.Version != pgSchemaVersion {
		t.Fatalf("после ApplySchema: Schema = %+v, ожидалась текущая версия %d", st, pgSchemaVersion)
	}

	var baseID string
	if err := h.pg.db.QueryRowContext(ctx, `SELECT base_id FROM sync_meta WHERE id = 1`).Scan(&baseID); err != nil || baseID == "" {
		t.Fatalf("sync_meta.base_id = %q, err = %v; ожидался непустой идентификатор базы", baseID, err)
	}

	// Повторное применение идемпотентно и не меняет идентичность базы — иначе у всех
	// клиентов сбрасывались бы курсоры.
	if err := h.ApplySchema(ctx); err != nil {
		t.Fatalf("повторный ApplySchema: %v", err)
	}
	var again string
	if err := h.pg.db.QueryRowContext(ctx, `SELECT base_id FROM sync_meta WHERE id = 1`).Scan(&again); err != nil {
		t.Fatalf("чтение base_id: %v", err)
	}
	if again != baseID {
		t.Fatalf("base_id сменился при повторном ApplySchema: %q -> %q", baseID, again)
	}
}

func TestHub_SchemaBehindIsUpgraded(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	id := hubInsert(t, h.pg.db, "project:X", memory.KindSemantic)

	// База предыдущей версии: шаг номера изменения ещё не применён (версия ниже) —
	// повторный прогон шага должен быть безопасным.
	if _, err := h.pg.db.ExecContext(ctx, `UPDATE schema_version SET version = $1`, pgSchemaVersion-1); err != nil {
		t.Fatalf("понижение версии: %v", err)
	}
	st, err := h.Schema(ctx)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if st.Current() || st.Ahead() || st.Empty {
		t.Fatalf("старая схема: Schema = %+v, ожидалась «старее бинаря»", st)
	}
	if err := h.ApplySchema(ctx); err != nil {
		t.Fatalf("ApplySchema на старой схеме: %v", err)
	}
	if st, _ := h.Schema(ctx); !st.Current() {
		t.Fatalf("после обновления: Schema = %+v", st)
	}
	if changeXID(t, h, id) == 0 {
		t.Fatalf("запись потеряла номер изменения после обновления схемы")
	}
}

func TestHub_SchemaAhead(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	if _, err := h.pg.db.ExecContext(ctx, `UPDATE schema_version SET version = version + 1`); err != nil {
		t.Fatalf("повышение версии: %v", err)
	}

	st, err := h.Schema(ctx)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if !st.Ahead() || st.Current() {
		t.Fatalf("схема новее бинаря: Schema = %+v, ожидалось Ahead", st)
	}
	if err := h.ApplySchema(ctx); err == nil {
		t.Fatalf("ApplySchema на схеме новее бинаря прошёл, ожидалась ошибка «обновите memo»")
	}
	if st, _ := h.Schema(ctx); st.Version != pgSchemaVersion+1 {
		t.Fatalf("ApplySchema тронул версию схемы новее бинаря: %+v", st)
	}
}

// TestHub_ChangeXIDTriggers — номер изменения ставят триггеры сервера, поэтому его
// продвигают и прямые писатели (store с driver = "postgres", сырой SQL).
func TestHub_ChangeXIDTriggers(t *testing.T) {
	ctx := context.Background()
	s := newTestPgStore(t) // путь store.Open: автомиграция включает шаг номера изменения
	h := &Hub{pg: s}

	other := memory.Memory{Id: ulid.Make().String(), Kind: memory.KindSemantic, Scope: "project:X", Content: "адресат", Importance: 3, CreatedAt: 1, UpdatedAt: 1}
	if err := s.Put(ctx, other); err != nil {
		t.Fatalf("Put адресата: %v", err)
	}
	m := memory.Memory{Id: ulid.Make().String(), Kind: memory.KindTask, Scope: "project:X", Content: "задача", Importance: 3, CreatedAt: 1, UpdatedAt: 1, Status: memory.StatusTodo}
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	x := changeXID(t, h, m.Id)
	if x == 0 {
		t.Fatalf("вставка не поставила номер изменения")
	}

	// advanced проверяет, что шаг step продвинул (или не продвинул) номер записи m.
	advanced := func(name string, want bool, step func() error) {
		t.Helper()
		if err := step(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := changeXID(t, h, m.Id)
		if want && got <= x {
			t.Fatalf("%s: номер изменения не вырос (%d -> %d)", name, x, got)
		}
		if !want && got != x {
			t.Fatalf("%s: номер изменения сдвинулся (%d -> %d), а не должен", name, x, got)
		}
		x = got
	}

	advanced("Touch", false, func() error { return s.Touch(ctx, m.Id) })
	advanced("Put без изменений", false, func() error { return s.Put(ctx, m) })
	m.Status = memory.StatusDoing
	m.UpdatedAt = 2
	advanced("Put со сменой стадии", true, func() error { return s.Put(ctx, m) })
	m.Tags = []string{"sync"}
	advanced("Put с новым тегом", true, func() error { return s.Put(ctx, m) })
	advanced("DELETE тега напрямую", true, func() error {
		_, err := s.db.ExecContext(ctx, `DELETE FROM tags WHERE memory_id = $1`, m.Id)
		return err
	})
	advanced("INSERT связи напрямую", true, func() error {
		_, err := s.db.ExecContext(ctx, `INSERT INTO links (src, dst, rel) VALUES ($1, $2, 'part_of')`, m.Id, other.Id)
		return err
	})
	advanced("UPDATE связи напрямую", true, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE links SET rel = 'related' WHERE src = $1`, m.Id)
		return err
	})
	advanced("DELETE связи напрямую", true, func() error {
		_, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE src = $1`, m.Id)
		return err
	})
	advanced("Supersede (тумстоун)", true, func() error { return s.Supersede(ctx, m.Id, m.Id) })

	// Связь двигает номер только записи-владельца (src), не адресата.
	otherXID := changeXID(t, h, other.Id)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO links (src, dst, rel) VALUES ($1, $2, 'related')`, m.Id, other.Id); err != nil {
		t.Fatalf("вставка связи: %v", err)
	}
	if got := changeXID(t, h, other.Id); got != otherXID {
		t.Fatalf("связь сдвинула номер адресата: %d -> %d", otherXID, got)
	}

	// Явное значение при вставке перекрывается — номер ставит только сервер.
	forged := ulid.Make().String()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memories (id, kind, scope, content, created_at, updated_at, change_xid)
		VALUES ($1, 'semantic', 'project:X', 'подделка', 1, 1, '1'::xid8)`, forged); err != nil {
		t.Fatalf("вставка с явным change_xid: %v", err)
	}
	if got := changeXID(t, h, forged); got <= 1 {
		t.Fatalf("явный change_xid при вставке не перекрыт триггером: %d", got)
	}
}

// TestHub_ChangesPaging — постраничная выборка набора отдаёт каждую запись ровно один
// раз, фильтрует scope и виды, отдаёт теги и связи; продолжение с сохранённого курсора
// ничего не теряет.
func TestHub_ChangesPaging(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	db := h.pg.db

	want := map[string]bool{}
	// Отдельные транзакции — разные xid.
	for i := 0; i < 4; i++ {
		want[hubInsert(t, db, "project:X", memory.KindSemantic)] = true
	}
	// Группа одного xid больше страницы — курсор (xid, id) проходит её постранично.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	var group []string
	for i := 0; i < 5; i++ {
		id := hubInsert(t, tx, "project:X", memory.KindTask)
		group = append(group, id)
		want[id] = true
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Вне набора: другой проект и вид, который не синхронизируется.
	foreign := hubInsert(t, db, "project:Y", memory.KindSemantic)
	episode := hubInsert(t, db, "project:X", memory.KindEpisodic)

	if _, err := db.ExecContext(ctx, `INSERT INTO tags (memory_id, tag) VALUES ($1, 'sync')`, group[0]); err != nil {
		t.Fatalf("тег: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO links (src, dst, rel) VALUES ($1, $2, 'part_of')`, group[1], group[0]); err != nil {
		t.Fatalf("связь: %v", err)
	}

	kinds := []memory.Kind{memory.KindSemantic, memory.KindProcedural, memory.KindDebt, memory.KindTask}
	pages := pullAll(t, h, "project:X", kinds, HubCursor{}, 2)

	seen := map[string]int{}
	var prev HubCursor
	var prevPos HubCursor
	for i, p := range pages {
		if len(p.Records) > 2 {
			t.Fatalf("страница %d: %d записей при limit 2", i, len(p.Records))
		}
		if p.Next.less(prev) {
			t.Fatalf("страница %d: курсор убыл %+v -> %+v", i, prev, p.Next)
		}
		prev = p.Next
		for _, r := range p.Records {
			seen[r.Memory.Id]++
			pos := HubCursor{XID: r.XID, ID: r.Memory.Id}
			if !prevPos.less(pos) {
				t.Fatalf("нарушен порядок (change_xid, id): %+v после %+v", pos, prevPos)
			}
			prevPos = pos
			switch r.Memory.Id {
			case group[0]:
				if len(r.Memory.Tags) != 1 || r.Memory.Tags[0] != "sync" {
					t.Fatalf("теги %s: %v", r.Memory.Id, r.Memory.Tags)
				}
			case group[1]:
				if len(r.Memory.Links) != 1 || r.Memory.Links[0].Dst != group[0] || r.Memory.Links[0].Rel != memory.RelPartOf {
					t.Fatalf("связи %s: %v", r.Memory.Id, r.Memory.Links)
				}
			}
		}
	}
	for id := range want {
		if seen[id] != 1 {
			t.Fatalf("запись %s отдана %d раз за прогон, ожидался ровно 1", id, seen[id])
		}
	}
	if seen[foreign] != 0 || seen[episode] != 0 {
		t.Fatalf("утечка вне набора: чужой проект %d, episodic %d", seen[foreign], seen[episode])
	}
	if len(seen) != len(want) {
		t.Fatalf("отдано %d записей, ожидалось %d", len(seen), len(want))
	}

	// Прерванный прогон: первая страница применена, курсор сохранён, дальше сбой.
	stop := errors.New("сбой посреди прогона")
	var saved HubCursor
	first := map[string]bool{}
	err = h.Changes(ctx, "project:X", kinds, HubCursor{}, 2, func(p HubPage) error {
		for _, r := range p.Records {
			first[r.Memory.Id] = true
		}
		saved = p.Next
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("ошибка обработчика страницы не вернулась: %v", err)
	}
	// Продолжение с сохранённого курсора добирает остальное; повтор допустим только
	// для строк окна снимка (saved.ID == "").
	rest := pageIDs(pullAll(t, h, "project:X", kinds, saved, 2))
	got := map[string]bool{}
	for id := range first {
		got[id] = true
	}
	for _, id := range rest {
		if first[id] && saved.ID != "" {
			t.Fatalf("запись %s первой страницы перечитана после курсора %+v", id, saved)
		}
		got[id] = true
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("продолжение с курсора %+v потеряло запись %s", saved, id)
		}
	}

	// Без изменений — одна пустая страница с курсором не меньше прежнего.
	final := pages[len(pages)-1].Next
	again := pullAll(t, h, "project:X", kinds, final, 2)
	if again[len(again)-1].Next.less(final) {
		t.Fatalf("курсор убыл на повторном прогоне: %+v -> %+v", final, again[len(again)-1].Next)
	}
	for _, id := range pageIDs(again) {
		if !want[id] {
			t.Fatalf("повторный прогон отдал неизвестную запись %s", id)
		}
	}

	// Новая запись после курсора видна следующему прогону.
	fresh := hubInsert(t, db, "project:X", memory.KindDebt)
	found := false
	for _, id := range pageIDs(pullAll(t, h, "project:X", kinds, again[len(again)-1].Next, 2)) {
		found = found || id == fresh
	}
	if !found {
		t.Fatalf("новая запись %s не пришла после курсора", fresh)
	}
}

// TestHub_ChangesLateCommitNotLost — строка с меньшим номером, закоммиченная позже
// строки с большим, не теряется: курсор — горизонт снимка, а не последний номер.
func TestHub_ChangesLateCommitNotLost(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	db := h.pg.db

	t1, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx T1: %v", err)
	}
	defer t1.Rollback()
	late := hubInsert(t, t1, "project:X", memory.KindSemantic) // T1 получает xid первой

	early := hubInsert(t, db, "project:X", memory.KindSemantic) // T2: больший xid, коммит сразу

	pages := pullAll(t, h, "project:X", nil, HubCursor{}, 10)
	ids := pageIDs(pages)
	if len(ids) != 1 || ids[0] != early {
		t.Fatalf("до коммита T1 ожидалась только %s, получено %v", early, ids)
	}
	cursor := pages[len(pages)-1].Next

	if err := t1.Commit(); err != nil {
		t.Fatalf("Commit T1: %v", err)
	}
	lateXID, earlyXID := changeXID(t, h, late), changeXID(t, h, early)
	// Меньший xid у поздней строки — ровно тот случай, когда наивный курсор «последний
	// прочитанный номер» (earlyXID) пропустил бы её навсегда.
	if lateXID >= earlyXID {
		t.Fatalf("сценарий не воспроизведён: xid поздней строки %d не меньше %d", lateXID, earlyXID)
	}
	if cursor.XID > lateXID {
		t.Fatalf("курсор %+v ушёл за xid незавершённой транзакции %d", cursor, lateXID)
	}

	found := false
	for _, id := range pageIDs(pullAll(t, h, "project:X", nil, cursor, 10)) {
		found = found || id == late
	}
	if !found {
		t.Fatalf("строка %s поздней транзакции потеряна после курсора %+v", late, cursor)
	}
}

func TestHub_ChangesRejectsNonPositiveLimit(t *testing.T) {
	h := newTestHubApplied(t)
	err := h.Changes(context.Background(), "project:X", nil, HubCursor{}, 0, func(HubPage) error { return nil })
	if err == nil {
		t.Fatalf("limit 0 принят, ожидалась ошибка")
	}
}
