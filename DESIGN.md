# AgentMemory — Архитектура

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
- Ноль тяжёлых внешних зависимостей на старте (embedded SQLite, локальные модели).

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

`scope` (namespace) разделяет `user:*`, `project:<name>`, `agent:self` даже для одного
агента. Recall всегда фильтруется по scope.

### 2.1 Мультипроектность (модель scope)

Один агент работает с несколькими проектами из **одного общего файла БД**
(`~/.agentmemory/memory.db`); разделение — через scope, физического дробления нет.
Модель: **изоляция проектов + общий пользовательский слой**.

- **Идентичность инстанса.** Каждый проект запускает свой `memoryd` (через `.mcp.json`
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
- **Конкурентность.** Несколько инстансов пишут в один файл — БД открыта в `WAL` +
  `busy_timeout`; читатели параллельны, писатель сериализуется.

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
│  Store (SQLite): metadata + FTS5 + vec │
└──────────────────────────────────────┘
        ▲                     ▲
        │ Embedder            │ Reasoner
   (ollama/bge-m3, opt.)  (DeepSeek chat)
```

- **Writer** — приём и структурирование записи: дедуп, классификация, связывание.
- **Recall** — гибридный поиск (FTS5 + vec) → ранжирование → сборка Markdown-брифа.
- **Maint** — фоновые джобы: устаревание, консолидация, переиндексация.
- **Store** — единый SQLite-файл: метаданные + лексический индекс (FTS5) + вектор (`sqlite-vec`).
- **Embedder** — опциональный семантический вектор (локальный ollama). Деградирует до FTS5.
- **Reasoner** — DeepSeek chat для суммаризации выдачи и консолидации (LLM-операции).

---

## 4. Модель данных (SQLite)

```sql
-- Атомарная единица памяти
CREATE TABLE memories (
    id            TEXT PRIMARY KEY,          -- ULID (сортируемый по времени)
    kind          TEXT NOT NULL,             -- semantic | episodic | procedural
    scope         TEXT NOT NULL,             -- user:* | project:<name> | agent:self
    content       TEXT NOT NULL,             -- сам факт (1-3 предложения, атомарно)
    summary       TEXT,                      -- ультра-короткая форма для брифа
    importance    INTEGER NOT NULL DEFAULT 3,-- 1..5, задаёт агент
    created_at    INTEGER NOT NULL,          -- unix ts
    updated_at    INTEGER NOT NULL,
    accessed_at   INTEGER,                   -- для decay/усиления
    access_count  INTEGER NOT NULL DEFAULT 0,
    superseded_by TEXT,                      -- id заменившей записи (мягкое забывание)
    source        TEXT                       -- session id / откуда пришло
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
    rel TEXT NOT NULL DEFAULT 'related',     -- related | supersedes | causes | part_of
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
```

Ключевые решения:
- **ULID** как id — сортируемость по времени + отсутствие координации.
- **`superseded_by`** вместо DELETE — «мягкое забывание»: старый факт вне recall, история цела.
- **`importance` + `accessed_at` + `access_count`** — три сигнала для ранжирования.
- **vec-таблица отдельная** — вектор добавляется/переиндексируется без миграции `memories`.

---

## 5. Компоненты и пакеты

```
/cmd/memoryd            — точка входа: MCP-сервер (stdio); подкоманда `init` —
                          интерактивный установщик памяти в проект под конкретное
                          агентское приложение (MCP-конфиг, скил-инструкция, .env)
/internal/memory
    types.go            — Memory, Query, Brief, Candidate
    store/              — интерфейс Store + реализация SQLite (FTS5 + vec)
    embed/              — интерфейс Embedder (noop | ollama | openai)
    reason/             — интерфейс Reasoner (DeepSeek)
    write/              — ingest: дедуп, классификация, связывание
    recall/             — гибридный поиск (RRF) + ранжирование + сборка брифа
    maint/              — джобы: decay, consolidation, reindex, dedup-sweep
/internal/mcp           — регистрация tools, JSON-схемы, роутинг
/internal/config        — загрузка конфига (TOML/env)
```

---

## 6. Интерфейсы (Go)

```go
type Store interface {
    Put(ctx context.Context, m Memory) error
    Get(ctx context.Context, id string) (Memory, error)
    SearchLexical(ctx context.Context, q Query) ([]Candidate, error)
    SearchVector(ctx context.Context, vec []float32, q Query) ([]Candidate, error) // no-op пока нет Embedder
    Neighbors(ctx context.Context, id string) ([]Memory, error)                    // граф-обход
    Supersede(ctx context.Context, oldID, newID string) error
    Touch(ctx context.Context, id string) error                                    // accessed_at++, access_count++
    // maintenance-выборки (§11); Tags/Links не заполняются
    ListEpisodicForConsolidation(ctx context.Context, olderThan int64, limit int) ([]Memory, error)
    ListEpisodicForDecay(ctx context.Context, notAccessedSince int64, limit int) ([]Memory, error)
    // reindex-поддержка
    EmbedMeta(ctx context.Context) (model string, dim int, err error)
    ResetVectorIndex(ctx context.Context, model string, dim int) error
    AllForReindex(ctx context.Context) ([]Memory, error)
    PutVector(ctx context.Context, id string, vec []float32) error
}

type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Model() string
    Dim() int
}

type Reasoner interface {
    Complete(ctx context.Context, prompt string) (string, error) // DeepSeek chat
}
```

Всё за интерфейсами → бэкенды меняются конфигом; старт возможен с `Embedder=noop`,
`Reasoner=DeepSeek`.

---

## 7. Write path — как записывать «умно»

Наивное `save(text)` → эмбеддинг → в базу деградирует память. Правильно:

1. **Приём структурированного входа.** Агент передаёт уже атомарный факт с полями
   (`kind`, `content`, `importance`, `tags`, `scope`, опц. `links`). Классификацию делает
   агент — дёшево и надёжно.
2. **Дедупликация / обновление.** Перед записью ищем похожие в том же `scope`
   (лексически + SimHash по content). Если дубль — усиливаем/обновляем; если факт
   противоречит существующему — ставим `superseded_by`; иначе вставляем.
3. **Связывание.** Проставляем `links` (граф), в т.ч. висячие ссылки на ещё не созданные id.
4. **Индексация.** FTS5 всегда; вектор — если `Embedder != noop` (иначе откладывается в
   очередь на досчёт фоновым джобом).

---

## 8. Read path (recall) — «ровно сколько нужно»

Сердце системы. Recall **не отдаёт top-k сырьём**:

1. **Кандидаты.** Hybrid retrieval: FTS5 (лексика/имена/ключи) + vec (смысл, если включён)
   + фильтры `scope`/`kind`/свежесть. Лексический слой снимает русские падежные/числовые
   окончания стеммингом (доп. колонка `content_stemmed`, задача 8.3) — находит словоформы
   одной лексемы («проекта»/«проектом» ↔ «проект»), но не разные лексемы-синонимы
   («собирать»/«сборка», «компилировать»/«сборка») — для этого остаётся вектор.
2. **Слияние.** Reciprocal Rank Fusion (RRF) по рангам (k≈60) — устойчиво, без калибровки шкал:
   `score(doc) = Σ 1/(k + rank_i(doc))` по источникам.
3. **Ранжирование.** После RRF накладываем бизнес-сигналы:
   `final = w1·relevance + w2·importance + w3·recency_decay + w4·log(1+access_count)`.
4. **Сборка брифа.** Режем под `token_budget`, группируем по `kind`, важное — первым,
   честно указываем «свёрнуто ещё N».

**Формат выдачи — Markdown-бриф**, а не JSON-простыня:

```markdown
## Релевантная память (5 записей, ~420 токенов)

### Facts (semantic)
- Проект на Go 1.23, SQLite через mattn/go-sqlite3 [★4]
- API-ключ DeepSeek в env `DEEPSEEK_API_KEY` [★5]

### How-to (procedural)
- Сборка: `make build`; тесты: `make test` перед деплоем [★4]

### Recent context (episodic)
- В прошлой сессии чинили дедуп в memory_save — решение через ULID+scope [★3]

_Свёрнуто ещё 12 менее релевантных записей. Уточни запрос при необходимости._
```

Опционально: `Reasoner` (DeepSeek) сжимает найденное в 1 абзац + список фактов.

Опционально (`Query.Explain`, задача 8.1): под каждой записью брифа — компактная
трасса ранжирования (источники кандидата + разложение final на слагаемые шага 3),
без пересчёта и без изменения самого отбора/ранжирования — только для отладки и
тюнинга весов `[recall].weights`.

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

---

## 10. Reasoner — DeepSeek

DeepSeek chat (OpenAI-совместимый) в роли текстового процессора памяти:
- суммаризация recall-выдачи в абзац + факты (опционально);
- консолидация кластеров `episodic` в обобщённые `semantic`;
- дедуп-суждения в спорных случаях.

Ключ — `DEEPSEEK_API_KEY` (env). Для эмбеддингов DeepSeek **не используется**.

---

## 11. Maintenance — фоновые джобы

Отличают систему от прототипа:
- **Decay** — понижаем вес неиспользуемых `episodic`, старьё архивируем.
- **Consolidation** (через Reasoner) — кластер похожих `episodic` → один `semantic`,
  старые помечаем `superseded_by`.
- **Dedup sweep** — чистка накопившихся почти-дублей (SimHash).
- **Reindex** — перегон всего `content` через новый Embedder при смене модели.

---

## 12. MCP API (инструменты агента)

Набор маленький — агенту нужно 3–4 инструмента, не 15.

- **`memory_save`** — принимает структурированный факт:
  ```json
  { "kind": "semantic", "content": "...", "importance": 4,
    "tags": ["go","stack"], "scope": "project:agentmemory", "links": ["build-uses-make"] }
  ```
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
- **`memory_update`** — правка факта (в т.ч. пометка устаревшим).
- **`memory_forget`** — мягкое удаление (`superseded_by` / архив).

Транспорт: MCP over stdio (`/cmd/memoryd`).

**Web-морда (read + мягкое удаление).** `memoryd web` (Этап 6, `cmd/memoryd`) поднимает отдельный
HTTP-режим того же бинаря поверх того же файла БД: JSON API (`/api/stats`, `/api/memories`,
`/api/memories/{id}`, `/api/search`) на read-методах Store (`List`/`Stats`/`Get`/`Neighbors`/
`SearchLexical`, а также `VectorStatus`/`SemanticNeighbors` для карточки записи) — правок
содержимого и создания новых записей из браузера нет. Единственная write-операция —
`DELETE /api/memories/{id}`: мягкое удаление через доменный `write.Writer.Forget` (та же
семантика, что у MCP-инструмента `memory_forget` — тумстоун `Store.Supersede(id, id)`); hard
DELETE не делается, история и связи сохраняются. Слушает по умолчанию только `127.0.0.1:7070`
(localhost, наружу не выставляется); поиск (`/api/search`) — лексический (FTS5), гибридный
векторный поиск вне этой версии. Карточка записи (`/api/memories/{id}`, задача 6.4) при этом
показывает статус вектора и KNN-соседей по cosine — read-only чтение уже сохранённых векторов
из БД, без вызовов Embedder/ollama (при выключенном векторном слое деградирует, а не падает).
Статический фронтенд отдаётся из `embed.FS` (`cmd/memoryd/web/`), сам интерфейс — задачи 6.3/6.4.

**CLI (одноразовые save/recall).** `memoryd recall "<запрос>"` и `memoryd save` (Этап 7,
задача 7.1, `cmd/memoryd`) — one-shot подкоманды поверх тех же Writer/Recaller, что и MCP-сервер,
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

**Ambient-режим (хуки харнесса).** `memoryd hook -event <name> [-config path]` (Этап 7,
задача 7.2, `cmd/memoryd/hook.go`) — тонкая обёртка над тем же recall-путём, что и `memoryd
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
от fail-fast остального `memoryd` — хук не имеет права сломать старт или ход сессии агента.

Подключается `memoryd init` (задача 7.2, `cmd/memoryd/init.go`): интерактивный вопрос
«подключить автоматический recall через хуки Claude Code» (только session-start / оба
события / нет — дефолт первый) мержит секцию `hooks` в `.claude/settings.json` — команду
`<путь memoryd> hook -event <name> -config ${CLAUDE_PROJECT_DIR:-.}/agentmemory.toml`.
Merge, не перезапись: существующий settings.json и его чужие ключи/события/matcher-записи
не теряются, повторный `init` идемпотентен (та же команда не дублируется), битый JSON —
ошибка без записи. У opencode (второй поддерживаемый харнесс) аналога хуков нет — `init`
честно сообщает об этом в выводе, ambient-режим доступен только для Claude Code.

При `initialize` сервер публикует server-level `instructions` (`ServerOptions.Instructions`)
— компактный клиент-агностичный протокол (когда звать recall, что такое атомарный факт,
дефолты save/recall, выведенные из `[scope]`), собираемый `internal/mcp.buildInstructions`.
Так базовый протокол доезжает до любого MCP-клиента (не только Claude Code) без Claude-скила;
богатый проектный протокол по-прежнему живёт в скиле `/memoryd`.

---

## 13. Конфигурация (TOML + env)

```toml
[store]
path = "~/.agentmemory/memory.db"

[embedder]
provider = "ollama"                       # noop | ollama | openai
base_url = "http://localhost:11434"
model    = "bge-m3"                        # любая ollama-модель; dim определяется автоматически

[reasoner]
provider = "deepseek"                      # ключ в env DEEPSEEK_API_KEY
model    = "deepseek-chat"

[recall]
default_token_budget = 800
weights = { relevance = 1.0, importance = 0.5, recency = 0.4, access = 0.2 }

[maint]
decay_interval   = "24h"
consolidate_after = "168h"                 # неделя episodic → semantic

[scope]
project = "myproject"                      # → "project:myproject"; пусто = мультипроектность выключена
shared  = ["user:*"]                       # пространства, всегда добавляемые к recall по умолчанию
```

**`.env`.** Ключ `DEEPSEEK_API_KEY` — секрет, в TOML-конфиг никогда не пишется. Агентские
приложения (в т.ч. Claude Code) не пробрасывают `.env` в env MCP-сервера автоматически,
поэтому `memoryd` сам загружает `.env` (`KEY=VALUE`, `#`-комментарии, опциональные кавычки)
из каталога своего `-config` при старте, до создания reasoner'а — но только для переменных,
ещё не заданных в shell-env (shell-env приоритетнее). Отсутствие `.env` — не ошибка. Файл
раскладывается (`0600`, права только для владельца) и добавляется в `.gitignore` командой
`memoryd init` (§14 Этап 5, задача 5.4), если ключ был введён.

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
`memoryd` через MCP. Сюда же — `memoryd init`: интерактивный установщик памяти в
произвольный проект под конкретное агентское приложение (Claude Code и opencode),
генерирующий MCP-конфиг и скил «Протокол работы с памятью» (`memory_recall` в начале
задачи, `memory_save`/`memory_update`/`memory_forget` по ходу) — так любой проект, не
только сам AgentMemory, может подключить себе долговременную память в пару команд.

**Этап 6 — Web-морда (viewer + мягкое удаление).**
`memoryd web` — просмотр памяти в браузере: подкоманда поднимает HTTP-сервер поверх того же
общего файла БД (§12), JSON API на read-методах Store и статический фронтенд (задача
6.3) поверх него — дашборд статистики, постраничный список с фильтрами по scope/kind,
карточка записи с соседями по графу связей, лексический поиск. Единственная write-операция —
мягкое удаление записи (`DELETE /api/memories/{id}`, тумстоун через доменный `Writer.Forget`,
та же семантика, что у `memory_forget`).

---

## 15. Принципы

- **Всё за интерфейсами** (`Store`, `Embedder`, `Reasoner`) — бэкенды сменные, тесты простые.
- **Деградация, а не падение** — недоступность вектора/LLM понижает качество, но не ломает recall.
- **Никаких тихих обрезаний** — бриф всегда сообщает про свёрнутый хвост.
- **Память забывает** — без устаревания и консолидации тонет в шуме.
- **Атомарность записи** — одна запись = один факт; отсюда отсутствие чанкинга.
