// sqlite_sync.go — локальное состояние синхронизации с общей базой (Этап 19,
// DESIGN.md §4, §4.1, §7.1, §13): профиль разработчика, цели (проект → общая
// база), итог прогонов и курсор pull, пометки неотправленного, lease прогона.
// Только SQLite: при driver = "postgres" хранилище уже общее, и SyncLocalOf
// возвращает ok = false. Порт Store эти методы не получает — синхронизация
// отдельный вариант использования со своим контрактом (§6): SyncLocal —
// адаптер порта memsync.Local (задача 19.5), а профиль, цели и lease — для
// планировщика прогонов и морды.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"agentmemory/internal/memory"
	memsync "agentmemory/internal/memory/sync"
)

// syncDefaultIntervalSec — период фоновой синхронизации по умолчанию (5 мин),
// совпадает с DEFAULT колонки sync_profile.interval_s.
const syncDefaultIntervalSec = 300

// syncDefaultPort и syncDefaultSSLMode — значения по умолчанию колонок
// sync_targets.port и sync_targets.sslmode.
const (
	syncDefaultPort    = 5432
	syncDefaultSSLMode = "prefer"
)

// syncPasswordEnvPrefix — префикс имени переменной пароля общей базы проекта в
// .env каталога локальной БД (DESIGN.md §13).
const syncPasswordEnvPrefix = "MEMO_SYNC_PASSWORD_"

// syncedColumns — колонки memories, правка которых помечает запись
// неотправленной (DESIGN.md §4.1): всё, кроме id и локальных сигналов
// ранжирования accessed_at/access_count. Тот же состав, что список исключений
// триггера memories_change_upd общей базы; новая синхронизируемая колонка
// добавляется сюда, триггеры пересоздаются на ближайшем Open.
var syncedColumns = []string{
	"kind", "scope", "content", "summary", "importance", "created_at", "updated_at",
	"superseded_by", "source", "expires_at", "status", "task_type", "branch", "workflow",
	"author", "updated_by",
}

// syncTriggers помечают в sync_dirty записи scope какой-либо цели
// (scope = 'project:' || sync_targets.project), изменённые любым путём: вставка
// и правка синхронизируемых колонок memories, вставка/удаление тегов и
// исходящих связей. Виды записей триггер не фильтрует — состав набора решает
// интерактор, а Dirty снимает пометки лишних видов (DESIGN.md §4.1).
//
// Пометка — DELETE + INSERT, а не INSERT OR REPLACE: новая строка получает новый
// seq из AUTOINCREMENT (так снятие пометки по старому seq не теряет повторную
// правку), а конфликтная политика внешнего оператора (SQLite подменяет ею
// OR-клаузу операторов в теле триггера) на пометку не влияет.
//
// Пересоздаются на каждом Open после migrate (applyTriggers), как ftsTriggers:
// тело ссылается на колонки из syncedColumns.
var syncTriggers = func() string {
	changed := make([]string, len(syncedColumns))
	for i, c := range syncedColumns {
		changed[i] = fmt.Sprintf("old.%s IS NOT new.%s", c, c)
	}
	const markOwner = `
    DELETE FROM sync_dirty WHERE memory_id = %[1]s;
    INSERT INTO sync_dirty (memory_id, edited) VALUES (%[1]s, 1);`
	const scopeIsTarget = `EXISTS (SELECT 1 FROM sync_targets WHERE 'project:' || project = new.scope)`
	ownerIsTarget := func(ref string) string {
		return fmt.Sprintf(`EXISTS (SELECT 1 FROM memories m JOIN sync_targets t ON m.scope = 'project:' || t.project WHERE m.id = %s)`, ref)
	}
	var b strings.Builder
	trigger := func(name, event, when, ref string) {
		fmt.Fprintf(&b, "DROP TRIGGER IF EXISTS %s;\nCREATE TRIGGER %s %s\nWHEN %s\nBEGIN%s\nEND;\n\n",
			name, name, event, when, fmt.Sprintf(markOwner, ref))
	}
	trigger("sync_memories_ai", "AFTER INSERT ON memories", scopeIsTarget, "new.id")
	trigger("sync_memories_au", "AFTER UPDATE OF "+strings.Join(syncedColumns, ", ")+" ON memories",
		"("+strings.Join(changed, " OR ")+") AND "+scopeIsTarget, "new.id")
	trigger("sync_tags_ai", "AFTER INSERT ON tags", ownerIsTarget("new.memory_id"), "new.memory_id")
	trigger("sync_tags_ad", "AFTER DELETE ON tags", ownerIsTarget("old.memory_id"), "old.memory_id")
	trigger("sync_links_ai", "AFTER INSERT ON links", ownerIsTarget("new.src"), "new.src")
	trigger("sync_links_ad", "AFTER DELETE ON links", ownerIsTarget("old.src"), "old.src")
	return b.String()
}()

// SyncProfile — настройки машины (одна строка sync_profile).
type SyncProfile struct {
	AuthorName  string // имя разработчика; пусто — дефолт git config --global user.name
	IntervalSec int    // период фоновой синхронизации, сек
}

// SyncTarget — цель синхронизации: проект и параметры его общей базы.
type SyncTarget struct {
	Project     string // синхронизируется scope project:<Project>
	Host        string
	Port        int
	User        string
	DBName      string
	SSLMode     string
	PasswordEnv string // имя переменной пароля в .env каталога БД, не сам пароль
	Push        bool   // тумблер «Отправлять»
	Pull        bool   // тумблер «Загружать»
}

// SyncLocal — доступ к локальному состоянию синхронизации SQLite-хранилища.
// Время — из поля now хранилища (подменяется в тестах).
type SyncLocal struct {
	s *sqliteStore
}

var _ memsync.Local = (*SyncLocal)(nil)

// SyncLocal возвращает доступ к локальному состоянию синхронизации.
func (s *sqliteStore) SyncLocal() *SyncLocal {
	return &SyncLocal{s: s}
}

// SyncLocalOf возвращает локальное состояние синхронизации хранилища st; ok =
// false — хранилище не локальное (driver = "postgres"), синхронизация недоступна.
func SyncLocalOf(st Store) (*SyncLocal, bool) {
	l, ok := st.(interface{ SyncLocal() *SyncLocal })
	if !ok {
		return nil, false
	}
	return l.SyncLocal(), true
}

// Profile читает профиль; строки ещё нет — дефолты (пустое имя, 5 мин).
func (l *SyncLocal) Profile(ctx context.Context) (SyncProfile, error) {
	var name sql.NullString
	p := SyncProfile{IntervalSec: syncDefaultIntervalSec}
	err := l.s.db.QueryRowContext(ctx, `SELECT author_name, interval_s FROM sync_profile WHERE id = 1`).Scan(&name, &p.IntervalSec)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err != nil {
		return SyncProfile{}, fmt.Errorf("чтение sync_profile: %w", err)
	}
	p.AuthorName = name.String
	return p, nil
}

// SetProfile перезаписывает профиль. Пустое имя хранится как NULL (дефолт из
// git), IntervalSec <= 0 — интервал по умолчанию.
func (l *SyncLocal) SetProfile(ctx context.Context, p SyncProfile) error {
	interval := p.IntervalSec
	if interval <= 0 {
		interval = syncDefaultIntervalSec
	}
	_, err := l.s.db.ExecContext(ctx, `
		INSERT INTO sync_profile (id, author_name, interval_s) VALUES (1, ?, ?)
		ON CONFLICT(id) DO UPDATE SET author_name = excluded.author_name, interval_s = excluded.interval_s
	`, nullIfEmpty(strings.TrimSpace(p.AuthorName)), interval)
	if err != nil {
		return fmt.Errorf("обновление sync_profile: %w", err)
	}
	return nil
}

const syncTargetColumns = "project, host, port, user, dbname, sslmode, password_env, push, pull"

func scanSyncTarget(row rowScanner) (SyncTarget, error) {
	var t SyncTarget
	err := row.Scan(&t.Project, &t.Host, &t.Port, &t.User, &t.DBName, &t.SSLMode, &t.PasswordEnv, &t.Push, &t.Pull)
	return t, err
}

// Targets возвращает все цели по возрастанию имени проекта.
func (l *SyncLocal) Targets(ctx context.Context) ([]SyncTarget, error) {
	rows, err := l.s.db.QueryContext(ctx, `SELECT `+syncTargetColumns+` FROM sync_targets ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("чтение sync_targets: %w", err)
	}
	defer rows.Close()
	var out []SyncTarget
	for rows.Next() {
		t, err := scanSyncTarget(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование sync_targets: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Target возвращает цель проекта; нет цели — ErrNotFound.
func (l *SyncLocal) Target(ctx context.Context, project string) (SyncTarget, error) {
	t, err := scanSyncTarget(l.s.db.QueryRowContext(ctx, `SELECT `+syncTargetColumns+` FROM sync_targets WHERE project = ?`, project))
	if errors.Is(err, sql.ErrNoRows) {
		return SyncTarget{}, fmt.Errorf("цель синхронизации %q: %w", project, ErrNotFound)
	}
	if err != nil {
		return SyncTarget{}, fmt.Errorf("чтение цели синхронизации %q: %w", project, err)
	}
	return t, nil
}

// PutTarget создаёт или изменяет цель t.Project и возвращает её сохранённый вид.
// Port 0 и пустой SSLMode — значения по умолчанию (5432, prefer). PasswordEnv
// задаёт хранилище: у новой цели — MEMO_SYNC_PASSWORD_<PROJECT> с суффиксом
// _2, _3… при совпадении с чужой целью (DESIGN.md §13), у существующей — не
// меняется (под этим именем пароль уже лежит в .env); t.PasswordEnv игнорируется.
func (l *SyncLocal) PutTarget(ctx context.Context, t SyncTarget) (SyncTarget, error) {
	if t.Port == 0 {
		t.Port = syncDefaultPort
	}
	if t.SSLMode == "" {
		t.SSLMode = syncDefaultSSLMode
	}

	tx, err := beginWriteTx(ctx, l.s.db)
	if err != nil {
		return SyncTarget{}, err
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx, `SELECT password_env FROM sync_targets WHERE project = ?`, t.Project).Scan(&t.PasswordEnv)
	if errors.Is(err, sql.ErrNoRows) {
		t.PasswordEnv, err = allocPasswordEnv(ctx, tx, t.Project)
	}
	if err != nil {
		return SyncTarget{}, fmt.Errorf("имя переменной пароля цели %q: %w", t.Project, err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO sync_targets (`+syncTargetColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project) DO UPDATE SET host = excluded.host, port = excluded.port, user = excluded.user,
			dbname = excluded.dbname, sslmode = excluded.sslmode, push = excluded.push, pull = excluded.pull
	`, t.Project, t.Host, t.Port, t.User, t.DBName, t.SSLMode, t.PasswordEnv, t.Push, t.Pull)
	if err != nil {
		return SyncTarget{}, fmt.Errorf("запись цели синхронизации %q: %w", t.Project, err)
	}
	if err := tx.Commit(); err != nil {
		return SyncTarget{}, fmt.Errorf("commit цели синхронизации %q: %w", t.Project, err)
	}
	return t, nil
}

// allocPasswordEnv подбирает свободное имя переменной пароля для новой цели:
// имя проекта в верхнем регистре, всё кроме A-Z0-9 → '_'; занято — суффикс
// _2, _3…
func allocPasswordEnv(ctx context.Context, tx *sql.Tx, project string) (string, error) {
	base := syncPasswordEnvPrefix + strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, strings.ToUpper(project))
	name := base
	for n := 2; ; n++ {
		var taken bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sync_targets WHERE password_env = ?)`, name).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return name, nil
		}
		name = base + "_" + strconv.Itoa(n)
	}
}

// DeleteTarget удаляет цель вместе с её sync_state и пометками неотправленного
// записей проекта; сами записи остаются (DESIGN.md §7.1). Нет цели — ErrNotFound.
func (l *SyncLocal) DeleteTarget(ctx context.Context, project string) error {
	tx, err := l.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM sync_targets WHERE project = ?`, project)
	if err != nil {
		return fmt.Errorf("удаление цели синхронизации %q: %w", project, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("удаление цели синхронизации %q: %w", project, err)
	} else if n == 0 {
		return fmt.Errorf("цель синхронизации %q: %w", project, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sync_state WHERE project = ?`, project); err != nil {
		return fmt.Errorf("удаление sync_state %q: %w", project, err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM sync_dirty WHERE memory_id IN (SELECT id FROM memories WHERE scope = 'project:' || ?)
	`, project); err != nil {
		return fmt.Errorf("снятие пометок проекта %q: %w", project, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit удаления цели %q: %w", project, err)
	}
	return nil
}

// State читает итог прогонов цели; прогонов ещё не было — нулевое значение.
func (l *SyncLocal) State(ctx context.Context, project string) (memsync.State, error) {
	var (
		st                      memsync.State
		baseID, pullXID, pullID sql.NullString
		lastRun, lastOk         sql.NullInt64
		lastError               sql.NullString
	)
	err := l.s.db.QueryRowContext(ctx, `
		SELECT base_id, pull_xid, pull_id, push_marked, last_run_at, last_ok_at, last_error,
			last_pushed, last_pulled, last_conflicts, conflicts_total
		FROM sync_state WHERE project = ?
	`, project).Scan(&baseID, &pullXID, &pullID, &st.PushMarked, &lastRun, &lastOk, &lastError,
		&st.LastPushed, &st.LastPulled, &st.LastConflicts, &st.ConflictsTotal)
	if errors.Is(err, sql.ErrNoRows) {
		return memsync.State{}, nil
	}
	if err != nil {
		return memsync.State{}, fmt.Errorf("чтение sync_state %q: %w", project, err)
	}
	if pullXID.Valid && pullXID.String != "" {
		if st.Cursor.XID, err = strconv.ParseUint(pullXID.String, 10, 64); err != nil {
			return memsync.State{}, fmt.Errorf("курсор pull %q: %w", project, err)
		}
	}
	st.BaseID, st.Cursor.ID, st.LastError = baseID.String, pullID.String, lastError.String
	st.LastRunAt, st.LastOKAt = lastRun.Int64, lastOk.Int64
	return st, nil
}

// SaveState перезаписывает итог прогонов цели. Курсор хранится десятичной
// строкой: xid8 беззнаковый 64-битный, INTEGER SQLite — знаковый.
func (l *SyncLocal) SaveState(ctx context.Context, project string, st memsync.State) error {
	var pullXID any
	if st.Cursor.XID != 0 {
		pullXID = strconv.FormatUint(st.Cursor.XID, 10)
	}
	var lastRun, lastOk any
	if st.LastRunAt != 0 {
		lastRun = st.LastRunAt
	}
	if st.LastOKAt != 0 {
		lastOk = st.LastOKAt
	}
	_, err := l.s.db.ExecContext(ctx, `
		INSERT INTO sync_state (project, base_id, pull_xid, pull_id, push_marked, last_run_at, last_ok_at,
			last_error, last_pushed, last_pulled, last_conflicts, conflicts_total)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(project) DO UPDATE SET base_id = excluded.base_id, pull_xid = excluded.pull_xid,
			pull_id = excluded.pull_id, push_marked = excluded.push_marked, last_run_at = excluded.last_run_at,
			last_ok_at = excluded.last_ok_at, last_error = excluded.last_error, last_pushed = excluded.last_pushed,
			last_pulled = excluded.last_pulled, last_conflicts = excluded.last_conflicts,
			conflicts_total = excluded.conflicts_total
	`, project, nullIfEmpty(st.BaseID), pullXID, nullIfEmpty(st.Cursor.ID), st.PushMarked, lastRun, lastOk,
		nullIfEmpty(st.LastError), st.LastPushed, st.LastPulled, st.LastConflicts, st.ConflictsTotal)
	if err != nil {
		return fmt.Errorf("запись sync_state %q: %w", project, err)
	}
	return nil
}

// Lease захватывает прогон цели project процессом holder до момента until.
// false — цель держит другой процесс и его срок не истёк. Повторный захват своим
// holder продлевает срок, истёкший чужой — перехватывается. Один оператор —
// захват атомарен между процессами (DESIGN.md §11).
func (l *SyncLocal) Lease(ctx context.Context, project, holder string, until int64) (bool, error) {
	res, err := l.s.db.ExecContext(ctx, `
		INSERT INTO sync_lease (project, holder, expires_at) VALUES (?, ?, ?)
		ON CONFLICT(project) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
		WHERE sync_lease.expires_at < ? OR sync_lease.holder = excluded.holder
	`, project, holder, until, l.s.now())
	if err != nil {
		return false, fmt.Errorf("захват lease %q: %w", project, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("захват lease %q: %w", project, err)
	}
	return n > 0, nil
}

// Release снимает lease цели, если его держит holder; чужой не трогает.
func (l *SyncLocal) Release(ctx context.Context, project, holder string) error {
	if _, err := l.s.db.ExecContext(ctx, `DELETE FROM sync_lease WHERE project = ? AND holder = ?`, project, holder); err != nil {
		return fmt.Errorf("снятие lease %q: %w", project, err)
	}
	return nil
}

// SchemaAhead сообщает, что локальную БД уже обновил более новый memo
// (user_version больше schemaVersion бинаря): такой процесс синхронизацию не
// запускает (DESIGN.md §4.1).
func (l *SyncLocal) SchemaAhead(ctx context.Context) (bool, error) {
	v, err := l.s.SchemaVersion(ctx)
	if err != nil {
		return false, err
	}
	return v > schemaVersion, nil
}

// kindStrings переводит виды записей в аргументы kindPredicate.
func kindStrings(kinds []memory.Kind) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

// Mark помечает неотправленными записи scope видов kinds (пусто — любых) с
// новым seq: ids == nil — весь набор, иначе только перечисленные из него. Новая
// пометка — первичная разметка (edited = 0), уже стоящая сохраняет edited —
// правка разметкой не затирается. INSERT OR REPLACE удаляет прежнюю строку и
// вставляет новую, поэтому seq выдаёт AUTOINCREMENT; SELECT по sync_dirty SQLite
// вычисляет целиком до вставки, так что edited читается из прежней строки.
func (l *SyncLocal) Mark(ctx context.Context, scope string, kinds []memory.Kind, ids []string) error {
	if ids != nil && len(ids) == 0 {
		return nil
	}
	conds := []string{"m.scope = ?"}
	args := []any{scope}
	if clause, kargs := kindPredicate(kindStrings(kinds)); clause != "" {
		conds = append(conds, clause)
		args = append(args, kargs...)
	}
	if ids != nil {
		in, idArgs := inPlaceholders(ids)
		conds = append(conds, "m.id IN ("+in+")")
		args = append(args, idArgs...)
	}
	if _, err := l.s.db.ExecContext(ctx, `
		INSERT OR REPLACE INTO sync_dirty (memory_id, edited)
		SELECT m.id, COALESCE(d.edited, 0) FROM memories m LEFT JOIN sync_dirty d ON d.memory_id = m.id
		WHERE `+strings.Join(conds, " AND "), args...); err != nil {
		return fmt.Errorf("разметка набора %q: %w", scope, err)
	}
	return nil
}

// Dirty возвращает до limit (<= 0 — все) помеченных записей scope видов kinds
// (пусто — любых) с seq > after в порядке пометки, с тегами и связями. Пометки
// записей scope других видов снимаются: состав набора задаёт вызывающий
// (DESIGN.md §4.1).
func (l *SyncLocal) Dirty(ctx context.Context, scope string, kinds []memory.Kind, after int64, limit int) ([]memsync.Pending, error) {
	tx, err := l.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()

	kindClause, kindArgs := kindPredicate(kindStrings(kinds))
	if kindClause != "" {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM sync_dirty WHERE memory_id IN (
				SELECT m.id FROM memories m WHERE m.scope = ? AND NOT (`+kindClause+`))
		`, append([]any{scope}, kindArgs...)...); err != nil {
			return nil, fmt.Errorf("снятие пометок вне набора %q: %w", scope, err)
		}
	}

	where := "m.scope = ? AND d.seq > ?"
	if kindClause != "" {
		where += " AND " + kindClause
	}
	if limit <= 0 {
		limit = -1 // LIMIT -1 в SQLite — без ограничения
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT `+memoryColumns+`, d.seq, d.edited
		FROM sync_dirty d JOIN memories m ON m.id = d.memory_id
		WHERE `+where+` ORDER BY d.seq LIMIT ?
	`, append(append([]any{scope, after}, kindArgs...), limit)...)
	if err != nil {
		return nil, fmt.Errorf("чтение неотправленного %q: %w", scope, err)
	}
	var out []memsync.Pending
	for rows.Next() {
		var p memsync.Pending
		p.Memory, err = scanMemory(rowWithExtra{row: rows, extra: []any{&p.Seq, &p.Edited}})
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("сканирование неотправленного %q: %w", scope, err)
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("чтение неотправленного %q: %w", scope, err)
	}

	if err := l.attachTagsAndLinks(ctx, tx, out); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit чтения неотправленного %q: %w", scope, err)
	}
	return out, nil
}

// MarkPushed снимает пометки отправленных записей, если их seq не изменился:
// правка, сделанная после чтения Dirty, получила новый seq и останется
// помеченной до следующего прогона.
func (l *SyncLocal) MarkPushed(ctx context.Context, done []memsync.Pending) error {
	if len(done) == 0 {
		return nil
	}
	tx, err := l.s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()
	for _, p := range done {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sync_dirty WHERE memory_id = ? AND seq = ?`, p.Memory.Id, p.Seq); err != nil {
			return fmt.Errorf("снятие пометки %q: %w", p.Memory.Id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit снятия пометок: %w", err)
	}
	return nil
}

// attachTagsAndLinks дочитывает теги и исходящие связи записей ps в транзакции q.
func (l *SyncLocal) attachTagsAndLinks(ctx context.Context, q dbQueryer, ps []memsync.Pending) error {
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.Memory.Id
	}
	tagsByID, err := l.s.loadTagsBatch(ctx, q, ids)
	if err != nil {
		return err
	}
	linksByID, err := l.s.loadLinksBatch(ctx, q, ids)
	if err != nil {
		return err
	}
	for i := range ps {
		ps[i].Memory.Tags = tagsByID[ps[i].Memory.Id]
		ps[i].Memory.Links = linksByID[ps[i].Memory.Id]
	}
	return nil
}

// inPlaceholders — "?,?,…" на n аргументов и сами аргументы ids.
func inPlaceholders(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// Versions возвращает записи ids (с тегами и связями, тумстоуны тоже) и их
// пометки неотправленного (Seq = 0 — пометки нет); отсутствующие пропущены.
// Одна транзакция — согласованное чтение записей и пометок.
func (l *SyncLocal) Versions(ctx context.Context, ids []string) (map[string]memsync.Pending, error) {
	out := make(map[string]memsync.Pending, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	tx, err := l.s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()

	in, args := inPlaceholders(ids)
	rows, err := tx.QueryContext(ctx, `
		SELECT `+memoryColumns+`, COALESCE(d.seq, 0), COALESCE(d.edited, 0)
		FROM memories m LEFT JOIN sync_dirty d ON d.memory_id = m.id
		WHERE m.id IN (`+in+`)
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("чтение локальных версий: %w", err)
	}
	var ps []memsync.Pending
	for rows.Next() {
		var p memsync.Pending
		p.Memory, err = scanMemory(rowWithExtra{row: rows, extra: []any{&p.Seq, &p.Edited}})
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("сканирование локальной версии: %w", err)
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("чтение локальных версий: %w", err)
	}
	if err := l.attachTagsAndLinks(ctx, tx, ps); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit чтения локальных версий: %w", err)
	}
	for _, p := range ps {
		out[p.Memory.Id] = p
	}
	return out, nil
}

// StampAuthor заполняет пустые author/updated_by записей ids именем name. Это
// заполнение метаданных, а не правка (DESIGN.md §7.1): updated_at не меняется, а
// пометки sync_dirty, которые переставил бы триггер, возвращаются как были — с
// прежними seq и edited (иначе MarkPushed по прочитанному seq не снял бы
// пометку, а запись стала бы правкой); записи без пометки её не получают.
func (l *SyncLocal) StampAuthor(ctx context.Context, ids []string, name string) error {
	if len(ids) == 0 || name == "" {
		return nil
	}
	tx, err := beginWriteTx(ctx, l.s.db)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	in, args := inPlaceholders(ids)
	type mark struct {
		id     string
		seq    int64
		edited bool
	}
	rows, err := tx.QueryContext(ctx, `SELECT memory_id, seq, edited FROM sync_dirty WHERE memory_id IN (`+in+`)`, args...)
	if err != nil {
		return fmt.Errorf("чтение пометок: %w", err)
	}
	var marks []mark
	for rows.Next() {
		var m mark
		if err := rows.Scan(&m.id, &m.seq, &m.edited); err != nil {
			rows.Close()
			return fmt.Errorf("сканирование пометки: %w", err)
		}
		marks = append(marks, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("чтение пометок: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE memories SET author = COALESCE(NULLIF(author, ''), ?), updated_by = COALESCE(NULLIF(updated_by, ''), ?)
		WHERE id IN (`+in+`) AND (COALESCE(author, '') = '' OR COALESCE(updated_by, '') = '')
	`, append([]any{name, name}, args...)...); err != nil {
		return fmt.Errorf("автор записей: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sync_dirty WHERE memory_id IN (`+in+`)`, args...); err != nil {
		return fmt.Errorf("снятие пометок автора: %w", err)
	}
	for _, m := range marks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sync_dirty (seq, memory_id, edited) VALUES (?, ?, ?)`, m.seq, m.id, m.edited); err != nil {
			return fmt.Errorf("возврат пометки %q: %w", m.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit автора записей: %w", err)
	}
	return nil
}

// Apply применяет версии общей базы одной транзакцией мимо write path
// (DESIGN.md §7.1): id, created_at, updated_at, author, updated_by — как в
// источнике, accessed_at/access_count — локальные (у новой записи — пустые).
// CAS: локальные updated_at и seq пометки (0 — записи/пометки нет) должны
// совпасть с ExpectAt/ExpectSeq, иначе запись — stale и не трогается. Сначала
// все записи с тегами, затем связи (FK на links.dst внутри пачки), затем
// снятие пометок применённых — их поставили триггеры этой же транзакции, и без
// снятия полученное ушло бы обратно. Смена content (из него строится вектор)
// удаляет строку вектора — запись встаёт в очередь ListMissingVectors.
func (l *SyncLocal) Apply(ctx context.Context, recs []memsync.Guarded) (applied, stale []string, err error) {
	if len(recs) == 0 {
		return nil, nil, nil
	}
	for _, g := range recs {
		if err := checkMemoryID(g.Memory.Id); err != nil {
			return nil, nil, err
		}
	}
	hasVec, err := l.s.hasVecTable(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("проверка memories_vec: %w", err)
	}

	tx, err := beginWriteTx(ctx, l.s.db)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	var ok []memory.Memory
	for _, g := range recs {
		m := g.Memory
		var (
			at, seq    int64
			content    string
			accessedAt sql.NullInt64
			count      int
		)
		err := tx.QueryRowContext(ctx, `
			SELECT m.updated_at, m.content, m.accessed_at, m.access_count, COALESCE(d.seq, 0)
			FROM memories m LEFT JOIN sync_dirty d ON d.memory_id = m.id WHERE m.id = ?
		`, m.Id).Scan(&at, &content, &accessedAt, &count, &seq)
		have := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, fmt.Errorf("чтение локальной версии %q: %w", m.Id, err)
		}
		if have != (g.ExpectAt != 0) || at != g.ExpectAt || seq != g.ExpectSeq {
			stale = append(stale, m.Id)
			continue
		}
		m.AccessedAt, m.AccessCount = nil, 0
		if have {
			if accessedAt.Valid {
				m.AccessedAt = &accessedAt.Int64
			}
			m.AccessCount = count
		}
		if err := putRowSQLite(ctx, tx, m); err != nil {
			return nil, nil, fmt.Errorf("применение %q: %w", m.Id, err)
		}
		if have && hasVec && content != m.Content {
			if _, err := tx.ExecContext(ctx, `DELETE FROM memories_vec WHERE memory_id = ?`, m.Id); err != nil {
				return nil, nil, fmt.Errorf("сброс вектора %q: %w", m.Id, err)
			}
		}
		ok = append(ok, m)
	}
	for _, m := range ok {
		if err := putLinksSQLite(ctx, tx, m.Id, m.Links); err != nil {
			return nil, nil, fmt.Errorf("связи %q: %w", m.Id, err)
		}
	}
	for _, m := range ok {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sync_dirty WHERE memory_id = ?`, m.Id); err != nil {
			return nil, nil, fmt.Errorf("снятие пометки %q: %w", m.Id, err)
		}
		applied = append(applied, m.Id)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit применения: %w", err)
	}
	return applied, stale, nil
}

// beginWriteTx начинает транзакцию с пишущего оператора. PutTarget, Apply и
// StampAuthor сначала читают, затем пишут: в WAL отложенная транзакция, начатая чтением,
// получает SQLITE_BUSY_SNAPSHOT сразу, без ожидания по busy_timeout, если
// другой процесс memo успел записать между её чтением и первой записью (тот же
// урок, что у applyTriggers). Пустой UPDATE берёт блокировку записи в начале —
// конкурент ждётся по busy_timeout.
func beginWriteTx(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("начало транзакции: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sync_dirty SET edited = edited WHERE 0`); err != nil {
		tx.Rollback()
		return nil, fmt.Errorf("блокировка записи: %w", err)
	}
	return tx, nil
}
