package store

// common.go собирает код, буквально совпадающий между sqliteStore и pgStore — запросы
// без единого диалектного плейсхолдера, работающие через один и тот же *sql.DB и
// scanMemory (rowScanner, определённый в sqlite.go — общая точка для обоих бэкендов,
// см. её комментарий). Здесь — не о том, ЧТО запрашивается (это по-прежнему решает
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
		SELECT id, kind, scope, content, summary, importance, created_at, updated_at, accessed_at, access_count, superseded_by, source, expires_at
		FROM memories
		WHERE superseded_by IS NULL
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
