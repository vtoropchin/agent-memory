// skill.go — генерация скила «Протокол работы с памятью», раскладываемого
// `memoryd init` в проект (DESIGN.md §14 Этап 5, задача 5.4). Инструктирует
// агентское приложение, как пользоваться MCP-инструментами agentmemory
// (memory_recall/memory_save/memory_update/memory_forget) — обобщённая версия
// раздела «Память проекта» из .claude/skills/dev/SKILL.md.
package main

import "fmt"

// memorySkillBody — тело инструкции, не зависящее от того, какое агентское
// приложение его раскладывает (frontmatter и путь файла — специфика
// конкретного agentTarget, см. agentRegistry в init.go). При добавлении
// opencode/codex это тело переиспользуется без изменений — меняется только
// обёртка в agentTarget.renderSkill. %[1]s — имя проекта.
const memorySkillBodyTemplate = `# Протокол работы с памятью — %[1]s

Проект **%[1]s** хранит долговременную память в MCP-сервере ` + "`agentmemory`" + `
(инструменты ` + "`memory_recall`/`memory_save`/`memory_update`/`memory_forget`" + `,
конфиг — ` + "`agentmemory.toml`" + ` в корне репозитория).

## Когда вызывать

- **В начале любой задачи** — ` + "`memory_recall`" + ` с описанием задачи. ` + "`scope`/`scopes`" + `
  не указывай: инстанс сам подставит домашнее пространство ` + "`project:%[1]s`" + `
  плюс общий слой ` + "`user:*`" + ` (см. ` + "`agentmemory.toml [scope]`" + `). Так
  подтягиваются решения, договорённости и прогресс прошлых сессий.
- **По ходу и в конце задачи** — фиксируй новое через ` + "`memory_save`" + `: один
  атомарный факт (1–3 предложения) за вызов. ` + "`scope`" + ` в схеме инструмента
  обязателен (без ` + "`omitempty`" + `) — передавай пустую строку ` + "`scope:\"\"`" + `,
  если хочешь дефолт ` + "`project:%[1]s`" + `, а не опускай ключ. Явно
  ` + "`scope:\"project:%[1]s\"`" + ` — для проектных фактов/решений/граблей; явно
  ` + "`scope:\"user:*\"`" + ` — для общих привычек и предпочтений пользователя, не
  привязанных к этому проекту.
- **Факт изменился** — ` + "`memory_update`" + ` (id записи + только изменённые поля).
- **Факт устарел или ошибочен** — ` + "`memory_forget`" + ` (мягкое удаление; можно
  указать ` + "`replaced_by`" + `, если факт заменён более новым).

## Правила scope

- ` + "`memory_save`" + ` без явного scope пишет в ` + "`project:%[1]s`" + ` — домашнее
  пространство этого инстанса.
- ` + "`memory_recall`" + ` без scope/scopes ищет в ` + "`project:%[1]s`" + ` + ` + "`user:*`" + `.
- Явный scope/scopes в вызове всегда перекрывает дефолт инстанса.

## Если сервер недоступен

Если инструменты ` + "`memory_*`" + ` не видны в сессии — агентское приложение не
подняло MCP-сервер ` + "`agentmemory`" + ` (воркспейс не одобрен, бинарь ` + "`memoryd`" + `
недоступен) или reasoner/embedder работают в деградированном режиме (пустой
` + "`DEEPSEEK_API_KEY`" + ` — проверь ` + "`.env`" + ` рядом с ` + "`agentmemory.toml`" + `, memoryd
подхватывает его сам при старте). Продолжай задачу без долговременной памяти,
предупредив об этом.
`

// memorySkillBody рендерит тело инструкции для конкретного проекта.
func memorySkillBody(project string) string {
	return fmt.Sprintf(memorySkillBodyTemplate, project)
}

// claudeSkillDescription — текст поля description фронтматтера SKILL.md для
// Claude Code: по нему агент решает, когда скил релевантен.
func claudeSkillDescription(project string) string {
	return fmt.Sprintf(
		"Протокол работы с долговременной памятью проекта %[1]s через MCP-сервер agentmemory (memory_recall/memory_save/memory_update/memory_forget). Используй в начале задачи для recall и по ходу/в конце задачи для сохранения новых фактов и решений в проекте %[1]s.",
		project,
	)
}

// renderClaudeSkill оборачивает memorySkillBody в SKILL.md для Claude Code —
// валидный YAML-фронтматтер (name/description) поверх общего тела.
func renderClaudeSkill(project string) string {
	frontMatter := fmt.Sprintf("---\nname: memoryd\ndescription: %s\n---\n\n", claudeSkillDescription(project))
	return frontMatter + memorySkillBody(project)
}
