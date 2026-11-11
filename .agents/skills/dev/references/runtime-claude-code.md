# Адаптер: Claude Code

Читайте, только если текущий клиент — Claude Code. Общая модель — [runtime.md](runtime.md).
Имена ниже проверяйте по фактическим схемам инструментов сессии.

| Возможность | Claude Code |
|---|---|
| Субагент со свежим контекстом | инструмент `Agent`: `subagent_type` с именем роли, если `/adapt` создал `.claude/agents/<роль>.md`; иначе `general-purpose` с файлом роли в промпте |
| Продолжение субагента | `SendMessage` тому же агенту |
| Поисковый субагент | агент `Explore` |
| Ревью на баги | встроенный скил `code-review` через инструмент `Skill`, аргумент — уровень `low`/`medium`/`high` (передавать всегда: без него берётся последний использованный), затем цель. Флаги `--fix` и `--comment` не используются |
| Ревью безопасности | встроенный скил `security-review` |
| Документация библиотек | MCP `context7`: `resolve-library-id` → `query-docs` |
| Память | MCP `memo`; хуки `SessionStart`/`UserPromptSubmit` делают авто-recall |

## Особенности

- Встроенный `/code-review` читает только `CLAUDE.md`, а `REVIEW.md` — нет («It doesn't read
  REVIEW.md», https://code.claude.com/docs/en/code-review-local.md). Сверка с «Повторяющимися
  находками» в фазе 5а делается отдельным шагом.
- Модель сессии названа в системном промпте, раздел Environment: «You are powered by the model
  named …». Переменной окружения с именем модели у скилов и хуков нет.
- Режим делегирования: сессия на семействе **Fable / Mythos**; `implementer` идёт на новейшем
  Opus (`model: opus` во frontmatter агента — алиас разрешается в последнюю версию сам).
- Субагент видит `CLAUDE.md` проекта и правила пользователя (`~/.claude/CLAUDE.md`,
  `.claude/rules/`) — проверено вживую 2 сентября 2026, вопреки документации. Разговора и
  авто-recall памяти у него нет.
- Агенты `.claude/agents/` читаются при старте сессии. Файл создан, а `Agent` отвечает «тип не
  найден» — сообщить пользователю об этом одной строкой и до перезапуска запускать роль через
  `general-purpose` с файлом роли в промпте.

## Frontmatter агентов для `.claude/agents/`

`/adapt` создаёт `.claude/agents/<роль>.md`: `name` и `description` — из файла роли, тело —
тело файла роли, а `tools`, `model`, `effort` — из таблицы. Без `memo` убираются инструменты
`mcp__memo__*`, без `context7` — `mcp__context7__*`.

| Роль | `tools` | `model` | `effort` |
|---|---|---|---|
| `architecture-reviewer` | `Read, Grep, Glob, Bash, Skill, mcp__memo__memory_recall` | `inherit` | — |
| `boundary-architect` | `Read, Grep, Glob, Write, Skill, mcp__memo__memory_recall, mcp__memo__memory_save, mcp__memo__memory_update` | `inherit` | — |
| `dependency-auditor` | `Read, Grep, Glob, Bash, Write, Skill, mcp__memo__memory_recall, mcp__memo__memory_save, mcp__memo__memory_update` | `inherit` | — |
| `implementer` | `Read, Edit, Write, Grep, Glob, Bash, Skill, mcp__memo__memory_recall, mcp__context7__resolve-library-id, mcp__context7__query-docs` | `opus` | `high` |
| `legacy-decoupler` | `Read, Grep, Glob, Bash, Write, Skill, mcp__memo__memory_recall, mcp__memo__memory_save, mcp__memo__memory_update` | `inherit` | — |
| `test-architect` | `Read, Grep, Glob, Write, Bash, Skill, mcp__memo__memory_recall, mcp__memo__memory_save, mcp__memo__memory_update` | `inherit` | — |
| `use-case-designer` | `Read, Grep, Glob, Write, Skill, mcp__memo__memory_recall, mcp__memo__memory_save, mcp__memo__memory_update` | `inherit` | — |
