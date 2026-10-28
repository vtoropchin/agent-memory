package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	memsync "agentmemory/internal/memory/sync"
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
func pullAll(t *testing.T, h *Hub, scope string, kinds []memory.Kind, from memsync.Cursor, limit int) []memsync.Page {
	t.Helper()
	var pages []memsync.Page
	if err := h.Pull(context.Background(), scope, kinds, from, limit, func(p memsync.Page) error {
		pages = append(pages, p)
		return nil
	}); err != nil {
		t.Fatalf("Changes: %v", err)
	}
	return pages
}

func pageIDs(pages []memsync.Page) []string {
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
	advanced("Supersede (тумстоун)", true, func() error { return s.Supersede(ctx, m.Id, m.Id, "") })

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
	pages := pullAll(t, h, "project:X", kinds, memsync.Cursor{}, 2)

	seen := map[string]int{}
	var prev memsync.Cursor
	var prevPos memsync.Cursor
	for i, p := range pages {
		if len(p.Records) > 2 {
			t.Fatalf("страница %d: %d записей при limit 2", i, len(p.Records))
		}
		if cursorLess(p.Next, prev) {
			t.Fatalf("страница %d: курсор убыл %+v -> %+v", i, prev, p.Next)
		}
		prev = p.Next
		for _, r := range p.Records {
			seen[r.Memory.Id]++
			pos := memsync.Cursor{XID: r.XID, ID: r.Memory.Id}
			if !cursorLess(prevPos, pos) {
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
	var saved memsync.Cursor
	first := map[string]bool{}
	err = h.Pull(ctx, "project:X", kinds, memsync.Cursor{}, 2, func(p memsync.Page) error {
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
	if cursorLess(again[len(again)-1].Next, final) {
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

	pages := pullAll(t, h, "project:X", nil, memsync.Cursor{}, 10)
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
	err := h.Pull(context.Background(), "project:X", nil, memsync.Cursor{}, 0, func(memsync.Page) error { return nil })
	if err == nil {
		t.Fatalf("limit 0 принят, ожидалась ошибка")
	}
}

// hubMem — запись для Push: semantic в scope, updated_at = at.
func hubMem(scope, content string, at int64, links ...memory.Link) memory.Memory {
	return memory.Memory{Id: ulid.Make().String(), Kind: memory.KindSemantic, Scope: scope, Content: content,
		Importance: 3, CreatedAt: at, UpdatedAt: at, Author: "alice", UpdatedBy: "alice", Links: links}
}

// TestHub_Base: пустая база — версия 0; после ApplySchema — base_id, версия бинаря,
// наибольший change_xid не больше xmax снимка.
func TestHub_Base(t *testing.T) {
	ctx := context.Background()
	h := newTestHub(t)
	b, err := h.Base(ctx)
	if err != nil {
		t.Fatalf("Base пустой базы: %v", err)
	}
	if b.Version != 0 || b.ID != "" || b.XMax == 0 {
		t.Fatalf("Base пустой базы = %+v, хочу версию 0 и xmax снимка", b)
	}

	if err := h.ApplySchema(ctx); err != nil {
		t.Fatalf("ApplySchema: %v", err)
	}
	id := hubInsert(t, h.pg.db, "project:X", memory.KindSemantic)
	b, err = h.Base(ctx)
	if err != nil {
		t.Fatalf("Base: %v", err)
	}
	if b.Version != pgSchemaVersion || b.ID == "" || b.MaxXID != changeXID(t, h, id) || b.MaxXID > b.XMax {
		t.Fatalf("Base = %+v, хочу версию %d, base_id и MaxXID = change_xid последней записи <= xmax", b, pgSchemaVersion)
	}
}

// TestHub_PullRefs: Refs страницы — адресаты связей её записей вне страницы любого
// scope и вида, с тегами; адресат внутри страницы в Refs не попадает.
func TestHub_PullRefs(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	episode := hubMem("project:X", "эпизод", 1)
	episode.Kind, episode.Tags = memory.KindEpisodic, []string{"t"}
	user := hubMem("user:*", "общий факт", 1)
	inPage := hubMem("project:X", "сосед", 1)
	src := hubMem("project:X", "запись", 2, memory.Link{Dst: episode.Id}, memory.Link{Dst: user.Id}, memory.Link{Dst: inPage.Id})
	recs := []memsync.Guarded{{Memory: episode}, {Memory: user}, {Memory: inPage}, {Memory: src}}
	if written, stale, err := h.Push(ctx, recs); err != nil || len(written) != 4 || len(stale) != 0 {
		t.Fatalf("Push = %v, %v, %v", written, stale, err)
	}

	pages := pullAll(t, h, "project:X", []memory.Kind{memory.KindSemantic}, memsync.Cursor{}, 10)
	if len(pages) != 1 || len(pages[0].Records) != 2 {
		t.Fatalf("Pull = %+v, хочу одну страницу с двумя записями набора", pages)
	}
	refs := map[string]memsync.Remote{}
	for _, r := range pages[0].Refs {
		refs[r.Memory.Id] = r
	}
	if len(refs) != 2 || refs[episode.Id].XID == 0 || len(refs[episode.Id].Memory.Tags) != 1 || refs[user.Id].Memory.Scope != "user:*" {
		t.Fatalf("Refs = %+v, хочу эпизод (с тегом и xid) и запись user:*, без соседа по странице", pages[0].Refs)
	}
}

// TestHub_PushCAS: новая запись — только вставкой; правка — по change_xid, который
// видел клиент; accessed_at/access_count общей базы не перезаписываются; связь на
// запись той же пачки проходит FK; Versions отдаёт версии и Refs.
func TestHub_PushCAS(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	dst := hubMem("project:X", "адресат", 1)
	src := hubMem("project:X", "запись", 1, memory.Link{Dst: dst.Id, Rel: memory.RelPartOf})
	src.Tags = []string{"a", "b"}
	src.AccessCount = 42 // локальный сигнал отправителя в общую базу не идёт
	written, stale, err := h.Push(ctx, []memsync.Guarded{{Memory: src}, {Memory: dst}})
	if err != nil || len(written) != 2 || len(stale) != 0 {
		t.Fatalf("Push новых = %v, %v, %v", written, stale, err)
	}

	snap, err := h.Versions(ctx, []string{src.Id, ulid.Make().String()})
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(snap.Records) != 1 || len(snap.Refs) != 1 || snap.Refs[0].Memory.Id != dst.Id {
		t.Fatalf("Versions = %+v, хочу запись и её адресат в Refs", snap)
	}
	got := snap.Records[0]
	if got.XID != changeXID(t, h, src.Id) || got.Memory.AccessCount != 0 || len(got.Memory.Tags) != 2 ||
		len(got.Memory.Links) != 1 || got.Memory.Links[0].Rel != memory.RelPartOf || got.Memory.Author != "alice" {
		t.Fatalf("версия в общей базе = %+v", got)
	}

	if _, err := h.pg.db.ExecContext(ctx, `UPDATE memories SET access_count = 9 WHERE id = $1`, src.Id); err != nil {
		t.Fatalf("access_count: %v", err)
	}
	v2 := src
	v2.Content, v2.UpdatedAt, v2.UpdatedBy, v2.Tags = "правка", 2, "bob", []string{"c"}
	if written, stale, err := h.Push(ctx, []memsync.Guarded{{Memory: v2, ExpectXID: got.XID}, {Memory: dst}}); err != nil || len(written) != 1 || len(stale) != 1 || stale[0] != dst.Id {
		t.Fatalf("Push правки = %v, %v, %v; хочу правку записанной, повторную вставку адресата — stale", written, stale, err)
	}
	snap, _ = h.Versions(ctx, []string{src.Id})
	after := snap.Records[0]
	if after.Memory.Content != "правка" || after.Memory.UpdatedBy != "bob" || after.Memory.AccessCount != 9 ||
		len(after.Memory.Tags) != 1 || after.XID == got.XID {
		t.Fatalf("после правки = %+v, хочу новый текст, прежний access_count 9 и новый change_xid", after)
	}

	v3 := v2
	v3.Content = "устаревшая правка"
	if written, stale, err := h.Push(ctx, []memsync.Guarded{{Memory: v3, ExpectXID: got.XID}}); err != nil || len(written) != 0 || len(stale) != 1 {
		t.Fatalf("Push по старому change_xid = %v, %v, %v; хочу stale", written, stale, err)
	}
	if snap, _ = h.Versions(ctx, []string{src.Id}); snap.Records[0].Memory.Content != "правка" {
		t.Fatalf("stale-правка записалась: %+v", snap.Records[0].Memory)
	}
}

// TestHub_PushResetsVector: смена текста при push удаляет строку вектора общей базы
// (её открывают и как driver = "postgres"), правка без смены текста — нет.
func TestHub_PushResetsVector(t *testing.T) {
	ctx := context.Background()
	s := newTestPgStoreWithVector(t)
	if err := s.ResetVectorIndex(ctx, "test", 2); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}
	h := newHub(s.db)
	m := hubMem("project:X", "текст", 1)
	if _, _, err := h.Push(ctx, []memsync.Guarded{{Memory: m}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	embed := func() {
		t.Helper()
		if err := s.PutVector(ctx, m.Id, []float32{1, 0}); err != nil {
			t.Fatalf("PutVector: %v", err)
		}
	}
	push := func() {
		t.Helper()
		snap, err := h.Versions(ctx, []string{m.Id})
		if err != nil || len(snap.Records) != 1 {
			t.Fatalf("Versions: %+v, %v", snap, err)
		}
		m.UpdatedAt++
		if w, _, err := h.Push(ctx, []memsync.Guarded{{Memory: m, ExpectXID: snap.Records[0].XID}}); err != nil || len(w) != 1 {
			t.Fatalf("Push: %v, %v", w, err)
		}
	}
	embedded := func() bool {
		t.Helper()
		vs, err := s.VectorStatus(ctx, m.Id)
		if err != nil {
			t.Fatalf("VectorStatus: %v", err)
		}
		return vs.Embedded
	}

	embed()
	m.Importance = 5
	push()
	if !embedded() {
		t.Fatalf("вектор сброшен без смены текста")
	}
	m.Content = "другой текст"
	push()
	if embedded() {
		t.Fatalf("вектор не сброшен при смене текста")
	}
}

// TestHub_PushCrossLinksNoWait: строка, которую держит CAS-блокировка параллельного
// Push (pushLockQuery), не мешает вставить связь на неё — проверка FK берёт KEY
// SHARE. Иначе два писателя с перекрёстными связями X→Y и Y→X взаимно
// заблокировались бы во втором проходе.
func TestHub_PushCrossLinksNoWait(t *testing.T) {
	ctx := context.Background()
	h := newTestHubApplied(t)
	x, y := hubMem("project:X", "x", 1), hubMem("project:X", "y", 1)
	if _, _, err := h.Push(ctx, []memsync.Guarded{{Memory: x}, {Memory: y}}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	snap, err := h.Versions(ctx, []string{x.Id, y.Id})
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	xids := map[string]uint64{}
	for _, r := range snap.Records {
		xids[r.Memory.Id] = r.XID
	}

	other, err := h.pg.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Rollback()
	var content string
	if err := other.QueryRowContext(ctx, pushLockQuery, y.Id, strconv.FormatUint(xids[y.Id], 10)).Scan(&content); err != nil {
		t.Fatalf("блокировка y: %v", err)
	}

	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	x.Links, x.UpdatedAt = []memory.Link{{Dst: y.Id}}, 2
	written, _, err := h.Push(wait, []memsync.Guarded{{Memory: x, ExpectXID: xids[x.Id]}})
	if err != nil || len(written) != 1 {
		t.Fatalf("Push связи на заблокированную строку = %v, %v; хочу запись без ожидания", written, err)
	}
}
