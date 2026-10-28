package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
	memsync "agentmemory/internal/memory/sync"
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
	ps, err := l.Dirty(context.Background(), scope, nil, 0, 0)
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

	if st, err := l.State(ctx, "alpha"); err != nil || st != (memsync.State{}) {
		t.Fatalf("State до прогонов = %+v, %v; хочу нулевое", st, err)
	}

	st := memsync.State{BaseID: "b-1", Cursor: memsync.Cursor{XID: 1<<63 + 5, ID: "01ABC"}, PushMarked: true, LastRunAt: 2000, LastOKAt: 1900,
		LastError: "сервер недоступен", LastPushed: 3, LastPulled: 4, LastConflicts: 1, ConflictsTotal: 7}
	if err := l.SaveState(ctx, "alpha", st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	if got, err := l.State(ctx, "alpha"); err != nil || got != st {
		t.Errorf("State = %+v, %v; хочу %+v", got, err, st)
	}

	st = memsync.State{BaseID: "b-2", LastRunAt: 3000, LastOKAt: 3000}
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
	if st, err := l.State(ctx, "alpha"); err != nil || st != (memsync.State{}) {
		t.Errorf("State после удаления цели = %+v, %v; хочу нулевое", st, err)
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
	ps, err := l.Dirty(ctx, scope, nil, 0, 0)
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
	ps, _ = l.Dirty(ctx, scope, nil, 0, 0)
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
	ps, _ = l.Dirty(ctx, scope, nil, 0, 0)
	if len(ps) != 2 {
		t.Fatalf("теги/связи не пометили владельцев: %+v", ps)
	}
	byID := map[string]memsync.Pending{}
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
	ps, _ := l.Dirty(ctx, "project:alpha", nil, 0, 0)
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
	read, _ := l.Dirty(ctx, "project:alpha", nil, 0, 0)

	m.Content, m.UpdatedAt = "версия 2", 1001
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := l.MarkPushed(ctx, read); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	ps, _ := l.Dirty(ctx, "project:alpha", nil, 0, 0)
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
	first, _ := l.Dirty(ctx, scope, set, 0, 0)
	if len(first) != 1 || first[0].Memory.Id != task.Id {
		t.Fatalf("Mark(ids) = %+v, хочу только задачу набора", first)
	}

	// Повторная разметка даёт уже помеченной записи новый seq.
	if err := l.Mark(ctx, scope, set, nil); err != nil {
		t.Fatalf("Mark(весь набор): %v", err)
	}
	ps, err := l.Dirty(ctx, scope, set, 0, 0)
	if err != nil {
		t.Fatalf("Dirty: %v", err)
	}
	if len(ps) != 2 || ps[0].Edited || ps[1].Edited {
		t.Fatalf("Dirty после разметки = %+v, хочу задачу и факт с edited=false", ps)
	}
	for _, p := range ps {
		if p.Memory.Id == task.Id && p.Seq <= first[0].Seq {
			t.Errorf("повторная разметка не сменила seq: %d -> %d", first[0].Seq, p.Seq)
		}
	}
	if after, _ := l.Dirty(ctx, scope, set, ps[0].Seq, 0); len(after) != 1 || after[0].Memory.Id != ps[1].Memory.Id {
		t.Errorf("Dirty(after = %d) = %+v, хочу только %s", ps[0].Seq, after, ps[1].Memory.Id)
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
	all, _ := l.Dirty(ctx, scope, set, 0, 0)
	if ps, _ = l.Dirty(ctx, scope, set, 0, 1); len(ps) != 1 || len(all) != 2 || ps[0].Memory.Id != all[0].Memory.Id {
		t.Fatalf("Dirty(limit 1) = %+v, хочу первую по порядку пометки из %+v", ps, all)
	}
	for _, p := range all {
		if p.Memory.Id == fact.Id && !p.Edited {
			t.Errorf("правка поверх разметки потеряла edited: %+v", p)
		}
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

// putTarget заводит цель alpha (push и pull) — с ней триггеры помечают записи
// scope project:alpha.
func putTarget(t *testing.T, l *SyncLocal) {
	t.Helper()
	if _, err := l.PutTarget(context.Background(), SyncTarget{Project: "alpha", Host: "h", User: "u", DBName: "d", Push: true, Pull: true}); err != nil {
		t.Fatalf("PutTarget: %v", err)
	}
}

// TestSyncLocal_Versions: записи с тегами, связями и пометкой; тумстоун тоже;
// отсутствующие пропущены, у записи без пометки Seq = 0.
func TestSyncLocal_Versions(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	putTarget(t, l)
	dst := putSyncMem(t, s, "project:alpha", memory.KindSemantic, "адресат")
	src := memory.Memory{Id: ulid.Make().String(), Kind: memory.KindTask, Scope: "project:alpha", Content: "задача",
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000, Tags: []string{"a"}, Links: []memory.Link{{Dst: dst.Id, Rel: memory.RelRelated}}}
	if err := s.Put(ctx, src); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := l.MarkPushed(ctx, []memsync.Pending{mustVersion(t, l, dst.Id)}); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	if err := s.Supersede(ctx, dst.Id, "", ""); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	got, err := l.Versions(ctx, []string{src.Id, dst.Id, ulid.Make().String()})
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Versions = %d записей, хочу 2 (отсутствующая пропущена)", len(got))
	}
	p := got[src.Id]
	if p.Seq == 0 || !p.Edited || len(p.Memory.Tags) != 1 || len(p.Memory.Links) != 1 || p.Memory.Links[0].Dst != dst.Id {
		t.Errorf("версия задачи = %+v, хочу пометку правки, тег и связь", p)
	}
	if d := got[dst.Id]; d.Memory.SupersededBy == nil || d.Seq == 0 {
		t.Errorf("тумстоун = %+v, хочу superseded_by и пометку правки", d)
	}
	if empty, err := l.Versions(ctx, nil); err != nil || len(empty) != 0 {
		t.Errorf("Versions(nil) = %v, %v", empty, err)
	}
}

func mustVersion(t *testing.T, l *SyncLocal, id string) memsync.Pending {
	t.Helper()
	v, err := l.Versions(context.Background(), []string{id})
	if err != nil || len(v) != 1 {
		t.Fatalf("Versions(%s) = %v, %v", id, v, err)
	}
	return v[id]
}

// TestSyncLocal_StampAuthor: пустые author/updated_by заполняются, заданные —
// нет; updated_at и пометки (seq, edited) прежние, без пометки — без пометки.
func TestSyncLocal_StampAuthor(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	putTarget(t, l)
	marked := putSyncMem(t, s, "project:alpha", memory.KindSemantic, "помечена правкой")
	clean := putSyncMem(t, s, "project:alpha", memory.KindSemantic, "без пометки")
	half := memory.Memory{Id: ulid.Make().String(), Kind: memory.KindSemantic, Scope: "project:alpha", Content: "автор есть",
		Importance: 3, CreatedAt: 1000, UpdatedAt: 1000, Author: "bob"}
	if err := s.Put(ctx, half); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := l.MarkPushed(ctx, []memsync.Pending{mustVersion(t, l, clean.Id)}); err != nil {
		t.Fatalf("MarkPushed: %v", err)
	}
	before := mustVersion(t, l, marked.Id)

	if err := l.StampAuthor(ctx, []string{marked.Id, clean.Id, half.Id}, "ivan"); err != nil {
		t.Fatalf("StampAuthor: %v", err)
	}
	after := mustVersion(t, l, marked.Id)
	if after.Memory.Author != "ivan" || after.Memory.UpdatedBy != "ivan" || after.Memory.UpdatedAt != 1000 {
		t.Errorf("помеченная = %+v, хочу автора ivan и прежний updated_at", after.Memory)
	}
	if after.Seq != before.Seq || after.Edited != before.Edited {
		t.Errorf("пометка изменилась: было seq=%d edited=%v, стало seq=%d edited=%v", before.Seq, before.Edited, after.Seq, after.Edited)
	}
	if c := mustVersion(t, l, clean.Id); c.Seq != 0 || c.Memory.Author != "ivan" {
		t.Errorf("непомеченная = %+v (seq %d), хочу автора ivan без пометки", c.Memory, c.Seq)
	}
	if h := mustVersion(t, l, half.Id); h.Memory.Author != "bob" || h.Memory.UpdatedBy != "ivan" {
		t.Errorf("с автором = %q/%q, хочу bob/ivan", h.Memory.Author, h.Memory.UpdatedBy)
	}
}

// TestSyncLocal_Apply: версия источника применяется целиком (id, created_at,
// updated_at, авторы), локальные accessed_at/access_count сохраняются; связь на
// запись той же пачки проходит FK; пометки применённых сняты, вектор сброшен при
// смене текста; CAS по updated_at и seq отсекает изменившиеся.
func TestSyncLocal_Apply(t *testing.T) {
	ctx := context.Background()
	s, l := newSyncTestStore(t)
	putTarget(t, l)
	old := putSyncMem(t, s, "project:alpha", memory.KindSemantic, "старый текст")
	for range 3 {
		if err := s.Touch(ctx, old.Id); err != nil {
			t.Fatalf("Touch: %v", err)
		}
	}
	if err := s.ResetVectorIndex(ctx, "test", 2); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}
	if err := s.PutVector(ctx, old.Id, []float32{1, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}
	other := putSyncMem(t, s, "project:alpha", memory.KindSemantic, "чужая правка")
	local := mustVersion(t, l, old.Id)
	touched := local.Memory

	summary := "сводка"
	newID := ulid.Make().String()
	upd := memory.Memory{Id: old.Id, Kind: memory.KindSemantic, Scope: "project:alpha", Content: "новый текст", Summary: &summary,
		Importance: 4, CreatedAt: 900, UpdatedAt: 2000, Author: "bob", UpdatedBy: "eve", AccessCount: 99, Tags: []string{"x"},
		Links: []memory.Link{{Dst: newID, Rel: memory.RelPartOf}}}
	fresh := memory.Memory{Id: newID, Kind: memory.KindTask, Scope: "project:alpha", Content: "новая", Importance: 3,
		CreatedAt: 1500, UpdatedAt: 1500, Author: "bob", UpdatedBy: "bob", AccessCount: 5}
	otherV := mustVersion(t, l, other.Id)
	recs := []memsync.Guarded{
		{Memory: upd, ExpectAt: local.Memory.UpdatedAt, ExpectSeq: local.Seq},
		{Memory: fresh},
		{Memory: other, ExpectAt: otherV.Memory.UpdatedAt, ExpectSeq: otherV.Seq + 1},                                        // seq сменился — stale
		{Memory: memory.Memory{Id: other.Id, Kind: memory.KindSemantic, Scope: "project:alpha", Content: "x", UpdatedAt: 5}}, // ждали отсутствия — stale
	}
	applied, stale, err := l.Apply(ctx, recs)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 2 || applied[0] != old.Id || applied[1] != newID || len(stale) != 2 {
		t.Fatalf("Apply = applied %v, stale %v; хочу две применённые и две stale", applied, stale)
	}

	got := mustVersion(t, l, old.Id)
	m := got.Memory
	if m.Content != "новый текст" || m.CreatedAt != 900 || m.UpdatedAt != 2000 || m.Author != "bob" || m.UpdatedBy != "eve" ||
		m.Importance != 4 || len(m.Tags) != 1 || len(m.Links) != 1 || m.Links[0].Dst != newID {
		t.Errorf("применённая версия = %+v", m)
	}
	if m.AccessCount != touched.AccessCount || m.AccessedAt == nil || *m.AccessedAt != *touched.AccessedAt {
		t.Errorf("локальные сигналы: access_count %d, accessed_at %v; хочу %d, %v", m.AccessCount, m.AccessedAt, touched.AccessCount, *touched.AccessedAt)
	}
	if got.Seq != 0 {
		t.Errorf("пометка применённой не снята: seq %d", got.Seq)
	}
	if f := mustVersion(t, l, newID); f.Seq != 0 || f.Memory.AccessCount != 0 || f.Memory.AccessedAt != nil || f.Memory.UpdatedAt != 1500 {
		t.Errorf("новая запись = %+v (seq %d), хочу без пометки и без чужих сигналов", f.Memory, f.Seq)
	}
	if o := mustVersion(t, l, other.Id); o.Memory.Content != "чужая правка" || o.Seq != otherV.Seq {
		t.Errorf("stale-запись тронута: %+v (seq %d)", o.Memory, o.Seq)
	}
	if vs, err := s.VectorStatus(ctx, old.Id); err != nil || vs.Embedded {
		t.Errorf("вектор после смены текста: %+v, %v; хочу сброшенным", vs, err)
	}

	// Та же версия ещё раз (заглушка без смены текста) вектор не трогает.
	if err := s.PutVector(ctx, old.Id, []float32{1, 0}); err != nil {
		t.Fatalf("PutVector: %v", err)
	}
	upd.Importance = 5
	if _, _, err := l.Apply(ctx, []memsync.Guarded{{Memory: upd, ExpectAt: 2000}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if vs, _ := s.VectorStatus(ctx, old.Id); !vs.Embedded {
		t.Errorf("вектор сброшен без смены текста")
	}
}

// TestBeginWriteTx_TakesWriteLock: транзакция beginWriteTx держит блокировку
// записи с первого оператора — второй писатель без ожидания получает отказ, а не
// проходит между чтением и записью первой.
func TestBeginWriteTx_TakesWriteLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer s.Close()
	other, err := sql.Open("sqlite3", sqliteDSN(path, "_journal_mode=WAL&_busy_timeout=0"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	tx, err := beginWriteTx(ctx, s.db)
	if err != nil {
		t.Fatalf("beginWriteTx: %v", err)
	}
	if _, err := other.ExecContext(ctx, `INSERT INTO sync_profile (id) VALUES (1)`); err == nil {
		t.Fatalf("второй писатель прошёл при открытой beginWriteTx — блокировка записи не взята")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ExecContext(ctx, `INSERT INTO sync_profile (id) VALUES (1)`); err != nil {
		t.Fatalf("после отката второй писатель: %v", err)
	}
}

// TestSyncLocal_PutTargetWaitsForOtherWriter: другой процесс memo на том же файле
// держит запись (MCP-сервер стартует рядом с мордой) — PutTarget ждёт его по
// busy_timeout, а не падает «database is locked»: транзакция, начатая чтением,
// после чужого коммита получала бы SQLITE_BUSY_SNAPSHOT без ожидания (приёмка 19.10).
func TestSyncLocal_PutTargetWaitsForOtherWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	other, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite второго процесса: %v", err)
	}
	t.Cleanup(func() { other.Close() })

	tx, err := other.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO sync_profile (id, author_name, interval_s) VALUES (1, 'другой', 300)`); err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		committed <- tx.Commit()
	}()
	if _, err := s.SyncLocal().PutTarget(ctx, SyncTarget{Project: "alpha", Host: "h", User: "u", DBName: "d", Push: true}); err != nil {
		t.Fatalf("PutTarget при чужой записи: %v", err)
	}
	if err := <-committed; err != nil {
		t.Fatalf("commit второго процесса: %v", err)
	}
}
