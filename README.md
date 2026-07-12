# AgentMemory

Долговременная память для ИИ-агентов на Go. Агент во время работы сохраняет атомарные
факты, события и процедуры, а в другой сессии — своей или чужой — запрашивает их обратно
по смыслу и получает компактный Markdown-бриф, обрезанный под бюджет токенов, а не сырую
выдачу top-k.

Хранилище — один файл SQLite (метаданные + полнотекстовый индекс FTS5 + опциональный
вектор через `sqlite-vec`). Агент общается с памятью через MCP по stdio.

## Зачем это

- Сохранил факт в одной сессии — достал по смыслу в другой, без ручного копирования в промпт.
- Recall отдаёт не сырьё, а ранжированный и обрезанный под токен-бюджет бриф.
- Память не превращается в свалку: дедупликация при записи, устаревание episodic-фактов,
  консолидация в semantic — фоновыми джобами.
- Работает локально: SQLite-файл, без обязательных внешних сервисов. Вектор (ollama) и
  суммаризация (DeepSeek) — опциональные слои: недоступны — recall деградирует до FTS5,
  а не падает.
- Подключается к проекту одной командой (`memoryd init`), которая сама раскладывает
  MCP-конфиг и инструкцию для агента.

## Быстрый старт

Предпосылки:

- Go 1.26+
- C-компилятор (CGO обязателен — `mattn/go-sqlite3` собирается с тегом `sqlite_fts5`)
- опционально: [ollama](https://ollama.com) с моделью `bge-m3` для векторного поиска
- опционально: ключ DeepSeek (`DEEPSEEK_API_KEY`) для суммаризации и консолидации памяти

Без ollama и DeepSeek память тоже работает — на чистом FTS5, без суммаризации выдачи.

Собери и поставь бинарь себе в `~/.local/bin` (без sudo):

```sh
make install
```

Если `~/.local/bin` не в `PATH`, `make install` подскажет строку для `~/.zshrc`.

Подключи память к своему проекту — из корня проекта, куда её ставишь:

```sh
memoryd init
```

В интерактивном режиме спросит имя проекта, агентское приложение (`claude` или
`opencode`), адрес и модель ollama, путь к файлу БД и, по желанию, ключ DeepSeek — и
разложит:

- MCP-конфиг (`.mcp.json` для Claude Code или `opencode.json` для opencode);
- скил-инструкцию «Протокол работы с памятью» (когда звать recall/save/update/forget);
- `agentmemory.toml` со введёнными настройками;
- `.env` с ключом DeepSeek (если ввёл) — с правами `0600` и строкой в `.gitignore`.

Для неинтерактивного запуска (CI, скрипты) — флагами и `-no-input`:

```sh
memoryd init -project myproject -agent claude -no-input
```

После этого агентское приложение само поднимает `memoryd` как MCP-сервер `agentmemory`.
Агенту в системный промпт/скил уже прописано: `memory_recall` — в начале задачи (подтянуть
контекст), `memory_save` — по ходу работы, когда появляется факт стоит запомнить.

Посмотреть, что накопилось, глазами — read-only web-морда:

```sh
memoryd web
```

и открыть `http://127.0.0.1:7070` — дашборд статистики, список записей с фильтрами по
scope/kind, карточка записи с соседями по графу и лексический поиск. Слушает только
localhost.

## Использование

Бинарь `memoryd` — три режима, различаются первым аргументом:

| Команда | Что делает |
|---|---|
| `memoryd [-config <path>] [-db <path>]` | MCP-сервер по stdio (режим по умолчанию) |
| `memoryd init [флаги]` | установщик: раскладывает MCP-конфиг, скил, `agentmemory.toml`, `.env` в проект |
| `memoryd web [флаги]` | HTTP-морда для просмотра памяти в браузере (read-only + мягкое удаление) |

`memoryd --help` (`-h`/`help`) печатает сводку; `memoryd init -h` и `memoryd web -h` —
флаги конкретной подкоманды.

Флаги режима сервера:

| Флаг | Значение |
|---|---|
| `-config <path>` | путь к TOML-конфигу (опционально) |
| `-db <path>` | путь к SQLite-файлу, переопределяет `store.path` из конфига |

Ключ DeepSeek берётся из `DEEPSEEK_API_KEY` в окружении или из `.env` рядом с конфигом.

Флаги `memoryd init`:

| Флаг | Значение |
|---|---|
| `-project` | имя проекта (дефолт — базовое имя каталога `-dir`) |
| `-dir` | каталог, в который скаффолдить артефакты (дефолт `.`) |
| `-force` | перезаписывать существующие файлы |
| `-agent` | `claude` или `opencode` (дефолт `claude`) |
| `-ollama-url` | base URL ollama (дефолт `http://localhost:11434`) |
| `-ollama-model` | модель эмбеддингов ollama (дефолт `bge-m3`) |
| `-store` | путь к файлу SQLite (дефолт `~/.agentmemory/memory.db`) |
| `-deepseek-key` | ключ DeepSeek — пишется в `<dir>/.env` (попадёт в историю shell, если задан флагом) |
| `-no-input` | не спрашивать интерактивно — только флаги и дефолты |

Флаги `memoryd web`:

| Флаг | Значение |
|---|---|
| `-addr` | адрес прослушивания (дефолт `127.0.0.1:7070`) |
| `-config` | путь к TOML-конфигу (опционально) |
| `-db` | путь к SQLite-файлу, переопределяет `store.path` из конфига |

MCP-инструменты, которые видит агент: `memory_save`, `memory_recall`, `memory_update`,
`memory_forget` — набор маленький намеренно, подробности по формату аргументов в
`DESIGN.md` §12.

## Конфигурация

TOML-файл, секции `[store] [embedder] [reasoner] [recall] [maint] [scope]`. Пример со
всеми полями и комментариями — `config.example.toml`. Если конфиг не передан или в нём
чего-то не хватает, действуют дефолты из `internal/config.Default()` (эквивалент этапа
MVP: только SQLite+FTS5, без вектора и без LLM).

Коротко по секциям:

- `[store]` — путь к файлу SQLite.
- `[embedder]` — `noop | ollama | openai`; при `ollama` `dim` определяется автоматически
  пробным эмбеддингом при старте, руками не задаётся.
- `[reasoner]` — `noop | deepseek`; ключ **не в этом файле**.
- `[recall]` — дефолтный токен-бюджет брифа и веса ранжирования (relevance/importance/recency/access).
- `[maint]` — интервалы фоновых джобов (decay, consolidation).
- `[scope]` — имя текущего проекта (мультипроектность через общий файл БД + scope) и
  список пространств, всегда добавляемых к recall (по умолчанию `["user:*"]`).

Ключ DeepSeek — только `DEEPSEEK_API_KEY` в окружении или `.env` рядом с конфигом
(`KEY=VALUE`, поддерживает `#`-комментарии и кавычки). Shell-env приоритетнее `.env`.
В TOML-конфиг ключ никогда не пишется.

## Как это устроено

```
Agent ── MCP tools (stdio) ── Memory API (save/recall/update/forget)
                                     │
                    ┌────────────────┼────────────────┐
                  Writer           Recall            Maint
                (дедуп,        (FTS5+vec → RRF →   (decay,
                классификация,   ранжирование →     consolidation,
                связывание)      Markdown-бриф)      reindex)
                    └────────────────┼────────────────┘
                              Store: SQLite (metadata + FTS5 + vec)
                                     │              │
                                 Embedder        Reasoner
                            (ollama/bge-m3,   (DeepSeek chat,
                              опционален)       опционален)
```

- **Store** — единый SQLite-файл: таблица `memories`, FTS5-индекс, опциональная
  vec-таблица (`sqlite-vec`), всё за интерфейсом `Store`.
- **Writer** (`internal/memory/write`) — приём факта: дедуп по scope, обновление или
  пометка `superseded_by` вместо DELETE, проставление связей.
- **Recall** (`internal/memory/recall`) — гибридный поиск (FTS5 + вектор) → слияние через
  Reciprocal Rank Fusion → ранжирование по relevance/importance/свежести/частоте обращения
  → сборка Markdown-брифа под токен-бюджет.
- **Maint** (`internal/memory/maint`) — фоновые джобы: decay episodic-записей,
  консолидация episodic→semantic через Reasoner, dedup-sweep, reindex при смене
  Embedder-модели.
- **Embedder / Reasoner** — за интерфейсами, оба опциональны и деградируют вместо падения:
  без ollama recall работает на чистом FTS5, без DeepSeek — без суммаризации и
  консолидации.

Подробности — модель данных, интерфейсы, write/read path, roadmap — в `DESIGN.md`.

## Разработка

```sh
make build   # CGO_ENABLED=1 go build -tags sqlite_fts5 ./...
make test    # CGO_ENABLED=1 go test -tags sqlite_fts5 ./...
make run     # CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memoryd — MCP-сервер из исходников
make web     # CGO_ENABLED=1 go run -tags sqlite_fts5 ./cmd/memoryd web — web-морда из исходников
```

Тег `sqlite_fts5` и `CGO_ENABLED=1` обязательны везде — без них FTS5 не соберётся.

Код:

- `cmd/memoryd` — точка входа: сервер, `init`, `web`.
- `internal/memory` — домен: `types.go`, `store/` (SQLite), `embed/` (Embedder), `reason/`
  (Reasoner), `write/` (ingest), `recall/` (поиск и бриф), `maint/` (фоновые джобы).
- `internal/mcp` — регистрация MCP-инструментов и роутинг.
- `internal/config` — загрузка TOML/env.

Архитектурные решения и границы пакетов — в `DESIGN.md`, читай перед изменениями.
