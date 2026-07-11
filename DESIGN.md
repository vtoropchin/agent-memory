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
/cmd/memoryd            — точка входа: MCP-сервер (stdio)
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
   + фильтры `scope`/`kind`/свежесть.
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
- **`memory_recall`** — главный инструмент, возвращает Markdown-бриф:
  ```json
  { "query": "как собирать и тестировать", "scope": "project:agentmemory",
    "kinds": ["procedural","semantic"], "token_budget": 800 }
  ```
- **`memory_update`** — правка факта (в т.ч. пометка устаревшим).
- **`memory_forget`** — мягкое удаление (`superseded_by` / архив).

Транспорт: MCP over stdio (`/cmd/memoryd`). HTTP-слой — опция на будущее.

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
```

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
`memoryd` через MCP.

---

## 15. Принципы

- **Всё за интерфейсами** (`Store`, `Embedder`, `Reasoner`) — бэкенды сменные, тесты простые.
- **Деградация, а не падение** — недоступность вектора/LLM понижает качество, но не ломает recall.
- **Никаких тихих обрезаний** — бриф всегда сообщает про свёрнутый хвост.
- **Память забывает** — без устаревания и консолидации тонет в шуме.
- **Атомарность записи** — одна запись = один факт; отсюда отсутствие чанкинга.
