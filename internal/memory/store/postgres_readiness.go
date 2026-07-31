// postgres_readiness.go — read-only диагностика ГОТОВНОСТИ PostgreSQL к первому
// штатному старту memo (DESIGN.md §15, задача 12.1), поверх уже открытого через
// OpenPostgresReadOnly/OpenPostgresReadOnlyContext соединения. Отличие от Health
// (postgres.go): Health проверяет ДОСТУПНОСТЬ ("сервер отвечает, база открывается"),
// этот файл — ГОТОВНОСТЬ ("первый store.Open/ResetVectorIndex на этой базе не
// деградирует") — сервер может быть вполне доступен, но не готов (нет pgvector, нет
// прав на CREATE), и это не повод валить пробу подключения, а повод предупредить.
//
// Не часть интерфейса Store: это специфика ТОЛЬКО postgres-бэкенда (у sqlite нет ни
// расширений, ни ролей с правами CREATE), добавлять её в общий Store размыло бы
// контракт, единый для обоих бэкендов (DESIGN.md §15 "всё за интерфейсами" — про
// СМЕНУ бэкенда, а не про утечку специфики одного бэкенда в общий интерфейс). Вместо
// этого — необязательный интерфейс (по образцу io.ReaderFrom/io.WriterTo в stdlib):
// вызывающий код (`memo init`, cmd/memo) type-assert'ит уже открытый Store и получает
// диагностику поверх ТОГО ЖЕ соединения, не открывая второе.
package store

import (
	"context"
	"fmt"
)

// PostgresReadiness — результат read-only диагностики готовности БД к первому
// штатному старту memo (см. пакетный комментарий выше про разницу с Health —
// доступность против готовности, деградация, а не отказ).
type PostgresReadiness struct {
	// VectorExtension — резолвится ли тип vector (to_regtype('vector') IS NOT
	// NULL) по search_path текущего соединения. false здесь НЕ означает
	// готовую деградацию: ResetVectorIndex сам выполняет
	// CREATE EXTENSION IF NOT EXISTS vector при первом reindex
	// (postgres_vector.go) — если у роли есть право CREATE, векторный слой
	// поднимется сам, без участия администратора. false лишь предупреждает,
	// что ЕСЛИ прав не хватит, recall молча деградирует до чистого
	// лексического поиска (§15).
	//
	// Проверяется через to_regtype, а не через pg_extension (как было раньше)
	// намеренно: pg_extension — каталог уровня БД, он фиксирует, что
	// расширение установлено ГДЕ-ТО, но не говорит, видно ли имя типа vector
	// по search_path этого соединения. Если pgvector стоит в схеме вне
	// search_path, pg_extension сказал бы "готово", а первый же
	// CREATE TABLE ... embedding vector(N) упал бы с "type vector does not
	// exist" — то есть проверка врала бы ровно в сценарии, ради которого она
	// существует. to_regtype резолвит имя типа той же логикой, от которой
	// зависит рабочий код (CREATE TABLE), поэтому не может разойтись с ним.
	VectorExtension bool
	// CanCreate — есть ли у текущей роли право CREATE в схеме, зафиксированной
	// этим соединением при открытии (см. pgStore.schema/current_schema()). Без
	// него первый штатный старт memo не сможет накатить схему памяти (pgStore.init).
	CanCreate bool
}

// PostgresReadinessChecker — необязательный интерфейс, которому удовлетворяет
// Store, полученный через OpenPostgresReadOnly/OpenPostgresReadOnlyContext.
// Позволяет вызывающему коду получить диагностику готовности БД поверх уже
// открытого read-only соединения одним доп. запросом, не открывая второе
// соединение и не расширяя общий интерфейс Store постгрес-специфичным методом.
type PostgresReadinessChecker interface {
	CheckPostgresReadiness(ctx context.Context) (PostgresReadiness, error)
}

var _ PostgresReadinessChecker = (*pgStore)(nil)

// CheckPostgresReadiness выполняет ОДИН read-only запрос (никакого DDL — тот же
// инвариант, что и у остального OpenPostgresReadOnly-пути: проба подключения не
// имеет права ничего создавать/менять на сервере, см. комментарий у
// OpenPostgresReadOnly): резолвится ли имя типа vector по search_path этого
// соединения (см. PostgresReadiness.VectorExtension, почему именно to_regtype)
// и право CREATE в схеме s.schema, зафиксированной один раз при открытии
// соединения. Оба значения читаются одним SELECT, а не двумя, чтобы не
// удваивать round-trip ради диагностики, которая и так идёт поверх уже
// открытого соединения.
//
// has_schema_privilege($1::text, 'CREATE') — явный каст параметра к text: у
// двухаргументной формы has_schema_privilege есть перегрузки (text,text) и
// (oid,text), а s.schema передаётся как нетипизированный bind-параметр —
// без ::text выбор перегрузки зависит от того, как драйвер/сервер выведут
// тип параметра, чего эта диагностика полагаться не должна.
//
// Ошибка означает, что сам диагностический запрос не выполнился (напр. на
// нестандартно урезанных правах роли нет доступа даже к to_regtype) — это
// НЕ означает, что соединение или Health неисправны: они уже подтверждены
// отдельно до вызова этого метода. Вызывающая сторона (`memo init`) обязана
// трактовать такую ошибку так же, как false в PostgresReadiness — предупреждение,
// а не отказ пробы.
func (s *pgStore) CheckPostgresReadiness(ctx context.Context) (PostgresReadiness, error) {
	var r PostgresReadiness
	err := s.db.QueryRowContext(ctx, `
		SELECT
			to_regtype('vector') IS NOT NULL,
			has_schema_privilege($1::text, 'CREATE')
	`, s.schema).Scan(&r.VectorExtension, &r.CanCreate)
	if err != nil {
		return PostgresReadiness{}, fmt.Errorf("диагностика готовности БД: %w", err)
	}
	return r, nil
}
