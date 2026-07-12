# Бэклог задач AgentMemory

Каждая задача — отдельный .md-файл, самодостаточный для выполнения младшей моделью
(агентом `go-developer`): контекст, шаги, границы, критерий приёмки. Источник истины
по архитектуре — [DESIGN.md](../DESIGN.md); нумерация этапов — DESIGN.md §14.

Оркестратор (`/dev`) при делегировании передаёт агенту файл задачи целиком.
После выполнения статус в файле обновляется (сделана/в работе/ожидает) с датой и фактами
(билд/тесты/verify).

## Этап 2 — Reasoner (DeepSeek)

| Задача | Файл | Статус |
|--------|------|--------|
| 2.1 DeepSeek-клиент Reasoner | [2.1-deepseek-reasoner.md](2.1-deepseek-reasoner.md) | ✅ выполнена |
| 2.2 Суммаризация recall | [2.2-recall-summarize.md](2.2-recall-summarize.md) | ✅ выполнена |
| 2.3 Консолидация episodic→semantic | [2.3-consolidation.md](2.3-consolidation.md) | ✅ выполнена |
| 2.4 Связка Reasoner в memoryd | [2.4-wiring-reasoner.md](2.4-wiring-reasoner.md) | ✅ выполнена |

## Мультипроектность (scope) — DESIGN.md §2.1

Модель: **изоляция проектов + общий слой `user:*`**, один общий файл БД. `mp-1` —
foundational (набор scope в поиске + идентичность проекта), выполнять СЛЕДУЮЩЕЙ, до 3.2.

| Задача | Файл | Статус |
|--------|------|--------|
| MP-1 Ядро модели scope | [mp-1-scope-core.md](mp-1-scope-core.md) | ✅ выполнена |
| MP-2 Разрешение scope в recall/save + memoryd | [mp-2-scope-recall-wiring.md](mp-2-scope-recall-wiring.md) | ✅ выполнена |

## Этап 3 — Embedder + гибридный поиск

| Задача | Файл | Статус |
|--------|------|--------|
| 3.1 Ollama-Embedder | [3.1-ollama-embedder.md](3.1-ollama-embedder.md) | ✅ выполнена |
| 3.2 sqlite-vec в Store | [3.2-sqlite-vec-store.md](3.2-sqlite-vec-store.md) | ✅ выполнена |
| 3.3 Вектор в write path | [3.3-write-vector.md](3.3-write-vector.md) | ✅ выполнена |
| 3.4 Гибридный recall | [3.4-hybrid-recall.md](3.4-hybrid-recall.md) | ✅ выполнена |
| 3.5 Reindex при смене модели | [3.5-reindex.md](3.5-reindex.md) | ✅ выполнена |
| 3.6 Связка Embedder в memoryd | [3.6-wiring-embedder.md](3.6-wiring-embedder.md) | ✅ выполнена |

## Этап 4 — Maintenance

| Задача | Файл | Статус |
|--------|------|--------|
| 4.1 Decay | [4.1-decay.md](4.1-decay.md) | ✅ выполнена |
| 4.2 Dedup-sweep | [4.2-dedup-sweep.md](4.2-dedup-sweep.md) | ✅ выполнена |
| 4.3 Планировщик джобов | [4.3-scheduler.md](4.3-scheduler.md) | ✅ выполнена |

## Этап 5 — Догфудинг

| Задача | Файл | Статус |
|--------|------|--------|
| 5.1 Импортёр файловой памяти | [5.1-import-file-memory.md](5.1-import-file-memory.md) | ✅ выполнена |
| 5.2 Подключение memoryd к проекту | [5.2-connect-memoryd.md](5.2-connect-memoryd.md) | ✅ выполнена |
| 5.3 Подкоманда `memoryd init` (скаффолдинг) | [5.3-init-scaffold.md](5.3-init-scaffold.md) | ✅ выполнена |
| 5.4 `init`: генерация скила + интерактивный сетап | [5.4-init-interactive-skills.md](5.4-init-interactive-skills.md) | ✅ выполнена |

## Этап 6 — Web-морда (read-only viewer)

Просмотр содержимого памяти в браузере. Форма — подкоманда `memoryd web` (HTTP поверх
общего файла БД, read-only), фронтенд на Tailwind (бандл внутри бинаря, не CDN — решение
пользователя), поиск v1 — лексический (FTS5).
6.1 — foundational (read-методы store), выполнять первой; 6.3 — после фиксации контракта
API из 6.2.

| Задача | Файл | Статус |
|--------|------|--------|
| 6.1 Read-only query-слой в Store | [6.1-store-read-api.md](6.1-store-read-api.md) | ✅ выполнена |
| 6.2 Подкоманда `memoryd web` (HTTP + JSON API) | [6.2-memoryd-web-server.md](6.2-memoryd-web-server.md) | ✅ выполнена |
| 6.3 Frontend web-морды (Tailwind bundled) | [6.3-web-frontend-tailwind.md](6.3-web-frontend-tailwind.md) | ✅ выполнена |
| 6.4 Эмбеддинг-информация в морде (KNN-соседи + статус вектора) | [6.4-web-embedding-insight.md](6.4-web-embedding-insight.md) | ✅ выполнена |

## Этап 7 — Ambient memory (хуки харнесса)

Память перестаёт зависеть от дисциплины агента: харнесс сам дёргает recall на
событиях жизненного цикла сессии. 7.1 — foundational (хукам нечем звать память
без CLI), выполнять первой.

| Задача | Файл | Статус |
|--------|------|--------|
| 7.1 CLI-подкоманды `recall`/`save` | [7.1-cli-recall-save.md](7.1-cli-recall-save.md) | ✅ выполнена |
| 7.2 `memoryd hook` + генерация хуков в `init` | [7.2-init-hooks.md](7.2-init-hooks.md) | ✅ выполнена |

## Этап 8 — Качество recall

Независимые задачи, порядок свободный. 8.1 (explain) полезно сделать раньше —
облегчает верификацию остальных.

| Задача | Файл | Статус |
|--------|------|--------|
| 8.1 Explain-трасса ранжирования | [8.1-recall-explain.md](8.1-recall-explain.md) | ✅ выполнена |
| 8.2 Фильтры recall по тегам и датам | [8.2-recall-filters.md](8.2-recall-filters.md) | ✅ выполнена |
| 8.3 Русская морфология в FTS5 (спайк + реализация) | [8.3-fts-russian-morphology.md](8.3-fts-russian-morphology.md) | ⬜ ожидает |
| 8.4 OpenAI-совместимый Embedder | [8.4-openai-embedder.md](8.4-openai-embedder.md) | ⬜ ожидает |

## Этап 9 — Ops и модель данных

Независимые задачи. 9.3 закрывает тех-долг 5.1 (links в `memory_update`).
8.3 и 9.4 обе вводят миграцию схемы — согласовать механизм (`PRAGMA user_version`)
в той, что пойдёт первой.

| Задача | Файл | Статус |
|--------|------|--------|
| 9.1 `memoryd doctor` — диагностика здоровья | [9.1-doctor.md](9.1-doctor.md) | ⬜ ожидает |
| 9.2 `memoryd export`/`import` — бэкап и перенос | [9.2-export-import.md](9.2-export-import.md) | ⬜ ожидает |
| 9.3 Типизированные связи + links в `memory_update` | [9.3-typed-links-mcp.md](9.3-typed-links-mcp.md) | ⬜ ожидает |
| 9.4 TTL — явное время жизни записи | [9.4-ttl-expiry.md](9.4-ttl-expiry.md) | ⬜ ожидает |

## Этап 10 — Web v2

| Задача | Файл | Статус |
|--------|------|--------|
| 10.1 Семантический поиск в морде | [10.1-web-semantic-search.md](10.1-web-semantic-search.md) | ⬜ ожидает |
| 10.2 Правка importance/tags из морды | [10.2-web-edit-metadata.md](10.2-web-edit-metadata.md) | ⬜ ожидает |

### Отклонённые идеи (чтобы не возвращаться)

- **Автоклассификация `kind`/связей через LLM** — против DESIGN §7 (классификацию
  делает агент: дёшево и надёжно); пересматривать только вместе с принципом.
- **LLM-реранк top-N кандидатов** — отложен: латентность+стоимость на каждый recall
  при недоказанной пользе; вернуться, если explain (8.1) покажет систематические
  промахи ранжирования.
- **Batch-save в MCP** — агент и так делает несколько вызовов; выигрыш копеечный,
  а атомарность единицы памяти размывается.

## Общие требования ко всем задачам

- Сборка/тесты только с флагами: `CGO_ENABLED=1 go build -tags sqlite_fts5 ./...`,
  `CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./...` (см. Makefile).
- Комментарии к экспортируемым сущностям — на русском, в стиле существующего кода.
- Деградация, а не падение (DESIGN §15): недоступность LLM/вектора понижает качество,
  но не ломает основной флоу.
- Не выходить за рамки задачи; смежные пакеты трогать только там, где задача явно разрешает.
- В финальном отчёте: список файлов, сигнатуры новых типов/методов, факты билда/тестов.
