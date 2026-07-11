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

## Этап 3 — Embedder + гибридный поиск

| Задача | Файл | Статус |
|--------|------|--------|
| 3.1 Ollama-Embedder | [3.1-ollama-embedder.md](3.1-ollama-embedder.md) | ⏳ |
| 3.2 sqlite-vec в Store | [3.2-sqlite-vec-store.md](3.2-sqlite-vec-store.md) | ⏳ |
| 3.3 Вектор в write path | [3.3-write-vector.md](3.3-write-vector.md) | ⏳ ожидает 3.1, 3.2 |
| 3.4 Гибридный recall | [3.4-hybrid-recall.md](3.4-hybrid-recall.md) | ⏳ ожидает 3.1, 3.2 |
| 3.5 Reindex при смене модели | [3.5-reindex.md](3.5-reindex.md) | ⏳ ожидает 3.1, 3.2 |
| 3.6 Связка Embedder в memoryd | [3.6-wiring-embedder.md](3.6-wiring-embedder.md) | ⏳ ожидает 3.1–3.5 |

## Этап 4 — Maintenance

| Задача | Файл | Статус |
|--------|------|--------|
| 4.1 Decay | [4.1-decay.md](4.1-decay.md) | ⏳ |
| 4.2 Dedup-sweep | [4.2-dedup-sweep.md](4.2-dedup-sweep.md) | ⏳ |
| 4.3 Планировщик джобов | [4.3-scheduler.md](4.3-scheduler.md) | ⏳ ожидает 4.1, 4.2 |

## Этап 5 — Догфудинг

| Задача | Файл | Статус |
|--------|------|--------|
| 5.1 Импортёр файловой памяти | [5.1-import-file-memory.md](5.1-import-file-memory.md) | ⏳ |
| 5.2 Подключение memoryd к проекту | [5.2-connect-memoryd.md](5.2-connect-memoryd.md) | ⏳ ожидает 5.1 |

## Общие требования ко всем задачам

- Сборка/тесты только с флагами: `CGO_ENABLED=1 go build -tags sqlite_fts5 ./...`,
  `CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./...` (см. Makefile).
- Комментарии к экспортируемым сущностям — на русском, в стиле существующего кода.
- Деградация, а не падение (DESIGN §15): недоступность LLM/вектора понижает качество,
  но не ломает основной флоу.
- Не выходить за рамки задачи; смежные пакеты трогать только там, где задача явно разрешает.
- В финальном отчёте: список файлов, сигнатуры новых типов/методов, факты билда/тестов.
