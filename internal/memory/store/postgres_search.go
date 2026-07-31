package store

import (
	"context"
	"fmt"
	"strings"

	"agentmemory/internal/memory"
)

// hasVecTable сообщает, создана ли уже таблица memories_vec. Как и у sqlite-vec
// (sqliteStore.hasVecTable), она не входит в статическую схему (dim неизвестен
// заранее) — появляется только после первого ResetVectorIndex (postgres_vector.go).
//
// to_regclass принимает своим аргументом текст, который резолвится ПО ТЕМ ЖЕ
// правилам search_path, что и DROP TABLE (полное сканирование схем, а не только
// current_schema()) — поэтому, как и у DROP в ResetVectorIndex, ему нельзя
// передавать голое "memories_vec": он может найти чужой одноимённый объект в
// другой схеме search_path раньше "своего" (или вместо него, если своего ещё нет).
// s.vecTable() — то же схема-квалифицированное имя, что использует весь остальной
// код этого драйвера для memories_vec (см. развёрнутый комментарий у
// ResetVectorIndex) — здесь оно передаётся bind-параметром, а не конкатенацией в
// текст запроса: to_regclass(text) сам разбирает квалифицированное имя, включая
// кавычки, ничем не отличаясь в этом от позиции идентификатора в SQL.
func (s *pgStore) hasVecTable(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, s.vecTable()).Scan(&exists); err != nil {
		return false, fmt.Errorf("проверка memories_vec: %w", err)
	}
	return exists, nil
}

// SearchLexical ищет по search_vec (tsvector, GIN-индекс), фильтрует по общим
// metaFiltersPG (scope/kind/tags/TTL/superseded, DESIGN.md §8), исключает записи с
// проставленным superseded_by. Score — ts_rank(search_vec, tsquery) (больше значит
// релевантнее — то же направление, что даёт -bm25(memories_fts) в sqliteStore.SearchLexical),
// Source = "lexical".
//
// Как и в sqlite.go, Tags кандидатов НЕ заполняются (read-hot-path recall, см. коммент
// у sqliteStore.SearchLexical) — при пустом запросе (после токенизации) MATCH-условие
// не накладывается вовсе, порядок — по importance/created_at, как в sqlite.go.
func (s *pgStore) SearchLexical(ctx context.Context, q memory.Query) ([]memory.Candidate, error) {
	a := &argList{}
	conds := metaFiltersPG(a, q, s.now())

	tsExpr, hasQuery := sanitizeTSQuery(a, q.Text)

	var query string
	if hasQuery {
		conds = append(conds, "m.search_vec @@ "+tsExpr)
		query = "SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, m.expires_at" +
			", ts_rank(m.search_vec, " + tsExpr + ") AS score FROM memories m"
	} else {
		query = "SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, m.expires_at" +
			", 0.0 AS score FROM memories m"
	}

	query += " WHERE " + strings.Join(conds, " AND ")
	if hasQuery {
		query += " ORDER BY score DESC"
	} else {
		query += " ORDER BY m.importance DESC, m.created_at DESC"
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	query += " LIMIT " + a.add(limit)

	rows, err := s.db.QueryContext(ctx, query, a.args...)
	if err != nil {
		return nil, fmt.Errorf("лексический поиск: %w", err)
	}
	defer rows.Close()

	var candidates []memory.Candidate
	for rows.Next() {
		var score float64
		m, err := scanMemory(rowWithExtra{row: rows, extra: []any{&score}})
		if err != nil {
			return nil, fmt.Errorf("сканирование кандидата: %w", err)
		}
		candidates = append(candidates, memory.Candidate{Memory: m, Score: score, Source: "lexical"})
	}
	return candidates, rows.Err()
}

// SearchVector ищет по векторному индексу memories_vec (pgvector, HNSW,
// vector_cosine_ops) и применяет те же metaFiltersPG-фильтры, что и SearchLexical.
// Score — 1/(1+distance) (больше значит ближе), Source = "vector", порядок — по
// возрастанию distance (ближайшие первыми). Паритет контракта со sqliteStore.SearchVector.
//
// Важное отличие от SQLite (DESIGN.md §4, «Фильтр + KNN»): sqlite-vec применяет k
// (MATCH ... AND k = ?) ДО внешних фильтров по memories — «k-before-filter», отсюда
// запас vectorTagFilterPad в sqliteStore.SearchVector. pgvector так не делает: WHERE
// накладывается на весь набор кандидатов ДО сортировки по <=> и LIMIT (планировщик
// использует HNSW-индекс для ORDER BY/LIMIT поверх уже отфильтрованного набора, либо
// полный скан при узком фильтре — в любом случае результат корректен без запаса).
// Поэтому здесь LIMIT — ровно q.Limit, без вариаций от Query.Tags: искать «пропавшую»
// константу-запас, аналогичную vectorTagFilterPad, в этом файле не нужно — её здесь
// нет по конструкции, а не по недосмотру.
//
// Если memories_vec ещё не создана (Embedder выключен, ResetVectorIndex ни разу не
// вызывался) или вектор запроса пуст, возвращает (nil, nil) без ошибки — деградация до
// чистого лексического поиска (DESIGN.md §15).
func (s *pgStore) SearchVector(ctx context.Context, vec []float32, q memory.Query) ([]memory.Candidate, error) {
	if len(vec) == 0 {
		return nil, nil
	}

	exists, err := s.hasVecTable(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	a := &argList{}
	vecPh := a.add(vectorLiteral(vec))
	conds := metaFiltersPG(a, q, s.now())

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	limitPh := a.add(limit)

	query := `
		SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, m.expires_at,
			(v.embedding <=> ` + vecPh + `::vector) AS distance
		FROM ` + s.vecTable() + ` v
		JOIN memories m ON m.id = v.memory_id
		WHERE ` + strings.Join(conds, " AND ") + `
		ORDER BY distance ASC
		LIMIT ` + limitPh

	rows, err := s.db.QueryContext(ctx, query, a.args...)
	if err != nil {
		return nil, fmt.Errorf("векторный поиск: %w", err)
	}
	defer rows.Close()

	var candidates []memory.Candidate
	for rows.Next() {
		var distance float64
		m, err := scanMemory(rowWithExtra{row: rows, extra: []any{&distance}})
		if err != nil {
			return nil, fmt.Errorf("сканирование кандидата: %w", err)
		}
		candidates = append(candidates, memory.Candidate{Memory: m, Score: 1.0 / (1.0 + distance), Source: "vector"})
	}
	return candidates, rows.Err()
}

// SemanticNeighbors возвращает до k ближайших по cosine-дистанции живых записей к
// записи id (исключая её саму и superseded), отсортированных по возрастанию дистанции.
// Candidate.Score = distance (честная дистанция, не 1/(1+d)), Candidate.Source =
// "vector". Паритет контракта со sqliteStore.SemanticNeighbors.
//
// Как и в SearchVector, здесь НЕТ аналога semanticNeighborsPad: self/superseded
// исключаются в WHERE вместе с остальными фильтрами и до LIMIT $k, а не после —
// pgvector комбинирует фильтр и KNN в одном плане (DESIGN.md §4), поэтому запрошенные
// k всегда ровно k подходящих соседей (если их столько есть), без риска, что якорь
// (дистанция 0) вытеснит настоящего соседа из top-k.
//
// Если k<=0, у записи id нет вектора, либо memories_vec ещё не создана — возвращает
// (nil, nil) без ошибки (деградация DESIGN.md §15).
func (s *pgStore) SemanticNeighbors(ctx context.Context, id string, k int) ([]memory.Candidate, error) {
	if k <= 0 {
		return nil, nil
	}

	exists, err := s.hasVecTable(ctx)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}

	var hasVector bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.vecTable()+` WHERE memory_id = $1)`, id).Scan(&hasVector); err != nil {
		return nil, fmt.Errorf("проверка вектора %q: %w", id, err)
	}
	if !hasVector {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.kind, m.scope, m.content, m.summary, m.importance, m.created_at, m.updated_at, m.accessed_at, m.access_count, m.superseded_by, m.source, m.expires_at,
			(v.embedding <=> anchor.embedding) AS distance
		FROM `+s.vecTable()+` v
		JOIN memories m ON m.id = v.memory_id
		CROSS JOIN (SELECT embedding FROM `+s.vecTable()+` WHERE memory_id = $1) AS anchor
		WHERE m.id <> $1 AND m.superseded_by IS NULL
		ORDER BY distance ASC
		LIMIT $2
	`, id, k)
	if err != nil {
		return nil, fmt.Errorf("поиск семантических соседей %q: %w", id, err)
	}
	defer rows.Close()

	var candidates []memory.Candidate
	for rows.Next() {
		var distance float64
		m, err := scanMemory(rowWithExtra{row: rows, extra: []any{&distance}})
		if err != nil {
			return nil, fmt.Errorf("сканирование семантического соседа %q: %w", id, err)
		}
		candidates = append(candidates, memory.Candidate{Memory: m, Score: distance, Source: "vector"})
	}
	return candidates, rows.Err()
}
