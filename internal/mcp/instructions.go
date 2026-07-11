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
// `/memoryd` (.claude/skills/memoryd/SKILL.md), это дублирование ядра
// осознанное. Текст выводит дефолты scope из уже разрешённого scope инстанса
// через SaveScope()/RecallScopes().
func buildInstructions(scope config.ScopeConfig) string {
	var b strings.Builder

	b.WriteString("AgentMemory — долговременная память агента. 4 инструмента: memory_save, memory_recall, memory_update, memory_forget.\n\n")
	b.WriteString("- memory_recall — вызывай в начале задачи: возвращает Markdown-бриф релевантной памяти по смыслу запроса.\n")
	b.WriteString("- memory_save — сохраняй один атомарный факт/событие/процедуру (1–3 предложения) за вызов, не сваливай несколько фактов в одну запись.\n")
	b.WriteString("- memory_update — правка существующего факта (содержание, важность, теги).\n")
	b.WriteString("- memory_forget — мягкое удаление (тумстоун): запись перестаёт участвовать в recall, история сохраняется.\n\n")

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
