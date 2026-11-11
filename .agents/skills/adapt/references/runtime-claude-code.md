# Адаптер: Claude Code

Читайте, только если среди клиентов проекта есть Claude Code. Общие правила — `wiring.md`.

## Скилы и агенты

- Скилы Claude Code ищет в `.claude/skills/` проекта (установщик кладёт их туда напрямую или
  симлинками) и в `~/.claude/skills/`.
- Именованные агенты — файлы `.claude/agents/<роль>.md` проекта. Для каждой роли из
  `references/agents/` скила `dev` создать файл: frontmatter `name`, `description` — из файла
  роли; `tools`, `model`, `effort` — из таблицы в `references/runtime-claude-code.md` скила
  `dev`; тело — тело файла роли без изменений. Без `memo` убрать инструменты `mcp__memo__*`,
  без `context7` — `mcp__context7__*`. Есть ли `context7`: сервер в `.mcp.json` проекта, в
  `~/.claude.json` или инструменты `mcp__context7__*` в текущей сессии; не найден нигде —
  убрать и назвать в отчёте (пользователь может вернуть после подключения).
- Агенты читаются при старте сессии: после создания — назвать перезапуск ручным шагом.

## Файл правил

`CLAUDE.md`. Если правила пишутся в `AGENTS.md` (есть и другие клиенты), в `CLAUDE.md` нужна
строка `@AGENTS.md` — Claude Code подставит файл целиком.

## Память `memo`

`memo init -agent claude …` (`wiring.md`, §4) кладёт `.mcp.json` с сервером `memo`, скилы в
`.claude/skills/memo*` и хуки в `.claude/settings.json`. Если установщик набора сделал
`.claude/skills` симлинками на `.agents/skills`, скилы `memo` окажутся в `.claude/skills/`
отдельными каталогами — это нормально.
