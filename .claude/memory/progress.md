---
name: progress
kind: project
importance: 5
tags: [roadmap, progress]
---

Состояние проекта на 2026-07-11: **зафиксирован дизайн** (`DESIGN.md`) и dev-тулинг
(скилл `/dev`, агенты `go-developer` / `go-reviewer`, файловая память). Кода приложения ещё нет.

**Следующий шаг:** Этап 1 (MVP) по `DESIGN.md §14` — каркас проекта (`go.mod`, пакеты,
интерфейсы-заглушки, SQL-схема), затем `Store` на SQLite+FTS5, MCP-сервер с
`save`/`recall`/`forget`/`update`, `Embedder=noop`, ранжирование по importance+recency,
Markdown-бриф.

**Как применять:** обновляй этот файл по мере закрытия этапов roadmap. Отмечай, что реально
собрано/протестировано/запущено (с фактами), а не только «сделано».
