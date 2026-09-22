package store

// common.go собирает код, буквально совпадающий между sqliteStore и pgStore — запросы
// без единого диалектного плейсхолдера, работающие через один и тот же *sql.DB, и
// общий row-mapping записи (memoryColumns/scanMemory/rowWithExtra — единая точка
// формы memories для обоих бэкендов, см. их комментарии) вместе с
// драйверо-нейтральными data-шагами миграций (taskModelDataSteps). Здесь — не о том, ЧТО запрашивается (это по-прежнему решает
// каждый Store-метод), а о том, чтобы один и тот же SQL-текст не жил в двух файлах,
// расходясь при будущих правках одного и забытом втором.
//
// Правило включения сюда: функция попадает в common.go, только если её SQL целиком
// совпадает у sqlite.go и postgres_*.go (проверено дословно) — при малейшем дилектном
// расхождении (плейсхолдеры ?/$N, разный синтаксис) место функции — в своём файле, не
// здесь. Обвязка (Tags/Links, транзакции) вокруг общего SQL у бэкендов может
// отличаться (postgres грузит их батчем, sqlite — построчно, см. postgres_crud.go) —
// это ответственность вызывающих Store-методов, не этой общей части.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"agentmemory/internal/memory"
)

// statsFromDB считает сводные счётчики памяти (DESIGN.md §12): всего живых и архивных
// записей, разбивки живых записей по scope и kind. Текст запросов побайтово совпадает
// у sqliteStore.Stats и pgStore.Stats — сюда вынесен как единая реализация, вызываемая
// обоими (s.db у обеих реализаций — *sql.DB).
func statsFromDB(ctx context.Context, db *sql.DB) (Stats, error) {
	stats := Stats{ByScope: map[string]int{}, ByKind: map[string]int{}}

	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE superseded_by IS NULL`).Scan(&stats.Total); err != nil {
		return Stats{}, fmt.Errorf("подсчёт живых записей: %w", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE superseded_by IS NOT NULL`).Scan(&stats.Superseded); err != nil {
		return Stats{}, fmt.Errorf("подсчёт архивных записей: %w", err)
	}

	scopeRows, err := db.QueryContext(ctx, `SELECT scope, COUNT(*) FROM memories WHERE superseded_by IS NULL GROUP BY scope`)
	if err != nil {
		return Stats{}, fmt.Errorf("разбивка по scope: %w", err)
	}
	defer scopeRows.Close()
	for scopeRows.Next() {
		var scope string
		var n int
		if err := scopeRows.Scan(&scope, &n); err != nil {
			return Stats{}, fmt.Errorf("сканирование разбивки по scope: %w", err)
		}
		stats.ByScope[scope] = n
	}
	if err := scopeRows.Err(); err != nil {
		return Stats{}, fmt.Errorf("чтение разбивки по scope: %w", err)
	}

	kindRows, err := db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM memories WHERE superseded_by IS NULL GROUP BY kind`)
	if err != nil {
		return Stats{}, fmt.Errorf("разбивка по kind: %w", err)
	}
	defer kindRows.Close()
	for kindRows.Next() {
		var kind string
		var n int
		if err := kindRows.Scan(&kind, &n); err != nil {
			return Stats{}, fmt.Errorf("сканирование разбивки по kind: %w", err)
		}
		stats.ByKind[kind] = n
	}
	if err := kindRows.Err(); err != nil {
		return Stats{}, fmt.Errorf("чтение разбивки по kind: %w", err)
	}

	return stats, nil
}

// pendingVectorsFromDB считает живые записи без строки в векторном индексе
// vecTable — очередь на досчёт вектора (тот же предикат, что у
// ListMissingVectors обоих драйверов). Вызывающий сам проверяет, что таблица
// существует (hasVecTable): имя таблицы у драйверов разное (в PostgreSQL —
// схема-квалифицированное, см. pgStore.vecTable), поэтому передаётся параметром;
// подставляется из кода, не из пользовательского ввода.
func pendingVectorsFromDB(ctx context.Context, db *sql.DB, vecTable string) (int, error) {
	var n int
	query := `
		SELECT COUNT(*)
		FROM memories m
		LEFT JOIN ` + vecTable + ` v ON v.memory_id = m.id
		WHERE m.superseded_by IS NULL AND v.memory_id IS NULL
	`
	if err := db.QueryRowContext(ctx, query).Scan(&n); err != nil {
		return 0, fmt.Errorf("подсчёт очереди на эмбеддинг: %w", err)
	}
	return n, nil
}

// lastMaintRunFromDB читает отметку последнего прогона планировщика из maint_meta;
// нет строки — 0 без ошибки (прогонов ещё не было). Текст запроса общий для
// sqliteStore.LastMaintRun и pgStore.LastMaintRun (параметров нет — плейсхолдеры
// драйверов не расходятся); запись — в каждом драйвере своя (upsert с
// плейсхолдером).
func lastMaintRunFromDB(ctx context.Context, db *sql.DB) (int64, error) {
	var at int64
	err := db.QueryRowContext(ctx, `SELECT last_run_at FROM maint_meta WHERE id = 1`).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("чтение maint_meta: %w", err)
	}
	return at, nil
}

// embedMetaFromDB возвращает модель и размерность вектора, зафиксированные последним
// reindex. Если reindex ещё не запускался, возвращает ("", 0, nil). Текст запроса
// побайтово совпадает у sqliteStore.EmbedMeta и pgStore.EmbedMeta.
func embedMetaFromDB(ctx context.Context, db *sql.DB) (string, int, error) {
	var model string
	var dim int
	err := db.QueryRowContext(ctx, `SELECT model, dim FROM embed_meta WHERE id = 1`).Scan(&model, &dim)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("чтение embed_meta: %w", err)
	}
	return model, dim, nil
}

// liveMemoriesFromDB читает все не устаревшие (superseded_by IS NULL) записи БЕЗ
// Tags/Links — общая для sqliteStore.AllForReindex и pgStore.AllForReindex часть
// (сам SELECT побайтово совпадает); Tags/Links вызывающая сторона достраивает сама —
// sqlite построчно (loadTags/loadLinks, дёшево для локального файла), postgres
// батчем (loadTagsAndLinksBatch, postgres_crud.go — дорого делать построчно по сети).
// Это расхождение НЕ диалектное (SQL тот же), а про число round-trip'ов, поэтому и
// живёт в вызывающих методах, а не здесь.
func liveMemoriesFromDB(ctx context.Context, db *sql.DB) ([]memory.Memory, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+memoryColumns+`
		FROM memories m
		WHERE m.superseded_by IS NULL
	`)
	if err != nil {
		return nil, fmt.Errorf("чтение записей для reindex: %w", err)
	}
	defer rows.Close()

	var all []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование записи для reindex: %w", err)
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

// memoryColumns — список колонок memories в том порядке, в каком их читает
// scanMemory; единственное место этого перечня для обоих драйверов (sqlite.go и
// postgres_*.go): каждый SELECT записи собирается как "SELECT " + memoryColumns
// (при необходимости с довеском вроде knn.distance — см. rowWithExtra), а
// таблица в нём обязана иметь алиас m. Новая колонка добавляется здесь, в Put
// обоих драйверов и в scanMemory — и ни в одном из ~20 запросов по отдельности
// (до Этапа 15 список был продублирован в каждом, и одна забытая копия молча
// теряла бы поле на одном из путей чтения).
const memoryColumns = "m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, m.expires_at, m.status, m.task_type, m.branch, m.workflow"

// nullIfEmpty — пустая строка -> SQL NULL для nullable-колонок task-полей
// (status/task_type/branch/workflow; см. довод про NULL в Put).
func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// rowScanner — общий интерфейс *sql.Row и *sql.Rows, используемый scanMemory.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanMemory читает одну строку memories в порядке memoryColumns.
func scanMemory(row rowScanner) (memory.Memory, error) {
	var (
		m          memory.Memory
		kind       string
		summary    sql.NullString
		accessedAt sql.NullInt64
		superseded sql.NullString
		source     sql.NullString
		expiresAt  sql.NullInt64
		status     sql.NullString
		taskType   sql.NullString
		branch     sql.NullString
		workflow   sql.NullString
	)

	if err := row.Scan(&m.Id, &kind, &m.Scope, &m.Content, &summary, &m.Importance,
		&m.CreatedAt, &m.UpdatedAt, &accessedAt, &m.AccessCount, &superseded, &source, &expiresAt, &status,
		&taskType, &branch, &workflow); err != nil {
		return memory.Memory{}, err
	}

	m.Kind = memory.Kind(kind)
	if summary.Valid {
		m.Summary = &summary.String
	}
	if accessedAt.Valid {
		m.AccessedAt = &accessedAt.Int64
	}
	if superseded.Valid {
		m.SupersededBy = &superseded.String
	}
	if source.Valid {
		m.Source = &source.String
	}
	if expiresAt.Valid {
		m.ExpiresAt = expiresAt.Int64
	}
	if status.Valid {
		m.Status = memory.Status(status.String)
	}
	if taskType.Valid {
		m.Type = memory.TaskType(taskType.String)
	}
	m.Branch = branch.String     // NULL -> "" (sql.NullString.String пуст при !Valid)
	m.Workflow = workflow.String // то же
	return m, nil
}

// rowWithExtra адаптирует rowScanner так, чтобы scanMemory (рассчитанный ровно на
// memoryColumns) можно было переиспользовать для запросов, добавляющих
// дополнительные колонки после стандартных (например, score в SearchLexical):
// dest от scanMemory дополняется extra перед вызовом настоящего Scan.
type rowWithExtra struct {
	row   rowScanner
	extra []any
}

func (r rowWithExtra) Scan(dest ...any) error {
	return r.row.Scan(append(dest, r.extra...)...)
}

// taskModelDataSteps — data-шаги миграции модели задачи v2 (Этап 15; SQLite
// migrateTaskModel 3 -> 4, PostgreSQL migrateTaskModelPG 0 -> 1) в порядке
// применения. Плейсхолдер ? — SQLite; PostgreSQL применяет тот же перечень
// через pgPlaceholders (postgres.go), чтобы
// правила blocked -> todo и backfill task_type не расходились между
// драйверами. Порядок существенен: сперва теги эпиков/историй, затем
// «всё остальное → task» — иначе последний шаг перекрыл бы первые два.
var taskModelDataSteps = []struct {
	name string
	sql  string
	args []any
}{
	{
		name: "перевод задач blocked -> todo",
		sql:  `UPDATE memories SET status = ? WHERE kind = ? AND status = 'blocked'`,
		args: []any{string(memory.StatusTodo), string(memory.KindTask)},
	},
	{
		name: "backfill task_type=epic по тегам",
		sql: `UPDATE memories SET task_type = ? WHERE kind = ? AND task_type IS NULL
			AND EXISTS (SELECT 1 FROM tags t WHERE t.memory_id = memories.id AND t.tag IN ('epic', 'эпик'))`,
		args: []any{string(memory.TaskTypeEpic), string(memory.KindTask)},
	},
	{
		name: "backfill task_type=story по тегам",
		sql: `UPDATE memories SET task_type = ? WHERE kind = ? AND task_type IS NULL
			AND EXISTS (SELECT 1 FROM tags t WHERE t.memory_id = memories.id AND t.tag IN ('story', 'История'))`,
		args: []any{string(memory.TaskTypeStory), string(memory.KindTask)},
	},
	{
		name: "backfill task_type=task для остальных задач",
		sql:  `UPDATE memories SET task_type = ? WHERE kind = ? AND task_type IS NULL`,
		args: []any{string(memory.TaskTypeTask), string(memory.KindTask)},
	},
}
