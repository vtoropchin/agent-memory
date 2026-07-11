---
name: scope-multiproject
kind: project
importance: 5
tags: [scope, multiproject, design-decision]
---

Решение (2026-07-11, пользователь): AgentMemory используется агентом с **несколькими
проектами** одновременно. Модель — **изоляция проектов + общий слой `user:*`**, физически
**один общий файл БД** (`~/.agentmemory/memory.db`), разделение через scope. Зафиксировано
в DESIGN.md §2.1.

Правила разрешения scope:
- Каждый проект запускает свой `memoryd` с `[scope] project = "<name>"` → `project:<name>`.
- **Save** без scope → `project:<name>` инстанса; общие привычки — явно `user:*`.
- **Recall** без scope → набор `["project:<name>", "user:*"]` (фильтр `scope IN (...)`);
  чужие `project:*` невидимы. Явный scope из вызова перекрывает дефолт.
- `project` не задан → мультипроектность выключена (adhoc/одно-проектный режим).

**Реализация — задачи [[task-files]]:** `tasks/mp-1-scope-core.md` (Query.Scopes +
scope-предикат `IN` в store + ScopeConfig/хелперы; выполнять СЛЕДУЮЩЕЙ, до 3.2) и
`tasks/mp-2-scope-recall-wiring.md` (дефолты scope в mcp save/recall + проброс cfg в
memoryd). Затронуты зависимостями: 3.2, 3.4, 3.6, 5.2. Конкурентный доступ к общему файлу
уже покрыт WAL+busy_timeout (см. sqlite.go).

**Почему так:** до изменения recall фильтровал по одиночному scope и без scope не фильтровал
вовсе — утечка контекста между проектами. Набор scope + идентичность инстанса закрывают это,
сохраняя переиспользование `user:*` между проектами.

Связано: [[progress]] [[task-files]]
