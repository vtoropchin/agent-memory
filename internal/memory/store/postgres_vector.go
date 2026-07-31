package store

import (
	"context"
	"fmt"
	"strings"

	"agentmemory/internal/memory"
)

// quoteIdent экранирует идентификатор Postgres для безопасной подстановки в текст
// SQL-запроса (двойные кавычки вокруг + удвоение вложенных кавычек — правило,
// которым сам Postgres квотирует идентификаторы при выводе, см. "Identifiers and
// Key Words" в его документации). Нужен там, где имя объекта нельзя передать
// bind-параметром (позиции DDL/FROM/JOIN не параметризуются) и оно при этом не
// строковый литерал в коде, а получено динамически с сервера (current_schema(),
// см. pgStore.schema) — теоретически может требовать экранирования, хотя в этом
// проекте схемы всегда простые ulid-имена.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// vecTable возвращает схема-квалифицированное имя таблицы memories_vec —
// "<pgStore.schema>".memories_vec. ЕДИНСТВЕННЫЙ способ, которым код этого файла и
// hasVecTable (postgres_search.go) обязаны ссылаться на memories_vec — вместо
// голого идентификатора "memories_vec" в SQL. Причина — у ResetVectorIndex.
func (s *pgStore) vecTable() string {
	return quoteIdent(s.schema) + ".memories_vec"
}

// EmbedMeta возвращает модель и размерность вектора, зафиксированные последним
// reindex. Если reindex ещё не запускался, возвращает ("", 0, nil). Паритет с
// sqliteStore.EmbedMeta — сам запрос общий, embedMetaFromDB (common.go).
func (s *pgStore) EmbedMeta(ctx context.Context) (string, int, error) {
	return embedMetaFromDB(ctx, s.db)
}

// ResetVectorIndex пересоздаёт векторный индекс memories_vec под новую модель/dim и
// фиксирует их в embed_meta — источнике истины для reindex (DESIGN.md §9). Вызывается
// при первом включении Embedder или при смене модели. Таблица пересоздаётся с нуля
// (DROP + CREATE) — старые векторы теряются; вызывающая сторона (reindex-джоб) обязана
// после этого пересчитать эмбеддинги через AllForReindex + PutVector. Паритет с
// sqliteStore.ResetVectorIndex.
//
// CREATE EXTENSION IF NOT EXISTS vector — здесь, а не в схеме при Open (postgres.go):
// расширение нужно ровно с этого момента (появления memories_vec), а не раньше — та
// же лень, что у самой таблицы (dim неизвестен заранее, DESIGN.md §15). pgvector —
// "trusted"-расширение (начиная с 0.5.0): его может создать владелец БД с правом
// CREATE, без прав суперпользователя.
//
// HNSW-индекс (vector_cosine_ops) — под оператор дистанции <=> (cosine), которым
// пользуются SearchVector/SemanticNeighbors (postgres_search.go).
//
// !!! Почему DROP TABLE обязан быть схема-квалифицирован (s.vecTable(), а не голое
// "memories_vec") — реальный инцидент на общей базе postgres.lan (DESIGN.md §4,
// §15 "деградация, а не падение" — тут же и обратная сторона: небезопасная операция
// не должна тихо задеть чужие данные). Постгрес резолвит НЕквалифицированные имена
// в разных командах по-разному:
//   - CREATE TABLE (без "IF NOT EXISTS" тоже) всегда создаёт объект в ПЕРВОЙ схеме
//     search_path, где у роли есть право CREATE — это ровно current_schema()
//     (см. комментарий у pgStore.init), детерминированно и без сканирования схем;
//   - DROP TABLE (как и любое чтение/DML: SELECT, JOIN, to_regclass) резолвит имя,
//     сканируя search_path СХЕМА ЗА СХЕМОЙ и беря ПЕРВОЕ совпадение, независимо от
//     того, есть ли объект с таким именем в current_schema() вообще.
//
// memories_vec — единственная таблица в этом драйвере, которую можно DROP (она
// лениво создаётся заново при каждом ResetVectorIndex, см. пакетный комментарий
// postgres_vector.go выше). Пока в целевой схеме (current_schema()) memories_vec
// ещё не существует (первый вызов ResetVectorIndex на свежей БД), а search_path
// содержит НЕСКОЛЬКО схем, из которых в одной из последующих (например public)
// memories_vec уже существует (общий сервер, другой проект/тест/прежний прогон) —
// голый `DROP TABLE IF EXISTS memories_vec` находит и удаляет ЭТОТ чужой объект,
// а не создаёт-и-удаляет "свой" в целевой схеме, как ожидал бы автор по аналогии с
// CREATE TABLE. Асимметрия CREATE (target schema) vs DROP/SELECT (search-path scan)
// и есть источник бага — именно так на postgres.lan была случайно снесена
// public.memories_vec при разработке тестовой изоляции (см. историю
// postgres_test.go до этого фикса).
//
// Фикс — резолвить схему РОВНО ОДИН РАЗ (current_schema() в pgStore.init, при
// открытии соединения) и явно квалифицировать ВСЕ обращения к memories_vec этим
// именем (s.vecTable(), а не голый идентификатор) — DROP, CREATE TABLE, CREATE
// INDEX ON, INSERT/JOIN (PutVector, ListMissingVectors, VectorStatus,
// SearchVector, SemanticNeighbors — postgres_vector.go и postgres_search.go) и
// to_regclass (hasVecTable). Тогда DROP и CREATE в рамках одного вызова
// гарантированно работают с ОДНИМ И ТЕМ ЖЕ объектом (тем, что в current_schema()),
// независимо от того, что ещё лежит дальше по search_path — свойство, которого у
// голого имени нет и не может быть. Остальные таблицы (memories/tags/links/
// embed_meta/schema_version) этой болезни не подвержены: они создаются в
// current_schema() эагерно при каждом init(), до любого DML — окна, в котором их
// нет в целевой схеме, но есть где-то ещё по search_path, попросту не существует.
func (s *pgStore) ResetVectorIndex(ctx context.Context, model string, dim int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("начало транзакции: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("создание расширения vector: %w", err)
	}

	vecTable := s.vecTable()

	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+vecTable); err != nil {
		return fmt.Errorf("удаление memories_vec: %w", err)
	}

	// dim приходит из кода (Embedder.Dim()), не из пользовательского ввода — безопасно
	// подставлять напрямую в DDL, тип vector(N) не параметризуется bind-параметром
	// (та же оговорка, что у float[%d] в sqliteStore.ResetVectorIndex). vecTable —
	// тоже не пользовательский ввод (current_schema() с сервера, квотирован
	// quoteIdent), см. развёрнутый комментарий выше.
	createStmt := fmt.Sprintf(
		`CREATE TABLE %s (memory_id text PRIMARY KEY REFERENCES memories(id) ON DELETE CASCADE, embedding vector(%d))`,
		vecTable, dim,
	)
	if _, err := tx.ExecContext(ctx, createStmt); err != nil {
		return fmt.Errorf("создание memories_vec: %w", err)
	}

	// Имя индекса (в отличие от имени таблицы) не нуждается в квалификации схемой:
	// Postgres всегда создаёт индекс в той же схеме, что и таблица из ON, независимо
	// от search_path — здесь асимметрии CREATE/DROP нет (у индекса своя схема-DROP
	// тоже привязана к таблице, а не резолвится отдельно).
	if _, err := tx.ExecContext(ctx, `
		CREATE INDEX memories_vec_embedding_hnsw_idx ON `+vecTable+` USING hnsw (embedding vector_cosine_ops)
	`); err != nil {
		return fmt.Errorf("создание HNSW-индекса memories_vec: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO embed_meta (id, model, dim) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET model = excluded.model, dim = excluded.dim
	`, model, dim); err != nil {
		return fmt.Errorf("обновление embed_meta: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// PutVector сохраняет (или заменяет) вектор записи id в memories_vec.
//
// В отличие от sqliteStore.PutVector (DELETE+INSERT в транзакции — vec0 у sqlite-vec
// не поддерживает конфликт-резолюцию по своему PRIMARY KEY), memories_vec здесь —
// обычная таблица с настоящим PRIMARY KEY(memory_id), поэтому ON CONFLICT DO UPDATE
// работает штатно: один атомарный оператор вместо DELETE+INSERT, без транзакции
// вокруг (сам оператор атомарен). Итоговое состояние (replace) идентично контракту
// метода — расхождение только в реализации, не в поведении.
//
// Требует, чтобы индекс уже был создан ResetVectorIndex — иначе вернёт ошибку внешнего
// ключа/отсутствия таблицы (reindex-джоб обязан вызвать ResetVectorIndex до первого
// PutVector, как и в SQLite).
func (s *pgStore) PutVector(ctx context.Context, id string, vec []float32) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO `+s.vecTable()+` (memory_id, embedding) VALUES ($1, $2::vector)
		ON CONFLICT (memory_id) DO UPDATE SET embedding = excluded.embedding
	`, id, vectorLiteral(vec))
	if err != nil {
		return fmt.Errorf("запись вектора %q: %w", id, err)
	}
	return nil
}

// ListMissingVectors возвращает не устаревшие записи без строки в memories_vec —
// очередь на досчёт вектора фоновым джобом (DESIGN.md §7.4, §9). Если memories_vec ещё
// не создана, возвращает пустой список без ошибки (деградация, DESIGN.md §15).
// Паритет с sqliteStore.ListMissingVectors.
func (s *pgStore) ListMissingVectors(ctx context.Context, limit int) ([]memory.Memory, error) {
	exists, err := s.hasVecTable(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	a := &argList{}
	query := `
		SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, m.expires_at
		FROM memories m
		LEFT JOIN ` + s.vecTable() + ` v ON v.memory_id = m.id
		WHERE m.superseded_by IS NULL AND v.memory_id IS NULL
		ORDER BY m.created_at
	`
	if limit > 0 {
		query += " LIMIT " + a.add(limit)
	}

	rows, err := s.db.QueryContext(ctx, query, a.args...)
	if err != nil {
		return nil, fmt.Errorf("выборка записей без вектора: %w", err)
	}
	defer rows.Close()

	var missing []memory.Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("сканирование записи без вектора: %w", err)
		}
		missing = append(missing, m)
	}
	return missing, rows.Err()
}

// VectorStatus возвращает состояние вектора записи id. Если memories_vec ещё не
// создана, возвращает нулевой VectorStatus{} без ошибки (деградация, DESIGN.md §15).
// Паритет с sqliteStore.VectorStatus.
func (s *pgStore) VectorStatus(ctx context.Context, id string) (VectorStatus, error) {
	exists, err := s.hasVecTable(ctx)
	if err != nil {
		return VectorStatus{}, err
	}
	if !exists {
		return VectorStatus{}, nil
	}

	var embedded bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.vecTable()+` WHERE memory_id = $1)`, id).Scan(&embedded); err != nil {
		return VectorStatus{}, fmt.Errorf("проверка вектора %q: %w", id, err)
	}

	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		return VectorStatus{}, fmt.Errorf("чтение embed_meta для статуса %q: %w", id, err)
	}

	return VectorStatus{Embedded: embedded, Model: model, Dim: dim}, nil
}
