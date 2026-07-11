# Задача MCP-INSTRUCTIONS — Серверные MCP `instructions` в memoryd

**Статус:** ✅ выполнена (2026-07-11). Добавлены `internal/mcp/instructions.go`
(`buildInstructions`), правка `internal/mcp/mcp.go` (второй аргумент `NewServer` теперь
`&sdkmcp.ServerOptions{Instructions: buildInstructions(scope)}`), тесты
`internal/mcp/instructions_test.go`, абзац в `DESIGN.md §12`. `CGO_ENABLED=1 go build/vet/test
-tags sqlite_fts5 -count=1 ./...` — зелёное. E2e проверено живым бинарём `cmd/memoryd` по MCP
stdio: ответ `initialize` содержит непустое `instructions` с `project:agentmemory`.
**Область:** `internal/mcp` (+ `DESIGN.md §12` синхронизация). `internal/*` логику
(`write`/`recall`/`store`/`maint`/`config`) и `cmd/memoryd` **не трогать**.
**Зависимости:** MP-1/MP-2 (`ScopeConfig`, `SaveScope()`/`RecallScopes()` — уже готовы;
`NewServer` уже принимает `config.ScopeConfig`).

## Контекст

Сейчас `internal/mcp/mcp.go` создаёт сервер как
`sdkmcp.NewServer(&sdkmcp.Implementation{...}, nil)` — второй аргумент (`*ServerOptions`)
равен `nil`, поэтому сервер **не публикует поле `instructions`** в ответе `initialize`.
Из-за этого в MCP-рукопожатии клиент видит только 4 инструмента с однострочными описаниями
и ноль протокола: когда звать recall, что такое «атомарный факт», какие дефолты scope.

Протокол работы с памятью сегодня живёт **только** в Claude-специфичном скиле `/memoryd`
(`.claude/skills/memoryd/SKILL.md`). Это нормально для Claude Code, но:
- другой MCP-клиент (opencode, codex, Cursor) скил прочитать не умеет — получит голые
  инструменты без протокола;
- даже в Claude Code, если скил не подгружен/не сработал, протокола нет.

`ServerOptions.Instructions string` («Optional instructions for connected clients») — штатный
способ отдать компактный протокол **любому** MCP-клиенту автоматически при `initialize`, без
триггера. Сервер уже знает `scope` (`ScopeConfig` в `NewServer`) → может подставить реальный
`project:<name>` инстанса в текст.

**Дублирование ядра со скилом осознанное:** сервер публикует переносимый минимум, богатый
проектный протокол остаётся в скиле `/memoryd` (его НЕ трогаем).

## Что сделать

1. **`internal/mcp/instructions.go` (новый файл).** Приватная функция
   `buildInstructions(scope config.ScopeConfig) string` — собирает компактный
   клиент-агностичный протокол. Содержание (ядро):
   - Одной фразой: что это долговременная память агента, 4 инструмента `memory_*`.
   - `memory_recall` — вызывать **в начале задачи**, возвращает Markdown-бриф релевантной
     памяти.
   - `memory_save` — сохранять **один атомарный факт** (1–3 предложения) за вызов.
   - `memory_update` / `memory_forget` — правка факта / мягкое удаление (тумстоун).
   - **Дефолты scope** с подставленным реальным значением из конфига:
     - если `scope.Project != ""` (через `SaveScope()`/`RecallScopes()`): save без scope →
       `project:<name>`; recall без scope → `project:<name>` + `user:*` (перечислить реальные
       строки из `RecallScopes()`); общие факты пользователя — явно `user:*`.
     - если `scope.Project == ""` (adhoc/одно-проектный режим): recall без scope → без
       фильтра (по всей БД); save **требует явного** `scope` (иначе доменная ошибка).
   - Текст короткий (ориентир — до ~15 строк), в тоне описаний инструментов; русский, как
     остальные пользовательские строки сервера.
2. **`internal/mcp/mcp.go`.** Заменить `nil` на
   `&sdkmcp.ServerOptions{Instructions: buildInstructions(scope)}` в `NewServer`
   (параметр `scope` уже в сигнатуре — новый проброс не нужен). Больше ничего в файле не
   менять.
3. **`DESIGN.md §12`.** Добавить короткий абзац: сервер при `initialize` публикует
   server-level `instructions` — компактный протокол, выведенный из `[scope]`, чтобы
   базовый протокол доезжал до любого MCP-клиента без Claude-скила. (Синхронизация дизайна
   с реализацией — иначе go-reviewer отметит рассинхрон.)

## Тесты

`internal/mcp/instructions_test.go` (или дополнить `server_test.go`), таблично:
- `TestBuildInstructions_Multiproject`: `ScopeConfig{Project:"agentmemory",
  Shared:["user:*"]}` → результат непустой, содержит подстроки `project:agentmemory` и
  `user:*`, упоминает «атомарный факт» и recall.
- `TestBuildInstructions_Adhoc`: `ScopeConfig{Project:""}` → результат непустой, НЕ содержит
  `project:` префикса как дефолта, упоминает, что save требует явного scope / recall без
  фильтра. Не паникует на пустом `Shared`.
- (опц.) проверка, что `NewServer(...)` с таким scope не паникует и Run-независимо
  конструируется (smoke).

## Границы

- **Только** `internal/mcp/*` + абзац в `DESIGN.md §12`.
- НЕ трогать: `internal/memory/{write,recall,store,maint,embed,reason}`, `internal/config`,
  `cmd/memoryd`, скил `.claude/skills/memoryd/SKILL.md`.
- Не менять описания/схемы 4 инструментов — только добавить server-level `instructions`.
- За рамки задачи не выходить.

## Критерий приёмки

- `CGO_ENABLED=1 go build/vet/test -tags sqlite_fts5 -count=1 ./...` — зелёное.
- Сервер публикует `instructions` при `initialize` (проверяется e2e на финализации:
  живой бинарь по MCP stdio, в ответе `initialize` присутствует непустое `instructions`
  с `project:agentmemory`).
- `DESIGN.md §12` синхронизирован.
