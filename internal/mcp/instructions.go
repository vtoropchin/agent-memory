package mcp

import (
	"fmt"
	"strings"

	"agentmemory/internal/config"
)

// buildInstructions собирает компактный клиент-агностичный протокол работы с
// памятью, публикуемый сервером как server-level instructions при MCP
// initialize (DESIGN.md §12). Это переносимый минимум для любого MCP-клиента
// (не только Claude Code) — богатый проектный протокол остаётся в скиле
// `/memo` (.claude/skills/memo/SKILL.md), это дублирование ядра
// осознанное. Текст выводит дефолты scope из уже разрешённого scope инстанса
// через SaveScope()/RecallScopes().
func buildInstructions(scope config.ScopeConfig) string {
	var b strings.Builder

	b.WriteString("AgentMemory — долговременная память агента. 5 инструментов: memory_save, memory_recall, memory_get, memory_update, memory_forget.\n\n")
	b.WriteString("- memory_recall — вызывай в начале задачи: возвращает Markdown-бриф релевантной памяти по смыслу запроса.\n")
	b.WriteString("- memory_recall поддерживает фильтры tags (хотя бы один из тегов) и created_after/created_before (длительность назад от сейчас, напр. \"168h\" = за неделю).\n")
	b.WriteString("- memory_save — сохраняй один атомарный факт/событие/процедуру (1–3 предложения) за вызов, не сваливай несколько фактов в одну запись.\n")
	b.WriteString("- факту с известным сроком годности задавай ttl (memory_save/memory_update) — Go-длительность от сейчас, напр. \"72h\"; в memory_update \"0\" снимает срок.\n")
	b.WriteString("- links (в memory_save и memory_update) — связь либо строка-id (rel=related), либо объект {\"id\":..,\"rel\":..} с типом: related|supersedes|causes|part_of|blocked_by; links в memory_update ДОБАВЛЯЕТ связи, не заменяет их; снять связь — unlinks (те же пары id+rel) в memory_update.\n")
	b.WriteString("- memory_update — правка существующего факта (содержание, важность, теги, links/unlinks).\n")
	b.WriteString("- memory_forget — мягкое удаление (тумстоун): запись перестаёт участвовать в recall, история сохраняется.\n")
	b.WriteString("- memory_get — прочитать запись целиком по id (полный content); memory_recall показывает только краткую форму.\n")
	b.WriteString("- kind=debt — техдолг: известная проблема, отложенная осознанно; в отличие от episodic сам не затухает — закрывай явно через memory_forget(id, replaced_by=<id записи о фиксе>).\n")
	b.WriteString("- kind=task — задача агента вместо файла TODO: content — полный текст задачи (что сделать, границы, критерий приёмки), summary ОБЯЗАТЕЛЕН и служит заголовком в брифе, importance — приоритет.\n")
	b.WriteString("- status задачи — стадия kanban-доски: backlog — сырая идея | todo — готова к работе (по умолчанию) | doing — в работе | review — ревью кода (PR открыт) | testing — проверка/тестирование | release — влито, ждёт релиза | done — в релизе. Переходы назад допустимы; стадии blocked нет — блокировка это связь (см. ниже).\n")
	b.WriteString("- блокировка задачи задачей: на ЗАБЛОКИРОВАННОЙ задаче links:[{\"id\":<блокер>,\"rel\":\"blocked_by\"}] (блокер — только kind=task). Заблокированную нельзя перевести дальше todo (doing и далее) — сервер вернёт ошибку с перечнем блокеров; блокировка снимается сама, когда блокер станет done или будет отменён (memory_forget), вручную — unlinks той же пары. Нужно решение человека — оформи его задачей-блокером. В брифе заблокированная задача помечена ⛔ (ждёт: <блокер>), в memory_get — секции «Заблокирована (ждёт)» и «Блокирует».\n")
	b.WriteString("- type задачи: epic — крупная цель, дробится на story | story — пользовательская история, дробится на task | task — атомарная единица работы (по умолчанию). Иерархия — links с rel=part_of ОТ дочерней К родительской: задача part_of истории, история part_of эпика; теги epic/эпик/История для этого не нужны.\n")
	b.WriteString("- ход работы веди через memory_update(id, status=...): взял — doing (заодно проставь branch = текущая git-ветка и workflow = имя процесса: dev, ultracode, code-review, …), открыл PR — review, влил — release, в релизе — done; отменённую задачу убирай через memory_forget.\n")
	b.WriteString("- memory_recall без фильтра НЕ показывает выполненные задачи (status=done); сделанное — memory_recall(statuses:[\"done\"]), свой список дел — memory_recall(statuses:[\"todo\",\"doing\"]), список эпиков — memory_recall(types:[\"epic\"]).\n\n")

	if scope.Project == "" {
		b.WriteString("Мультипроектность выключена (adhoc-режим):\n")
		b.WriteString("- memory_recall без scope/scopes ищет по всей памяти без фильтра.\n")
		b.WriteString("- memory_save требует явный scope в вызове — без него вернётся ошибка.\n")
		return b.String()
	}

	saveScope := scope.SaveScope()
	recallScopes := scope.RecallScopes()
	fmt.Fprintf(&b, "Дефолты этого инстанса (project=%q):\n", scope.Project)
	fmt.Fprintf(&b, "- memory_save без scope сохраняет в %q.\n", saveScope)
	fmt.Fprintf(&b, "- memory_recall без scope/scopes ищет в %s.\n", strings.Join(recallScopes, ", "))
	// Совет про общий слой выводим из scope.Shared, а не из литерала "user:*",
	// чтобы он был согласован с реально показанными recall-scope при нестандартном
	// [scope] shared. Пустой Shared → общего слоя у инстанса нет, строку не печатаем.
	switch len(scope.Shared) {
	case 0:
		// нет общего слоя — совета про общий scope нет
	case 1:
		fmt.Fprintf(&b, "- Общие для всех проектов факты сохраняй явно в scope %q.\n", scope.Shared[0])
	default:
		quoted := make([]string, len(scope.Shared))
		for i, s := range scope.Shared {
			quoted[i] = fmt.Sprintf("%q", s)
		}
		fmt.Fprintf(&b, "- Общие факты сохраняй явно в один из общих scope: %s.\n", strings.Join(quoted, ", "))
	}

	return b.String()
}
