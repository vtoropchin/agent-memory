// postgres.go — реализация Store поверх PostgreSQL+pgvector (DESIGN.md §4, §13,
// store.driver="postgres"): общая база команды, вторая реализация Store рядом со
// SQLite (sqlite.go). Использует github.com/jackc/pgx/v5 через адаптер
// github.com/jackc/pgx/v5/stdlib (регистрирует database/sql-драйвер "pgx") — тот же
// стиль работы с *sql.DB/*sql.Tx, что и в sqlite.go, вместо отдельного pgx-native API.
//
// Общее со sqlite.go: доменная модель (memory.Memory), контракты методов Store
// (DESIGN.md §6), ErrNotFound, listDefaultLimit, memoryColumns/scanMemory/rowWithExtra
// (common.go; переиспользуются как есть — они оперируют абстрактным rowScanner и не
// зависят от драйвера).
// Расхождения — только там, где их явно допускает DESIGN.md §4 (таблица «SQLite vs
// PostgreSQL»): стемминг силами СУБД (`to_tsvector('russian', ...)` вместо
// content_stemmed/normalizeForFTS), отсутствие k-before-filter у pgvector (см.
// postgres_search.go) и поля HealthInfo (см. Health ниже). Остальные методы —
// поведенческий паритет со SQLite.
//
// Плейсхолдеры — $1, $2, ... (а не ? как в sqlite.go): запросы с динамическим числом
// аргументов (metaFilters, scopePredicate, IN(...)) собираются через argList
// (postgres_query.go), которая сама нумерует позиции.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // регистрирует database/sql-драйвер "pgx"

	"agentmemory/internal/config"
)

// pgStore — реализация Store поверх PostgreSQL (метаданные + tsvector/GIN + pgvector/HNSW).
type pgStore struct {
	db  *sql.DB
	now func() int64 // источник текущего unix ts; подменяется в тестах (см. sqliteStore.now)

	// schema — имя схемы, зафиксированное один раз при init() через current_schema()
	// (см. init). Используется исключительно для ОДНОЗНАЧНОЙ адресации memories_vec
	// (postgres_vector.go, hasVecTable в postgres_search.go) — таблицы, которая, в
	// отличие от memories/tags/links/embed_meta/schema_version, создаётся не сразу
	// при Open, а лениво в ResetVectorIndex, и потому может в момент первого вызова
	// ещё отсутствовать в целевой схеме. Развёрнутое обоснование — у ResetVectorIndex.
	schema string
}

var _ Store = (*pgStore)(nil)

// dialPostgres открывает пул соединений с PostgreSQL по cfg.DSN() и проверяет его
// пробным пингом. Общая часть openPostgres и OpenPostgresReadOnly — то, что должно
// произойти в ЛЮБОМ случае перед тем, как решать, накатывать схему или нет.
//
// Пул настроен явно, а не оставлен на дефолт database/sql (MaxOpenConns=0 —
// неограниченно): postgres.lan (DESIGN.md §4, §15) — общая на всю команду база, и
// профиль нагрузки этого драйвера — не один процесс, а много. Open вызывается на
// КАЖДЫЙ CLI-инвок memo (короткоживущий процесс: одна-две команды и выход) и
// отдельно на старт MCP-сервера (держит один *pgStore открытым постоянно, DESIGN.md
// §1 "не промышленный масштаб" — там же и оценка, что параллелизм внутри одного
// такого сервера скромный, не десятки одновременных запросов). Неограниченный пул
// на этом фоне не даёт выгоды (ни CLI, ни MCP-серверу параллелизм в разы больше
// нескольких соединений не нужен), зато у нескольких таких процессов/агентов,
// работающих на одной БД одновременно, есть риск совместно упереться в
// max_connections сервера — настройка, которой memo не владеет и знать не может.
//   - MaxOpenConns=10 — с запасом на кратковременный всплеск параллельных операций
//     MCP-сервера (recall параллельно с save и т.п.), но не настолько много, чтобы
//     один процесс мог в одиночку исчерпать заметную долю дефолтного
//     max_connections=100 общего сервера.
//   - MaxIdleConns=5 — половина от MaxOpenConns: держит горячий пул на переиспользование
//     без пересоздания TCP+TLS-хендшейка на каждый запрос, но не копит простаивающие
//     соединения сверх реальной наблюдаемой параллельности.
//   - ConnMaxIdleTime=5m — простаивающие соединения долгоживущего MCP-сервера (CLI
//     этого не увидит — процесс успевает завершиться раньше) освобождаются после
//     нескольких минут затишья, а не висят часами занятыми слотами на общем сервере.
//   - ConnMaxLifetime=30m — периодическая ротация физических соединений даже под
//     непрерывной нагрузкой: переживает рестарт/переконфигурацию сервера или
//     промежуточного узла сети (LAN, но не факт, что без балансировщика) без
//     необходимости перезапускать memo, и подчищает соединения, накопившие
//     server-side состояние (temp-объекты, память сессии) за долгую жизнь.
//
// ctx ограничивает по времени именно установление соединения (см. PingContext
// ниже) — вызывающие без своего дедлайна передают context.Background() и
// получают прежнее поведение.
func dialPostgres(ctx context.Context, cfg config.StoreConfig) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("открытие postgres %s: %w", cfg.Target(), err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	// PingContext, а не Ping: sql.Open ленив, и именно этот вызов выполняет
	// настоящую работу — DNS-резолв, TCP-коннект и стартовый хендшейк pgx, —
	// поэтому только через ctx её и можно ограничить по времени. Без дедлайна
	// вызывающий висит до таймаута ОС на хосте, который дропает пакеты
	// (закрытый firewall, опечатка в адресе); для `memo init` это прямо
	// нарушало бы принцип «установка не блокируется недоступным сервисом».
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("проверка соединения %s: %w", cfg.Target(), err)
	}
	return db, nil
}

// openPostgres открывает соединение с PostgreSQL по cfg.DSN(), применяет схему и
// возвращает готовый Store. Вызывается из фабрики Open (factory.go) при
// cfg.Driver == "postgres". В отличие от OpenSQLite, ничего не создаёт на диске —
// база должна уже существовать на сервере (её создание — забота оператора/CI, не
// этого кода); расширение vector создаётся лениво в ResetVectorIndex (см.
// postgres_vector.go), а не здесь — до первого reindex векторный слой не нужен,
// как и memories_vec у sqlite-vec (DESIGN.md §15).
func openPostgres(cfg config.StoreConfig) (Store, error) {
	db, err := dialPostgres(context.Background(), cfg)
	if err != nil {
		return nil, err
	}

	s := &pgStore{db: db, now: defaultNow}
	if err := s.init(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("инициализация схемы %s: %w", cfg.Target(), err)
	}

	return s, nil
}

// OpenPostgresReadOnly открывает соединение с PostgreSQL БЕЗ применения схемы —
// никаких CREATE/ALTER/DROP, только Ping и фиксация current_schema() (нужна
// pgStore.schema, см. её комментарий — от неё зависят EmbedMeta/ListMissingVectors/
// vecTable через hasVecTable, даже read-only). Отдельная от openPostgres точка входа
// для `memo doctor` (cmd/memo/doctor.go, postgresDatabaseSection): обычный путь через
// фабрику Open (и, соответственно, openPostgres.init) накатывает схему как побочный
// эффект открытия — недопустимо для read-only диагностики (DESIGN.md §15, задача 9.1:
// doctor "ничего не чинит и не пишет в БД"). Симметрично тому, как databaseSection для
// sqlite проверяет os.Stat ДО OpenSQLite, чтобы не создать файл, — только для postgres
// нет локального файла, чьё существование можно проверить заранее, поэтому граница
// проходит не "до открытия", а в том, какой набор операций открытие выполняет.
//
// Если схема памяти (таблицы) на сервере ещё не создана, методы Store, которым она
// нужна (Stats, EmbedMeta, ...), вернут ошибку вида "relation ... does not exist" —
// это ожидаемый диагностический сигнал ("БД доступна, но схема ещё не накатана"), а
// не повод создать её здесь.
//
// Без ctx (эта форма) открытие ограничено только таймаутами ОС — так её вызывает
// `memo doctor`, которому пользователь сам сказал «диагностируй» и может прервать
// по Ctrl+C. Вызывающим, которым нужен дедлайн (напр. проба подключения в
// `memo init`, где висящая установка недопустима), — OpenPostgresReadOnlyContext.
func OpenPostgresReadOnly(cfg config.StoreConfig) (Store, error) {
	return OpenPostgresReadOnlyContext(context.Background(), cfg)
}

// OpenPostgresReadOnlyContext — та же read-only точка входа, что
// OpenPostgresReadOnly, но с ctx: он ограничивает и установление соединения
// (dialPostgres -> PingContext), и чтение current_schema(). Отдельная форма, а не
// смена сигнатуры существующей функции — чтобы не трогать вызов из `memo doctor`
// и его контракт.
func OpenPostgresReadOnlyContext(ctx context.Context, cfg config.StoreConfig) (Store, error) {
	db, err := dialPostgres(ctx, cfg)
	if err != nil {
		return nil, err
	}

	s := &pgStore{db: db, now: defaultNow}
	if err := db.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&s.schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("определение текущей схемы %s: %w", cfg.Target(), err)
	}

	return s, nil
}

// Close закрывает пул соединений с БД.
func (s *pgStore) Close() error {
	return s.db.Close()
}

// baseSchemaStatements — DDL хранилища метаданных (DESIGN.md §4), идемпотентен (IF
// NOT EXISTS везде) — init можно вызывать многократно на одной БД. Каждый элемент —
// РОВНО одна SQL-команда: pgx/database/sql по умолчанию использует extended query
// protocol, который допускает не более одной команды за Exec (в отличие от
// mattn/go-sqlite3, чей db.Exec умеет выполнить целую пачку `;`-разделённых
// операторов одним вызовом, как в sqlite.go baseSchema/ftsTriggers) — поэтому здесь
// схема оформлена как срез отдельных операторов, а не одна большая строка.
//
// search_vec (tsvector) и GIN-индекс по ней — функциональный аналог memories_fts:
// в отличие от SQLite (отдельная virtual table memories_fts, синхронизируемая
// триггерами), здесь это просто ещё одна колонка memories — Postgres не нуждается в
// отдельной FTS-структуре, GIN-индекс по tsvector-колонке и есть лексический индекс.
//
// memories_vec сюда НЕ входит — как и у sqlite-vec, её форма (размерность вектора)
// известна только после первого ResetVectorIndex (postgres_vector.go).
var baseSchemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS schema_version (
		id      smallint PRIMARY KEY CHECK (id = 1),
		version integer NOT NULL
	)`,
	// triggers_checksum добавлена отдельным ALTER (не в CREATE TABLE выше) — так
	// колонка появляется идемпотентно и на уже существующих БД, где schema_version
	// была создана ДО того, как эта колонка появилась в коде, а не только на
	// свежих. Хранит sha256 от searchVecTriggerStatements, применённых последним
	// init (см. applySearchVecTriggers) — позволяет пропускать их безусловное
	// переприменение на каждом Open (см. развёрнутый комментарий у init/
	// applySearchVecTriggers про гонку, которую это раньше вызывало).
	`ALTER TABLE schema_version ADD COLUMN IF NOT EXISTS triggers_checksum text`,
	`CREATE TABLE IF NOT EXISTS memories (
		id            text PRIMARY KEY,
		kind          text NOT NULL,
		scope         text NOT NULL,
		content       text NOT NULL,
		summary       text,
		importance    integer NOT NULL DEFAULT 3,
		created_at    bigint NOT NULL,
		updated_at    bigint NOT NULL,
		accessed_at   bigint,
		access_count  integer NOT NULL DEFAULT 0,
		superseded_by text,
		source        text,
		expires_at    bigint,
		status        text,
		search_vec    tsvector
	)`,
	// status добавляется ещё и отдельным ALTER (не только в CREATE TABLE выше) —
	// идемпотентно и для БД, созданных до появления задач (kind=task, задача
	// 14.1): в SQLite ту же роль играет шаг миграции migrateStatus, здесь
	// хватило ADD COLUMN IF NOT EXISTS (тот же приём, что у
	// schema_version.triggers_checksum), потому что data-шагов у той задачи не
	// было. Task-поля Этапа 15 (task_type/branch/workflow) сюда НЕ добавляются —
	// они идут версионным шагом pgMigrations[0] вместе со своими data-UPDATE
	// (см. migrateTaskModelPG).
	`ALTER TABLE memories ADD COLUMN IF NOT EXISTS status text`,
	`CREATE INDEX IF NOT EXISTS memories_search_vec_idx ON memories USING GIN (search_vec)`,
	`CREATE TABLE IF NOT EXISTS tags (
		memory_id text NOT NULL REFERENCES memories(id),
		tag       text NOT NULL,
		PRIMARY KEY (memory_id, tag)
	)`,
	`CREATE TABLE IF NOT EXISTS links (
		src text NOT NULL REFERENCES memories(id),
		dst text NOT NULL REFERENCES memories(id),
		rel text NOT NULL DEFAULT 'related',
		PRIMARY KEY (src, dst, rel)
	)`,
	`CREATE TABLE IF NOT EXISTS embed_meta (
		id    smallint PRIMARY KEY CHECK (id = 1),
		model text NOT NULL,
		dim   integer NOT NULL
	)`,
}

// searchVecTriggerStatements синхронизирует memories.search_vec с content/summary
// (memories) и tags — функциональный аналог ftsTriggers из sqlite.go, но
// стемминг здесь делает сама СУБД: to_tsvector('russian', ...) применяет
// snowball-стеммер русского языка сервера, поэтому отдельная стеммированная
// колонка (content_stemmed в SQLite, задача 8.3) не нужна — задача явно оговаривает
// это расхождение (DESIGN.md §4, таблица «SQLite vs PostgreSQL»).
//
// memories_search_vec_update — BEFORE INSERT OR UPDATE OF (content, summary):
// пересчитывает NEW.search_vec ДО записи строки (эффективнее AFTER + отдельный
// UPDATE). Список колонок у UPDATE OF означает, что триггер не перезапускает
// пересчёт на UPDATE, не трогающий content/summary (напр. Supersede/Touch) — но
// исходный Put ВСЕГДА перечисляет content/summary в SET (даже если значение не
// изменилось), так что для апсерта эффект идентичен sqlite: search_vec актуален
// после каждого Put.
//
// tags_search_vec_update — AFTER INSERT OR DELETE ON tags (без UPDATE — тегов
// никогда не обновляют на месте, только удаляют и вставляют заново, см. Put; та же
// пара событий, что у tags_ai/tags_ad в sqlite.go ftsTriggers): дочитывает
// актуальные content/summary из memories и пересобирает search_vec целиком.
// Порядок Put (upsert memories -> DELETE+INSERT tags) гарантирует, что к моменту
// срабатывания этого триггера memories уже содержит финальные content/summary в
// этой же транзакции.
//
// CREATE OR REPLACE / DROP TRIGGER IF EXISTS + CREATE (не "IF NOT EXISTS" — для
// триггеров в Postgres такого синтаксиса нет): раньше выполнялись безусловно при
// каждом Open (как ftsTriggers в sqlite.go), чтобы определение триггеров
// гарантированно было актуальным независимо от истории миграций. Это оказалось
// небезопасно под конкурентными Open (см. развёрнутый комментарий у init про
// гонку) и избыточно на горячем пути (Open вызывается на каждый CLI-инвок
// memo) — applySearchVecTriggers (ниже) применяет эти операторы только когда
// их текст (searchVecTriggersChecksum) разошёлся с последним применённым, но
// сам механизм "определение триггеров гарантированно актуально" остаётся: при
// любом изменении этого среза чек-сумма меняется и триггеры переприменяются
// автоматически, без отдельной ручной миграции.
var searchVecTriggerStatements = []string{
	`CREATE OR REPLACE FUNCTION memories_tags_text(mid text) RETURNS text AS $$
		SELECT COALESCE(string_agg(tag, ' '), '') FROM tags WHERE memory_id = mid;
	$$ LANGUAGE sql STABLE`,

	`CREATE OR REPLACE FUNCTION memories_search_vec_update() RETURNS trigger AS $$
	BEGIN
		NEW.search_vec := to_tsvector('russian',
			coalesce(NEW.content, '') || ' ' || coalesce(NEW.summary, '') || ' ' || memories_tags_text(NEW.id));
		RETURN NEW;
	END;
	$$ LANGUAGE plpgsql`,

	`DROP TRIGGER IF EXISTS memories_search_vec_trg ON memories`,
	`CREATE TRIGGER memories_search_vec_trg
		BEFORE INSERT OR UPDATE OF content, summary ON memories
		FOR EACH ROW EXECUTE FUNCTION memories_search_vec_update()`,

	`CREATE OR REPLACE FUNCTION tags_search_vec_update() RETURNS trigger AS $$
	DECLARE
		mid text := COALESCE(NEW.memory_id, OLD.memory_id);
	BEGIN
		UPDATE memories SET search_vec = to_tsvector('russian',
			coalesce(content, '') || ' ' || coalesce(summary, '') || ' ' || memories_tags_text(mid))
		WHERE id = mid;
		RETURN NULL;
	END;
	$$ LANGUAGE plpgsql`,

	`DROP TRIGGER IF EXISTS tags_search_vec_trg ON tags`,
	`CREATE TRIGGER tags_search_vec_trg
		AFTER INSERT OR DELETE ON tags
		FOR EACH ROW EXECUTE FUNCTION tags_search_vec_update()`,
}

// pgSchemaVersion — целевая версия схемы (см. таблицу schema_version). В отличие от
// sqlite.go (где schemaVersion отражает уже прожитую историю миграций FTS5/TTL/
// status), PostgreSQL-бэкенд был введён сразу с целевой схемой (задача, поставившая
// эту реализацию, прямо этого требует — не тащить исторические миграции SQLite) —
// поэтому стартовое значение было 0 при пустом pgMigrations. Первый реальный шаг —
// Этап 15 (migrateTaskModelPG, 0 -> 1): нумерация версий SQLite (4) и PostgreSQL (1)
// намеренно не совпадает — у них разная история, общая только форма memories на
// выходе. Следующая миграция добавляется как pgMigrations[1] (1 -> 2) и поднимает
// эту константу до 2.
const pgSchemaVersion = 1

// pgMigrations — упорядоченный список шагов миграции схемы, pgMigrations[i] соответствует
// переходу версии i -> i+1 (см. pgSchemaVersion).
var pgMigrations = []migrationStep{
	migrateTaskModelPG, // 0 -> 1 (Этап 15): memories + task_type/branch/workflow, blocked -> todo, backfill task_type
}

// migrateTaskModelPG — миграция 0 -> 1 (Этап 15): модель задачи v2, паритет с
// migrateTaskModel (sqlite.go). Колонки task_type/branch/workflow живут ТОЛЬКО
// здесь, а не в baseSchemaStatements (в отличие от status, добавленной там через
// ADD COLUMN IF NOT EXISTS): миграция несёт data-шаги (blocked -> todo, backfill
// task_type по тегам), а baseSchemaStatements накатывается на каждом Open, и
// UPDATE по всей таблице при каждом старте недопустим даже идемпотентный
// (DESIGN.md §14 Этап 15). Раз data-шаги версионные — и колонки рядом с ними,
// в одном месте: свежая БД получает memories из baseSchemaStatements без новых
// колонок и тут же, в том же init, проходит этот шаг. ADD COLUMN IF NOT EXISTS
// — на случай, если колонка уже есть (ручная правка); data-шаги — те же
// taskModelDataSteps, что и у SQLite, с перенумерованными плейсхолдерами.
func migrateTaskModelPG(ctx context.Context, tx *sql.Tx) error {
	for _, column := range []string{"task_type", "branch", "workflow"} {
		// column — константа из списка выше, не пользовательский ввод.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE memories ADD COLUMN IF NOT EXISTS %s text`, column)); err != nil {
			return fmt.Errorf("добавление колонки %s: %w", column, err)
		}
	}
	for _, step := range taskModelDataSteps {
		if _, err := tx.ExecContext(ctx, pgPlaceholders(step.sql), step.args...); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

// pgPlaceholders перенумеровывает позиционные плейсхолдеры ? (SQLite) в $1, $2, …
// (PostgreSQL) слева направо — для SQL, общего для обоих драйверов
// (taskModelDataSteps). Применять только к тексту, где каждый ? — плейсхолдер
// (литералов с ? в таких запросах нет).
func pgPlaceholders(query string) string {
	var b strings.Builder
	n := 0
	for _, r := range query {
		if r == '?' {
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// schemaInitLockKey — ключ Postgres advisory-лока (pg_advisory_lock), которым init
// сериализует применение схемы между КОНКУРЕНТНЫМИ Open. Значение — произвольная
// константа, уникальная в рамках этого приложения: пространство ключей advisory-локов
// общее на всю БД, коллизия с другим приложением, использующим тот же bigint, крайне
// маловероятна и не катастрофична (просто лишняя избыточная сериализация с ним).
//
// Суть гонки, которую этот лок устраняет (найдена ревью экспериментально: 8
// конкурентных горутин на одной схеме, стабильно 3 из 3 прогонов): раньше init
// выполнял DDL (CREATE TABLE/INDEX IF NOT EXISTS, CREATE OR REPLACE FUNCTION, DROP
// TRIGGER IF EXISTS + CREATE TRIGGER) безусловно и БЕЗ КООРДИНАЦИИ на каждый Open,
// отдельными Exec вне транзакции. Postgres не гарантирует атомарность "проверить-и-
// создать" для CREATE ... IF NOT EXISTS под конкуренцией: CREATE TABLE создаёт
// составной тип в pg_type, и две параллельные попытки создать одноимённую таблицу
// могут обе пройти проверку "объекта ещё нет" ДО того, как одна из них закоммитит
// INSERT в pg_type, — вторая падает на "повторяющееся значение ключа нарушает
// ограничение уникальности pg_type_typname_nsp_index". Хуже с DROP TRIGGER IF EXISTS
// + CREATE TRIGGER: между DROP и CREATE другая сессия может успеть создать тот же
// триггер — CREATE TRIGGER падает на "уже существует" (для триггеров в Postgres нет
// "IF NOT EXISTS"), либо обе сессии одновременно правят одну и ту же строку
// системного каталога — "tuple concurrently updated".
//
// В проде это ложный отказ, а не деградация (DESIGN.md §15): MCP-сервер уже держит
// Store открытым (сам прошёл init при старте), и любой параллельный `memo save`/
// `doctor`/`migrate` в тот же момент падает целиком с ошибкой "уже существует", хотя
// схема полностью исправна — при том что init вызывается на КАЖДЫЙ CLI-инвок memo
// (Open -> init всегда), а не один раз в жизни БД.
const schemaInitLockKey int64 = 0x41674D656D6F5343 // произвольно, "AgMemoSC" в hex

// init приводит схему к целевому виду: baseSchemaStatements (идемпотентно, каждый
// оператор отдельным Exec — см. baseSchemaStatements), затем migrate (версионные
// правки схемы, см. pgSchemaVersion), затем applySearchVecTriggers (см. её
// комментарий и searchVecTriggerStatements). Порядок совпадает со sqlite.go:
// baseSchema -> migrate -> ftsTriggers.
//
// Всё выполняется на ОДНОМ выделенном *sql.Conn (не через s.db напрямую), взятом из
// пула на время init и удерживающем advisory lock (schemaInitLockKey) — см. её
// комментарий про природу гонки, которую это устраняет. pg_advisory_lock —
// сессионный (session-level) лок: держится, пока жива сессия, или пока его явно не
// снимут pg_advisory_unlock НА ТОЙ ЖЕ сессии; *sql.DB — пул из нескольких физических
// соединений, и обычные db.ExecContext(...) могли бы уйти на разные соединения, из-за
// чего lock/unlock не гарантированно оказались бы в одной сессии. Лок виден и
// блокирует одинаково между процессами (advisory-локи Postgres — на уровне БД, а не
// одного backend'а) — конкурентные `memo`-инвоки разных процессов на одной БД
// сериализуются так же, как и горутины одного процесса.
func (s *pgStore) init(ctx context.Context) (err error) {
	conn, connErr := s.db.Conn(ctx)
	if connErr != nil {
		return fmt.Errorf("получение соединения для инициализации схемы: %w", connErr)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("закрытие соединения инициализации схемы: %w", closeErr))
		}
	}()

	if _, lockErr := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, schemaInitLockKey); lockErr != nil {
		return fmt.Errorf("захват advisory lock инициализации схемы: %w", lockErr)
	}
	defer func() {
		// Отдельный context.Background(): ctx мог быть отменён к этому моменту
		// (defer срабатывает и при ошибке применения схемы выше по стеку), а снять
		// лок нужно всегда — иначе он провисит на conn до его закрытия/возврата в
		// пул, блокируя init всех последующих Open, использующих то же
		// соединение (advisory-лок — per-session, а не per-statement).
		if _, unlockErr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, schemaInitLockKey); unlockErr != nil {
			err = errors.Join(err, fmt.Errorf("снятие advisory lock инициализации схемы: %w", unlockErr))
		}
	}()

	// current_schema() — та же схема, в которую Postgres кладёт объекты, созданные
	// БЕЗ явного указания схемы (это её официальное определение, см. документацию
	// Postgres по current_schema(): "the schema that will be used for any tables or
	// other named objects that are created without specifying a target schema"), то
	// есть ровно то, что ниже сделает baseSchemaStatements для memories/tags/links/
	// embed_meta/schema_version через `CREATE TABLE IF NOT EXISTS`. Фиксируем её один
	// раз здесь и переиспользуем в pgStore.schema для memories_vec (postgres_vector.go)
	// — единственной таблицы, которую нельзя оставить резолвиться по search_path
	// целиком, см. комментарий у ResetVectorIndex.
	if scanErr := conn.QueryRowContext(ctx, `SELECT current_schema()`).Scan(&s.schema); scanErr != nil {
		return fmt.Errorf("определение текущей схемы: %w", scanErr)
	}

	for _, stmt := range baseSchemaStatements {
		if _, execErr := conn.ExecContext(ctx, stmt); execErr != nil {
			return fmt.Errorf("применение схемы: %w", execErr)
		}
	}

	if migErr := s.migrate(ctx, conn); migErr != nil {
		return fmt.Errorf("миграция схемы: %w", migErr)
	}

	if trigErr := applySearchVecTriggers(ctx, conn); trigErr != nil {
		return fmt.Errorf("применение триггеров search_vec: %w", trigErr)
	}

	return nil
}

// migrate применяет по порядку шаги pgMigrations, начиная с версии, прочитанной из
// schema_version (0, если строки ещё нет — свежая БД), до pgSchemaVersion. Каждый шаг —
// в своей транзакции; версия пишется сразу после успешного шага (а не одной
// транзакцией на всё), чтобы обрыв между шагами не переигрывал уже применённые —
// тот же принцип, что и у sqliteStore.migrate (PRAGMA user_version там, таблица
// schema_version здесь). conn — выделенное соединение init, держащее advisory lock
// (см. её комментарий) — миграции этим же локом сериализованы между конкурентными
// Open, отдельной координации им не нужно.
func (s *pgStore) migrate(ctx context.Context, conn *sql.Conn) error {
	var version int
	err := conn.QueryRowContext(ctx, `SELECT version FROM schema_version WHERE id = 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		version = 0
	} else if err != nil {
		return fmt.Errorf("чтение schema_version: %w", err)
	}

	for version < pgSchemaVersion {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("начало транзакции миграции: %w", err)
		}

		if err := pgMigrations[version](ctx, tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("миграция схемы %d -> %d: %w", version, version+1, err)
		}

		version++
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO schema_version (id, version) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET version = excluded.version
		`, version); err != nil {
			tx.Rollback()
			return fmt.Errorf("обновление schema_version до %d: %w", version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit миграции схемы (версия %d): %w", version, err)
		}
	}
	return nil
}

// searchVecTriggersChecksum — sha256 (hex) конкатенации searchVecTriggerStatements в
// их текущем порядке. Меняется тогда и только тогда, когда меняется текст этих
// операторов — используется applySearchVecTriggers, чтобы отличить "уже применено в
// актуальном виде" от "нужно (пере)применить", не храня и не сравнивая весь текст
// целиком.
func searchVecTriggersChecksum() string {
	h := sha256.New()
	for _, stmt := range searchVecTriggerStatements {
		h.Write([]byte(stmt))
		h.Write([]byte{0}) // разделитель, чтобы конкатенация двух операторов не совпала с одним склеенным
	}
	return hex.EncodeToString(h.Sum(nil))
}

// applySearchVecTriggers применяет searchVecTriggerStatements, но только если их
// текущая чек-сумма (searchVecTriggersChecksum) расходится с сохранённой в
// schema_version.triggers_checksum — то есть НЕ безусловно на каждый Open, как было
// раньше (см. комментарий у searchVecTriggerStatements и у schemaInitLockKey про
// найденную этим гонку). Экономит DDL-запись в системный каталог (DROP+CREATE TRIGGER
// x2, CREATE OR REPLACE FUNCTION x3) на подавляющем большинстве вызовов Open — они
// происходят на каждый CLI-инвок memo, то есть в проде многократно за сессию.
//
// Механизм "определение триггеров гарантированно актуально" при этом не теряется:
// если searchVecTriggerStatements когда-нибудь изменятся (новая версия memo), чек-
// сумма разойдётся с сохранённой в БД — операторы применятся заново автоматически,
// без отдельной ручной миграции, как и раньше. conn — то же выделенное соединение,
// что держит advisory lock (см. init) — вызывать эту функцию вне сериализованного
// init небезопасно ровно по той же причине, что и остальной DDL там.
func applySearchVecTriggers(ctx context.Context, conn *sql.Conn) error {
	want := searchVecTriggersChecksum()

	var got sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT triggers_checksum FROM schema_version WHERE id = 1`).Scan(&got)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("чтение triggers_checksum: %w", err)
	}
	if got.Valid && got.String == want {
		return nil // уже применены в актуальном виде — переприменять не нужно
	}

	for _, stmt := range searchVecTriggerStatements {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("%w", err)
		}
	}

	// migrate() выше уже создал строку schema_version (pgMigrations непуст с Этапа
	// 15) — ON CONFLICT ниже трогает только triggers_checksum, version из excluded
	// не берётся и остаётся прежним; 0 в VALUES — лишь на случай, когда строки
	// почему-то нет (согласуется с версией свежей, ещё не мигрированной БД).
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO schema_version (id, version, triggers_checksum) VALUES (1, 0, $1)
		ON CONFLICT (id) DO UPDATE SET triggers_checksum = excluded.triggers_checksum
	`, want); err != nil {
		return fmt.Errorf("запись triggers_checksum: %w", err)
	}
	return nil
}

// Health возвращает быстрый read-only снимок здоровья соединения (DESIGN.md §6:
// «PostgreSQL: эквивалентные по смыслу серверные проверки», задача 9.1 `memo doctor`,
// см. cmd/memo/doctor.go postgresDatabaseSection). У PostgreSQL нет файла и PRAGMA —
// QuickCheck/JournalMode заполняются содержательными эквивалентами, а не пустыми
// строками (иначе postgresDatabaseSection показала бы неинформативную дыру):
//   - QuickCheck — "ok" при успешном round-trip (SELECT 1); postgresDatabaseSection
//     сравнивает именно с "ok", как и для SQLite.
//   - JournalMode — версия сервера и его роль (primary/standby, pg_is_in_recovery) —
//     ближайший по духу к SQLite journal_mode содержательный режим сервера. Ни один
//     текущий вызывающий код (см. grep JournalMode) это поле для postgres не
//     показывает, но комментарий у HealthInfo обещает "текущий режим журнала" — здесь
//     это её PostgreSQL-аналог, а не пустая строка.
func (s *pgStore) Health(ctx context.Context) (HealthInfo, error) {
	var probe int
	if err := s.db.QueryRowContext(ctx, `SELECT 1`).Scan(&probe); err != nil {
		return HealthInfo{}, fmt.Errorf("проверка соединения: %w", err)
	}

	var version string
	if err := s.db.QueryRowContext(ctx, `SHOW server_version`).Scan(&version); err != nil {
		return HealthInfo{}, fmt.Errorf("чтение версии сервера: %w", err)
	}

	var inRecovery bool
	if err := s.db.QueryRowContext(ctx, `SELECT pg_is_in_recovery()`).Scan(&inRecovery); err != nil {
		return HealthInfo{}, fmt.Errorf("чтение режима сервера: %w", err)
	}
	role := "primary"
	if inRecovery {
		role = "standby"
	}

	return HealthInfo{
		QuickCheck:  "ok",
		JournalMode: fmt.Sprintf("postgresql %s (%s)", version, role),
	}, nil
}
