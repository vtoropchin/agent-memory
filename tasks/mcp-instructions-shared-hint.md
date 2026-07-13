# Задача MCP-INSTRUCTIONS-SHARED — совет про общий scope из конфига, не хардкод

**Статус:** ✅ выполнена и финализирована.
**Область:** только `internal/mcp/instructions.go` + `internal/mcp/instructions_test.go`.
**Зависимости:** MCP-INSTRUCTIONS (коммит `1734bda`, `buildInstructions` уже существует).

## Контекст

Follow-up к задаче MCP-INSTRUCTIONS (`tasks/mcp-instructions.md`). Функция
`buildInstructions(scope config.ScopeConfig) string` (`internal/mcp/instructions.go`)
в мультипроектной ветке (`scope.Project != ""`) печатает три строки дефолтов. Две из них
выводятся из конфига через `SaveScope()`/`RecallScopes()`, а третья — **захардкожена**:

```go
b.WriteString("- Общие для всех проектов факты сохраняй явно в scope \"user:*\".\n")
```

Строка выше (про recall) уже собирается динамически из `RecallScopes()` (в неё входит
`scope.Shared`). Из-за хардкода при нестандартном `[scope] shared` (например
`shared = ["team:backend"]`) совет про save разойдётся с реально показанными recall-scope:
recall-строка напечатает `team:backend`, а save-совет — `user:*`, которого у инстанса нет.
Пока `shared = ["user:*"]` (дефолт) рассогласования нет, но текст должен быть согласован с
конфигом, а не полагаться на конвенцию.

**Цель:** вывести строку-совет про сохранение общих фактов из `scope.Shared`, а не из
литерала `"user:*"`.

## Что сделать

В `internal/mcp/instructions.go`, мультипроектная ветка (`scope.Project != ""`), заменить
захардкоженную строку на вывод из `scope.Shared`:

1. **`Shared` непустой, один элемент** (типичный случай, `["user:*"]`): печатать
   `- Общие для всех проектов факты сохраняй явно в scope "<shared[0]>".` — при
   `shared = ["user:*"]` строка обязана получиться **байт-в-байт прежней** (нулевая
   регрессия текста для дефолтного конфига).
2. **`Shared` непустой, несколько элементов** (например `["user:*","team:backend"]`):
   перечислить все общие scope, например
   `- Общие факты сохраняй явно в один из общих scope: "user:*", "team:backend".`
   (формулировку подбери близко к существующему тону; главное — перечислены реальные
   значения `Shared`, каждое в кавычках).
3. **`Shared` пустой** (`nil` или `[]`) при непустом `Project`: у инстанса нет общего слоя
   (recall = только `project:<name>`), поэтому строку-совет про общий scope **не печатать
   вовсе** (не печатать пустую строку/висячие кавычки). Остальные две строки (save/recall)
   остаются.

Не менять adhoc-ветку (`Project == ""`), doc-комментарий функции, `mcp.go`, схемы
инструментов и любые другие файлы. Логику `SaveScope()`/`RecallScopes()` в `config` не
трогать — только чтение `scope.Shared`/готовых методов внутри `buildInstructions`.

## Тесты

Дополнить `internal/mcp/instructions_test.go`:

- **Регрессия дефолта:** `ScopeConfig{Project:"agentmemory", Shared:["user:*"]}` → текст
  содержит прежнюю строку `сохраняй явно в scope "user:*"` (проверить точную подстроку —
  что дефолтный вывод не изменился).
- **Кастомный один Shared:** `Shared:["team:backend"]` → совет содержит `"team:backend"` и
  НЕ содержит `"user:*"` (доказать, что хардкод устранён).
- **Несколько Shared:** `Shared:["user:*","team:backend"]` → совет содержит оба значения.
- **Пустой Shared при заданном Project:** `ScopeConfig{Project:"agentmemory", Shared:nil}`
  → строки-совета про общий scope НЕТ (проверить отсутствие подстроки про «общие … scope»),
  при этом save/recall-строки присутствуют, функция не паникует. (Существующий тест
  `TestBuildInstructions_AdhocEmptyShared` покрывает adhoc — этот про мультипроект с пустым
  Shared, другой случай.)

Существующие тесты `TestBuildInstructions_Multiproject` / `_Adhoc` /
`_AdhocEmptyShared` должны остаться зелёными (при необходимости подправить ассерты, если
они завязаны на точную формулировку, но НЕ ослаблять).

## Границы

- **Только** `internal/mcp/instructions.go` + `internal/mcp/instructions_test.go`.
- НЕ трогать: `internal/mcp/mcp.go`, схемы/описания инструментов, `internal/config`,
  `cmd/memo`, `DESIGN.md` (деталь ниже уровня §12 — синхронизация не требуется), скил
  `/memo`.
- За рамки не выходить: это косметика согласованности текста, поведение save/recall не
  меняется.

## Критерий приёмки

- `CGO_ENABLED=1 go build/vet/test -tags sqlite_fts5 -count=1 ./...` — зелёное.
- Дефолтный конфиг (`shared = ["user:*"]`) даёт **тот же** текст instructions, что и до
  правки (регрессия текста = 0).
- Нестандартный `Shared` отражается в совете про save; пустой `Shared` при заданном
  `Project` — строка-совет отсутствует.
- (Финализация по /dev: арх-ревью go-reviewer, code-review, simplify, build/test,
  e2e через живой бинарь `initialize` с кастомным `shared` в конфиге → в `instructions`
  видно значение из `Shared`, а не `user:*`.)
