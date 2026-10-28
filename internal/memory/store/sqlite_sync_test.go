package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
)

// newSyncTestStore открывает чистую SQLite-БД во временном каталоге.
func newSyncTestStore(t *testing.T) (*sqliteStore, *SyncLocal) {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, s.SyncLocal()
}

// putSyncMem кладёт запись kind в scope и возвращает её.
func putSyncMem(t *testing.T, s *sqliteStore, scope string, kind memory.Kind, content string) memory.Memory {
	t.Helper()
	m := memory.Memory{Id: ulid.Make().String(), Kind: kind, Scope: scope, Content: content,
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000}
	if err := s.Put(context.Background(), m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return m
}

// dirtyIDs — id помеченных записей scope (любых видов) в порядке пометки.
func dirtyIDs(t *testing.T, l *SyncLocal, scope string) []string {
	t.Helper()
	ps, err := l.Dirty(context.Background(), scope, nil, 0)
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.Memory.Id
	}
	return ids
}

func TestSyncLocalOf(t *testing.T) {
	s, _ := newSyncTestStore(t)
	if _, ok := SyncLocalOf(s); !ok {
		t.Error("SyncLocalOf(sqlite) = false, хочу true")
	}
	if _, ok := SyncLocalOf(&pgStore{}); ok {
		t.Error("SyncLocalOf(postgres) = true: синхронизация при driver=postgres недоступна")
	}
}

func TestSyncProfile(t *testing.T) {
	ctx := context.Background()
	_, l := newSyncTestStore(t)

	p, err := l.Profile(ctx)
	if err != nil {
		t.Fatalf("Profile: %v", err)
	}
	if p != (SyncProfile{IntervalSec: 300}) {
		t.Errorf("профиль по умолчанию = %+v, хочу пустое имя и 300 с", p)
	}

	if err := l.SetProfile(ctx, SyncProfile{AuthorName: " Иван ", IntervalSec: 120}); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	if p, _ = l.Profile(ctx); p != (SyncProfile{AuthorName: "Иван", IntervalSec: 120}) {
		t.Errorf("Profile = %+v, хочу Иван/120", p)
	}

	if err := l.SetProfile(ctx, SyncProfile{}); err != nil {
		t.Fatalf("SetProfile: %v", err)
	}
	if p, _ = l.Profile(ctx); p != (SyncProfile{IntervalSec: 300}) {
		t.Errorf("сброс профиля: %+v, хочу пустое имя и 300 с", p)
	}
}

func TestSyncTargetsCRUD(t *testing.T) {
	ctx := context.Background()
	_, l := newSyncTestStore(t)

	if _, err := l.Target(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Target несуществующей цели: %v, хочу ErrNotFound", err)
	}

	got, err := l.PutTarget(ctx, SyncTarget{Project: "alpha", Host: "pg.lan", User: "memo", DBName: "team", Push: true, PasswordEnv: "игнорируется"})
	if err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	want := SyncTarget{Project: "alpha", Host: "pg.lan", Port: 5432, User: "memo", DBName: "team", SSLMode: "prefer",
		PasswordEnv: "MEMO_SYNC_PASSWORD_ALPHA", Push: true}
	if got != want {
		t.Errorf("PutTarget = %+v, хочу %+v", got, want)
	}
	if read, _ := l.Target(ctx, "alpha"); read != want {
		t.Errorf("Target = %+v, хочу %+v", read, want)
	}

	// Правка: параметры меняются, имя переменной пароля — нет.
	upd, err := l.PutTarget(ctx, SyncTarget{Project: "alpha", Host: "pg2.lan", Port: 6432, User: "u", DBName: "d", SSLMode: "require", Pull: true})
	if err != nil {
		t.Fatalf("PutTarget (правка): %v", err)
	}
	want = SyncTarget{Project: "alpha", Host: "pg2.lan", Port: 6432, User: "u", DBName: "d", SSLMode: "require",
		PasswordEnv: "MEMO_SYNC_PASSWORD_ALPHA", Pull: true}
	if upd != want {
		t.Errorf("PutTarget (правка) = %+v, хочу %+v", upd, want)
	}

	// Совпадение нормализованных имён — суффикс.
	for _, c := range []struct{ project, env string }{
		{"Alpha", "MEMO_SYNC_PASSWORD_ALPHA_2"},
		{"ALPHA", "MEMO_SYNC_PASSWORD_ALPHA_3"},
		{"al-pha", "MEMO_SYNC_PASSWORD_AL_PHA"},
	} {
		got, err := l.PutTarget(ctx, SyncTarget{Project: c.project, Host: "h", User: "u", DBName: "d"})
		if err != nil {
			t.Fatalf("PutTarget %q: %v", c.project, err)
		}
		if got.PasswordEnv != c.env {
			t.Errorf("PasswordEnv(%q) = %q, хочу %q", c.project, got.PasswordEnv, c.env)
		}
	}

	all, err := l.Targets(ctx)
	if err != nil {
		t.Fatalf("Targets: %v", err)
	}
	var names []string
	for _, tg := range all {
		names = append(names, tg.Project)
	}
	if len(names) != 4 || names[0] != "ALPHA" || names[3] != "alpha" {
		t.Errorf("Targets = %v, хочу 4 цели по возрастанию имени", names)
	}

	if err := l.DeleteTarget(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if _, err := l.Target(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Target после удаления: %v, хочу ErrNotFound", err)
	}
	if err := l.DeleteTarget(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("повторный DeleteTarget: %v, хочу ErrNotFound", err)
	}
}

func TestSyncState(t *testing.T) {
	ctx := context.Background()
	_, l := newSyncTestStore(t)

	if _, err := l.State(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("State до прогонов: %v, хочу ErrNotFound", err)
	}

	st := SyncState{BaseID: "b-1", PullXID: 1<<63 + 5, PullID: "01ABC", PushMarked: true, LastRunAt: 2000, LastOkAt: 1900,
		LastError: "сервер недоступен", LastPushed: 3, LastPulled: 4, LastConflicts: 1, ConflictsTotal: 7}
	if err := l.SaveState(ctx, "alpha", st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if got, err := l.State(ctx, "alpha"); err != nil || got != st {
		t.Errorf("State = %+v, %v; хочу %+v", got, err, st)
	}

	st = SyncState{BaseID: "b-2", LastRunAt: 3000, LastOkAt: 3000}
	if err := l.SaveState(ctx, "alpha", st); err != nil {
		t.Fatalf("SaveState (перезапись): %v", err)
	}
	if got, _ := l.State(ctx, "alpha"); got != st {
		t.Errorf("State после перезаписи = %+v, хочу %+v", got, st)
	}

	// Удаление цели стирает её состояние.
	if _, err := l.PutTarget(ctx, SyncTarget{Project: "alpha", Host: "h", User: "u", DBName: "d"}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	if err := l.DeleteTarget(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if _, err := l.State(ctx, "alpha"); !errors.Is(err, ErrNotFound) {
		t.Errorf("State после удаления цели: %v, хочу ErrNotFound", err)
	}
}

func TestSyncLease(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	now := int64(1000)
	s.now = func() int64 { return now }

	ok, err := l.Lease(ctx, "alpha", "proc-A", now+300)
	if err != nil || !ok {
		t.Fatalf("первый захват: %v, %v; хочу true", ok, err)
	}
	if ok, _ := l.Lease(ctx, "alpha", "proc-B", now+300); ok {
		t.Error("захват занятого lease чужим процессом должен отклоняться")
	}
	if ok, _ := l.Lease(ctx, "alpha", "proc-A", now+600); !ok {
		t.Error("свой lease должен продлеваться")
	}
	if ok, _ := l.Lease(ctx, "beta", "proc-B", now+300); !ok {
		t.Error("lease другой цели независим")
	}

	now += 601 // срок proc-A истёк
	if ok, _ := l.Lease(ctx, "alpha", "proc-B", now+300); !ok {
		t.Error("истёкший lease должен перехватываться")
	}
	if ok, _ := l.Lease(ctx, "alpha", "proc-A", now+300); ok {
		t.Error("перехваченный lease прежний держатель вернуть не может")
	}

	// Release чужого не снимает, своего — снимает.
	if err := l.Release(ctx, "alpha", "proc-A"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if ok, _ := l.Lease(ctx, "alpha", "proc-A", now+300); ok {
		t.Error("Release чужим держателем не должен снимать lease")
	}
	if err := l.Release(ctx, "alpha", "proc-B"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if ok, _ := l.Lease(ctx, "alpha", "proc-A", now+300); !ok {
		t.Error("после Release цель должна захватываться")
	}
}

func TestSyncSchemaAhead(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	if ahead, err := l.SchemaAhead(ctx); err != nil || ahead {
		t.Fatalf("SchemaAhead на своей версии = %v, %v; хочу false", ahead, err)
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	if ahead, err := l.SchemaAhead(ctx); err != nil || !ahead {
		t.Errorf("SchemaAhead на версии новее бинаря = %v, %v; хочу true", ahead, err)
	}
}

// TestSyncDirty_Triggers: изменения записей scope цели помечаются любым путём
// (вставка, правка, теги, связи, Supersede), Touch и чужие scope — нет.
func TestSyncDirty_Triggers(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	const scope = "project:alpha"

	before := putSyncMem(t, s, scope, memory.KindSemantic, "до появления цели")
	if _, err := l.PutTarget(ctx, SyncTarget{Project: "alpha", Host: "h", User: "u", DBName: "d", Push: true}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 0 {
		t.Fatalf("запись до появления цели помечена: %v", ids)
	}

	other := putSyncMem(t, s, "project:beta", memory.KindSemantic, "чужой проект")
	created := putSyncMem(t, s, scope, memory.KindSemantic, "новая запись")
	ps, err := l.Dirty(ctx, scope, nil, 0)
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	if len(ps) != 1 || ps[0].Memory.Id != created.Id || !ps[0].Edited || ps[0].Memory.Content != "новая запись" {
		t.Fatalf("Dirty после вставки = %+v, хочу одну правку %s", ps, created.Id)
	}
	if ids := dirtyIDs(t, l, "project:beta"); len(ids) != 0 {
		t.Errorf("чужой scope помечен: %v (%s)", ids, other.Id)
	}

	// Touch — не правка.
	if err := s.Touch(ctx, before.Id); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 1 {
		t.Errorf("Touch пометил запись: %v", ids)
	}

	// Supersede — правка.
	if err := s.Supersede(ctx, before.Id, before.Id, "Иван"); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 2 || ids[1] != before.Id {
		t.Errorf("Supersede не пометил запись: %v", ids)
	}

	// Пометка снимается по seq и возвращается после новой правки тегов и связей.
	ps, _ = l.Dirty(ctx, scope, nil, 0)
	if err := l.MarkPushed(ctx, ps); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 0 {
		t.Fatalf("MarkPushed не снял пометки: %v", ids)
	}
	if _, err := s.db.Exec(`INSERT INTO tags (memory_id, tag) VALUES (?, 'x')`, created.Id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO links (src, dst, rel) VALUES (?, ?, 'related')`, before.Id, created.Id); err != nil {
		t.Fatal(err)
	}
	ps, _ = l.Dirty(ctx, scope, nil, 0)
	if len(ps) != 2 {
		t.Fatalf("теги/связи не пометили владельцев: %+v", ps)
	}
	byID := map[string]SyncPending{}
	for _, p := range ps {
		byID[p.Memory.Id] = p
	}
	if tags := byID[created.Id].Memory.Tags; len(tags) != 1 || tags[0] != "x" {
		t.Errorf("Dirty без тегов записи: %v", tags)
	}
	if links := byID[before.Id].Memory.Links; len(links) != 1 || links[0].Dst != created.Id {
		t.Errorf("Dirty без связей записи: %v", links)
	}
	if err := l.MarkPushed(ctx, ps); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM tags WHERE memory_id = ?`, created.Id); err != nil {
		t.Fatal(err)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 1 || ids[0] != created.Id {
		t.Errorf("удаление тега не пометило владельца: %v", ids)
	}
}

// TestSyncDirty_UpdateWithoutChangesNotMarked: UPDATE синхронизируемых колонок
// теми же значениями пометку не ставит.
func TestSyncDirty_UpdateWithoutChangesNotMarked(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	if _, err := l.PutTarget(ctx, SyncTarget{Project: "alpha", Host: "h", User: "u", DBName: "d"}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	m := putSyncMem(t, s, "project:alpha", memory.KindSemantic, "факт")
	ps, _ := l.Dirty(ctx, "project:alpha", nil, 0)
	if err := l.MarkPushed(ctx, ps); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE memories SET content = content, updated_at = updated_at WHERE id = ?`, m.Id); err != nil {
		t.Fatal(err)
	}
	if ids := dirtyIDs(t, l, "project:alpha"); len(ids) != 0 {
		t.Errorf("UPDATE без изменения значений пометил запись: %v", ids)
	}
}

// TestSyncDirty_SeqGuard: правка после чтения Dirty получает новый seq, и
// MarkPushed со старым seq её не снимает.
func TestSyncDirty_SeqGuard(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	if _, err := l.PutTarget(ctx, SyncTarget{Project: "alpha", Host: "h", User: "u", DBName: "d"}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	m := putSyncMem(t, s, "project:alpha", memory.KindSemantic, "версия 1")
	read, _ := l.Dirty(ctx, "project:alpha", nil, 0)

	m.Content, m.UpdatedAt = "версия 2", 1001
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := l.MarkPushed(ctx, read); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	ps, _ := l.Dirty(ctx, "project:alpha", nil, 0)
	if len(ps) != 1 || ps[0].Memory.Content != "версия 2" || ps[0].Seq <= read[0].Seq {
		t.Errorf("правка после чтения потеряна: %+v (прочитан seq %d)", ps, read[0].Seq)
	}
}

// TestSyncDirty_MarkAndKinds: Mark размечает набор (edited = 0), не затирая
// правок; Dirty отдаёт только виды набора и снимает пометки остальных; limit
// режет выдачу в порядке пометки.
func TestSyncDirty_MarkAndKinds(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	const scope = "project:alpha"
	set := []memory.Kind{memory.KindSemantic, memory.KindTask}

	fact := putSyncMem(t, s, scope, memory.KindSemantic, "факт")
	task := putSyncMem(t, s, scope, memory.KindTask, "задача")
	episode := putSyncMem(t, s, scope, memory.KindEpisodic, "эпизод")
	putSyncMem(t, s, "project:beta", memory.KindSemantic, "чужой")

	if err := l.Mark(ctx, scope, set, []string{}); err != nil {
		t.Fatalf("Mark(пустой список): %v", err)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 0 {
		t.Fatalf("Mark с пустым списком id что-то пометил: %v", ids)
	}

	if err := l.Mark(ctx, scope, set, []string{task.Id, episode.Id}); err != nil {
		t.Fatalf("Mark(ids): %v", err)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 1 || ids[0] != task.Id {
		t.Fatalf("Mark(ids) = %v, хочу только задачу набора", ids)
	}

	if err := l.Mark(ctx, scope, set, nil); err != nil {
		t.Fatalf("Mark(весь набор): %v", err)
	}
	ps, err := l.Dirty(ctx, scope, set, 0)
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	if len(ps) != 2 || ps[0].Memory.Id != task.Id || ps[1].Memory.Id != fact.Id || ps[0].Edited || ps[1].Edited {
		t.Fatalf("Dirty после разметки = %+v, хочу задачу и факт с edited=false", ps)
	}

	// Правка поверх разметки — edited = 1; повторная разметка её не затирает.
	if _, err := l.PutTarget(ctx, SyncTarget{Project: "alpha", Host: "h", User: "u", DBName: "d"}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
	fact.Content, fact.UpdatedAt = "факт v2", 1001
	if err := s.Put(ctx, fact); err != nil {
		t.Fatalf("Put: %v", err)
	}
	putSyncMem(t, s, scope, memory.KindEpisodic, "ещё эпизод") // триггер пометит, Dirty снимет
	if err := l.Mark(ctx, scope, set, nil); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	ps, _ = l.Dirty(ctx, scope, set, 1)
	if len(ps) != 1 || ps[0].Memory.Id != task.Id {
		t.Fatalf("Dirty(limit 1) = %+v, хочу первую по порядку пометки задачу", ps)
	}
	ps, _ = l.Dirty(ctx, scope, set, 0)
	if len(ps) != 2 || ps[1].Memory.Id != fact.Id || !ps[1].Edited {
		t.Errorf("правка поверх разметки потеряла edited: %+v", ps)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 2 {
		t.Errorf("пометки видов вне набора не сняты: %v", ids)
	}

	// Удаление цели снимает пометки проекта.
	if err := l.DeleteTarget(ctx, "alpha"); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}
	if ids := dirtyIDs(t, l, scope); len(ids) != 0 {
		t.Errorf("пометки удалённой цели остались: %v", ids)
	}
}

// TestSyncTables_ExistingDBMigrates: БД, созданная до появления таблиц sync_*,
// получает их и триггеры на Open без потери записей.
func TestSyncTables_ExistingDBMigrates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	m := putSyncMem(t, s, "project:alpha", memory.KindSemantic, "старый факт")
	for _, stmt := range []string{
		`DROP TRIGGER sync_memories_ai`, `DROP TRIGGER sync_memories_au`, `DROP TRIGGER sync_tags_ai`,
		`DROP TRIGGER sync_tags_ad`, `DROP TRIGGER sync_links_ai`, `DROP TRIGGER sync_links_ad`,
		`DROP TABLE sync_profile`, `DROP TABLE sync_targets`, `DROP TABLE sync_state`,
		`DROP TABLE sync_dirty`, `DROP TABLE sync_lease`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	s.Close()

	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("повторный OpenSQLite: %v", err)
	}
	defer s.Close()
	got, err := s.Get(ctx, m.Id)
	if err != nil || got.Content != "старый факт" {
		t.Fatalf("запись после миграции: %+v, %v", got, err)
	}
	l := s.SyncLocal()
	if _, err := l.PutTarget(ctx, SyncTarget{Project: "alpha", Host: "h", User: "u", DBName: "d"}); err != nil {
		t.Fatalf("PutTarget после миграции: %v", err)
	}
	if err := l.Mark(ctx, "project:alpha", nil, nil); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if ids := dirtyIDs(t, l, "project:alpha"); len(ids) != 1 || ids[0] != m.Id {
		t.Errorf("разметка существующей записи после миграции: %v", ids)
	}
}
