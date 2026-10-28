# Agent Memo — Архитектура

Система долговременной памяти для ИИ-агентов на Go. Агент во время выполнения задач
записывает важную информацию, а в других сессиях запрашивает её обратно — получая
**релевантную, структурированную, сжатую под бюджет токенов** выжимку, а не свалку.

---

## 1. Цели и не-цели

**Цели**
- Агент сохраняет атомарные факты/события/процедуры во время работы.
- В любой другой сессии агент запрашивает знание по смыслу и получает компактный бриф.
- Выдача **не перегружает контекст**: сгруппирована, отранжирована, обрезана под бюджет.
- Память **не деградирует** со временем: дедупликация, устаревание, консолидация.
- Ноль тяжёлых внешних зависимостей на старте (embedded SQLite, локальные модели) —
  верно для дефолтного локального режима одиночного разработчика. Командный сценарий
  (общая память нескольких разработчиков, §2.1) — осознанное исключение: внешний сервер
  БД (PostgreSQL) включается явным выбором в конфиге (`[store] driver`, §13), не по
  умолчанию. Совмещённый режим (локальная SQLite + синхронизация выбранных проектов с
  общей PostgreSQL, §2.1, §7.1) сервер на горячий путь не ставит: память работает
  локально, недоступность общей базы — ошибка в статусе синхронизации, а не отказ.

**Не-цели**
- Не RAG над большими документами (единица памяти атомарна, чанкинг не нужен).
- Не хранилище секретов (ключи/пароли в память не пишем).
- Не замена векторной СУБД промышленного масштаба (мультитенант — на будущее).

---

## 2. Ключевые концепции — типы памяти

Разделение типов превращает свалку в систему. Заимствовано из когнитивной архитектуры
и из проверенной практики (`type` во frontmatter пользовательской памяти).

| Тип (`kind`) | Что хранит | Пример | Время жизни |
|--------------|-----------|--------|-------------|
| `semantic`   | факты, знания, предпочтения | «Проект на Go 1.23, SQLite через mattn/go-sqlite3» | долго |
| `episodic`   | события сессий | «В сессии X чинили race в кэше — решили мьютексом» | средне, устаревает |
| `procedural` | как делать (рецепты, воркфлоу) | «Деплой: `make test` → `make build` → …» | долго |
| `debt`       | известная проблема кода/архитектуры, отложенная осознанно | «`SQLiteMemoryCount` сканирует таблицу целиком вместо `EXISTS`, оставлено до задачи 12.3 — приоритет низкий» | до явного закрытия, не затухает |
| `task`       | единица работы агента вместо файла TODO: полный документ задачи + состояние (`status`) | «Задачи в memo вместо файлов» (заголовок в `summary`, текст задачи в `content`, `status = doing`) | до явного закрытия, не затухает |

`scope` (namespace) разделяет `user:*`, `project:<name>`, `agent:self` даже для одного
агента. Recall всегда фильтруется по scope.

### 2.1 Мультипроектность (модель scope)

Один агент (или несколько разработчиков одной команды) работает с несколькими проектами
через **единое хранилище**; разделение по проектам — через scope, физического дробления
нет. Модель: **изоляция проектов + общий пользовательский слой** — одна и та же поверх
любого из трёх режимов хранилища (первые два выбираются конфигом `[store] driver`, §13,
третий — настройкой синхронизации в локальной БД):

- **Локальный (SQLite, дефолт).** Хранилище — один файл `~/.agentmemory/memory.db` на
  машине разработчика. Сценарий — один пользователь, несколько *инстансов* `memo`
  (несколько проектов/агентских сессий у него).
- **Общий (PostgreSQL).** Хранилище — сервер PostgreSQL, к которому подключаются
  инстансы `memo` нескольких разработчиков команды. Сценарий — общая память команды:
  факты и процедуры, сохранённые одним разработчиком, доступны остальным через тот же
  `project:<name>`.
- **Локальный + синхронизация (Этап 19, §7.1).** Каждый разработчик работает на своей
  SQLite (режим «Локальный»), а данные **выбранных проектов** синхронизируются в обе
  стороны с общей PostgreSQL-базой команды — своя база на каждый проект. Сценарий — два
  разработчика ведут свои задачи: общая база хранит данные команды, у каждого локально —
  всё, и работа не зависит от доступности сервера. Синхронизируется **набор
  синхронизации** проекта X: записи scope `project:X` видов `semantic`, `procedural`,
  `debt`, `task` — живые, тумстоуны и `done`-задачи, вместе с тегами и связями внутри
  набора. **Не синхронизируются:** `episodic` (эпизоды консолидируются на каждой машине
  своим `Consolidator`, §11, — общие эпизоды дали бы дубли фактов), `user:*` и прочие
  scope, векторы (каждый досчитывает у себя, §9), `accessed_at`/`access_count`
  (локальные сигналы ранжирования). Конфликт — last-write-wins по `updated_at`, запись
  целиком. При `driver = "postgres"` хранилище уже общее, и синхронизация недоступна.

Модель scope одинакова во всех режимах:

- **Идентичность инстанса.** Каждый проект запускает свой `memo` (через `.mcp.json`
  в репозитории) с настроенным `[scope] project = "<name>"`. Отсюда выводится
  `project:<name>` — «домашнее» пространство инстанса.
- **Save.** Если инструмент не передал scope, он подставляется как `project:<name>`
  инстанса. Агент может явно записать в `user:*` (общая привычка/предпочтение) или иной scope.
- **Recall (набор scope).** По умолчанию ищет в **наборе**
  `["project:<name>", "user:*"]` — свой проект плюс общий слой; чужие `project:*`
  невидимы. Фильтр — `scope IN (...)`, не одиночное равенство. Агент может переопределить
  набор явно (в т.ч. заглянуть в другой проект, если осознанно нужно).
- **Разрешение приоритета:** явные scope из вызова > дефолт инстанса из конфига.
  Если `project` в конфиге не задан — мультипроектность выключена: save требует явный
  scope, recall без scope не фильтрует (одно-проектный/adhoc-режим).
- **Конкурентность** различается по режиму хранилища:
  - SQLite: несколько инстансов пишут в один файл на одной машине — БД открыта в
    `WAL` + `busy_timeout`; читатели параллельны, писатель сериализуется приложением.
  - PostgreSQL: несколько инстансов на разных машинах пишут в один сервер одновременно —
    конкурентность и сериализация записи штатные, средствами транзакций/MVCC сервера,
    не приложения.
  - Локальный + синхронизация: локально — как SQLite; на общей базе параллельные
    прогоны разных машин разводятся транзакциями сервера и сравнением версии при
    записи (§7.1), прогоны нескольких процессов одной машины — lease в локальной БД (§11).

На общей PostgreSQL-базе (`driver = "postgres"`) `user:*` физически общий для всех
разработчиков команды — известное ограничение, зафиксировано как следующий шаг в §14.
Режим синхронизации его не наследует: `user:*` в общую базу не уходит и остаётся личным
на машине разработчика.

---

## 3. Архитектура — слои

```
┌─────────────────────────────────────────────┐
│  Agent (другая сессия / другой агент)         │
└───────────────┬─────────────────────────────┘
                │  MCP tools (stdio)
        ┌───────▼────────┐
        │   Memory API    │  save / recall / forget / update
        └───────┬─────────┘
     ┌──────────┼──────────────┐
     ▼          ▼              ▼
┌─────────┐ ┌─────────┐  ┌──────────┐
│ Writer  │ │ Recall  │  │  Maint   │
│(ingest) │ │(query)  │  │ (jobs)   │  decay / consolidation / reindex
└────┬────┘ └────┬────┘  └────┬─────┘
     │           │            │
     ▼           ▼            ▼
┌──────────────────────────────────────┐
│  Store: metadata + lexical + vector    │
│  (SQLite, дефолт | PostgreSQL, команда)│
└──────────────────────────────────────┘
        ▲                     ▲
        │ Embedder            │ Reasoner
   (ollama/bge-m3, opt.)  (DeepSeek | CLIProxyAPI)
```

- **Writer** — приём и структурирование записи: дедуп, классификация, связывание.
- **Recall** — гибридный поиск (FTS5 + vec) → ранжирование → сборка Markdown-брифа.
- **Maint** — фоновые джобы: устаревание, консолидация, переиндексация.
- **Store** — интерфейс (§6) с двумя реализациями, выбираемыми конфигом (`[store] driver`,
  §13): SQLite (дефолт, один файл: метаданные + лексический индекс FTS5 + вектор
  `sqlite-vec`) и PostgreSQL (общая база команды: метаданные + `tsvector`/GIN + вектор
  pgvector/HNSW). Подробности различий — §4.
- **Embedder** — опциональный семантический вектор (локальный ollama). Деградирует до FTS5.
- **Reasoner** — OpenAI-совместимый chat (DeepSeek или локальный CLIProxyAPI) для
  суммаризации выдачи и консолидации (LLM-операции).
- **Sync** (Этап 19, §7.1) — фоновая двусторонняя синхронизация наборов проектов
  локальной SQLite с общими PostgreSQL-базами; работает рядом с Writer/Maint поверх своих
  портов, а не через `Store`.

---

## 4. Модель данных

Логическая модель — таблицы, поля и их семантика (`superseded_by`, `expires_at`, граф
`links`, теги) — одна и та же на обеих реализациях `Store` (SQLite и PostgreSQL, §13
`[store] driver`). Ниже — референсная схема на SQLite; различия PostgreSQL сведены в
таблицу после раздела «Ключевые решения».

```sql
-- Атомарная единица памяти
CREATE TABLE memories (
    id            TEXT PRIMARY KEY,          -- ULID (сортируемый по времени)
    kind          TEXT NOT NULL,             -- semantic | episodic | procedural | debt | task
    scope         TEXT NOT NULL,             -- user:* | project:<name> | agent:self
    content       TEXT NOT NULL,             -- сам факт (1-3 предложения, атомарно); для task — весь текст задачи
    summary       TEXT,                      -- ультра-короткая форма для брифа (для task — заголовок, обязателен)
    importance    INTEGER NOT NULL DEFAULT 3,-- 1..5, задаёт агент
    created_at    INTEGER NOT NULL,          -- unix ts
    updated_at    INTEGER NOT NULL,
    accessed_at   INTEGER,                   -- для decay/усиления
    access_count  INTEGER NOT NULL DEFAULT 0,
    superseded_by TEXT,                      -- id заменившей записи (мягкое забывание)
    source        TEXT,                      -- session id / откуда пришло
    expires_at    INTEGER,                   -- явный TTL: unix ts истечения (NULL = вечная, задача 9.4)
    status        TEXT,                      -- стадия задачи: backlog|todo|doing|review|testing|release|done (NULL для kind != task; задачи 14.1/15.1)
    task_type     TEXT,                      -- тип задачи: epic|story|task (NULL для kind != task, Этап 15)
    branch        TEXT,                      -- git-ветка, в которой ведётся задача (NULL для kind != task, Этап 15)
    workflow      TEXT,                      -- имя процесса, которым задача выполняется: dev|ultracode|... (NULL для kind != task, Этап 15)
    author        TEXT,                      -- кто создал запись; не меняется после создания (NULL — неизвестен, Этап 19)
    updated_by    TEXT                       -- кто изменил запись последним (NULL — неизвестен, Этап 19)
);

CREATE TABLE tags (
    memory_id TEXT NOT NULL REFERENCES memories(id),
    tag       TEXT NOT NULL,
    PRIMARY KEY (memory_id, tag)
);

-- Граф связей ([[links]])
CREATE TABLE links (
    src TEXT NOT NULL REFERENCES memories(id),
    dst TEXT NOT NULL REFERENCES memories(id),
    rel TEXT NOT NULL DEFAULT 'related',     -- related | supersedes | causes | part_of | blocked_by
    PRIMARY KEY (src, dst, rel)
);

-- Лексический индекс (стартовый поиск, всегда доступен)
CREATE VIRTUAL TABLE memories_fts USING fts5(
    content, summary, tags,
    content='memories', content_rowid='rowid'
);

-- Векторный индекс (создаётся когда включён Embedder; dim берётся из модели)
-- CREATE VIRTUAL TABLE memories_vec USING vec0(
--     memory_id TEXT PRIMARY KEY,
--     embedding float[<DIM>] distance_metric=cosine
-- );

-- Метка модели эмбеддингов — источник истины для reindex
CREATE TABLE embed_meta (
    id    INTEGER PRIMARY KEY CHECK (id = 1),
    model TEXT NOT NULL,
    dim   INTEGER NOT NULL
);

-- Отметка последнего прогона планировщика maint (Этап 16): читает `memo web`
CREATE TABLE maint_meta (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    last_run_at INTEGER NOT NULL
);

-- Синхронизация с общей базой (Этап 19, §7.1). Только SQLite: при driver = "postgres"
-- хранилище уже общее, таблицы не создаются. Пароль сюда не пишется никогда (§13).
CREATE TABLE sync_profile (                     -- одна строка: настройки машины
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    author_name TEXT,                           -- имя разработчика; NULL — дефолт git config --global user.name
    interval_s  INTEGER NOT NULL DEFAULT 300    -- период фоновой синхронизации, сек
);
CREATE TABLE sync_targets (                     -- цель = проект + его общая база
    project      TEXT PRIMARY KEY,              -- синхронизируется scope project:<project>
    host         TEXT NOT NULL,
    port         INTEGER NOT NULL DEFAULT 5432,
    user         TEXT NOT NULL,
    dbname       TEXT NOT NULL,
    sslmode      TEXT NOT NULL DEFAULT 'prefer',
    password_env TEXT NOT NULL UNIQUE,          -- имя переменной пароля в <каталог БД>/.env (§13), не сам пароль
    push         INTEGER NOT NULL DEFAULT 0,    -- тумблер «Отправлять»
    pull         INTEGER NOT NULL DEFAULT 0     -- тумблер «Загружать»
);
CREATE TABLE sync_state (                       -- итог прогонов по цели; пишет только Syncer
    project         TEXT PRIMARY KEY,
    base_id         TEXT,                       -- sync_meta.base_id общей базы, к которой относится курсор
    pull_xid        TEXT,                       -- курсор pull (§4.1): xid8 десятичной строкой
    pull_id         TEXT,                       -- … и id внутри этого xid ('' — с начала xid)
    push_marked     INTEGER NOT NULL DEFAULT 0, -- набор размечен для текущего включения «Отправлять»
    last_run_at     INTEGER,
    last_ok_at      INTEGER,
    last_error      TEXT,                       -- NULL — последний прогон успешен
    last_pushed     INTEGER NOT NULL DEFAULT 0,
    last_pulled     INTEGER NOT NULL DEFAULT 0,
    last_conflicts  INTEGER NOT NULL DEFAULT 0,
    conflicts_total INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE sync_dirty (                       -- неотправленные изменения; заполняют триггеры
    seq       INTEGER PRIMARY KEY AUTOINCREMENT, -- номер пометки: INSERT OR REPLACE даёт новый, не переиспользуется
    memory_id TEXT NOT NULL UNIQUE,
    edited    INTEGER NOT NULL                  -- 1 — правка (триггер), 0 — первичная разметка (Mark)
);
CREATE TABLE sync_lease (                       -- кто из процессов memo этой машины ведёт прогон цели
    project    TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,                   -- id процесса (ULID при старте)
    expires_at INTEGER NOT NULL                 -- истёкший lease перехватывается
);
```

Ключевые решения:
- **ULID** как id — сортируемость по времени + отсутствие координации.
- **`superseded_by`** вместо DELETE — «мягкое забывание»: старый факт вне recall, история цела.
- **`kind=debt` без отдельного поля статуса.** Открыт/закрыт долга — не колонка
  `status`, а тот же `superseded_by`, что и у любой другой памяти: живая запись —
  открытый долг, тумстоун (`Writer.Forget`/`Store.Supersede` с `replaced_by` на
  запись о фиксе, либо без него, если долг снят без фикса) — закрытый. Так не
  нужна миграция схемы (`kind` — обычная TEXT-строка без CHECK), история решения
  остаётся в графе тумстоунов, и работает уже существующий механизм забывания —
  не отдельный.
- **`kind=task` — единственный тип со `status`.** У задачи, в отличие от долга,
  есть промежуточное состояние: «взята в работу» и «ещё не начата» — разные вещи
  для агента, восстанавливающего контекст, поэтому здесь колонка `status`
  (миграция схемы `2 → 3`) оправдана, а у остальных `kind` она NULL. С Этапа 15
  `status` — это **стадия kanban-доски** (`backlog|todo|doing|review|testing|
  release|done`, миграция `3 → 4`), а «заблокирована» — не стадия, а вычисляемое
  свойство из связей `blocked_by` (см. §14 Этап 15). Там же появляются три
  task-only колонки `task_type`/`branch`/`workflow` — по тому же принципу, что
  и `status`: nullable у остальных `kind`, валидация применимости в write path. Отмена задачи — по-прежнему тумстоун (`Writer.Forget`), а
  `done` — живая запись: историю сделанного не стирают, из дефолтной выдачи
  recall она уходит фильтром (§8). `content` задачи — полный документ (что
  сделать, границы, критерий приёмки), `summary` — заголовок, и он обязателен:
  в бриф выводится именно он (§8), а полный текст агент забирает `memory_get`
  (§12).
- **`importance` + `accessed_at` + `access_count`** — три сигнала для ранжирования.
- **vec-таблица отдельная** — вектор добавляется/переиндексируется без миграции `memories`.
- **`expires_at`** — явный TTL (задача 9.4), дополняет эвристический decay (§11): у части
  фактов срок годности известен заранее и точно (токен деплоя, фичефлаг до релиза, встреча
  на конкретный день). NULL/0 = вечная запись. Двойная защита: `expires_at IS NULL OR
  expires_at > now` в общем фильтре recall (§8) скрывает просроченную запись немедленно —
  между прогонами планировщика; фоновый `ExpireSweep` (§11) следом превращает её в
  тумстоун (`Supersede(id, id)`) — та же гигиена, что у decay/`memory_forget`.
- **`author` / `updated_by`** (Этап 19) — авторство для команды: создатель записи и
  последний редактор, в обоих бэкендах. Ставит write path из имени разработчика, которое
  composition root передаёт `write.Writer` опцией (имя — `sync_profile.author_name`, иначе
  `git config --global user.name`, иначе пусто — не ошибка; при `driver = "postgres"` —
  сразу git). `author` пишется при создании и больше не меняется; `updated_by` — при
  каждой правке и при `Supersede` (тумстоун — тоже правка, поэтому порт `Supersede`
  получает имя редактора; maint-джобы собираются с тем же именем, что `Writer`).
  Имя читается при старте процесса: смена имени в морде доходит до уже запущенных
  MCP-серверов после их перезапуска. Синхронизация переносит оба поля как есть (§7.1),
  а записям без автора проставляет имя отправителя при их первой вставке в общую базу.
  `Touch` их не трогает: чтение — не правка.
- **`updated_at` строго растёт на правке** (Этап 19): write path и `Supersede` ставят
  `updated_at = max(now, прежний updated_at + 1)`. Время хранится в секундах, и без
  этого правило LWW (§7.1) не отличило бы вторую правку в ту же секунду от первой —
  ничья отдала бы победу уже отправленной версии и тихо затёрла бы собственную, более
  позднюю правку. Заодно правка после просмотра версии с машины, чьи часы впереди,
  всё равно получается новее её.

**SQLite vs PostgreSQL — различия реализации**

| Аспект | SQLite (дефолт, локальный) | PostgreSQL (общая база команды) |
|---|---|---|
| Лексический индекс | `FTS5` virtual table (`memories_fts`); русский стемминг — колонка `content_stemmed`, считается в Go (`normalizeForFTS`, задача 8.3) | нативный `tsvector`-столбец с конфигурацией `russian` (встроенный snowball-стемминг) + GIN-индекс — функциональный аналог `content_stemmed`, стемминг делает сервер |
| Векторный индекс | `sqlite-vec` (`memories_vec`, virtual table `vec0`) | pgvector: таблица `memories_vec` с колонкой `vector(N)`, HNSW-индекс (`vector_cosine_ops`) |
| Фильтр + KNN | «k-before-filter»: фильтры (scope/kind/tags) накладываются поверх top-k из KNN, отсюда запасы `semanticNeighborsPad`/`vectorTagFilterPad` в `store/sqlite.go` | pgvector умеет комбинировать HNSW-KNN с внешними фильтрами напрямую — запасы под k-before-filter не нужны |
| Версия схемы | `PRAGMA user_version` | служебная таблица версии схемы |
| Миграции | пошаговые (`migrations[i]`: версия `i → i+1`, каждый шаг в своей транзакции) | тот же механизм пошаговых миграций, отдельная реализация шагов под PostgreSQL |
| Пароль/креды | не требуются (файл на диске) | только из env `MEMO_POSTGRES_PASSWORD` (§13), никогда в TOML |
| Стоп-слова | нет: `normalizeForFTS` стеммирует все слова подряд, включая предлоги и союзы | есть: конфигурация `russian` отбрасывает стоп-слова на этапе `to_tsvector`/`plainto_tsquery` |
| Синхронизация (Этап 19) | локальная сторона: таблицы `sync_*` и триггеры пометки неотправленного (`sync_dirty`) | общая сторона: колонка `memories.change_xid xid8` (серверный номер изменения) + триггеры + `sync_meta`; таблиц `sync_*` нет |
| Открытие | `Open` всегда приводит схему к целевой | `Open` (`driver = "postgres"`) — так же; общая база синхронизации открывается **без** автомиграции (ниже) |

Обе реализации живут строго за интерфейсом `Store` (§6) — `Writer`/`Recall`/`Maint` не
знают, какой бэкенд активен.

**Наблюдаемое следствие разных стеммеров.** Строка «Стоп-слова» выше — единственное
различие таблицы, заметное пользователю в выдаче: запрос, значимая часть которого
состоит из служебных слов, на SQLite найдёт записи по совпадению предлога (`«на»`
матчит любую запись, где есть `«на»`), а на PostgreSQL — не найдёт ничего, потому что
стоп-слова из запроса выброшены. На запросах из знаменательных слов выдача обоих
бэкендов совпадает. Это не дефект паритета, а следствие того, что морфологию на
PostgreSQL делает сервер: поведение с отбрасыванием стоп-слов точнее, и специально
подгонять его под SQLite не нужно.

### 4.1 Синхронизация: данные обеих сторон (Этап 19)

**Общая база = схема postgres-бэкенда + серверный номер изменения.** Отдельной «схемы
синхронизации» нет: общую базу можно открыть `memo web` или использовать как
`driver = "postgres"` напрямую, и прямые писатели обязаны продвигать номер изменения
так же, как синхронизация, — поэтому номер ставят **триггеры сервера**, а не код `sync`.

```sql
-- PostgreSQL, шаг migrateChangeXIDPG (номер — по порядку вливания, см. ниже), DDL + data-шаги
ALTER TABLE memories ADD COLUMN change_xid xid8 NOT NULL DEFAULT pg_current_xact_id();
CREATE INDEX memories_change_idx ON memories (scope, change_xid);
CREATE TABLE sync_meta (id smallint PRIMARY KEY CHECK (id = 1), base_id text NOT NULL);
INSERT INTO sync_meta VALUES (1, gen_random_uuid()::text);   -- идентичность базы для курсора
-- memories_change_ins: BEFORE INSERT ON memories → NEW.change_xid := pg_current_xact_id()
-- memories_change_upd: BEFORE UPDATE ON memories
--   WHEN ((to_jsonb(OLD) - '{accessed_at,access_count,search_vec,change_xid}'::text[])
--         IS DISTINCT FROM (to_jsonb(NEW) - <тот же список>))  → NEW.change_xid := pg_current_xact_id()
--   (список ИСКЛЮЧЕНИЙ, а не перечень синхронизируемых колонок: новая колонка —
--    author/updated_by из шага 19.1 или будущая — синхронизируется без правки триггера)
-- tags_change / links_change: AFTER INSERT OR UPDATE OR DELETE ON tags | links →
--   UPDATE memories SET change_xid = pg_current_xact_id()
--   WHERE id = <memory_id | src> AND change_xid <> pg_current_xact_id()
```

- **Номер изменения — `xid8` транзакции**, последней изменившей синхронизируемые поля,
  теги или связи записи. Не `bigserial` и не время: см. «Почему pull не пропускает
  строки» ниже. `xid8` — 64-битный xid с эпохой, не заворачивается за время жизни
  кластера и упорядочиваем (btree), поэтому годится и для индекса, и для курсора.
- **`Touch` номер не двигает**: `accessed_at`/`access_count` (и производный `search_vec`)
  исключены из сравнения `WHEN` — чтение на одной машине не гонит запись на все остальные.
  Сравнение — «всё, кроме локальных колонок»: забытая в перечне синхронизируемая колонка
  молча терялась бы при синхронизации, а забытая в исключениях локальная стоит лишь
  идемпотентного перечитывания.
- **Изменение тегов/связей двигает номер записи-владельца** (`tags.memory_id`,
  `links.src`); условие `change_xid <> pg_current_xact_id()` гасит повторные `UPDATE`
  внутри одного `Put` (тот удаляет и вставляет теги и связи пачкой).
- **`sync_meta.base_id`** — случайный идентификатор базы. Курсор клиента действителен
  только вместе с ним: другая база (сменили параметры цели, пересоздали базу) — другой
  `base_id`, и курсор сбрасывается (§7.1).
- Триггеры синхронизации — **версионный шаг миграции**, а не часть
  `searchVecTriggerStatements` с чек-суммой: старый бинарь с `driver = "postgres"`,
  переприменяя свои триггеры `search_vec`, не трогает чужие.

**Почему pull не пропускает строки при параллельных коммитах.** Наивный курсор по
возрастающему номеру (`bigserial`, `nextval` в триггере) теряет строки: номер выдаётся
при записи, а видимой строка становится при коммите. T1 получает 100, T2 — 101, T2
коммитится первой; читатель видит 101, сдвигает курсор на 101, потом коммитится T1 со
100 — и она меньше курсора навсегда. Время (`updated_at`, commit timestamp) страдает тем
же и вдобавок зависит от часов. Решение — курсор не «последний прочитанный номер», а
**горизонт снимка**:

1. Pull читает в одной транзакции `REPEATABLE READ READ ONLY` (один снимок на все
   страницы, теги и связи; xid читателю не выдаётся). Первым запросом берётся
   `pg_snapshot_xmin(pg_current_snapshot())` — самый ранний xid, ещё активный в момент
   снимка. По определению PostgreSQL все транзакции с меньшим xid к этому моменту либо
   закоммичены и **видны в снимке**, либо откачены.
2. Выбираются строки набора синхронизации с `change_xid >= курсор`, keyset-страницами по
   `(change_xid, id)`.
3. Новый курсор — этот `xmin`. Любая строка с `change_xid < xmin` закоммичена до снимка,
   поэтому прочитана сейчас или раньше. Любая транзакция, ещё не завершённая к снимку,
   имеет xid `>= xmin` (активные — по определению `xmin`, будущие получат xid из счётчика
   не меньше `xmax` снимка), значит, её строки попадут в следующий pull. Пропуск
   невозможен (пока база не восстановлена из копии — п. 5); цена — повторное чтение строк из окна `[xmin, xmax)`, уже видимых сейчас.
   Повтор безвреден: применение идемпотентно (равная версия пропускается, §7.1).
4. Курсор между страницами — пара `(xid, id)`, чтобы сбой или таймаут посреди прогона не
   отбрасывал прогресс. После страницы, последняя строка которой — `(L, I)`: если `L <
   xmin`, сохраняется `(L, I)` — транзакция `L` завершилась до снимка, все её строки уже
   видны и упорядочены по `id`, а все строки с меньшим xid применены; иначе сохраняется
   `(xmin, '')` — строки из окна перечитаются. После последней страницы — `(xmin, '')`.
   Следующий pull читает `(change_xid, id) > (xid, id)` (при `id = ''` — `change_xid >=
   xid`). Так даже группа из тысяч строк одного xid (backfill миграции `2 → 3`, большой
   прямой писатель) проходится постранично с продвижением. Курсор не убывает (`xmin`
   снимков монотонен; на всякий случай берётся максимум со старым).
5. **Идентичность базы — `sync_meta.base_id`.** Смена `base_id` (другая база, схема
   применена заново на пустую) — курсор сбрасывается в начало. **Восстановление общей
   базы** (дамп в новый кластер, PITR) переносит и `base_id`, и `change_xid`, а xid'ы
   после него переиспользуются — эвристикой это надёжно не ловится. Поэтому
   восстановление требует **явной ротации `base_id`** — операции рядом с применением
   схемы (`memo sync -rotate-base -project X`, кнопка в разделе): у всех клиентов
   курсор сбросится и набор сверится целиком. Признаки, которые всё же видны — курсор
   клиента больше `xmax` снимка или `max(change_xid)` больше `xmax`, — останавливают
   прогон с ошибкой «общая база восстановлена — выполните ротацию», а не сбрасывают
   курсор молча.

Долгая транзакция на сервере (в том числе чужая, `idle in transaction`) держит `xmin` и
заставляет перечитывать окно, но не теряет строк — это цена эффективности, а не
корректности. Отвергнутые варианты: `bigserial` + курсор (теряет строки, выше);
`track_commit_timestamp` (настройка сервера и та же гонка видимости на границе); логическая
репликация (права `REPLICATION`, слоты на сервере, тяжело для команды); `LISTEN/NOTIFY`
(не долговечно); сериализация всех писателей одной блокировкой (бьёт прямых писателей).

**Локальная сторона (SQLite)** — таблицы `sync_*` в §4 выше:

- **Неотправленное — `sync_dirty`**, его заполняют триггеры на `memories` (`AFTER
  INSERT`, `AFTER UPDATE OF` синхронизируемых колонок — тот же список, что `WHEN` выше),
  `tags` и `links` (`AFTER INSERT/DELETE`) — для записей **scope какой-либо цели**
  (`scope = 'project:' || sync_targets.project`). Виды триггер не фильтрует: состав
  набора — правило интерактора (§6), и его дубль в SQL молча разошёлся бы с ним;
  `Dirty(scope, kinds)` отдаёт пометки набора и снимает пометки записей scope вне
  `kinds`. Так ловится любое изменение любым путём (MCP, морда, maint, import), а не
  только то, о котором вспомнил write path. Пометка — `INSERT OR REPLACE` с новым
  `seq` из `AUTOINCREMENT` (номера не переиспользуются даже после удаления строк):
  повторная правка записи, пока она отправляется, получает новый `seq`, и снятие пометки
  после отправки (`DELETE … WHERE memory_id = ? AND seq = ?`) её не потеряет.
- **Применение полученного не помечает запись**: адаптер в той же транзакции снимает
  пометку, иначе полученное ушло бы обратно (пинг-понг).
- **Первичная разметка** (`Mark`, `edited = 0`). Новая цель, включение «Отправлять» и
  смена общей базы (`base_id`) размечают **весь** набор проекта: записи, уже лежащие в
  общей базе, при отправке окажутся равными серверным и пропустятся — лишний проход
  дешевле отдельного учёта «что уже было в какой базе». Разметку делает сам `Syncer`
  в начале прогона (нет `sync_state`, `push_marked = 0` при включённом push или сменился
  `base_id`), а не морда: так же сработает цель, заведённая из CLI. Пометка разметки
  конфликтом не считается — конфликт только у `edited = 1`.
- Таблицы — в базовой схеме (`CREATE TABLE IF NOT EXISTS`), как `maint_meta`: data-шагов
  у них нет. Триггеры пересоздаются на каждом Open **после** `migrate` (как `ftsTriggers`):
  их тело ссылается на `author`/`updated_by` из шага `4 → 5` и должно меняться вместе
  со списком синхронизируемых колонок.
- **Версия локальной БД новее бинаря** (`user_version` больше `schemaVersion` — БД уже
  обновил более новый `memo`, а старая сессия ещё жива): такой процесс синхронизацию не
  запускает — старая логика не должна писать в новую схему и перетирать `sync_state`.

**Версии схем после Этапа 19.** SQLite `4 → 5` (`migrations[4]`): `author`,
`updated_by` (два `ADD COLUMN`, без data-шагов; у SQLite нет `ADD COLUMN IF NOT EXISTS`,
поэтому версионно). PostgreSQL — два шага: `author`, `updated_by` (19.1) и
`migrateChangeXIDPG` (19.2): `change_xid` с индексом, триггеры номера изменения,
`sync_meta`. Шаги независимы (триггер сравнивает строку через список исключений), поэтому
их порядок не важен. Синхронизация требует, чтобы версия общей базы **равнялась**
`pgSchemaVersion` бинаря: меньше — «примените схему», больше — «обновите memo», прогон
не идёт (§7.1). Номера шагов фиксирует порядок вливания задач 19.1 и 19.2: какая
вливается второй, та берёт следующий номер.

**Открытие общей базы без автомиграции.** `store.Open` с `driver = "postgres"` по-прежнему
приводит схему к целевой при каждом открытии — это режим «своё хранилище». Общая база
синхронизации открывается отдельной функцией, которая DDL не выполняет вовсе и только
сообщает состояние: пусто (нет `schema_version`) / версия N меньше, равна или больше
`pgSchemaVersion`. Создание и обновление схемы — отдельная явная операция (кнопка с
подтверждением в морде, `memo sync -apply-schema`), она выполняет тот же `init`, что
`store.Open`, под тем же advisory lock, и отказывает на схеме новее бинаря. В `store` это
`OpenHub` (только подключение) → `Hub.Schema` (состояние) / `Hub.ApplySchema`; сам `Hub` —
адаптер порта `Shared` (19.5): `Base`, `Pull` (страницы изменений набора по курсору `(xid, id)`
с `Refs` в одном снимке), `Versions`, `Push`. Расширение `vector` для общей базы не требуется:
векторы не синхронизируются, а `memories_vec` создаётся только при `ResetVectorIndex`.

---

## 5. Компоненты и пакеты

```
/cmd/memo               — точка входа: MCP-сервер (stdio, без подкоманды) +
                          подкоманды на cobra (`init`/`web`/`recall`/`save`/`hook`/
                          `doctor`/`export`/`import`, задача 11.1) — cobra только
                          маршрутизирует по имени, разбор флагов каждой подкоманды
                          остаётся на её собственном stdlib flag.FlagSet (контракты
                          exit-кода/stdout-stderr не через это меняются). `init` —
                          интерактивный установщик памяти в проект под конкретное
                          агентское приложение (MCP-конфиг, два скила — «Протокол
                          работы с памятью» и «Первичная инициализация памяти», .env,
                          agentmemory.toml) с выбором провайдеров embedder/reasoner;
                          интерактив (TTY) — TUI на github.com/charmbracelet/huh
                          (select стрелками, скрытый ввод ключей), неинтерактив
                          (`-no-input`/пайп) — только флаги/дефолты, без сети.
                          cobra/huh — зависимости CLI-инсталлятора, не ядра памяти
                          (осознанный трейдофф к §1 "ноль тяжёлых зависимостей на
                          старте": ядро Store/embed/reason/write/recall/maint новых
                          зависимостей не получает).
/internal/memory
    types.go            — Memory, Query, Brief, Candidate
    store/              — интерфейс Store + реализации: SQLite (FTS5 + sqlite-vec, дефолт)
                          и PostgreSQL (tsvector/GIN + pgvector/HNSW, общая база команды)
    embed/              — интерфейс Embedder (noop | ollama | openai)
    reason/             — интерфейс Reasoner + OpenAI-совместимый chat (DeepSeek, CLIProxyAPI)
    write/              — ingest: дедуп, классификация, связывание
    recall/             — гибридный поиск (RRF) + ранжирование + сборка брифа
    maint/              — джобы: decay, consolidation, reindex, dedup-sweep
    board/              — kanban-доска: Build, Describe (свои порты, store не импортирует)
    sync/               — синхронизация с общей базой (Этап 19, §7.1): Syncer (push/pull,
                          LWW), Scheduler; свои порты Local/Shared/Dialer, реализации —
                          в store; store не импортирует (базовая линия нарушений 0).
                          Имя пакета совпадает со stdlib `sync` — импортёры, которым
                          нужны оба, берут псевдоним `memsync`
/internal/mcp           — регистрация tools, JSON-схемы, роутинг
/internal/config        — загрузка конфига (TOML/env)
```

---

## 6. Интерфейсы (Go)

```go
type Store interface {
    Put(ctx context.Context, m Memory) error
    Get(ctx context.Context, id string) (Memory, error)
    GetMany(ctx context.Context, ids []string) ([]Memory, error)                   // по списку id, с Tags/Links батчем; тумстоуны включены, отсутствующие пропущены (Этап 15)
    ListTasks(ctx context.Context, scopes []string) ([]Memory, error)              // все живые задачи scopes с Tags/Links, updated_at DESC — источник доски (Этап 15)
    SearchLexical(ctx context.Context, q Query) ([]Candidate, error)
    SearchVector(ctx context.Context, vec []float32, q Query) ([]Candidate, error) // no-op пока нет Embedder
    Neighbors(ctx context.Context, id string) ([]Memory, error)                    // граф-обход; Tags и Links соседей загружены (Этап 15)
    Supersede(ctx context.Context, oldID, newID, by string) error                  // by — имя редактора в updated_by; пусто — не трогать (Этап 19)
    Touch(ctx context.Context, id string) error                                    // accessed_at++, access_count++
    // maintenance-выборки (§11); Tags/Links не заполняются
    ListEpisodicForConsolidation(ctx context.Context, olderThan int64, limit int) ([]Memory, error)
    ListEpisodicForDecay(ctx context.Context, notAccessedSince int64, limit int) ([]Memory, error)
    // reindex-поддержка
    EmbedMeta(ctx context.Context) (model string, dim int, err error)
    ResetVectorIndex(ctx context.Context, model string, dim int) error
    AllForReindex(ctx context.Context) ([]Memory, error)
    PutVector(ctx context.Context, id string, vec []float32) error
    ListMissingVectors(ctx context.Context, limit int) ([]Memory, error) // очередь досчёта вектора
    // read/ops-выборки (web-морда §6.x, doctor §9.1)
    Stats(ctx context.Context) (Stats, error)                                        // + PendingVectors — очередь на эмбеддинг (Этап 16)
    List(ctx context.Context, f BrowseFilter) (items []Memory, total int, err error) // BrowseFilter.ExpiresBefore — «истекает скоро», expires_at ASC (Этап 16)
    VectorStatus(ctx context.Context, id string) (VectorStatus, error)
    SemanticNeighbors(ctx context.Context, id string, k int) ([]Candidate, error)
    Health(ctx context.Context) (HealthInfo, error) // SQLite: PRAGMA quick_check + journal_mode; PostgreSQL: эквивалентные по смыслу серверные проверки
    SchemaVersion(ctx context.Context) (int, error) // SQLite: PRAGMA user_version; PostgreSQL: schema_version — подпись для морды (Этап 16)
    // отметка прогона планировщика (maint_meta, Этап 16): пишет maint.Scheduler, читает `memo web`
    LastMaintRun(ctx context.Context) (int64, error) // 0 — прогонов не было
    SetLastMaintRun(ctx context.Context, at int64) error
}

type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Model() string
    Dim() int
}

type Reasoner interface {
    Complete(ctx context.Context, prompt string) (string, error) // OpenAI-совместимый chat
}
```

Всё за интерфейсами → бэкенды меняются конфигом; старт возможен с `Embedder=noop`,
`Reasoner=DeepSeek`.

**Порты синхронизации (Этап 19).** Объявляет потребитель — пакет `internal/memory/sync`
(как `board.Source`), импортирует он только `internal/memory` и stdlib;
реализации — в `store` (локальная — SQLite, общая — PostgreSQL), сборка — `cmd/memo`.
В отличие от `board.Source`, порты используют собственные типы пакета (`Cursor`,
`Guarded`, `Page`, `Pending`), поэтому `store` импортирует `sync` — направление
адаптер → вариант использования, правило зависимостей не нарушено, цикла нет (`sync`
не импортирует `store`). В `store` пакет берётся под псевдонимом `memsync` (коллизия со
stdlib `sync`).
`Store` эти методы не получает: синхронизация — отдельный вариант использования со своим
узким контрактом, `Writer`/`Recall` о ней не знают. Сигнатуры ниже — зафиксированы задачей
19.4 (`internal/memory/sync/ports.go`, там же контракт каждого метода). Цели, профиль
(имя, интервал) и lease в `Local` не входят: `Syncer` прогоняет одну цель, которую ему
передали, а читать цели и брать lease — дело планировщика (19.6), его порт объявляется у
него.

```go
package sync

type Cursor struct { XID uint64; ID string } // §4.1; нулевой — с начала

// Guarded — запись к применению с ожидаемой версией на принимающей стороне
// (оптимистичная блокировка): не совпала — запись пропускается и уйдёт в следующий прогон.
type Guarded struct {
    Memory    memory.Memory
    ExpectXID uint64 // общая сторона: change_xid, который видел интерактор; 0 — записи нет
    ExpectAt  int64  // локальная сторона: updated_at, который видел интерактор; 0 — записи нет
    ExpectSeq int64  // локальная сторона: seq пометки sync_dirty; 0 — пометки нет
}

type Remote struct { Memory memory.Memory; XID uint64 }            // версия в общей базе
type Pending struct { Memory memory.Memory; Seq int64; Edited bool } // версия в локальной + пометка

// Local — локальная SQLite инстанса.
type Local interface {
    State(ctx context.Context, project string) (State, error) // нулевое — прогонов не было
    SaveState(ctx context.Context, project string, st State) error
    SchemaAhead(ctx context.Context) (bool, error) // локальная БД новее бинаря — не синхронизировать
    // ids == nil — весь набор; новая пометка — edited = 0, уже стоящая сохраняет edited
    Mark(ctx context.Context, scope string, kinds []memory.Kind, ids []string) error
    // пометки с seq > after по возрастанию seq; after даёт циклу push завершиться,
    // хотя stale-записи остаются помеченными
    Dirty(ctx context.Context, scope string, kinds []memory.Kind, after int64, limit int) ([]Pending, error)
    Versions(ctx context.Context, ids []string) (map[string]Pending, error)     // запись (с Tags/Links) + пометка
    StampAuthor(ctx context.Context, ids []string, name string) error           // пустые author/updated_by → name; без updated_at и без пометки
    Apply(ctx context.Context, recs []Guarded) (applied, stale []string, err error)
    MarkPushed(ctx context.Context, done []Pending) error                       // снять пометку, если seq не изменился
}

// Shared — общая база одного проекта, открытая без автомиграции.
type Shared interface {
    Base(ctx context.Context) (Base, error) // Base{ID, Version, XMax, MaxXID} в одном снимке; Version 0 — схемы нет
    Pull(ctx context.Context, scope string, kinds []memory.Kind, from Cursor, limit int,
        page func(Page) error) error        // один снимок REPEATABLE READ на все страницы
    Versions(ctx context.Context, ids []string) (Snapshot, error) // версии по id + их Refs, один снимок
    Push(ctx context.Context, recs []Guarded) (written, stale []string, err error) // одна пачка — одна транзакция
    Close() error
}

// Page — страница изменений: записи (с Tags/Links) по возрастанию (XID, Id); Refs —
// ВСЕ адресаты их связей, которых нет в странице, любого scope и вида (тот же снимок):
// по ним интерактор отличает рёбра внутри набора от рёбер вне его; Next — безопасный
// курсор после применения страницы (§4.1). Snapshot{Records, Refs} — то же для выборки
// по id (Refs — адресаты, которых нет среди запрошенных id).
type Page struct { Records, Refs []Remote; Next Cursor }

type Dialer interface { Dial(ctx context.Context, project string) (Shared, error) }

type Target struct { Project string; Push, Pull bool }

// Run(ctx, Target, name) (Result, error): name — автор отправляемых записей
// (пусто — авторы не проставляются); ошибка — вторым значением, Result при ошибке
// отражает уже сделанное.
type Result struct {
    Project                          string
    Pushed, Pulled, Conflicts        int
    Stamped, Deferred, DroppedLinks  int // проставлено авторов; отложено из-за гонки CAS; отброшено рёбер
}
```

Решающие правила живут в интеракторе, адаптеры их только исполняют: LWW, состав набора
синхронизации, фильтр и слияние связей, правило версии схемы (`Base().Version ==
want`, `want` — `pgSchemaVersion` бинаря, передаёт composition root). Адаптеры отвечают
за механику: транзакции, CAS-условие записи, двухпроходность FK внутри пачки
(сначала все записи, затем связи), снятие пометки `sync_dirty`, сброс вектора при смене текста. Параметры
подключения и пароль интерактору не нужны — их знает `Dialer`, собранный в `cmd/memo`.

**Адаптеры (задача 19.5).** Порты реализуют сами типы хранилища, без обёрток и без
собственных дублей типов порта: `store.SyncLocal` — `Local` (там же профиль, цели и lease
для планировщика и морды), `store.Hub` — `Shared`. `Apply` и `Push` пишут запись теми же
хелперами строки, тегов и связей, что `Put`, но в два прохода. CAS локальной стороны —
`updated_at` и `seq` пометки (отсутствие записи или пометки — 0), общей — `SELECT … FOR
NO KEY UPDATE` по `change_xid` под явным `READ COMMITTED`; вставка новой — `ON CONFLICT DO
NOTHING`. `StampAuthor` возвращает пометки `sync_dirty` с прежними `seq` и `edited`
(триггер переставил бы их). `Apply` и `StampAuthor` читают, затем пишут, поэтому
начинаются с пустой записи: в WAL транзакция, начатая чтением, получила бы
`SQLITE_BUSY_SNAPSHOT` без ожидания, если между её чтением и записью писал другой
процесс. Сборка прогона — `cmd/memo/syncrun.go`: `runSync` (цель → lease на
таймаут прогона с запасом → `Syncer.Run` с `store.HubSchemaVersion`), `hubDialer` (цель
из `sync_targets`, пароль — `syncPassword` по §13, соединение под коротким таймаутом;
пароль вырезается из текста ошибки, пустой пароль при отказе подключения даёт подсказку,
где его задать), `saveSyncPassword` для морды (19.8).

---

## 7. Write path — как записывать «умно»

Наивное `save(text)` → эмбеддинг → в базу деградирует память. Правильно:

1. **Приём структурированного входа.** Агент передаёт уже атомарный факт с полями
   (`kind`, `content`, `importance`, `tags`, `scope`, опц. `links`). Классификацию делает
   агент — дёшево и надёжно.
2. **Дедупликация / обновление.** Перед записью ищем похожие в том же `scope`
   (лексически + SimHash по content). Если дубль — усиливаем/обновляем; если факт
   противоречит существующему — ставим `superseded_by`; иначе вставляем.
   **Исключение — `kind=task`** (`write.SkipDedup`, то же правило действует и в
   ретроактивном dedup-sweep, §11): задачи не сравниваются на дубли вовсе. Их
   `content` — документ, и две разные задачи по одной подсистеме текстуально
   похожи сильнее порога, откалиброванного на коротких фактах; цена ошибки
   несимметрична — лишний факт это шум, а слитая задача это потерянная работа.
3. **Связывание.** Проставляем `links` (граф), в т.ч. висячие ссылки на ещё не созданные id.
4. **Индексация.** FTS5 всегда; вектор — если `Embedder != noop` (иначе откладывается в
   очередь на досчёт фоновым джобом).

### 7.1 Sync path — синхронизация с общей базой (Этап 19)

Прогон `Syncer.Run(ctx, target, name)` синхронизирует набор одного проекта (§2.1) с его общей
базой. Порядок: **проверка → pull → push** (каждая половина — только если включён её
тумблер). Pull идёт первым: после него push реже встречает в общей базе версии, которых
клиент ещё не видел, то есть реже конфликтует.

**Проверка.** Локальная БД новее бинаря (`SchemaAhead`) → прогон не идёт (§4.1). `Dial`
(без автомиграции) → `Base()`. Версия схемы общей базы не равна `pgSchemaVersion` →
прогон не идёт, ошибка в статусе («примените схему» / «обновите memo»). Признаки
восстановления базы (курсор или `max(change_xid)` больше `xmax`, §4.1) → прогон не идёт,
ошибка «выполните ротацию» (проверяется, только если `base_id` прежний: после ротации
курсор и так сброшен). `base_id` не совпал с сохранённым → курсор в начало,
`push_marked = 0` — набор размечается целиком (`Mark` с `ids == nil`) при включённом push
сейчас или при его включении позже. Нет `sync_state` или push включён при
`push_marked = 0` → разметка и `push_marked = 1`; push выключен → `push_marked = 0`.
Итог прогона (счётчики, `last_error`) пишется в `sync_state` и после отмены контекста
(таймаут прогона), кроме случая `SchemaAhead`.
Полный pull и полная сверка при push идемпотентны.

**Last-write-wins.** Версии сравниваются по `updated_at`, запись — целиком (поля, теги,
связи внутри набора). Новее — побеждает. **Равные версии** (все синхронизируемые поля,
теги и связи набора совпадают) — ничего не делается. Сравнение — в канонической форме:
`NULL` и пустая строка у необязательных полей равны, теги — множество, `rel` по
умолчанию — `related`; иначе ложные расхождения давали бы лишние применения и сбросы
векторов. **Ничья** (равный `updated_at`, разное содержимое) — побеждает общая база:
правило детерминировано и сходится на всех машинах. Благодаря строго растущему
`updated_at` (§4) ничья возможна только между разными машинами. Расхождение часов —
принятый риск (§14 Этап 19).

**Конфликт** (счётчик в статусе) — LWW отбросил чьё-то изменение, которого другая
сторона не видела: (а) отброшена локальная **правка** (пометка `edited = 1`, а общая
версия новее или ничья; пометка первичной разметки конфликтом не считается); (б) push
перезаписал в общей базе чужую версию (`updated_by` ≠ имя отправителя), которой этот
клиент не видел к началу прогона: её `change_xid` не меньше курсора pull **на начало
прогона**. Сюда входит и версия, которую pull этого прогона сравнил, но локальная правка
оказалась новее: правка сделана без знания о ней, и чужое изменение отброшено. Обычное
«я изменил — отправил» конфликтом не считается. Принятая неточность: версию из окна
перечитывания `[xmin, xmax)` (§4.1), уже полученную прошлым прогоном, правило считает
невиденной — при долгой транзакции на сервере счётчик может засчитать ложный конфликт.
Точный учёт («последний известный `change_xid`» по записи) отложен до первой жалобы на
ложные конфликты.

**Применение общей версии локально** (общее для pull и для проигравших при push).
`Local.Apply` — мимо write path: без дедупа и классификации, `id`, `created_at`,
`updated_at`, `author`, `updated_by` — как в источнике (дедуп слил бы задачу с чужим
id, а новый `updated_at` вызвал бы пинг-понг); локальные `accessed_at`/`access_count`
сохраняются. В одной транзакции: сначала записи, затем связи, снятие пометки
`sync_dirty`, удаление строки вектора, если сменился `content` — из него строится
вектор (запись попадает в очередь `ListMissingVectors`, §9). CAS по `updated_at`+`seq`: локальная
запись изменилась после чтения — пропуск, запись останется помеченной и разрешится
следующим прогоном.
- **Связи:** итоговые связи записи = её связи из общей базы **внутри** набора ∪ её
  локальные связи на записи **вне** набора (episodic, `user:*`, другие проекты). `Put`
  заменяет связи записи целиком, поэтому слияние делает интерактор до `Apply`, — иначе
  pull стирал бы локальные связи на эпизоды. Входящие связи (чужие `src`) не трогаются.
- **FK на `links.dst`.** Все адресаты связей записей страницы, которых в странице нет,
  приходят в `Refs` — любого scope и вида (тот же снимок; общая база держит тот же FK,
  поэтому адресат там есть всегда). Адаптер не знает, что есть локально: что из `Refs`
  применять, решает интерактор, а `Refs` вне набора служат только классификации рёбер
  (внутри набора или вне). Адресат, которого нет в снимке, классифицируется по локальной
  базе (сосед по пачке push). **`Refs` применяются без связей из общей базы** — как
  заглушка версии, со своими локальными связями, если запись уже есть: у самих `Refs`
  адресаты могут лежать на дальних страницах, и применение со связями ломало бы FK и
  останавливало pull навсегда. Полные связи `Ref`-записи придут с её собственной
  страницей того же или следующего прогона: её `change_xid` не меньше курсора, а LWW
  увидит ничью с отличием в связях и применит общую версию. Ребро, адресат которого всё
  же не нашёлся, отбрасывается со счётчиком `DroppedLinks` (§15).

**Pull.**
1. `Shared.Pull` от сохранённого курсора отдаёт страницы изменений набора с `Refs` (§4.1).
2. Для каждой записи страницы и `Refs`: `Local.Versions` → решение LWW. Побеждает общая
   (локальной нет, она старее или ничья) → в пачку `Apply`. Побеждает локальная → её
   отправит push; обычно она уже помечена, а если нет (общую версию записал прямой
   писатель с отстающими часами) — интерактор помечает её сам (`Mark`).
3. После применения страницы — `SaveState` с курсором страницы (§4.1). Сбой или таймаут
   посреди прогона оставляет курсор на последней применённой странице, следующий прогон
   продолжает с неё.

**Push** — пачками ограниченного размера (порядка сотен записей); каждая пачка — своя
транзакция на общей стороне и свой `MarkPushed`, поэтому большой первичный push под
таймаутом прогона продвигается, а не откатывается целиком каждый тик.
1. `Dirty(after, limit)` → очередная пачка помеченных записей набора с `seq > after`
   (`after` — наибольший `seq` прошлой пачки: отложенные `stale` остаются помеченными, и
   без него цикл не завершился бы); `Shared.Versions` →
   версии общей базы (с `change_xid`) и `Refs` их связей.
2. LWW по каждой. **Равные** → только снять пометку. **Побеждает общая** (новее или ничья)
   → при включённом pull общая версия сразу применяется локально (`Apply`, выше), а не
   ждёт pull: строка, которую клиент уже прочитал, в pull больше не придёт, и без этого
   шага стороны разошлись бы навсегда. При выключенном «Загружать» локальная запись не
   трогается: пометка снимается, правка считается конфликтом — тумблер обещает не писать
   общие данные в локальную БД. **Побеждает локальная** (в общей нет или она старее) → в
   пачку `Push` с `ExpectXID`.
3. **Авторы:** записи пачки, которых в общей базе ещё нет (`ExpectXID == 0`) и у которых
   нет `author`/`updated_by`, получают имя отправителя — в исходящей копии и локально
   (`StampAuthor`: без смены `updated_at` и без новой пометки, это заполнение
   метаданных, а не правка). Записи, уже лежащие в общей базе, не штампуются: иначе
   чужая запись без автора (её писал прямой писатель на старом бинаре или с пустым
   именем) получала бы чужое авторство и вечно проигрывала бы ничью. Пустое имя — шаг
   пропускается.
4. **Связи в общую базу** — только внутри набора: локальное ребро на запись вне набора
   не отправляется. Слияние зеркально применению: итоговые связи записи в общей базе =
   её локальные связи внутри набора ∪ её связи в общей базе на записи вне набора (их
   могли поставить прямые писатели — push их не стирает). Адресат из набора, которого
   нет в общей базе и нет в пачке, добавляется в пачку из локальных данных (замыкание по
   FK общей базы; пачка может немного превысить лимит).
5. `Shared.Push` — одна транзакция `READ COMMITTED` на пачку: записи по возрастанию `id`
   (единый порядок блокировок у параллельных писателей — без взаимоблокировок), CAS
   `WHERE change_xid = ExpectXID` (или `INSERT … ON CONFLICT DO NOTHING` для новой),
   затем связи записанных (связи `stale`-записей не пишутся). Блокировка CAS — `FOR NO
   KEY UPDATE`: проверка FK при вставке связи берёт `KEY SHARE` на адресат в
   произвольном порядке, и с `FOR UPDATE` два писателя с перекрёстными связями
   (X→Y и Y→X) взаимно блокировались бы во втором проходе. `accessed_at`/
   `access_count` общей базы не перезаписываются. Не прошедшие CAS — `stale`:
   параллельный писатель успел раньше (под `READ COMMITTED` ожидающий `UPDATE`
   перепроверяет условие на новой версии строки и не находит её), запись остаётся
   помеченной и разрешится следующим прогоном (`Deferred`). Векторы общей базы (если её
   открывают с `driver = "postgres"` и эмбеддером) при смене текста сбрасываются тем же
   правилом, что локально.
6. `MarkPushed` (seq-guarded) — снять пометку у записанных и равных; у проигравших её уже
   снял `Apply` (или шаг 2 при выключенном pull). `SaveState` — после каждой пачки.

**Идемпотентность и отсутствие пинг-понга.** Повторный прогон без изменений —
0/0/0: pull перечитывает окно `[xmin, xmax)` и свои же отправленные записи, но равные
версии пропускаются; применённое pull'ом не помечается неотправленным. Тумстоуны
(`superseded_by`) — обычная версия записи: `Supersede` двигает `updated_at`, поэтому
`forget` на одной машине побеждает старую живую версию на другой. `superseded_by`
может указывать на запись вне набора (`replaced_by` на эпизод) — это просто строка, FK
на ней нет, а запись-тумстоун из recall исключена в любом случае.

**Что остаётся локальным.** Maint-джобы работают на каждой машине сами по себе (§11):
`ExpireSweep` и dedup-sweep на двух машинах дадут два тумстоуна одной записи — LWW
оставит один; консолидация видит только свои эпизоды. Дубли одного факта, независимо
сохранённые двумя разработчиками, приходят друг к другу с разными id и сливаются
обычным dedup-sweep.

**Выключение.** Снятие тумблера останавливает соответствующую половину; удаление цели
стирает её `sync_state` и пометки проекта, **скачанные данные остаются** (локальная
память не зависит от того, откуда записи пришли).

---

## 8. Read path (recall) — «ровно сколько нужно»

Сердце системы. Recall **не отдаёт top-k сырьём**:

1. **Кандидаты.** Hybrid retrieval: FTS5 (лексика/имена/ключи) + vec (смысл, если включён)
   + фильтры `scope`/`kind`/`status`/свежесть. Фильтр по состоянию (`Query.Statuses`)
   несимметричен остальным: пустое значение — не «без фильтра», а исключение
   выполненных задач (`kind=task` и `status=done`) из выдачи; увидеть сделанное
   можно, спросив явно (`statuses=["done"]`). Записи прочих `kind` фильтр не
   затрагивает. Лексический слой снимает русские падежные/числовые
   окончания стеммингом (доп. колонка `content_stemmed`, задача 8.3) — находит словоформы
   одной лексемы («проекта»/«проектом» ↔ «проект»), но не разные лексемы-синонимы
   («собирать»/«сборка», «компилировать»/«сборка») — для этого остаётся вектор.
2. **Слияние.** Reciprocal Rank Fusion (RRF) по рангам (k≈60) — устойчиво, без калибровки шкал:
   `score(doc) = Σ 1/(k + rank_i(doc))` по источникам.
3. **Ранжирование.** После RRF накладываем бизнес-сигналы:
   `final = w1·relevance + w2·importance + w3·recency_decay + w4·log(1+access_count)`.
   Для `kind=debt` результат дополнительно умножается на понижающий множитель
   `debt_weight` (`config.RecallConfig.DebtWeight`, §13, дефолт `0.6`) — накопленные
   долги не должны вытеснять факты из бюджета токенов; для остальных `kind`
   множитель равен `1`.
4. **Сборка брифа.** Режем под `token_budget`, группируем по `kind` в фиксированном
   порядке — Facts (semantic) → How-to (procedural) → Tasks (task) → Tech debt (debt)
   → Recent context (episodic), важное внутри секции — первым, честно указываем
   «свёрнуто ещё N». Строка задачи начинается с маркера состояния
   (`- [doing] Заголовок [★4]`) — список задач без статусов бесполезен, а сам
   статус в бриф иначе не попадает.

**Формат выдачи — Markdown-бриф**, а не JSON-простыня:

```markdown
## Релевантная память (5 записей, ~420 токенов)

### Facts (semantic)
- Проект на Go 1.23, SQLite через mattn/go-sqlite3 [★4]
- API-ключ DeepSeek в env `DEEPSEEK_API_KEY` [★5]

### How-to (procedural)
- Сборка: `make build`; тесты: `make test` перед деплоем [★4]

### Tasks (task)
- [doing] Перевести хранение задач из файлов в память [★4]

### Tech debt (debt)
- `SQLiteMemoryCount` сканирует таблицу целиком вместо `EXISTS`, приоритет низкий [★2]

### Recent context (episodic)
- В прошлой сессии чинили дедуп в memory_save — решение через ULID+scope [★3]

_Свёрнуто ещё 12 менее релевантных записей. Уточни запрос при необходимости._
```

Опционально: `Reasoner` (DeepSeek) сжимает найденное в 1 абзац + список фактов.

Опционально (`Query.Explain`, задача 8.1): под каждой записью брифа — компактная
трасса ранжирования (источники кандидата + разложение final на слагаемые шага 3),
без пересчёта и без изменения самого отбора/ранжирования — только для отладки и
тюнинга весов `[recall].weights`. Понижающий множитель `debt_weight` виден в трассе
отдельным полем `KindFactor` (`1` для всех `kind`, кроме `debt`, где равен
`debt_weight`) — инвариант «сумма слагаемых == Final» из задачи 8.1 для `kind=debt`
превращается в «(сумма слагаемых) × KindFactor == Final».

---

## 9. Embedder — конфигурируемый локальный вектор

- **Дефолт: `bge-m3` через ollama** (1024 dim, мультиязычный — важно, факты будут на русском,
  код на английском; контекст 8192).
- **Любая ollama-модель** переключается одной строкой конфига (`model = "..."`).
- **`dim` не хардкодится** — определяется пробным эмбеддингом при старте (заодно проверка,
  что ollama жив и модель подтянута).
- **Смена модели ≠ смена строки.** Векторы разных моделей несравнимы (разное пространство,
  часто разный dim). При старте сверяем `embed_meta` с текущей моделью:
  совпало — работаем; не совпало — пересоздаём vec-таблицу с новым dim и запускаем
  **reindex** (фоновый джоб), пока идёт — recall деградирует до FTS5.
- **Отказоустойчивость.** ollama недоступен → recall на чистом FTS5, save откладывает вектор
  в очередь. Вектор — опциональный слой, а не критическая зависимость.
- **Кэш.** Эмбеддинг — чистая функция от `(model, text)`; кэшируем по `sha256(model+text)`.

> ⚠️ Ни DeepSeek, ни Anthropic (Claude/Haiku) **не** предоставляют embeddings API — это
> chat-модели. Вектор берём только у провайдера с embeddings endpoint (локальный ollama —
> дефолт; OpenAI `text-embedding-3-small` — опция на будущее за тем же интерфейсом).

> ℹ️ `provider = "openai"` — это не только сам OpenAI: `/v1/embeddings` де-факто стандарт,
> поэтому одна реализация (`internal/memory/embed.OpenAI`, `base_url` конфигурируем)
> покрывает любой совместимый сервер — OpenRouter, LM Studio, vLLM и т.п. Ключ читается
> только из env `OPENAI_API_KEY` (то же правило, что для `DEEPSEEK_API_KEY`, §13).

---

## 10. Reasoner — OpenAI-совместимый chat

Chat-модель в роли текстового процессора памяти:
- суммаризация recall-выдачи в абзац + факты (опционально);
- консолидация кластеров `episodic` в обобщённые `semantic`;
- дедуп-суждения в спорных случаях.

Адаптер один — `reason.OpenAI` (`POST {base_url}/chat/completions`, Bearer-ключ);
провайдеры различаются только base URL, env-ключом и моделью, которые выбирает
composition root по реестру `reasonerProviders` (`cmd/memo/init.go`):

| `provider` | Дефолтный `base_url` | Ключ (env) | Модель |
|---|---|---|---|
| `deepseek` | `https://api.deepseek.com` | `DEEPSEEK_API_KEY` | дефолт `deepseek-chat` |
| `cliproxy` | `http://127.0.0.1:8317/v1` | `CLIPROXY_API_KEY` | обязательна: набор зависит от аккаунтов прокси (`GET {base_url}/models`) |

`cliproxy` — локальный [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI):
OpenAI-совместимый прокси к Gemini/Claude/Codex по подписке; ключ — один из `api-keys`
его конфига. `reasoner.base_url` в TOML перекрывает дефолт провайдера. Без ключа или
модели — деградация до Noop (§15). Для эмбеддингов reasoner **не используется**.

---

## 11. Maintenance — фоновые джобы

Отличают систему от прототипа:
- **ExpireSweep** (задача 9.4) — архивирует записи с истёкшим явным TTL (`expires_at`, §4):
  `Store.ListExpired` (0 < expires_at < now, без superseded_by) → тумстоун
  `Supersede(id, id)`. В отличие от Decay (эвристика, постепенное понижение веса),
  здесь срок известен заранее и точно — запись сразу архивируется, без промежуточного
  понижения importance. Первый джоб в порядке `Scheduler.RunOnce`: просроченные не
  должны тратить прогон Decay/Consolidate впустую. Параметров конфига не требует —
  момент истечения уже задан самой записью.
- **Decay** — понижаем вес неиспользуемых `episodic`, старьё архивируем.
- **Consolidation** (через Reasoner) — кластер похожих `episodic` → один `semantic`,
  старые помечаем `superseded_by`.
- **Dedup sweep** — чистка накопившихся почти-дублей (SimHash), кандидаты группируются
  по паре `(scope, kind)` (`maint.groupForDedup`) — дубли ищутся только внутри одной
  группы: запись одного `kind` никогда не supersede'ит запись другого `kind` в том же
  `scope`, а `debt` дедуплицируется только между долгами того же scope. Задачи
  (`kind=task`) в sweep не участвуют вовсе — то же исключение, что и на write path
  (§7, `write.SkipDedup`).
- **Reindex** — перегон всего `content` через новый Embedder при смене модели.

`kind=debt` и `kind=task` ни один из этих джобов не касается: `Decay`/`Consolidation`
работают только с `episodic` (см. `Store.ListEpisodicForConsolidation`/
`ListEpisodicForDecay`, §6) — долг и задача не затухают и не консолидируются в
`semantic`, они живут до явного закрытия (`memory_forget` либо, для задачи, перевод
в `status=done`, §4, §12). `ExpireSweep`/TTL применимы к записи любого `kind`,
включая `debt` и `task`, наравне со всеми.

**Фоновая синхронизация (Этап 19, §7.1).** `sync.Scheduler` (по образцу
`maint.Scheduler`: поле `now func() int64`, первый прогон сразу при старте) работает в
**любом** долгоживущем процессе `memo` на SQLite — MCP-сервере и `memo web`; one-shot
команды (`save`/`recall`/`hook`) его не запускают. Синхронизация — свойство локальной БД,
а не инстанса: процесс синхронизирует **все** цели из `sync_targets`, а не только свой
`[scope] project`.
- **Интервал** — `sync_profile.interval_s` (дефолт 5 мин, минимум 1 мин), перечитывается
  на каждом тике: правка в морде действует без перезапуска процессов.
- **Lease.** Несколько процессов одной машины (MCP-серверы разных сессий + `memo web`)
  не должны гонять одну цель параллельно: перед прогоном цели процесс захватывает
  `sync_lease(project)` условным `UPDATE … WHERE expires_at < now OR holder = me` (одна
  транзакция SQLite — захват атомарен), `holder` — ULID процесса, срок — 5 мин при
  таймауте прогона 2 мин; после прогона lease снимается. Занято — цель в этом тике
  пропускается без ошибки. Упавший процесс не держит цель дольше срока: истёкший lease
  перехватывается. Машины между собой lease не делят — их разводят транзакции и CAS
  общей базы.
- **Деградация (§15).** Прогон идёт под таймаутом (соединение и весь прогон), ошибка
  (сервер недоступен, нет пароля, неверная версия схемы) пишется в
  `sync_state.last_error` и видна в морде/`doctor`; процесс, recall и save продолжают
  работать, следующий тик повторит попытку. Паник и падений процесса из-за общей базы нет.
- При `driver = "postgres"` планировщик синхронизации не запускается.

---

## 12. MCP API (инструменты агента)

Набор маленький — агенту нужно несколько инструментов, не 15.

- **`memory_save`** — принимает структурированный факт. `links` — связи с уже
  существующими записями графа (задача 9.3): элемент либо ULID-строка (id,
  back-compat, тип связи будет `related`), либо объект `{"id":..,"rel":..}` с
  явным типом (`related` | `supersedes` | `causes` | `part_of`, DESIGN.md §4):
  ```json
  { "kind": "semantic", "content": "...", "importance": 4,
    "tags": ["go","stack"], "scope": "project:agentmemory",
    "links": ["01J8Z0000000000000000BUILD", {"id": "01J8Z0000000000000000MAKE", "rel": "causes"}] }
  ```
  `rel=supersedes` здесь — ТОЛЬКО ребро графа связей (агент знает, что запись
  замещает другую по смыслу); оно НЕ подменяет фактическое устаревание записи
  (`memory.Memory.SupersededBy` / `memory_forget` с `replaced_by`) — это две
  независимые вещи, ребро не исключает целевую запись из recall.
  Опциональный `ttl` (задача 9.4) — явный срок жизни факта: Go-длительность от
  текущего момента (`time.ParseDuration`, напр. `"72h"`), а не абсолютная дата —
  тот же принцип, что у `created_after`/`created_before` ниже; пусто = вечно.
  Невалидная строка — ошибка (fail-fast), а не молчаливый откат к вечной записи.
  `kind: "debt"` — техдолг: известная проблема кода/архитектуры, отложенная
  осознанно (не отдельная таблица и не отдельные инструменты — четвёртый `kind`
  наравне с `semantic`/`episodic`/`procedural`, §2, §4). Конвенция содержания:
  `content` атомарно — что не так + где (пакет/файл) + почему отложено + критерий
  закрытия; `importance` — приоритет долга (1..5); `tags` — область (напр.
  `"pkg:store"`). Долг не участвует в decay/consolidation (§11) — закрывается
  явно, той же операцией, что и любая другая память: `memory_forget(id,
  replaced_by=<id записи о фиксе>)`, если фикса нет и долг просто снят — без
  `replaced_by`. Отдельной подкоманды `memo debt` в CLI в этой итерации нет —
  сущность полностью доступна через `memory_save`/`memory_recall`/`memory_forget`.
  `kind: "task"` — задача агента вместо файла TODO (§2, §4, задача 14.1):
  `content` — полный текст задачи (что сделать, границы, критерий приёмки),
  `summary` ОБЯЗАТЕЛЕН и служит заголовком (именно он выводится в бриф, §8; без
  него — ошибка `write.ErrTaskSummaryRequired`), `status` — стадия доски
  (`backlog`|`todo`|`doing`|`review`|`testing`|`release`|`done`, пусто → `todo`,
  Этап 15), `type` — `epic`|`story`|`task` (пусто → `task`), `branch` — git-ветка,
  `workflow` — имя процесса (`dev`, `ultracode`, …), `importance` — приоритет.
  Иерархия — `links` с `rel=part_of` строго от дочерней к родительской
  (история `part_of` эпика, задача `part_of` истории); блокировка — `rel=blocked_by`
  от заблокированной задачи к блокеру (см. §14 Этап 15). Поля `status`/`type`/
  `branch`/`workflow` применимы ТОЛЬКО к `kind=task`: у остальных типов они —
  ошибка (`write.ErrStatusNotApplicable` и аналоги), а не молчаливо
  отбрасываемое значение.
  Задачи не дедуплицируются (§7) и не затухают (§11).
- **`memory_get`** — читает одну запись целиком по `id`: полный `content` плюс
  метаданные (kind/status/scope/importance/tags/links, признак архивности, автор и
  последний редактор — Этап 19).
  Дополняет `memory_recall`, а не дублирует его: бриф намеренно компактен и
  показывает лишь краткую форму записи, чего достаточно для факта в 1–3
  предложения, но не для задачи-документа — её заголовок виден в брифе, а текст
  забирается этим инструментом. Фильтров scope/статуса здесь нет (id — точный
  адрес), `Touch` не вызывается: чтение по прямому id не сигнал релевантности
  поиска и не должно искажать ранжирование остальных записей.
- **`memory_recall`** — главный инструмент, возвращает Markdown-бриф. Фильтр по
  пространствам — набор `scopes` (перекрывает одиночный back-compat `scope`);
  пусто — дефолт инстанса (свой `project:<name>` + общий `user:*`), а при
  ненастроенном `project` — без фильтра (§2.1):
  ```json
  { "query": "как собирать и тестировать", "scopes": ["project:agentmemory","user:*"],
    "kinds": ["procedural","semantic"], "tags": ["стек"], "created_after": "168h",
    "token_budget": 800 }
  ```
  Опциональные `tags` (OR — хотя бы один из перечисленных) и `created_after`/
  `created_before` (задача 8.2) сужают кандидатов до ранжирования; формат дат —
  длительность назад от текущего момента (`time.ParseDuration`, напр. `"168h"` =
  за последнюю неделю), а не абсолютная дата — типичный запрос агента звучит как
  «за последнюю неделю», а не конкретным календарным днём.
  Опциональный `explain` (bool, задача 8.1) добавляет под каждой записью брифа
  трассу ранжирования (источники, RRF, разложение итогового скора) для отладки.
  Опциональный `statuses` (задача 14.1) фильтрует задачи по стадии; пустое
  значение означает не «все», а «всё, кроме выполненных задач» — свой список дел
  агент получает как `statuses: ["todo","doing"]`, сделанное — как
  `statuses: ["done"]` (§8). Опциональный `types` (Этап 15) фильтрует задачи по
  типу (`epic|story|task`); непустой набор сам по себе означает «только
  задачи» — список эпиков проекта: `types: ["epic"]`.
- **`memory_update`** — правка факта (в т.ч. пометка устаревшим). `links` — тот
  же формат, что в `memory_save` (задача 9.3, закрывает тех-долг задачи 5.1:
  до этой задачи типизированные связи можно было проставить только при
  создании) — но семантика АДДИТИВНА: связи ДОБАВЛЯЮТСЯ к уже существующим
  исходящим рёбрам записи, а не заменяют их (в отличие от `tags`, которые
  заменяются целиком):
  ```json
  { "id": "01J8Z0000000000000000FACT", "importance": 5,
    "links": [{"id": "01J8Z0000000000000000MAKE", "rel": "part_of"}] }
  ```
  Опциональный `ttl` (задача 9.4, тот же формат, что в `memory_save`) продлевает
  или снимает срок жизни: любая Go-длительность — новый срок от ТЕКУЩЕГО момента
  (продление); явный сентинел `"0"` (или пустая строка) — снять срок, запись
  становится вечной; поле не передано — текущий срок не меняется.
  Опциональный `unlinks` (Этап 15, задача 15.4) — связи к удалению в том же
  формате, что `links` (пара `id`+`rel`, строка = `related`): единственный
  способ снять ребро, в т.ч. вручную снять блокировку `blocked_by`; применяется
  после `links`, отсутствующая пара молча пропускается. Пост-состояние задачи
  проверяется после всех полей: заблокированная живым блокером задача не может
  оказаться дальше `todo` — `write.ErrTaskBlocked` с перечнем блокеров (§14
  Этап 15).
- **`memory_forget`** — мягкое удаление (`superseded_by` / архив).

Транспорт: MCP over stdio (`/cmd/memo`).

**Web-морда (read + курирование записей).** `memo web` (Этап 6, `cmd/memo`) поднимает
отдельный HTTP-режим того же бинаря поверх того же файла БД: JSON API (`/api/stats`,
`/api/memories`, `/api/memories/{id}`, `/api/search`, `/api/board`) на read-методах Store
(`List`/`Stats`/`Get`/`Neighbors`/`SearchLexical`, а также `VectorStatus`/`SemanticNeighbors`
для карточки записи). Write-операций три, все — тонкие
обёртки над доменным `write.Writer` (та же семантика, что у одноимённых MCP-инструментов):
`POST /api/memories` — создание задачи через `Writer.Save` (Этап 17, см. ниже);
`DELETE /api/memories/{id}` — мягкое удаление через `Writer.Forget` (тумстоун
`Store.Supersede(id, id)`, та же семантика, что у `memory_forget`; hard DELETE не делается,
история и связи сохраняются) и `PATCH /api/memories/{id}` — правка живой записи через
`Writer.Update`, **whitelist-DTO 1:1 в `write.UpdateInput` без собственной логики**: тело
`{"content","summary","importance","tags","status","type","branch","workflow","links","unlinks"}`,
все поля опциональны (отсутствующее — не трогать, частичный PATCH). `importance`/`tags` —
задача 10.2 (`tags:[]` — валидное «снять все теги», заменяющая семантика; importance вне 1..5 не
отклоняется 400, а клампится — симметрично MCP `memory_update`, единый `write.ClampImportance`);
рабочие метаданные задачи `status`/`type`/`branch`/`workflow`/`links`/`unlinks` — Этап 15
(kanban-доска без переноса карточек бессмысленна); `content`/`summary` — Этап 16 (панель
записи, «Изменить»: пустой `summary` снимает заголовок, пустой `content` → 400
`ErrEmptyContent`, переэмбеддинг делает `Writer.Update`). До Этапа 16 действовал инвариант
«содержание из браузера не редактируется никогда» — снят решением пользователя вместе с
принятием макета (§14 Этап 16). Граница по-прежнему формулируется явным списком: из браузера
**никогда** не меняются `kind` и `scope` существующей записи и не делается hard DELETE;
лишнее поле в теле → 400 с перечнем допустимых.

`POST /api/memories` (Этап 17) — **единственный способ завести запись из браузера, и только
`kind=task`**: инвариант «создания записей из браузера нет» снят ровно для задач (§14 Этап 17),
факты/эпизоды/процедуры/долг по-прежнему пишет только агент через `memory_save`. Тело —
whitelist-DTO 1:1 в `write.SaveInput`:
`{"kind":"task","scope","type","summary","content","status","importance","tags","branch",
"workflow","links":[{"id","rel":"part_of"}]}`. `kind` не `task` (в т.ч. пустой) → 400; `links`
с любым `rel`, кроме `part_of`, → 400 (при создании осмысленна одна связь — «входит в»;
блокировки ставятся уже на карточке через `PATCH`, где виден весь граф); пустой `scope` →
домашний scope инстанса (как в MCP `memory_save`); пустой `content` → уходит заголовок
(быстрая карточка с доски, иначе `ErrEmptyContent` не дал бы её завести — подстановка
на границе морды, для `memory_save` пустой `content` остаётся ошибкой). Ошибки домена
(`ErrTaskSummaryRequired`, `ErrUnknownStatus`, `ErrUnknownTaskType`, …) → 400, ошибка store →
500, успех → **201** и тот же `memoryDTO`, что у `GET /api/memories/{id}`. Дедупа нет —
задачи не сливаются (§7). Тумстоун (`SupersededBy != nil`) — 409, правка
архивной записи недоступна. Обе write-операции — не bulk, только одна запись за запрос.
Слушает по умолчанию только `127.0.0.1:7070` (localhost, наружу не выставляется).
`GET /api/stats` (Этап 16) кроме счётчиков отдаёт `homeScope`, `providers:{store:{driver,
target, schema}, embedder:{provider, model, dim, available, pending}, reasoner:{provider,
model, available}}` (имена — из конфига composition root, `available` эмбеддера — `Dim() > 0`
собранного экземпляра, `pending` — `Stats.PendingVectors`, `schema` — `Store.SchemaVersion`;
у noop-провайдера модель пустая) и `jobs:{lastRunAt|null}` (`Store.LastMaintRun`, пишет
планировщик MCP-сервера). `GET /api/memories` принимает `expiresBefore=<unix>` →
`BrowseFilter.ExpiresBefore` (ближайшие к истечению первыми). Поиск (`/api/search?q=...&mode=lexical|semantic`, задача 10.1) — `mode=semantic`
(дефолт) гоняет тот же гибридный путь, что и MCP `memory_recall` (`recall.Recaller.Candidates` —
FTS5 + вектор при доступном Embedder, RRF, бизнес-ранжирование, без сборки Markdown-брифа);
`mode=lexical` — прежний чистый `Store.SearchLexical`, для сравнения глазами. Ответ несёт `source`
кандидата (`"lexical"`/`"vector"`/`"lexical+vector"`, та же величина, что в explain-трассе брифа,
задача 8.1) и флаг `vector` — доступен ли векторный слой у этого инстанса; при недоступном/noop
Embedder гибрид молча деградирует до FTS5 (HTTP 200, `vector:false`), фронт показывает плашку
деградации, а не 500. Карточка записи (`/api/memories/{id}`, задача 6.4) отдельно показывает
статус вектора и KNN-соседей по cosine — read-only чтение уже сохранённых векторов из БД, без
вызовов Embedder/ollama; там же (задача 10.2) — интерактивные звёзды importance и редактируемые
чипы tags поверх PATCH. Статический фронтенд отдаётся из `embed.FS` (`cmd/memo/web/`), сам
интерфейс — задачи 6.3/6.4/10.1/10.2.

**Доска (Этап 15, задача 15.5).** `GET /api/board?scope=…(повторяемый)&type=…(повторяемый)&parent=…&branch=…&workflow=…&tag=…&doneLimit=N`
— тонкая доставка варианта использования `board.Build` (пакет
`internal/memory/board`, §14 Этап 15): ответ `{columns:[{status, total,
cards:[{id, summary, type, status, importance, tags, branch, workflow, updatedAt,
blocked, blockedBy:[{id, summary, status, type}], parent:{id, summary, status,
type}|null, progress:{done, total}|null}]}]}` — всегда семь колонок в порядке
`memory.Statuses`, карточки без `content` (запись целиком — `GET /api/memories/
{id}`), `total` у `done` — до усечения `doneLimit` (дефолт 30; «показать ещё»
— повторный запрос с бо́льшим `doneLimit`), неизвестный `type`/невалидный
`doneLimit` → 400. Карточка записи `GET /api/memories/{id}` через тот же
`board.Describe` отдаёт `parent`/`children`/`progress` (живой родитель;
тумстоун опускается) и `blockedBy`/`blocks`. Расширенный `PATCH /api/memories/
{id}`: помимо `importance`/`tags` принимает `status`, `type`, `branch`,
`workflow`, `links:[{id, rel}]` (аддитивно) и `unlinks:[{id, rel}]` — 1:1 в
`write.UpdateInput`; `rel` проверяется по `memory.LinkRels` на границе; ошибки
домена маппятся `errors.Is`: `write.ErrTaskBlocked` → 409 с телом `{error,
blockers:[{id, summary, status, type}]}`, `ErrUnknownStatus`/`ErrUnknownTaskType`/
`ErrStatusNotApplicable`/`ErrTaskFieldNotApplicable`/`ErrBlockerNotTask` → 400,
тумстоун → 409, как прежде. Пикер блокера/родителя на фронте — существующий
`GET /api/search?q=…&mode=lexical&kind=task`, отдельного эндпоинта нет.
**Экран доски (задача 15.6)** — `#/board?scope=…&type=…&parent=…&branch=…&workflow=…&tag=…&doneLimit=N&group=epic`
в той же одностраничной морде (vanilla JS + Tailwind, без сторонних библиотек):
hash-роутер с переключателем «Память | Доска», состояние фильтров живёт только в
URL-хэше (смена фильтра пишет хэш, `hashchange` грузит доску — ссылка
воспроизводит экран), семь колонок, свимлейны по ближайшему родителю, нативный
HTML5 drag&drop с оптимистичным переносом (`PATCH {status}`; 409/400 — откат
карточки и тост с перечнем блокеров), модалка задачи правит стадию/тип/ветку/
workflow/родителя/блокеров тем же PATCH; доменной логики доски на фронте нет — он
рисует ответ `board.Build`/`board.Describe` и маппит ошибки сервера в сообщения.
Представление остаётся на фронте: свимлейны — группировка по `parent` из ответа
(в `board.Build` не входит), «показать ещё» — удвоение `doneLimit`.

**Экраны новой морды (Этап 16, задачи 16.3–16.6)** — `#/` обзор, `#/memory` память,
`#/board` доска, панель записи/задачи по `id=` в том же хэше; один глобальный селект
scope на все три экрана. Фронт по-прежнему без доменной логики, но представление считает
сам — по факту реализации это: отбор «Добавлено за 7 дней» по `createdAt` из
`GET /api/memories?limit=10`, порядок «На доске» (`doing` → `review`/`testing` →
заблокированные) и счётчик заблокированных из ответа `/api/board`, свимлейны и «показать
ещё» (удвоение показанного), подсветка совпадений запроса. Три следствия контракта,
зафиксированные приёмкой 16.7: счётчики по видам и «в архиве» **для выбранного scope**
собираются доборами `GET /api/memories?scope&kind&limit=1` (в `/api/stats` per-scope
разбивки по видам нет — `ByScope` даёт только живые записи scope); фильтр «с архивом» в
режиме поиска недоступен (в `/api/search` параметра нет, `metaFilters` всегда режет
тумстоуны) и в этом режиме показывается выключенным; выдача `/api/search` не несёт `tags`
(read-hot-path `SearchLexical`/`Candidates` их не грузит), поэтому чипы тегов видны в
списке, но не в результатах поиска. Инвариант «у задачи один родитель `part_of`»
по-прежнему держит фронт: смена родителя из панели уходит парой `links` + `unlinks`
(ядро аддитивно, см. долг про `write.Update`).

**Раздел «Синхронизация» (Этап 19, §7.1).** Экран `#/sync` и JSON API под `/api/sync` —
тонкая доставка: настройки пишутся в локальную БД (§13), прогон — `sync.Syncer`, правил
синхронизации в `web.go` нет. Автор и последний редактор видны на панели записи и
карточке задачи (`author`/`updatedBy` в `memoryDTO`).
- `GET /api/sync` → `{available, reason, profile:{name, nameDefault, intervalSec},
  projects:[…], targets:[{project, host, port, user, dbname, sslmode, push, pull,
  passwordSet, state:{lastRunAt, lastOkAt, lastError, pushed, pulled, conflicts,
  conflictsTotal, pending}}]}`. `projects` — проекты из локальной БД (различные
  `project:*`) для выбора; имя можно ввести и вручную — новичок подключает проект,
  которого у него ещё нет. `nameDefault` — `git config --global user.name`. `pending` —
  число помеченных записей набора. При `driver = "postgres"` — `available:false`,
  `reason` «хранилище уже общее», остальные методы → 409.
- `PUT /api/sync/profile` `{name, intervalSec}` — имя разработчика и интервал.
- `PUT /api/sync/targets/{project}` `{host, port, user, dbname, sslmode, push, pull,
  password?}` — создать/изменить цель. **Пароль write-only:** поле отсутствует — не
  трогать, строка — записать в `.env` (§13), пустая строка — удалить; ни один ответ API
  пароль не возвращает, только `passwordSet`. Разметку набора для новой цели или
  включённого `push` делает первый прогон `Syncer` (§4.1), не морда.
- `DELETE /api/sync/targets/{project}` — удалить цель (данные остаются, §7.1).
- `POST /api/sync/targets/{project}/check` — «Проверить подключение»: `{reachable,
  error, schema:{have, want, state: empty|outdated|ok|newer}}`, только чтение, без DDL.
- `POST /api/sync/targets/{project}/schema` `{confirm:true}` — «Создать/обновить схему»:
  явная операция с подтверждением (§4.1); без `confirm` → 400; схема новее бинаря → 409.
- `POST /api/sync/targets/{project}/rotate` `{confirm:true}` — «База восстановлена из
  резервной копии»: новый `sync_meta.base_id`, у всех клиентов курсор сбросится (§4.1).
- `POST /api/sync/targets/{project}/run` — «Синхронизировать сейчас»: прогон под тем же
  lease, ответ — `Result`; lease занят → 409 «синхронизация уже идёт».

Валидация (имя проекта, порт, `sslmode`, интервал) — на границе морды. Пароль и строка
подключения с паролем не пишутся в логи и тексты ошибок.

**CLI `memo sync` и `memo doctor` (Этап 19).** `memo sync [-project X]` — разовый прогон
всех целей (или одной) под тем же lease, построчный итог по проектам, exit-код ≠ 0, если
хоть одна цель упала; `-status` — только состояние из `sync_state` без прогона;
`-apply-schema -project X` — явное создание/обновление схемы общей базы, `-rotate-base
-project X` — ротация `base_id` после восстановления базы (обе спрашивают подтверждение,
`-yes` — без вопроса). `memo doctor` получает секцию «Синхронизация»: по
каждой цели — доступность общей базы (read-only проба с таймаутом, как в `memo init`),
состояние версии схемы, время и итог последнего прогона, последняя ошибка, число
неотправленных; недоступная общая база — предупреждение, а не отказ здоровья локальной
памяти.

**Суженный инвариант оффлайновости (задачи 10.1, 16.1).** До задачи 10.1 морда была строгим
read-only-читателем: ни одного вызова Embedder/Reasoner. `/api/search?mode=semantic` ввёл
эмбеддинг ПОИСКОВОГО ЗАПРОСА (один вызов ollama/OpenAI на запрос, через ту же фабрику
`newEmbedder`, что у MCP-сервера/CLI); Этап 16 добавил второе место — `Writer` морды собирается
с тем же реальным Embedder, и правка `content` через PATCH переэмбеддит запись (иначе в БД
оставался бы устаревший вектор, невидимый очереди досчёта `ListMissingVectors`). Reasoner
по-прежнему не используется (`recall.Recaller` строится с `reason.NewNoop()` — в JSON API нет
суммаризации; фабрика `newReasoner` вызывается только ради `available` в `/api/stats`).
Инвариант теперь: **морда пишет в БД только через доменный `write.Writer` и эмбеддит ровно то
же, что MCP `memory_update`** — запрос при поиске и запись при правке её текста; при
недоступном Embedder поиск деградирует до чистого FTS5 целиком, а правка текста, как и в MCP,
оставляет прежний вектор до следующего полного reindex.

**CLI (одноразовые save/recall).** `memo recall "<запрос>"` и `memo save` (Этап 7,
задача 7.1, `cmd/memo`) — one-shot подкоманды поверх тех же Writer/Recaller, что и MCP-сервер,
но без демона и без транспорта: один прогон = открыть Store → выполнить одну операцию → закрыть.
Нужны там, где нельзя (или не нужно) держать MCP-сессию — прежде всего хукам агентского
харнесса (SessionStart/UserPromptSubmit, задача 7.2), а также разовой отладке/скриптам из shell.
`recall` собирает `recall.Recaller` (Reasoner по умолчанию не задействован — суммаризация через
`-summarize` явно, чтобы не платить его латентностью в хуке) и печатает в stdout ровно Markdown-
бриф (или сырой JSON под `-json`); `-quiet` делает пустую выдачу пустым stdout с exit 0 — для
хуков, которым нечего вставлять в промпт. `save` собирает `write.Writer` с реальным Embedder из
той же фабрики, что и сервер, и печатает одну строку `saved <id>`/`merged <id>` (дедуп — как у
`memory_save`). Разрешение scope в обеих подкомандах — те же `config.ScopeConfig.SaveScope()` /
`RecallScopes()`, что использует MCP-сервер (никакой отдельной копии правила). Оба пути
переиспользуют `newReasoner`/`newEmbedder` и их деградацию до Noop при недоступности
DeepSeek/ollama (предупреждение в stderr, не в stdout, — критично для recall, чей stdout — это
бриф для хука/скрипта); maint-планировщик в CLI-режиме не запускается, это не демон.

**Бэкап и перенос (export/import).** `memo export [-o file] [-scopes a,b]` и `memo import
[-dry-run] [-overwrite] [-rescope old=new] file.jsonl` (Этап 9, задача 9.2, `cmd/memo`) —
полный или частичный экспорт памяти инстанса в переносимый JSONL-бэкап и восстановление
обратно с сохранением идентичности записей. Формат: заголовочная строка с метаданными
(`format`/`version`/`exported_at`/`db`), далее одна запись на строку — вся `memory.Memory`
плюс `tags`/`links`, БЕЗ вектора (производные данные — их досчитает фоновый `Reindexer` после
импорта: пустая `embed_meta` новой БД триггерит полный reindex, §9/§11); тумстоуны
(`superseded_by`) экспортируются как есть — история переносится вместе с фактами. Полный обход
даёт новый read-метод `Store.AllForExport(ctx, scopes)` — все записи (включая superseded) с
tags/links, отсортированные по id (детерминированный, диффабельный порядок): ни `List` (без
Links), ни `AllForReindex` (без superseded) для полного бэкапа не подходят. Импорт идёт
напрямую через `Store.Put` (НЕ через `write.Writer`) — id (ULID) и все таймстампы
(`created_at`/`updated_at`/`accessed_at`/`access_count`) переносятся как есть, а не
пересчитываются: `Put` — чистый upsert по переданным полям, дедуп/классификация write-пути
здесь исказили бы точное восстановление бэкапа. Конфликт id — по умолчанию skip со счётчиком,
`-overwrite` заменяет; links на id, отсутствующий в целевом хранилище, — warning и пропуск
ребра (§15, деградация, а не падение); `-rescope old=new` переписывает только `scope` при
импорте (перенос памяти между проектами). `-dry-run` — архитектурная гарантия, а не
соглашение: хранилище вообще не открывается, разбирается и печатается отчёт только по файлу.

**Ambient-режим (хуки харнесса).** `memo hook -event <name> [-config path]` (Этап 7,
задача 7.2, `cmd/memo/hook.go`) — тонкая обёртка над тем же recall-путём, что и `memo
recall` (общий приватный хелпер `doRecall`, логика не задублирована), но вызывается не
человеком, а самим харнессом агента на событиях жизненного цикла сессии: харнесс сам
дёргает память, бриф вклеивается в контекст без участия агента. Читает JSON события со
stdin (поля харнесса — `session_id`, `cwd`, `hook_event_name`, `prompt` и т.д. — разбираются
мягко, лишние/неизвестные поля игнорируются). Два события: **`session-start`** — recall
фиксированным запросом о состоянии проекта/договорённостях/граблях по дефолтным scopes
инстанса, без фильтра по kind, уменьшенный бюджет токенов (600); **`prompt-submit`** — query
берётся из поля `prompt`, но только если он длиннее ~15 символов (короткие реплики вроде
«ок» не дают recall впустую отработать на каждом ходу), бюджет ещё меньше (400) — событие
срабатывает на каждом сообщении пользователя, раздувать контекст нельзя. Оба события
суммаризацию не запрашивают (латентность), пустая память — пустой stdout, непустая — под
заголовком `## Долговременная память проекта (авто-recall)` (видно происхождение блока).
**ЖЕЛЕЗНЫЙ инвариант:** любая ошибка (нет конфига, нет БД, битый JSON на stdin, неизвестный
`-event`) — пустой stdout и **exit-код 0**, лог только в stderr; это осознанное отступление
от fail-fast остального `memo` — хук не имеет права сломать старт или ход сессии агента.

Подключается `memo init` (задача 7.2, `cmd/memo/init.go`): интерактивный вопрос
«подключить автоматический recall через <хуки харнесса>» (только session-start / оба
события / нет — дефолт первый) — спрашивается у харнессов, которые умеют хуки
(`agentTarget.hooksLabel`: Claude Code и dsh). У Claude Code мержится секция `hooks` в
`.claude/settings.json` — команда
`<путь memo> hook -event <name> -config ${CLAUDE_PROJECT_DIR:-.}/agentmemory.toml`.
Merge, не перезапись: существующий settings.json и его чужие ключи/события/matcher-записи
не теряются, повторный `init` идемпотентен (те же команды не дублируются), битый JSON —
ошибка без записи. У opencode хуков нет — `init` честно сообщает об этом в выводе.

**Хуки в dsh — через мост, и это накладывает два отличия (проверено на живом харнессе).**
Своих хуков у dsh нет: события `SessionStart`/`UserPromptSubmit` поднимает мост
`@deepseek-ai/dsh-hooks-claude-code`, читающий конфиг формата Claude Code. Поэтому
`memo init -agent dsh` кладёт конфиг хуков в проект (`.dsh/memo-hooks.json`, генерируется
целиком, а не мержится: файл наш, и сужение `-hooks` обязано выключать событие), а строку
моста со ссылкой на него — в проектный оверлей (см. §12, `memo init -agent dsh`).
Отличия команд: (1) `-config` — абсолютный путь к каталогу проекта, а не
`${CLAUDE_PROJECT_DIR:-.}/agentmemory.toml` — мост подставляет `${CLAUDE_PROJECT_DIR}`
каталогом сессии (это может быть подкаталог проекта), а `:-`-форму фолбэка не понимает
вовсе; именно каталог, а не файл: только в этой форме `config.Load` разрешает относительный
`store.path` от проекта, а не от рабочего каталога процесса; (2) добавлен `-format json` — мост читает контекст
только из `hookSpecificOutput.additionalContext` и обычный stdout игнорирует, поэтому
`memo hook` получил флаг формата вывода (`text` — дефолт, поведение Claude Code не
менялось; `json` — обёртка Claude Code для мостов). Пустая память и в json-формате даёт
пустой stdout.

**`memo init -agent dsh` (третий харнесс).** У DeepSeek Harness нет проектного
MCP-конфига: серверы монтируются патч-слоями дерева плагинов Cordis (YAML) —
`$DSH_HOME/cordis.patch.yml`, `$DSH_HOME/profiles/<профиль>/cordis.patch.yml` или
`--patch <файл>` в CLI. Поэтому init не мержит запись в чужой файл, а **генерирует
проектный оверлей целиком** — `<dir>/.dsh/memo.cordis.yml` (как `agentmemory.toml`:
источник истины — параметры прогона, `-force` не влияет), а подключить его к профилю
пользователь должен сам: `memo init` печатает (в конце вывода) готовую команду
`dsh web --patch …` и куда вставить блок для постоянного действия (Desktop-приложение
использует профиль `desktop`). Блок обрамлён маркерами `# >>> memo <каталог>` /
`# <<< memo <каталог>`: повторный init находит вставленную копию в patch-слоях
`$DSH_HOME` и обновляет её сам — сам блок init никуда не вставляет, подключение остаётся
решением пользователя. Подключить оверлей через `include` Cordis (одна постоянная строка,
содержимое в проекте) нельзя: плагины внутри include резолвятся от каталога файла, а не от
установки dsh — проверено, «failed to import». Пути в оверлее абсолютные: dsh запускает MCP-процесс с рабочим каталогом
процесса харнесса (у Desktop-приложения это `/`), поэтому `${CLAUDE_PROJECT_DIR}` и
относительный `agentmemory.toml`, как у Claude Code/opencode, здесь не разрешаются;
`-config` — каталог проекта (относительный `store.path` разрешается от него).
Скилы dsh читает из проектных корней `<dir>/.dsh/skills` и `<dir>/.agents/skills` —
init использует второй, общий корень конвенции agents (в проектах там уже лежат скилы, и
его читают другие приложения). id строк оверлея (`memo`, `memo-hooks`) у всех проектов
одинаковы, поэтому в один профиль подключается оверлей одного проекта: загрузчик
оставляет последнюю строку с тем же id, и оверлеи разных проектов молча перекрывают друг
друга (проверено: `--patch` второго проекта переключал сессию первого на чужую память) —
об этом init предупреждает в выводе. Шов в реестре харнессов один:
`agentTarget.scaffold` (у Claude Code и opencode — мерж JSON-записи, у dsh — генерация
оверлея и файла хуков).

При `initialize` сервер публикует server-level `instructions` (`ServerOptions.Instructions`)
— компактный клиент-агностичный протокол (когда звать recall, что такое атомарный факт,
дефолты save/recall, выведенные из `[scope]`), собираемый `internal/mcp.buildInstructions`.
Так базовый протокол доезжает до любого MCP-клиента (не только Claude Code) без Claude-скила;
богатый проектный протокол по-прежнему живёт в скиле `/memo`.

---

## 13. Конфигурация (TOML + env)

```toml
[store]
driver = "sqlite"                          # sqlite (дефолт) | postgres
path = "~/.agentmemory/memory.db"          # driver = "sqlite"
# Поля ниже — альтернатива path, плоско в той же секции [store] (не отдельная
# [store.postgres] — так их читает internal/config.StoreConfig); используются
# только при driver = "postgres", пароль НЕ здесь (см. ниже):
# host     = "localhost"
# port     = 5432
# user     = "memo"
# dbname   = "agentmemory"
# sslmode  = "disable"

[embedder]
provider = "ollama"                       # noop | ollama | openai
base_url = "http://localhost:11434"
model    = "bge-m3"                        # любая ollama-модель; dim определяется автоматически

[reasoner]
provider = "deepseek"                      # noop | deepseek | cliproxy (§10); ключ в env DEEPSEEK_API_KEY
model    = "deepseek-chat"                 # base_url — опционально, пусто = дефолт провайдера

[recall]
default_token_budget = 800
weights = { relevance = 1.0, importance = 0.5, recency = 0.4, access = 0.2 }
debt_weight = 0.6                          # понижающий множитель скора kind=debt, (0,1]; 0/не задано = дефолт 0.6

[maint]
decay_interval   = "24h"
consolidate_after = "168h"                 # неделя episodic → semantic

[scope]
project = "myproject"                      # → "project:myproject"; пусто = мультипроектность выключена
shared  = ["user:*"]                       # пространства, всегда добавляемые к recall по умолчанию
```

**`.env`.** Ключи `DEEPSEEK_API_KEY`/`CLIPROXY_API_KEY` (reasoner), `OPENAI_API_KEY` (embedder, §9) и
`MEMO_POSTGRES_PASSWORD` (`[store] driver = "postgres"`) — секреты, в TOML-конфиг никогда
не пишутся; для PostgreSQL в TOML остаются только несекретные параметры подключения
(host/port/user/dbname/sslmode), пароль читается исключительно из env — то же правило,
что уже действует для ключей провайдеров. Агентские приложения (в т.ч. Claude Code) не пробрасывают
`.env` в env MCP-сервера автоматически, поэтому `memo` сам загружает `.env`
(`KEY=VALUE`, `#`-комментарии, опциональные кавычки) из каталога своего `-config` при
старте, до создания reasoner'а/embedder'а — но только для переменных, ещё не заданных в
shell-env (shell-env приоритетнее). Отсутствие `.env` — не ошибка. Файл раскладывается
(`0600`, права только для владельца) и добавляется в `.gitignore` командой `memo init`
(§14 Этап 5, задача 5.4; выбор провайдеров — задача 11.1), но **только с ключами
провайдеров, реально выбранных в этом прогоне init** — `DEEPSEEK_API_KEY`, если
`reasoner=deepseek` и ключ введён (`CLIPROXY_API_KEY` — при `reasoner=cliproxy`); `OPENAI_API_KEY`, если `embedder=openai` и ключ введён.
Аналогично `.mcp.json`/opencode-конфиг получают в `env` плейсхолдер только выбранного
провайдера, не оба сразу — и оверлей dsh в том же правиле, но своим синтаксисом
(см. ниже).

**Настройки синхронизации (Этап 19) — в локальной БД, не в TOML.** Цели, тумблеры, имя
разработчика и интервал лежат в таблицах `sync_*` (§4): их меняет морда, а читают все
процессы `memo` этой машины — у каждого проекта свой `agentmemory.toml`, а локальная БД
общая. TOML-секции `[sync]` нет. Пароль общей базы проекта хранится в **`.env` каталога
локальной БД** (`dirname([store] path)`, по умолчанию `~/.agentmemory/.env`; права
`0600`, каталог `0700`), а не в проектном `.env` рядом с `agentmemory.toml`: база одна на
машину, проектов много. Имя переменной — `MEMO_SYNC_PASSWORD_<PROJECT>`, где `<PROJECT>` —
имя проекта в верхнем регистре с заменой всего, кроме `A-Z0-9`, на `_`; совпадение
нормализованных имён двух проектов разрешается суффиксом `_2`, `_3`…, а итоговое имя
хранится в `sync_targets.password_env`. Морда пишет файл слиянием строк через временный
файл и `rename`, другие ключи не трогает. Адаптер общей базы читает пароль **из этого
файла** при каждом прогоне — смена пароля в морде действует без перезапуска; переменная
окружения процесса с тем же именем используется, только если в файле её нет. Порядок
обратный `loadDotEnv` намеренно: `loadDotEnv` на старте вливает в окружение `.env` из
каталога конфига, и если это тот же файл, приоритет окружения заморозил бы старый пароль
до перезапуска. Каталог локальной БД по умолчанию вне репозитория; если `[store] path`
указывает внутрь репозитория, пароль окажется в его `.env` — морда предупреждает об этом
при сохранении пароля (`.env` должен быть в `.gitignore`). `DEEPSEEK_API_KEY` и прочие
ключи этот файл не затрагивает.

**Плейсхолдеры ключей в оверлее dsh (у них другая логика, чем у остальных харнессов).**
Запись сервера в `.dsh/memo.cordis.yml` получает `env` с выражением dsh
`DEEPSEEK_API_KEY: !!js process.env.DEEPSEEK_API_KEY ?? ''` (`?? ''` — чтобы
незаданная переменная дала пустую строку, которую `loadDotEnv` перекрывает значением из
`.env`; той же гарантией пользуется `${VAR:-}` у Claude Code). В отличие от Claude Code и
opencode, плейсхолдер здесь **работает**, а не просто безвреден: dsh вычищает из
окружения MCP-процесса переменные вида `*KEY*`/`*PASSWORD*`/`*SECRET*`/`*TOKEN*` и
`DSH_*`, поэтому экспортированный в шелле ключ доезжает до `memo` только через явный
`env` в записи сервера (выражение вычисляется в процессе харнесса). Пароль PostgreSQL
не добавляется и здесь — то же правило, что у остальных харнессов.

**Пароль PostgreSQL и MCP-конфиги (задача 12.1).** В отличие от ключей провайдеров,
`MEMO_POSTGRES_PASSWORD` намеренно не попадает в `env` `.mcp.json` и в `environment`
`opencode.json` — плейсхолдера для него `memo init` не генерирует. Решение принято по
итогам проверки на живых харнессах (Claude Code 2.1.191, opencode 1.17.18), а не из
осторожности: добавить было бы безопасно. `${VAR:-}` у Claude Code и `{env:VAR}` у
opencode при незаданной переменной дают пустую строку (у Claude Code это верно только
для формы с дефолтом: `${VAR}` без `:-` подставляется литералом — по докам «uses the
unexpanded `${VAR}` text as-is»), а пустое значение `loadDotEnv` штатно перекрывает
значением из `.env`. Плейсхолдер, однако, не даёт ничего: оба харнесса передают
MCP-серверу полностью унаследованное окружение, а `env`/`environment` лишь дополняют
его, — поэтому экспортированный в шелле пароль доезжает до `memo` и без упоминания
переменной в конфиге. Итого пароль БД доезжает ровно двумя путями: (1)
`MEMO_POSTGRES_PASSWORD`, экспортированная в окружении, откуда запущено агентское
приложение (наследуется процессом MCP-сервера), либо (2) `<dir>/.env` рядом с
`agentmemory.toml`, который `memo` читает сам при старте (`loadDotEnv`, приоритет — у
непустого shell-env). Секрет не просачивается в коммитимый `.mcp.json`/`opencode.json`
даже в виде имени переменной, а хрупкая ручная правка `${MEMO_POSTGRES_PASSWORD:-}` →
`${MEMO_POSTGRES_PASSWORD}` (непустой литерал победил бы `.env`) исключена по
построению. Отсутствие пароля при `driver = "postgres"` диагностируется в рантайме
предупреждением `resolvePostgresPassword`.

**`memo init` — выбор провайдеров (задача 11.1).** Интерактивный (TTY) прогон — select
стрелками: embedder `ollama` (дефолт, база + реально установленные модели через
`GET /api/tags`) / `openai` (токен → `GET /v1/models`, фильтр по `embedding` в id) / `noop`;
reasoner `deepseek` (дефолт) / `cliproxy` (base URL → ключ → `GET {base_url}/models`) / `noop`. Для `ollama` визард диагностирует окружение и
различает 4 состояния: happy (сервер отвечает, есть распознанная embedding-модель) → select
только из embedding-моделей; no-models / not-running / not-installed → печатает причину,
рекомендации моделей с обоснованием и ОС-специфичные инструкции установки/запуска, затем меню
из 2 пунктов — «Готово, продолжаем» (пере-проба) или «Выбрать другую настройку» (возврат к
выбору провайдера эмбеддинга); ручного ввода имени модели в ollama-ветке нет, эскейп для
неинтерактивных случаев — флаг `-embedder-model` или `-no-input`. Для `openai` поведение
прежнее: недоступность сервера моделей не блокирует установку — причина печатается, откат на
ручной ввод имени модели (асимметрия осознанная: ollama локальна и диагностируема — бинарь,
сервер, модели, — openai — удалённый сервис). `-no-input` — тот же результат детерминированно
из флагов/дефолтов, без сетевых запросов к провайдерам (единственное исключение — проба
хранилища при `-store-driver=postgres`, см. ниже). Сгенерированный `agentmemory.toml` содержит
только секцию под выбранный провайдер (см. пример ниже — поддерживаются и `noop`-варианты для
обеих секций, см. `config.example.toml`).

**`memo init` — выбор бэкенда хранилища.** Тем же select'ом выбирается `[store] driver`:
`sqlite` (дефолт — дальше прежний вопрос про путь к файлу; секция `[store]` при этом
генерируется без строки `driver`, то есть дефолтный вывод `memo init` не изменился) или
`postgres` (§14 Этап 12) — тогда спрашиваются host/port/user/dbname/sslmode и пароль
(скрытый ввод); флаги — `-store-driver`, `-postgres-host`, `-postgres-port`,
`-postgres-user`, `-postgres-dbname`, `-postgres-sslmode`, `-postgres-password`, а `-store`
остаётся путём к файлу SQLite (и, заданный без `-store-driver`, сам означает sqlite — про
бэкенд тогда не спрашивают). Введённое подключение проверяется пробой **до** записи файлов
и до создания каталога, но после всех дешёвых локальных проверок; проба принципиально
read-only (`store.OpenPostgresReadOnlyContext` — соединение + `current_schema()` + `Health`:
`SELECT 1`, `SHOW server_version`, `pg_is_in_recovery`; тот же путь, что у `memo doctor`,
§15) и ограничена таймаутом, чтобы недоступный хост не подвешивал установку — тот же
принцип, что у запроса списка моделей. Той же пробой дополнительно, тем же соединением,
проверяется готовность базы (задача 12.1): резолвится ли тип `vector` по search_path этого
соединения (`to_regtype('vector') IS NOT NULL` — не через каталог `pg_extension`, который
фиксирует лишь, что расширение установлено где-то в БД, а не что оно видно по search_path
именно этого соединения) и право роли на `CREATE` в схеме, зафиксированной при открытии
соединения (`has_schema_privilege($1::text, 'CREATE')`, параметризованная форма, а не
литеральный вызов `current_schema()` внутри запроса) — тоже только `SELECT`, без DDL;
отсутствие любого из двух не проваливает пробу, а печатает `warning: ...` с объяснением
последствий (векторный слой не поднимется при первом `ResetVectorIndex` / первый штатный
старт `memo` не накатит схему) — деградация, а не отказ, база может готовиться
администратором отдельно. Установка памяти в проект не имеет права накатить
прод-схему в общую командную базу, тем более при опечатке в имени базы. Неудача пробы в
интерактивном прогоне — причина в вывод и меню из 2 пунктов («ввести параметры заново», с
сохранением уже введённых значений как дефолтов / «выбрать другую настройку» — возврат к
select бэкенда), тот же паттерн, что у ollama-ветки; при `-no-input` и вообще без TTY —
ошибка `memo init`, а не молча сгенерированный нерабочий конфиг. Пароль в TOML не пишется
никогда — только в `.env` (`MEMO_POSTGRES_PASSWORD`, мержем к уже существующим ключам, как
ключи провайдеров); пустой пароль — не ошибка (trust-аутентификация), `.env` из-за него не
создаётся, но строка `.env` в `.gitignore` при `postgres` появляется всегда. В отличие от
`-openai-key`, одноимённая env-переменная в init НЕ подхватывается: записывать секрет на
диск init должен по явному действию пользователя (флаг или ввод в промпте), а не потому,
что переменная оказалась в окружении — иначе пользователь не видит, что и откуда было
сохранено. Повторные прогоны от этого не страдают: для пробы пароль восстанавливается из
уже лежащего рядом `<dir>/.env` (файл проекта, а не окружение).

---

## 14. Этапы разработки (roadmap)

**Этап 1 — MVP (без эмбеддингов).**
SQLite + FTS5 + MCP (`save`/`recall`/`forget`/`update`), `Embedder=noop`,
ранжирование по importance+recency, Markdown-бриф. Уже полностью рабочая память.

**Этап 2 — Reasoner.**
Подключаем DeepSeek: суммаризация recall-выдачи, консолидация episodic→semantic.

**Этап 3 — Embedder + гибрид.**
Конфигурируемый ollama-Embedder (дефолт `bge-m3`), `sqlite-vec`, RRF, автоопределение dim,
reindex при смене модели. Схема к этому уже готова.

**Этап 4 — Maintenance полностью.**
Decay, dedup-sweep, планировщик джобов.

**Этап 5 — Самоприменение (dogfooding).**
Переезд разработки проекта с файловой памяти (`.claude/memory/`) на собственный
`memo` через MCP. Сюда же — `memo init`: интерактивный установщик памяти в
произвольный проект под конкретное агентское приложение (Claude Code, opencode и
DeepSeek Harness — см. §12), генерирующий MCP-конфиг и два скила: «Протокол работы с памятью» (`memo` —
`memory_recall` в начале задачи, `memory_save`/`memory_update`/`memory_forget` по
ходу) и «Первичная инициализация памяти» (`memo-bootstrap` — разовая инструкция
изучить проект и наполнить память ключевыми фактами) — так любой проект, не только
сам Agent Memo, может подключить себе долговременную память в пару команд.

**Этап 6 — Web-морда (viewer + мягкое удаление).**
`memo web` — просмотр памяти в браузере: подкоманда поднимает HTTP-сервер поверх того же
общего файла БД (§12), JSON API на read-методах Store и статический фронтенд (задача
6.3) поверх него — дашборд статистики, постраничный список с фильтрами по scope/kind,
карточка записи с соседями по графу связей, лексический поиск. Единственная write-операция —
мягкое удаление записи (`DELETE /api/memories/{id}`, тумстоун через доменный `Writer.Forget`,
та же семантика, что у `memory_forget`).

**Этап 7 — Ambient memory (хуки харнесса).**
One-shot CLI-подкоманды `memo recall`/`memo save` (поверх store+recall/write, без
демона) — фундамент для хуков. `memo hook -event session-start|prompt-submit` — тонкая
обёртка над recall-путём с железным инвариантом «любая ошибка → пустой stdout, exit 0»
(хук не ломает сессию); `memo init` генерирует секцию `hooks` в `.claude/settings.json`
(merge, идемпотентно). Recall становится ambient — харнесс сам подмешивает бриф в контекст.

**Этап 8 — Качество recall.**
Explain-трасса ранжирования (`Query.Explain` → источники + разложение скора под записью
брифа, аддитивно). Фильтры recall по тегам (OR) и датам через общий `metaFilters` (оба
источника). Русская морфология в FTS5 (стемминг-колонка `content_stemmed`, snowball; введён
механизм миграций `PRAGMA user_version`). OpenAI-совместимый Embedder (`/v1/embeddings`,
любой совместимый сервер) за тем же интерфейсом.

**Этап 9 — Ops и модель данных.**
`memo doctor` — read-only диагностика реального режима памяти (7 секций, exit-код по
здоровью). `memo export`/`import` — бэкап/перенос в переносимый JSONL с сохранением
идентичности (round-trip байт-в-байт; векторы досчитает reindex). Типизированные связи в
MCP (`rel` в `links`, back-compat строка|объект) + `links` в `memory_update`. TTL
(`expires_at`): фильтр протухших в recall + джоб `ExpireSweep` (детерминированное забывание
по сроку, дополняет decay).

**Этап 10 — Web v2.**
Гибридный (семантический) поиск в морде (`/api/search?mode=semantic`, экстракция
`Recaller.Candidates`; эмбеддинг поискового запроса допустим — суженный оффлайн-инвариант,
деградация до FTS). Курирование метаданных из карточки (`PATCH /api/memories/{id}` —
importance/tags через доменный `Writer.Update`; content правит только агент через MCP).

**Этап 12 — Общая память команды (PostgreSQL-бэкенд).**
`Store` становится интерфейсом с двумя реализациями (§4, §6): SQLite (дефолт, поведение
не меняется) и PostgreSQL 18 + pgvector 0.8.5 — общая база на нескольких разработчиков,
выбор через `[store] driver` (§13), параметры подключения в TOML, пароль только из env
`MEMO_POSTGRES_PASSWORD`. Лексический слой — нативный `tsvector` с конфигурацией `russian`
(встроенный snowball-стемминг) + GIN-индекс, функциональный аналог FTS5 + `content_stemmed`
(задача 8.3), но стемминг делает сервер, а не Go-код. Векторный слой — pgvector: колонка
`vector(N)` + HNSW-индекс (`vector_cosine_ops`); в отличие от `sqlite-vec` умеет применять
внешние фильтры вместе с KNN, поэтому запасы `semanticNeighborsPad`/`vectorTagFilterPad`
(нужны в SQLite под ограничение «k-before-filter») в PostgreSQL не нужны. Версионирование
схемы — служебная таблица версии (аналог `PRAGMA user_version`); механизм пошаговых
миграций (`i → i+1`, своя транзакция на шаг) сохраняется концептуально в обеих реализациях.
Добавляется команда переноса данных между бэкендами (`sqlite ↔ postgres`); векторы при
переносе не копируются — они производные от content+summary+модели эмбеддера (контракт
`AllForExport`, §12) и досчитываются штатным `Reindexer`/`ListMissingVectors` (§11) после
переноса. Выбор PostgreSQL, а не MySQL: у MySQL 8.4 нет нативного векторного типа (появился
в 9.0, но без ANN-индекса в Community-редакции) — векторный поиск свёлся бы к линейному
скану с передачей всех векторов по сети; pgvector с HNSW снимает это ограничение, а
встроенный FTS даёт русскую морфологию из коробки.
**Известное ограничение, отложено отдельной задачей:** на общей PostgreSQL-базе scope
`user:*` физически общий для всех разработчиков команды — смешивает личные предпочтения
разных людей под одним пространством. Разделение на `user:<login>` в этот этап не входит.
Режим синхронизации (Этап 19) это ограничение обходит, а не снимает: `user:*` там
остаётся локальным и в общую базу не уходит; для `driver = "postgres"` оно в силе.
**Второе известное ограничение:** `config.StoreConfig` не имеет понятия «схема
PostgreSQL» — продакшн-код (`store.Open`/`store.OpenPostgresReadOnly`) резолвит схему
через `current_schema()` при подключении, то есть полагается на `search_path` сервера
(обычно `public`), и явно задать схему в конфиге сейчас нельзя. Для однобазового
командного сценария (§14 Этап 12) это осознанный текущий скоуп, но DDL-корректность
(таблицы/индексы/триггеры создаются именно в той схеме) от этого допущения зависит
напрямую — стоит зафиксировать перед тем, как заводить несколько логических инстансов
в одной PostgreSQL-базе через разные схемы.

**Этап 13 — Сущность техдолга.**
`kind = "debt"` — четвёртый тип памяти наравне с `semantic`/`episodic`/`procedural`
(§2, §4), не отдельная таблица и не отдельные MCP-инструменты; миграций схемы не
требует (`kind` — TEXT без CHECK, одинаково в SQLite и PostgreSQL). Открытый долг —
обычная живая запись; закрытие — существующий тумстоун-механизм
(`memory_forget(id, replaced_by=<id записи о фиксе>)`, либо без `replaced_by`, если
долг снят без фикса), поле статуса не заводится. Recall (§8): долг входит в
дефолтную выдачу, но выводится отдельной секцией брифа "Tech debt (debt)" (между
How-to и Recent context) и получает понижающий множитель итогового скора
(`[recall] debt_weight`, §13, дефолт `0.6`) — накопленные долги не вытесняют факты
из бюджета токенов; множитель виден в explain-трассе отдельным полем `KindFactor`.
Maintenance (§11): decay/consolidation по-прежнему касаются только `episodic` — долг
не затухает и не консолидируется; dedup-sweep группирует по `(scope, kind)` — долги
дедуплицируются только между собой. Поверхности итерации: MCP (`kind: "debt"` в
перечислении, конвенция содержания в server instructions и в генерируемых
`memo init` скиллах `memo`/`memo-bootstrap`), web-морда (бейдж kind `debt` на
записи; счётчик открытых долгов и фильтр по ним — через общий чип
`stats.byKind`, тот же, что у остальных kind, `superseded_by IS NULL`, т.е.
считаются именно открытые долги; отдельная плашка-счётчик не заводилась —
дублировала бы чип). **Отложено осознанно:** подкоманда
`memo debt` в CLI и отчёты «закрыто за период» — сущность уже полностью
работоспособна через существующие `memory_save`/`memory_recall`/`memory_forget` и
web-фильтр по kind, выделенная поверхность под неё не была нужна для этой итерации.

**Этап 14 — Задачи агента как тип памяти.**
`kind = "task"` — пятый тип памяти (§2, §4): агент ведёт задачи в памяти вместо
файлов TODO. В отличие от долга, задаче нужно промежуточное состояние, поэтому
здесь заводится колонка `status` (`todo`|`doing`|`blocked`|`done`, миграция схемы
`2 → 3` в SQLite, `ADD COLUMN IF NOT EXISTS` в PostgreSQL) — единственный `kind`
со статусом, у остальных поле NULL. `content` задачи — полный документ (что
сделать, границы, критерий приёмки), `summary` — обязательный заголовок: в бриф
recall (§8) выводится он, поэтому многокилобайтная задача не распирает бюджет
токенов; полный текст агент забирает новым инструментом `memory_get` (§12) —
пятым и единственным добавленным (`memory_recall` компактен по замыслу, и без
`memory_get` задачу-документ можно было бы найти, но не прочитать). Recall:
отдельная секция брифа "Tasks (task)" между How-to и Tech debt, строка со
статусом (`- [doing] Заголовок [★4]`), фильтр `statuses`, у которого пустое
значение означает «всё, кроме выполненных задач» — сделанное не вытесняет
актуальное, но остаётся в истории живой записью и достаётся явным
`statuses: ["done"]`. Write path (§7): задачи исключены из дедупа
(`write.SkipDedup`) — и на вставке, и в ретроактивном sweep (§11); отмена задачи —
обычный тумстоун `memory_forget`. Поверхности итерации: ядро, MCP (`kind`/`status`/
`statuses` в схемах, `memory_get`, server instructions, генерируемые скиллы),
web-морда (бейдж kind `task` + чип состояния, read-only), CLI-флаги
`-kinds`/`-kind`/`-status`/`-summary`/`-statuses`, формат export/import (аддитивное
поле `status`). **Отложено осознанно:** подкоманда `memo task` в CLI, смена статуса
из web-морды (write-инвариант морды: курирование метаданных, но не содержания),
специальный вес `kind=task` в ранжировании (множитель остаётся `1` — в отличие от
`debt`, задачи и должны конкурировать с фактами на равных).

**Этап 15 — Kanban-доска задач.**
Задача (`kind=task`, Этап 14) получает модель, достаточную для доски в web-морде
и для ведения работы агентами: стадии-колонки, тип (эпик/история/задача),
иерархию с фиксированным направлением, блокировку задач задачами, привязку к
git-ветке и к процессу (`workflow`). Все решения ниже согласованы с пользователем.

- **Стадии = колонки.** `status ∈ {backlog, todo, doing, review, testing,
  release, done}` в порядке жизненного цикла: «бэклог → к выполнению → в работе →
  ревью → тестирование → к релизу → готово (в релизе)». Очереди «готово к
  тестированию»/«тестируется» сознательно склеены в `testing`: при «один человек +
  агенты» очередь и активная стадия не различаются, WIP-лимитов нет. Дефолт при
  сохранении — по-прежнему `todo`; `backlog` — для сырых идей, которые агент
  заводит, а человек отбирает. Переходы **не ограничены** порядком (карточку можно
  вернуть назад) — единственное правило см. в «Блокировка». `done` — живая запись,
  скрытая из дефолтного recall (Этап 14), без изменений.
- **`blocked` — не стадия, а вычисляемое свойство.** Статус `blocked` из перечня
  **удалён** (миграция `3 → 4` переводит существующие задачи `blocked → todo`; в
  PostgreSQL — тот же идемпотентный `UPDATE`). Задача заблокирована, если у неё
  есть связь `rel=blocked_by` на задачу, которая **жива** (`superseded_by IS
  NULL`) и **не `done`**. Блокировка снимается сама: блокер закрыт (`done`) или
  отменён (тумстоун) — заблокированная карточка свободна, никаких «ждём решения
  человека». Направление — от заблокированной к блокеру (так, как думает
  пользователь: «на этой карточке — кем заблокирована»), одно каноническое
  значение `rel`, без зеркального `blocks`; обратный список («что блокирует эта
  задача») — `Store.Neighbors` с Links соседей (см. ниже; отдельный
  `IncomingLinks` не заводится). Правило write
  path: заблокированную задачу нельзя перевести дальше `todo` (в `doing` и далее)
  — `write.ErrTaskBlocked` с перечнем живых блокеров; `backlog`/`todo` разрешены.
  Правило — **валидация пост-состояния**, а не «перехода»: после применения
  всех полей `Save`/`Update` проверяют `status ∉ {backlog, todo} ∧ живые
  блокеры ≠ ∅` — иначе `memory_update(status: doing, links: [blocked_by X])` в
  одном вызове зависел бы от порядка веток, а `memory_save` со статусом и
  блокером не проверялся бы вовсе. Само правило «жив и не done» — одна чистая
  функция сущностного уровня `memory.LiveBlockers(task, byID)` без I/O; каждый
  потребитель (write, recall, board) лишь поставляет карту записей-блокеров:
  write — через `Store.Get` (их единицы), recall и board — батчем
  `Store.GetMany(ids)` по id, поэтому блокер из другого scope тоже учитывается
  (кросс-scope блокировка поддерживается). Обратный список («что блокирует эта
  задача») и дети эпика — из `Store.Neighbors`, который с этого этапа грузит
  Links соседей (направление и `rel` различимы без нового обратного запроса;
  `IncomingLinks` заводится, только если `Neighbors` станет дорогим).
  Снять блокировку вручную — удалить связь: `links` в `memory_update` остаются
  аддитивными (Этап 5.1), поэтому появляется симметричное поле `unlinks` (пара
  `id`+`rel`), общее для всех `rel`, а не только для `blocked_by`; удаление ребра
  существует в коде один раз — в `write.UpdateInput.Unlinks`. В брифе recall
  заблокированная задача помечается (`- [todo] ⛔ Заголовок (ждёт: <блокер>)`), в
  `memory_get` — секция блокеров и блокируемых.
- **Тип задачи — поле `task_type` (`epic|story|task`, дефолт `task`)**, а не тег:
  сейчас эпики помечены вразнобой (`epic`/`эпик`/`История` + «ЭПИК» в заголовке), и
  доска не может на это опираться. Миграция `3 → 4` делает разовый backfill:
  живая задача с тегом `epic`/`эпик` → `epic`, с тегом `story`/`История` →
  `story`, остальные → `task` (сами теги не трогаются). Иерархия — существующий
  `rel=part_of` с **зафиксированным направлением «дочь → родитель»** (задача
  `part_of` истории, история `part_of` эпика; глубина не ограничивается, но
  доска показывает ровно один уровень — ближайшего родителя). Ретроактивно
  перевёрнутые рёбра в старых данных не чинятся автоматически (направление из
  данных не выводится надёжно) — только конвенция для новых записей в
  инструкциях сервера/скиллах. **Эпик — обычная карточка** со своим хранимым
  статусом, ходит по колонкам как все (вычисляемый статус эпика отклонён:
  статус в recall/MCP должен быть хранимым, а не «иногда вычисляемым»); доска
  умеет группировать по эпику (свимлейны) и фильтровать по нему, карточка
  эпика несёт прогресс детей (`done/total`).
- **`branch` и `workflow` — свободные строки** без валидации, только у
  `kind=task`: `branch` — имя git-ветки (`feat/kanban-board`), `workflow` — имя
  процесса, которым задача выполняется (`dev`, `ultracode`, `code-review`, …),
  не id запуска (трассировка «кто и когда» — `source`/`content`, как и раньше).
  Конвенция для агентов: при переводе в `doing` проставить `branch` и
  `workflow`. Оба — фильтры доски; ссылка на PR не хранится (в `content`).
- **Хранение — три nullable-колонки `memories`** (`task_type`, `branch`,
  `workflow`), по образцу `status`: одна миграция на весь этап, а не JSON-колонка
  «атрибутов задачи» (терялись бы фильтры в SQL и единообразие с `status`) и не
  отдельная таблица `tasks` (одна сущность — одна таблица; задача остаётся
  памятью с тем же `id`/`links`/FTS). Миграция несёт **data-шаги** (`blocked →
  todo`, backfill `task_type` по тегам), поэтому оформляется версионно в обоих
  драйверах: SQLite — `migrations[3]` (`3 → 4`, `PRAGMA user_version`; каждый
  `ADD COLUMN` идемпотентен по отдельности); PostgreSQL — **первый шаг
  `pgMigrations[0]`** (`0 → 1`, таблица `schema_version`), а не `ADD COLUMN IF
  NOT EXISTS` в базовой схеме, которая накатывается на каждом Open: `UPDATE` по
  всей таблице при каждом старте недопустим даже идемпотентный. Нумерация
  версий SQLite (4) и PostgreSQL (1) намеренно не совпадает — у них разная
  история. Старый export со `status: "blocked"` при импорте маппится в `todo`
  (одно место, аддитивно, с предупреждением).
- **Доска — вариант использования, не хендлер.** Новый пакет
  `internal/memory/board`: `Build(ctx, Filter) → Board` (колонки в порядке
  `memory.Statuses` → карточки: задача + вычисленные `Blocked`/`BlockedBy`/
  `Parent`/`Progress`) — тестируется без HTTP; в `recall` не кладётся (CRP:
  `Recaller` тащит Embedder/Reasoner, доске нужен один read), в `web.go` — тоже
  (логика умрёт при втором потребителе: отложенные `memo task` и MCP «покажи
  доску» получают `board.Build` бесплатно). Пакет **не импортирует `store`**: он
  объявляет свой узкий порт `Source { ListTasks(ctx, scopes); GetMany(ctx, ids) }`,
  который `store` удовлетворяет структурно — так `board` даёт 0 в
  `TestDependencyRule`, а не наследует базовую линию с драйверами БД. Store
  получает специализированный read `ListTasks(ctx, scopes)` (все живые задачи
  scope с Links, без пагинации — по образцу `ListEpisodicForDecay`; `List` с его
  страницами и без Links не растягивается флагами) и `GetMany(ctx, ids)`.
  Web-морда (`GET /api/board`, расширенный `PATCH /api/memories/{id}`, фронт
  `#/board`) — тонкая доставка: PATCH — whitelist-DTO, отображаемый 1:1 в
  `write.UpdateInput` без собственной логики (никаких проверок блокировки или
  направления `part_of` в `web.go`), доменные ошибки маппятся `errors.Is` в
  HTTP (`ErrTaskBlocked` → 409, `ErrUnknownStatus`/`ErrUnknownTaskType`/
  `…NotApplicable` → 400), а не в 500. Колонка `done` на доске усечена по
  давности (`doneLimit`, «показать ещё»), остальные — целиком.
- **Поверхности этапа:** ядро (`types.go`, `write`, `recall`, `board`), оба
  драйвера `store`, MCP (`type`/`branch`/`workflow`/`unlinks`, enum `status`,
  `rel=blocked_by`, фильтр `types` в recall, секции `memory_get`, инструкции
  сервера), CLI-флаги, export/import (аддитивные поля), генерируемые `memo init`
  скиллы, web-морда (доска + карточка). **Отложено осознанно:** настраиваемые
  колонки на проект (фиксированный перечень достаточен, пересмотр — когда
  появится проект с другим процессом), WIP-лимиты, порядок карточек внутри
  колонки (сортировка — importance, затем `updated_at`), фильтр recall по
  `branch` для авто-recall хука на старте сессии (кандидат в Этап 7.x),
  автоматический разворот перевёрнутых `part_of` в старых данных, подкоманда
  `memo task` в CLI, история переходов статуса (нет `updated_at` по стадиям —
  только общий `updated_at`).

**Этап 16 — Новая веб-морда по принятому дизайну.**
Морда переписывается под спецификацию
[`design/memo-web-redesign.md`](design/memo-web-redesign.md) (макет —
`design/memo-web-prototype.html`, принят 2026-09-22): три раздела «Обзор ·
Память · Доска», один глобальный селект scope, панель записи вместо модалки,
клавиатурная альтернатива DnD, тёмная тема на токенах. Спецификация первична
по виду и поведению; здесь — только решения, меняющие контракт API и
инварианты. Порядок: сначала бэкенд (16.1), затем фронт (16.2 каркас → 16.3
обзор → 16.4 память → 16.5 запись → 16.6 доска) и ручная приёмка (16.7);
каждая задача — отдельная сессия.

- **Инвариант §12 «content/summary из браузера не редактируются» снят**
  (решение пользователя вместе с принятием макета: сценарий 3 ТЗ — «поправил
  текст, не покидая списка»; обходной путь через MCP ради опечатки дороже
  риска). `PATCH /api/memories/{id}` принимает `content`/`summary` → те же
  поля `write.UpdateInput` (пустой `summary` — снять, как в `memory_update`;
  пустой `content` → `ErrEmptyContent` → 400). Whitelist-DTO 1:1 остаётся:
  `kind`/`scope`, создание записей и hard DELETE из браузера по-прежнему
  недоступны. Следствие для оффлайн-инварианта: `Writer` в `memo web`
  собирается с **реальным** Embedder (тем же, что у `/api/search`), а не с
  Noop — иначе правка текста оставляла бы в БД устаревший вектор, а запись
  не попадала бы в очередь досчёта (`ListMissingVectors` видит только записи
  без строки в `memories_vec`). Инвариант теперь: **морда пишет в БД только
  через доменный `write.Writer`; эмбеддит поисковый запрос и запись при
  правке её текста** — ровно то, что делает MCP `memory_update`. При
  недоступном Embedder правка текста, как и в MCP, оставляет прежний вектор
  до следующего полного reindex — известное ограничение write path, не
  морды.
- **`GET /api/stats` → `providers` и `jobs`.** Обзор должен сообщать
  деградацию до первого поиска (сейчас плашка появляется только по
  `vector:false` из `/api/search`). Кто что знает: имена провайдеров и
  драйвера — composition root (`cmd/memo`: конфиг + фабрики
  `newEmbedder`/`newReasoner`), доступность Embedder — `Dim() > 0` у
  собранного экземпляра (статическая, на момент старта — как у флага
  `vector` в `/api/search`; падение ollama посреди работы обнаружится при
  следующем запуске), доступность Reasoner — `newReasoner` вернул не Noop
  (клиент DeepSeek при этом не используется: суммаризации в JSON API нет),
  очередь на эмбеддинг и версия схемы — Store. Ответ:
  `providers:{store:{driver, target, schema}, embedder:{provider, model, dim,
  available, pending}, reasoner:{provider, model, available}}`, `target` —
  `config.StoreConfig.Target()` (без пароля); `jobs:{lastRunAt|null}`.
- **Три расширения порта Store** (обе реализации, паритет sqlite/postgres):
  `Stats.PendingVectors` — живые записи без строки в векторном индексе (тот
  же предикат, что у `ListMissingVectors`; нет индекса — 0), считается в
  `Stats`, а не отдельным методом; `SchemaVersion(ctx)` — `PRAGMA
  user_version` в SQLite и `schema_version` в PostgreSQL (у драйверов разная
  нумерация, §14 Этап 15 — это подпись, не признак совместимости);
  `LastMaintRun`/`SetLastMaintRun` — время последнего завершённого прогона
  планировщика в однострочной таблице `maint_meta` (по образцу `embed_meta`,
  чистый `CREATE TABLE IF NOT EXISTS` в базовой схеме обоих драйверов, без
  шага миграции — data-шагов нет). Пишет её `maint.Scheduler` в конце
  `RunOnce` через свой узкий порт (интерфейс объявляет потребитель); читает
  `memo web` — планировщик живёт в процессе MCP-сервера, морда его не
  запускает, поэтому без персистентности «последний прогон» не узнать.
  Записывается один момент на весь прогон, не по джобам: обзору нужна одна
  строка «фоновые джобы — последний прогон N часов назад».
- **`expiresBefore=<unix>` в `GET /api/memories`** → `BrowseFilter.ExpiresBefore`
  (`expires_at IS NOT NULL AND expires_at > 0 AND expires_at < ?`); при
  заданном фильтре сортировка — `expires_at ASC` (ближайшие первыми), иначе
  прежняя `created_at DESC`: секция «Истекает скоро» иначе усекалась бы по
  давности создания, а не по сроку. Отдельного `ListExpired` морда не
  использует — у него нет scope.
- **Фронт: стек прежний** (vanilla JS + Tailwind, бандл в бинаре, hash-роутер);
  `index.html`/`app.js` переписываются, не дописываются; доменной логики на
  фронте нет (правило блокировки, направление `part_of`, усечение `done` —
  бэкенд). Токены `:root` макета → `tailwind.input.css`; тексты — §5
  спецификации.
- **Отложено осознанно:** динамическая проверка доступности Embedder
  (health-пинг ollama по запросу `/api/stats`) — статической достаточно,
  пересмотр, если деградация посреди сеанса станет частой; время последнего
  прогона по каждому джобу отдельно; тесты фронта (проверка ручная, §9
  спецификации).

**Этап 17 — Создание задач, историй и эпиков с доски.**
Доска умела всё, кроме единственного действия, ради которого её открывают чаще
всего, — завести карточку; задачи приходилось заводить агентом через
`memory_save`. Спецификация вида и поведения — `design/memo-web-redesign.md`
§2.4/§2.6, кликабельный образец — `design/memo-web-prototype.html`. Порядок:
бэкенд (17.1) → форма и точки входа (17.2) → единый вид контролов (17.3).

- **Инвариант §12 «создания записей из браузера нет» снят для `kind=task`**
  (решение пользователя вместе с принятием макета). Границу держит не домен, а
  морда: `Writer.Save` обязан уметь писать любой вид — его зовёт MCP
  `memory_save`; `POST /api/memories` отвергает всё, кроме `kind=task`, и всё,
  кроме `rel=part_of` в связях. Почему только задача: у морды нет ни контекста
  для атомарного факта, ни дедупа в интерфейсе, а карточка доски — ровно та
  запись, которую заводят руками.
- **Пустое описание подставляется заголовком на границе морды**, а не в домене:
  `ErrEmptyContent` остаётся ошибкой для `memory_save` (факт без содержания
  бессмыслен), но быстрая карточка «заголовок и всё» — нормальный способ завести
  задачу с доски.
- **Форма создания — панель задачи в режиме правки**, а не отдельный интерфейс,
  и **не перерисовывается целиком**: смена типа/приоритета/тегов правит только
  затронутые узлы. Полная перерисовка даёт мигание (заново играет анимация
  появления панели) и роняет каретку — требование спецификации, а не деталь
  макета.
- **Отложено осознанно:** создание не-задач и правка `kind`/`scope` из браузера;
  hard DELETE; дедуп задач; тесты фронта (проверка ручная, §9 спецификации).

**Этап 18 — DeepSeek Harness как третье агентское приложение.**
Пользователь работает в DeepSeek Harness (`dsh`, Desktop-приложение
`DeepSeek Harness.app`), и память туда не устанавливалась: `memo init` знал только
Claude Code и opencode. Добавлен `-agent dsh` — реестр `agentRegistry` получил третью
запись, а пара полей «путь + JSON-мерж» уступила место одному шву
`agentTarget.scaffold` (у claude/opencode он мержит JSON-запись, у dsh — генерирует
проектный оверлей Cordis и файл хуков). Механика и отличия — §12; проверялось на живом
харнессе: оверлей композируется (`--dump-config`), `memo` поднимается как MCP-сервер с
абсолютным `-config`, его пять инструментов и server instructions доезжают до модели,
скилы `memo`/`memo-bootstrap` попадают в каталог сессии, авто-recall из хуков — в первый
же запрос к модели.

- **Один ручной шаг вместо автоматизации**: у dsh нет проектного MCP-конфига, поэтому
  `memo init` пишет оверлей в проект и печатает, как его подключить (`dsh web --patch …`
  либо строки в patch-слой профиля). Автовмерж в `$DSH_HOME` (флаг с путём к patch-слою)
  отклонён: init писал бы за пределами `-dir` и в файл, который ведёт сам пользователь, —
  цену этого шага платит один раз на проект.
- **Абсолютные пути в артефактах dsh** — следствие того, что MCP-процесс наследует
  рабочий каталог процесса харнесса (у Desktop-приложения это `/`), а не проекта:
  `${CLAUDE_PROJECT_DIR}` и относительный `agentmemory.toml` там не разрешаются. Файлы
  поэтому машинозависимые; в git их кладут осознанно (как `.mcp.json`).
- **Скилы — в `.agents/skills`** (общий корень конвенции agents, dsh читает его как
  проектный корень ранга 200), а не в dsh-специфичный `.dsh/skills` (ранг 100): рядом со
  скилами, которые уже лежат в проектах пользователя, и их видят другие приложения.
- **Хуки — через мост `dsh-hooks-claude-code`** (своих у dsh нет), что дало два отличия
  команд: абсолютный `-config` и новый флаг `memo hook -format json` (мост читает контекст
  только из `hookSpecificOutput.additionalContext`, обычный stdout игнорирует). Дефолт
  `text` оставлен прежним — поведение Claude Code не менялось; конфиг хуков dsh
  генерируется целиком, а не мержится, иначе сужение `-hooks` не выключало бы событие.
- **Попутно исправлен дефект скилов**, найденный на живом харнессе: `description` не
  кавычился, а двоеточие в тексте bootstrap-скила делало YAML-фронтматтер невалидным —
  харнесс молча выбрасывал `memo-bootstrap` из каталога (у Claude Code и opencode
  поведение не менялось: кавычки — валидный YAML в обоих).
- **Отложено осознанно:** автомерж оверлея в patch-слои dsh (флаг); нативный конфиг хуков
  dsh вместо моста Claude Code; per-workspace MCP-подключение (dsh монтирует строки
  статически, поэтому в один профиль — оверлей одного проекта).

**Этап 19 — Синхронизация проектов с общей PostgreSQL-базой команды.**
Третий режим хранилища (§2.1): у каждого разработчика своя SQLite, а наборы выбранных
проектов синхронизируются в обе стороны с общей PostgreSQL — своя база на проект.
Требования согласованы с пользователем 2026-10-28 (эпик в memo); механика — §4.1, §6,
§7.1, §11, §12, §13. Порядок: 19.0 (этот раздел) → 19.1 `author`/`updated_by`, 19.2 схема
общей базы → 19.3 локальное состояние, 19.4 интерактор → 19.5 адаптеры → 19.6 фон,
19.7 CLI и `doctor`, 19.8 web API → 19.9 экран → 19.10 приёмка e2e; каждая задача —
отдельная сессия.

- **Состав набора и LWW.** `project:X` × {`semantic`, `procedural`, `debt`, `task`} вместе
  с тумстоунами, тегами и связями внутри набора; `episodic`, `user:*`, векторы,
  `accessed_at`/`access_count` — локальные. Конфликт — last-write-wins по `updated_at`,
  запись целиком, ничья — в пользу общей базы; счётчик конфликтов в статусе.
  Расхождение часов машин — **принятый риск**: правка с отстающих часов проигрывает.
- **Курсор pull — горизонт снимка (`xid8`, `pg_snapshot_xmin`), а не последний номер**
  (§4.1): последовательность или время теряют строки параллельных транзакций, горизонт
  снимка — нет, ценой идемпотентного перечитывания окна. Между страницами курсор —
  пара `(xid, id)`, поэтому прогресс не теряется на таймауте. Восстановление общей базы
  из копии — явная ротация `base_id`, а не эвристика.
- **`updated_at` строго растёт на правке** (`max(now, прежний + 1)`, §4): иначе вторая
  правка в ту же секунду проигрывала бы ничью собственной отправленной версии.
- **Номер изменения ставят триггеры сервера**, поэтому прямые писатели
  (`driver = "postgres"`, `memo web` на общей базе) видны синхронизации так же, как
  синхронизация — им.
- **Неотправленное — триггеры SQLite (`sync_dirty`)**, а не отметка «отправленного
  `updated_at`» по записи: триггер ловит любой путь записи, `seq` (`AUTOINCREMENT`)
  защищает правку, сделанную во время отправки, и не нужно сравнивать версии всех
  записей проекта. Push — ограниченными пачками, каждая со своим коммитом.
- **FK при применении:** адаптер пишет записи пачки, затем связи; адресаты из дальних
  страниц приходят в `Refs` и применяются без своих связей, — иначе первичный pull
  иерархии задач больше одной страницы вставал бы на FK навсегда.
- **Автор проставляется только при первой вставке записи в общую базу**, а не всем
  записям без автора: штамп уже существующей общей записи давал бы вечную ничью.
- **Оптимистичная блокировка на обеих сторонах** (CAS по `change_xid` / `updated_at`+`seq`)
  вместо длинных транзакций с `SELECT … FOR UPDATE`: LWW решает интерактор без БД, а
  проигравшая гонку запись просто уходит в следующий прогон.
- **Пакет `internal/memory/sync` со своими портами** (`Local`, `Shared`, `Dialer`, §6),
  `store` не импортирует — базовая линия `TestDependencyRule` **0**, как у `board`.
  Синхронизация не идёт через `Store`: ей нужны мимо-дедупное применение, пометки и CAS,
  которые остальным вариантам использования не нужны и не должны быть доступны.
- **Схема общей базы — только явной операцией** (кнопка с подтверждением,
  `memo sync -apply-schema`); открытие для синхронизации DDL не делает. Версия ≠
  `pgSchemaVersion` бинаря — синхронизация стоит с понятной причиной.
- **Настройки — в локальной БД, пароль — в `.env` каталога локальной БД** под именем на
  проект (§13); API пароль не возвращает.
- **Отложено осознанно:** синхронизация `user:*` и `episodic` (разделение `user:<login>`
  и консолидация на общей стороне — отдельный замысел); полевой merge вместо записи
  целиком (пересмотр — если LWW начнёт терять правки разных полей одной задачи);
  гибридные логические часы против расхождения часов (пересмотр — при первом реальном
  случае проигрыша из-за часов); векторы в общей базе и их синхронизация (досчёт локально
  дешевле переноса и не зависит от модели эмбеддера у разных людей); автомиграция общей
  базы; несколько общих баз на один проект и один общий сервер на все проекты с
  разделением по scope; удаление скачанных данных при выключении; синхронизация поверх
  `driver = "postgres"`; синхронизация смены `scope`/`kind` записи (запись, ушедшая из
  набора, остаётся в общей базе прежней версией); интервал на каждую цель отдельно.

---

## 15. Принципы

- **Всё за интерфейсами** (`Store`, `Embedder`, `Reasoner`) — бэкенды сменные, тесты простые.
- **Деградация, а не падение** — недоступность вектора/LLM понижает качество, но не ломает recall.
- **Никаких тихих обрезаний** — бриф всегда сообщает про свёрнутый хвост.
- **Память забывает** — без устаревания и консолидации тонет в шуме.
- **Атомарность записи** — одна запись = один факт; отсюда отсутствие чанкинга.
