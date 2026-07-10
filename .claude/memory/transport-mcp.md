---
name: transport-mcp
kind: decision
importance: 4
tags: [mcp, api, transport]
---

Агент обращается к памяти через **MCP over stdio** (`/cmd/memoryd`). Инструменты:
`memory_save`, `memory_recall`, `memory_update`, `memory_forget`. Набор намеренно маленький.

**Почему:** MCP — стандарт для LLM-агентов, максимальная совместимость; маленький toolset
не перегружает агента.
**Как применять:** схемы и роутинг инструментов — в пакете `mcp/`, не протекают в другие
пакеты. HTTP-слой — опция на будущее, не сейчас.

Связано: [[write-structured]]
