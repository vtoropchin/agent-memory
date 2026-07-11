---
name: progress
kind: project
importance: 5
tags: [roadmap, progress]
---

Состояние проекта на 2026-07-11: **Этап 1 (MVP) закоммичен** (`f9fd7c5`),
**Этап 2 (Reasoner) закоммичен** (`6479186`), **задача 3.1 (Ollama-Embedder) закоммичена**
(`153524d`), **планирование мультипроектности закоммичено** (`c09c9ab` — только DESIGN §2.1
+ task-файлы mp-1/mp-2, без кода). **Задача MP-1 (ядро модели scope) закоммичена** (`d480d62`).
**Задача MP-2 (разрешение scope в save/recall + связка в memoryd) выполнена и финализирована**
в рабочем дереве (коммит в этой сессии). Бэклог этапов 3–5 нарезан по файлам в `tasks/`
(см. [[task-files]]); **каждая задача выполняется в отдельной новой сессии** — указание
пользователя.

**Сделано в MP-2 (✅, [[scope-multiproject]]):** разрешение scope на входе MCP-инструментов.
`mcp.NewServer(w, r, s, scope config.ScopeConfig)` — Server хранит `scope`. `handleSave`:
пустой входной scope → `s.scope.SaveScope()`; если и он пуст — `Writer.Save` сам вернёт
`write.ErrEmptyScope` (проверка домена не дублируется в mcp). `handleRecall` через новый
метод `resolveRecallScopes(in)` заполняет `Query.Scopes` по приоритету: `in.Scopes` >
одиночный `in.Scope` (back-compat) > `s.scope.RecallScopes()` (project + Shared) > `nil`
(adhoc, без фильтра). Схема `memory_recall` получила поле `scopes []string` рядом с
`scope`. `cmd/memoryd` пробрасывает `cfg.Scope`; `config.example.toml` — секция `[scope]`;
DESIGN §12 — пример recall с `scopes`. **Пакет `recall` не тронут:** `candQuery := q` уже
пробрасывает `Query.Scopes` в store. **Проверено (факты):** билд+vet `-tags sqlite_fts5`
чисты; `go test ./...` весь зелёный; арх-ревью go-reviewer — «соответствует §2.1», 1
косметика (пример §12, устранён); code review high — 0 находок; симплификация — заменил
хендмейд `equalStrings` на stdlib `slices.Equal`. **e2e verify:** `TestScopeIsolationE2E` —
два Server с разными project над ОДНИМ файлом БД через полный read-path (handleRecall →
Recaller.Recall → store): каждый видит свой `project:*` + `user:*`, чужой `project:*` не
протекает. Тесты `TestResolveRecallScopes` (5), `TestSaveDefaultScopeFromInstance` (3).

**Сделано в MP-1 (✅, [[scope-multiproject]]):** foundational-задача набора scope.
`Query.Scopes []string` в `types.go` (пусто → откат на одиночный `Scope`; оба пусты → без
фильтра). Единый хелпер `scopePredicate(scopes, scope) (clause, args)` в `store/sqlite.go`:
`m.scope IN (…)` > `m.scope = ?` > пустой clause; применён в `SearchLexical` (для
`SearchVector` из 3.2 — та же точка). `ScopeConfig{Project, Shared}` в `config.go` +
методы `SaveScope()` (→ `project:<name>` или `""`), `RecallScopes()` (→ `[project:<name>]+Shared`
или `nil`); дефолт `Project=""`, `Shared=["user:*"]`. DESIGN §13 — секция `[scope]` в примере.
Границы соблюдены: `recall`/`mcp`/`memoryd`/`write` не тронуты — дефолтную подстановку scope
в вызовы инструментов делает MP-2. **Проверено (факты):** билд `-tags sqlite_fts5` чист;
`go test` весь зелёный (`TestSearchLexicalScopes` + `config_test.go`); арх-ревью go-reviewer
— соответствует §2.1; code review high — 0 находок; симплификация — чисто. `verify`
неприменим (нет рантайм-поверхности MCP; проводка — MP-2).

**Сделано на Этапе 3 (задача 3.1, ✅):** пакет `internal/memory/embed` — новый
`ollama.go`: тип `Ollama` (реализует `Embedder`), `NewOllama(ctx, baseURL, model, opts…)`
с **пробным эмбеддингом** в конструкторе (определяет `dim` из ответа, проверяет живость
ollama/модели; ошибка → ошибка конструктора, откат на Noop оставлен вызывающему — задача
3.6), опция `WithHTTPClient`, `Embed` (POST `{baseURL}/api/embed`, `{"model","input"}` →
`{"embeddings"}`, не-2xx / несовпадение количества → ошибка, пустой texts → nil без
запроса), кэш `map[string][]float32` под `sync.RWMutex` по ключу `sha256(model+"\x00"+text)`
со сбросом целиком при >10000 (простая стратегия без LRU). Только stdlib, соседние пакеты
не тронуты. `ollama_test.go` — 12 тестов через `httptest.Server`, живых вызовов ollama нет.
**Проверено (факты):** `CGO_ENABLED=1 go build -tags sqlite_fts5 ./...` чист; тесты пакета
embed PASS (12/12); арх-ревью go-reviewer — «соответствует дизайну», отклонений нет; code
review — багов нет; симплификация — код чист (один осознанный skip). **Тех-долг:** `truncate`
дублируется в `embed/ollama.go` и `reason/deepseek.go` — при удобном случае вынести в
shared-util. `verify` неприменим (нет рантайм-поверхности/живой ollama; связка — задача 3.6).

**Сделано на Этапе 2** (по задачам tasks/2.1–2.4, все ✅):
- `reason.DeepSeek` — OpenAI-совместимый клиент chat completions (ключ из env через
  `cmd/memoryd`, опция WithBaseURL для httptest).
- Суммаризация recall: `Query.Summarize` → секция `### Сводка`; пустой результат в LLM
  не ходит; `ApproxTokens` пересчитывается по итоговому Markdown.
- `maint.Consolidator` — episodic→semantic: группы по scope целиком (batchLimit не режет
  кластер), дедуп source_ids, merged-факты не портят чужой summary и считаются отдельно
  (`Merged`), пер-групповая деградация и для ошибок store; `SaveInput.Summary` пишется
  одной операцией; `Store.ListEpisodicForConsolidation` (без Tags/Links, отражено в §6).
- Связка: `newReasoner` в memoryd — деградация до Noop при пустом ключе ИЛИ пустой model,
  fail-fast только на неизвестный provider; `config.example.toml`.

**Проверено (факты):** `CGO_ENABLED=1 go build/test -tags sqlite_fts5 ./...` — все пакеты
зелёные; финализация полная: арх-ревью («соответствует», 2 note), code review высокого
уровня — 10 подтверждённых находок, все исправлены; e2e verify живого бинаря через MCP
stdio: save→recall, деградация без ключа (Noop + warning, stdout чист), живой DeepSeek
вернул корректную «### Сводку». Ключ — в `.env` (в .gitignore; из git-индекса удалён,
т.к. случайно был застейджен).

**Инцидент:** пользователь `git checkout`-ом откатил правки Этапа 2 в отслеживаемых файлах
(новые файлы уцелели) — восстановлено агентами по файлам задач. Урок: коммитить сразу
после финализации этапа.

**Следующий шаг:** задача [tasks/3.2](../../tasks/3.2-sqlite-vec-store.md) (sqlite-vec в
Store) — разблокирована (MP-1 применил scopePredicate в SearchLexical, та же точка для
SearchVector). После 3.2 открываются 3.3/3.4/3.5. MP-2 закрыта → 5.2 ждёт только 5.1.
Порядок и зависимости — tasks/README.md. ollama+bge-m3 у пользователя есть.

**Как применять:** обновляй этот файл по мере закрытия задач/этапов. Отмечай, что реально
собрано/протестировано/запущено (с фактами), а не только «сделано».
