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
**Задача MP-2 (разрешение scope в save/recall + связка в memoryd) закоммичена** (`e2d0967`).
**Задача 3.2 (sqlite-vec в Store) закоммичена** (`6ed21a2`).
**Задача 3.3 (вектор в write path) закоммичена** (`b46baff`).
**Задача 3.4 (гибридный recall) закоммичена** (`f8fb3d2`).
**Задача 3.5 (reindex при смене модели + досчёт очереди) закоммичена** (`8789db2`).
**Задача 3.6 (связка Embedder в memoryd) выполнена и финализирована** в рабочем дереве
(коммит в этой сессии) — **Этап 3 полностью закрыт, векторный слой работает end-to-end
через бинарь**. Бэклог этапов 3–5 нарезан по файлам в `tasks/` (см. [[task-files]]);
**каждая задача выполняется в отдельной новой сессии** — указание пользователя.

**Сделано на Этапе 3 (задача 3.6, ✅):** связка реального Embedder в `cmd/memoryd`. Новая
фабрика `newEmbedder(ctx, cfg config.EmbedderConfig) (embed.Embedder, error)` — симметрична
`newReasoner`: `""`/`"noop"` → `embed.NewNoop()`; `"ollama"` → `embed.NewOllama(ctx, BaseURL,
Model)`, при ошибке (сервер недоступен / модель не подтянута) → предупреждение в stderr +
откат на Noop (деградация §15, recall на чистом FTS5), наружу ошибка НЕ пробрасывается;
неизвестный provider → ошибка старта (fail-fast). `ctx, cancel := signal.NotifyContext`
поднят выше по `run()` (до фабрик), т.к. `NewOllama` делает пробный эмбеддинг в конструкторе
и требует ctx; порядок defer LIFO (`cancel()` до `s.Close()`). Полученный `embedder` идёт в
`write.New`/`recall.New` вместо двух `embed.NewNoop()` (заглушка удалена). При `embedder.Dim()
> 0` (т.е. Embedder ≠ Noop) после сборки, но ДО `srv.Run`, в **фоновой горутине** по общему
`ctx` запускается `maint.NewReindexer(s, embedder).CheckAndReindex(ctx)` с логом
`reindexed=.. embedded=.. failed=..` (или ошибки) в stderr — досчёт векторов, отложенных
прошлыми запусками; горутина НЕ блокирует старт MCP-сервера. `config.example.toml` — в
`[embedder]` комментарий про пробный эмбеддинг/автоопределение dim/откат на noop; `[scope]`
не тронут. **Проверено (факты):** `CGO_ENABLED=1 go build/vet/test -tags sqlite_fts5
-count=1 ./...` — всё зелёное (10 пакетов; `cmd/memoryd` ~0.6–1.8s). Тесты `main_test.go`:
`TestNewEmbedder` (noop / пустой provider / ollama недоступен через немедленно закрытый
httptest → откат на Noop / неизвестный provider → ошибка), `TestNewEmbedder_OllamaAvailable`
(httptest с `{"embeddings":[[...]]}` → `*embed.Ollama`, `Dim()==3`). Арх-ревью go-reviewer —
«соответствует §5/§9/§13/§15», отклонений 0 (границы: только `cmd/memoryd`+`config.example.toml`;
`Dim()>0`-гвард консистентен с write/recall/maint, type assertion только в тестах). Code
review high — 0 корректностных багов (shutdown-гонка горутины vs `s.Close()` разобрана: после
Close операции store дают `sql: database is closed` — проглатываются по дизайну maint, НЕ
паника; терминация drainMissing гарантирована `progressed==0`; defer LIFO даёт горутине шанс
среагировать на ctx). Симплификация — код чист (симметрия с `newReasoner` — осознанный
паттерн, экстракция общего хелпера отклонена: разные интерфейсы/конструкторы/деградация).
**e2e verify через ЖИВОЙ бинарь + ЖИВОЙ ollama/bge-m3 (1024-dim), 3 сценария по MCP stdio:**
(A) provider=ollama — save факта → recall по **перефразировке без общих токенов** («как
компилировать проект и какие опции указывать» vs «…запускать сборку через Makefile с флагом
sqlite_fts5») **нашёл запись** = сработал ВЕКТОРНЫЙ источник (FTS5 unicode61 без стемминга
лексически не матчит), в stderr лог `фоновый reindex завершён: reindexed=true`; (B)
provider=ollama на битый порт — предупреждение + откат на Noop, save/recall на FTS5, та же
перефразировка НЕ находится (подтверждает, что в A сработал именно вектор), горутина reindex
не запускалась; (C) provider=noop — прежнее поведение, чисто. **Границы:** `embed/store/write/
recall/maint/mcp/config`(структуры) не тронуты — только связка. **Мелкий пре-существующий
косметик (вне рамок 3.6):** при SIGTERM `sdk.Run` возвращает `context.Canceled`, `run()`
оборачивает в ошибку → `main` log.Fatalf (ненулевой код выхода на штатном шатдауне) — сигнальная
обработка не из этой задачи.

**Сделано на Этапе 3 (задача 3.5, ✅):** reindex-джоб. Новый пакетный тип
`maint.Reindexer{store, embedder}` + `NewReindexer(s, e)` в `internal/memory/maint/reindex.go`;
единственная точка входа `CheckAndReindex(ctx) (ReindexResult{Reindexed bool, Embedded, Failed
int}, error)`. Логика: `Dim()<=0` (Noop) → мгновенный no-op (Embed/EmbedMeta не зовутся); иначе
`EmbedMeta` сверяется с `Model()/Dim()` — расхождение (в т.ч. пустая meta) → `ResetVectorIndex` +
перегон `AllForReindex` батчами по 64 (`reindexBatchSize`), `Reindexed=true`; независимо от
reindex — **всегда** досчёт очереди `drainMissing`: цикл `ListMissingVectors(ctx, 256)` теми же
батчами до пустой очереди. **Наружу только** ошибки `EmbedMeta/ResetVectorIndex/AllForReindex` и
`ctx.Err()` (проверяется между батчами в `embedItems`); ошибки `Embed/PutVector/ListMissingVectors`
— лог в stderr + продолжение (§15 деградация). **Две гарантии терминации `drainMissing`:** пустая
страница ИЛИ `progressed==0` (за целую страницу не сохранено НИ одного вектора — иначе тот же набор
вернулся бы снова = бесконечный цикл при стабильно недоступном Embedder). Приватные хелперы:
`drainMissing`, `embedItems` (батчинг+ctx), `embedBatch` (гварды точь-в-точь как `write.embedNow`:
ошибка Embed валит батч → `Failed+=len`, `len(vecs)<len(batch)` → хвост в Failed, пустой вектор /
ошибка PutVector отдельной записи → `Failed++`, остальные продолжают). **Осознанная особенность
метрики:** `Failed` — счётчик ПОПЫТОК, не уникальных записей: при стабильной ошибке Embed одна
запись учитывается несколько раз (полный reindex + немедленный шаг d, и на разных страницах
досчёта) — задокументировано в годоке поля; `Embedded` при этом точен. **Проверено (факты):**
`CGO_ENABLED=1 go build/vet/test -tags sqlite_fts5 -count=1 ./...` — всё зелёное (10 пакетов; maint
= 13 тестов поверх ЖИВОГО sqlite-vec: `SearchVector` реально находит сохранённые reindex'ом
векторы). Тесты: пустая meta→полный reindex+meta записана+вектора в KNN; совпадающая meta→только
досчёт вставленной через Store.Put записи; смена модели→пересоздание+перегон; частичный сбой батча
(первый батч падает, второй эмбеддится — упавшие остаются в `ListMissingVectors`); `n<len(batch)` и
пустой вектор части записей→корректный Failed без блокировки успешных; **ctx-отмена между батчами**
(через store-обёртку `cancelAfterNPutVector`, отменяющую после 64-го PutVector — надёжнее, чем
cancel внутри embedFn, который у mattn/go-sqlite3 детерминированно валит весь батч; проверено
30 прогонов не-флаки) → `errors.Is(ctx.Canceled)`, батч1 заэмбеджен, батч2 не тронут; Noop→no-op
(счётчик Embed==0, ResetVectorIndex не звался). Fake-Embedder получил хук `embedFn func(call, texts)`
для выборочного/частичного сбоя (обратно совместим с полями err/calls/dim/model). **Арх-ревью
go-reviewer — «соответствует §6/§9/§11/§15», отклонений 0** (границы maint: только через
`store.Store`+`embed.Embedder`, отдельной таблицы-очереди нет — очередь это `ListMissingVectors`).
**Code review high — 0 корректностных багов** (2 finder'а: логика/терминация/маршрутизация ошибок
чисты; «двойной учёт Failed» классифицирован как метрика, консистентная со спекой, не баг); находки
были только по покрытию тестами → закрыты доработкой (3 новых сценария). Симплификация: reuse-нота
`embedBatch`↔`write.embedNow` — экстракцию отклонили (batch vs single-item, разные сигнатуры).
Verify: рантайм-поверхность покрыта интеграц. тестами поверх живого sqlite-vec; e2e через бинарь
недостижим (Reindexer подключается к memoryd только в 3.6). **Границы:** `cmd/memoryd`, `recall`,
`write`, `mcp`, `config`, `store` не тронуты (store — только вызовы). Планировщик по расписанию —
задача 4.3.

**Сделано на Этапе 3 (задача 3.4, ✅):** гибридный recall — векторный источник кандидатов
подключён в read path рядом с лексическим через существующий RRF. `recall.New(s, cfg,
reasoner, embedder embed.Embedder, opts…)` — Recaller получил зависимость Embedder (параметр
добавлен перед `opts`). В `Recall` заглушка `SearchVector(ctx,nil,candQuery)` заменена на
приватный `searchVector(ctx, q.Text, candQuery)`: `Dim()<=0` (Noop) ИЛИ пустой `TrimSpace(text)`
→ `Embed` НЕ зовётся, `nil` (векторный источник выключен); иначе `Embed(ctx,[text])` →
`store.SearchVector(ctx, vecs[0], candQuery)`. Три ветки отказа (ошибка Embed / `len(vecs)<1` /
ошибка SearchVector) → `log.Printf` в stderr + продолжение ТОЛЬКО с FTS — §15 деградация.
**Поведенческое изменение (осознанное, по заданию):** раньше ошибка `SearchVector` пробрасывалась
наружу и роняла весь `Recall`; теперь глотается — векторный слой опционален. `fuseRRF` не тронут
(уже корректно суммирует вклады по `Memory.Id` при 1..N источниках, пустой/nil источник
пропускает — поэтому `Recall` линейный без `if vector!=nil`). Один и тот же `candQuery` (с уже
разрешёнными `Query.Scopes` от MP-2) идёт в оба `Search*` — scope-логика в recall НЕ дублируется.
Механически поправлены места вызова конструктора: `cmd/memoryd/main.go` (`embed.NewNoop()` +
комментарий, что реальный Embedder — задача 3.6), `internal/mcp/server_test.go`, все ~15 вызовов
в `recall_test.go`. **Проверено (факты):** `CGO_ENABLED=1 go build/vet/test -tags sqlite_fts5
-count=1 ./...` — всё зелёное (10 пакетов; recall-тесты гоняют ЖИВОЙ sqlite-vec + fakeEmbedder).
4 новых теста: `TestRecall_FoundOnlyByVector` (перефразировка `{собирать,тестировать,репозиторий}`
без общих токенов с content — FTS `unicode61` без стемминга не находит, всплывает ТОЛЬКО вектором
→ снятие ограничения FTS на русскую морфологию, [[mvp-notes]]), `TestRecall_FoundByBothSourcesRanksHigher`
(равные importance/recency/access, `now` через `WithNow` → изолирован сигнал RRF: запись из обоих
источников получает лишний `~1/61` и ранжируется выше), `TestRecall_EmbedErrorDegradesToLexical`,
`TestRecall_NoopEmbedderNeverCallsEmbed` (счётчик Embed==0). Арх-ревью go-reviewer — «соответствует
§8/§9/§15», отклонений нет. Code review high — 0 находок (два finder-агента + отдельный аудит
тестов: guard `len(vec)==0` в store.SearchVector делает `[][]float32{nil}` безопасным). Симплификация
— чисто (reuse-заметка: `searchVector` и `write.embedNow` делят скелет, но лог-сообщения и хвостовое
действие расходятся — экстракцию отклонили как маргинальную). **Verify:** векторный путь recall
прогнан e2e тестом `TestRecall_FoundOnlyByVector` поверх живого sqlite-vec; e2e через бинарь
недостижим (embedder в memoryd = Noop до 3.6). **Косметич. тех-долг:** устаревший комментарий в
`store/sqlite.go:484` («recall передаёт nil… Этап 3.4») — вне границ задачи (store off-limits),
подчистить при следующем касании store.

**Сделано на Этапе 3 (задача 3.3, ✅):** вектор в write path. `write.New(s, e embed.Embedder,
opts…)` — Writer получил зависимость Embedder; приватный `embedNow(ctx,id,content)` зовётся из
`Save` и из `Update` (только при `in.Content != nil`). `Dim()<=0` (Noop) → Embed НЕ вызывается
(дешёвая деградация, не «результат игнорируется»); иначе `Embed`+`store.PutVector`. Любая
ошибка (Embed / пустой ответ `len(vecs)<1` / PutVector) → запись всё равно сохранена, ошибка
только в лог stderr — §15 деградация, а не падение; вектор досчитает фон-джоб (3.5). Новый метод
`Store.ListMissingVectors(ctx,limit)`: записи без `superseded_by` и без строки в `memories_vec`
(LEFT JOIN … IS NULL); нет vec-таблицы → `nil,nil` (через `hasVecTable`); Tags/Links не грузятся
(как AllForReindex) — это и есть «очередь досчёта» (§7.4/§9, отдельной таблицы нет). **Попутный
багфикс `PutVector`:** `INSERT OR REPLACE` → `DELETE`+`INSERT` в транзакции — виртуальная vec0 не
поддерживает `OR REPLACE` по PK, ловила `UNIQUE constraint failed` ровно на пути переэмбеддинга
при `Update` (арх-ревью и code review признали фикс обоснованным, в рамках store-пакета задачи).
Механически поправлены места вызова конструктора: `cmd/memoryd/main.go` (передан `embed.NewNoop()`
+ комментарий, что реальный Embedder — задача 3.6), тесты `mcp`/`maint`/`write`. **Проверено
(факты):** `CGO_ENABLED=1 go build/vet/test -tags sqlite_fts5 ./...` — всё зелёное (10 пакетов,
тесты гоняют ЖИВОЙ sqlite-vec: Save кладёт вектор → SearchVector KNN находит его distance 0). Code
review high — 1 находка (паника `vecs[0]` без проверки длины в `embedNow`) → закрыта guard'ом
`len(vecs)<1`. Симплификация — чисто. Арх-ревью go-reviewer — «соответствует §4/§5/§6/§7/§9/§15».
Verify: рантайм-поверхность покрыта интеграц. тестами поверх живого sqlite-vec; e2e через бинарь
недостижим (embedder в memoryd = Noop до 3.6). **Тех-долг:** фоновый джоб, реально дренирующий
`ListMissingVectors`, — задача 3.5.

**Сделано на Этапе 3 (задача 3.2, ✅):** подключён `github.com/asg017/sqlite-vec-go-bindings`
(CGO). `Open()`: `sqlite_vec.Auto()` до `sql.Open` — регистрирует vec0 для всех соединений.
`ResetVectorIndex(model,dim)` в транзакции: `DROP TABLE IF EXISTS memories_vec` + `CREATE
VIRTUAL TABLE memories_vec USING vec0(memory_id TEXT PRIMARY KEY, embedding float[<dim>]
distance_metric=cosine)` (dim через fmt.Sprintf — из кода, не юзер-ввод) + upsert `embed_meta`.
Векторной таблицы **нет в статической схеме** (dim неизвестен заранее) — появляется только
после первого reindex. `SearchVector(vec,q)`: guard `len(vec)==0 → nil,nil` (recall шлёт nil
до 3.4 — деградация §15, не dimension mismatch); `hasVecTable` (sqlite_master) — нет таблицы
→ `nil,nil`; KNN через **CTE** (`WITH knn AS (SELECT memory_id,distance FROM memories_vec
WHERE embedding MATCH ? AND k=?)` затем JOIN memories + фильтры) — vec0 не джойнит MATCH+k с
внешними фильтрами в одном SELECT; `Score=1/(1+distance)`, `Source="vector"`, ORDER BY
distance ASC; Tags не грузятся (read-hot-path). `PutVector`: `INSERT OR REPLACE` +
`SerializeFloat32`; нет таблицы → **ошибка** (не молчит). `AllForReindex`: теперь
`WHERE superseded_by IS NULL` + грузит tags/links. **Симплификация:** вынес общий
`metaFilters(q) → (conds,args)` (superseded+scope+kind+since) — переиспользуют и SearchLexical,
и SearchVector; §8 требует идентичной фильтрации обоих источников (раньше был вынесен только
`scopePredicate`). **Проверено (факты):** билд+vet `-tags sqlite_fts5` чисты; `go test ./...`
весь зелёный (9 пакетов; тесты гоняют ЖИВОЕ расширение sqlite-vec: реальные vec0/KNN/serialize).
Арх-ревью go-reviewer — «соответствует §4/§6/§8/§9/§15», 1 замечание (KNN k-before-filter —
не отклонение, предписано заданием, к учёту в 3.4). Code review high — подтвердил тот же
k-before-filter + латентную cross-file мину: `recall` зовёт `SearchVector(ctx,nil,q)`; после
появления vec-таблицы (3.6) пустой вектор ушёл бы в MATCH → падение recall — **закрыто guard'ом
`len(vec)==0`**. Симплификация — вынес `metaFilters` (см. выше); удаление tags/links из
AllForReindex **отклонено** (задание явно требует «с tags/links»). **Известное ограничение
KNN — в tasks/3.2 и [[scope-multiproject]], учесть в 3.4.** `verify`: рантайм-поверхность —
методы Store над живым sqlite-vec, покрыты интеграционными тестами; e2e через бинарь недостижим
(вектор не подключён к memoryd — это 3.6).

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

**Сделано на Этапе 4** (по задачам tasks/4.x):
- **4.2 (Dedup-sweep)** ✅ — ретроактивная чистка почти-дублей. Новый пакетный тип
  `maint.DedupSweeper{store}` + `NewDedupSweeper(s)`; единственная точка входа
  `Sweep(ctx) (DedupResult{Scanned,Superseded}, error)` в `internal/memory/maint/dedup.go`.
  Из пакета `write` экспортирована ОБёртка `write.IsNearDuplicate(a, b string) bool`
  (принимает СЫРОЙ content, внутри `normalizeContent`+приватный `isNearDuplicate` — SimHash/порог
  `simhashDistanceThreshold` не задублированы, логика write не менялась). Логика Sweep:
  `AllForReindex` (все живые) → группировка по паре **(Scope, Kind)** через `map[dedupGroupKey]
  []Memory` (порядок групп на результат не влияет — группы независимы) → внутри группы записи
  сортируются по (CreatedAt, Id) для детерминизма → O(n²) попарное сравнение по content через
  `write.IsNearDuplicate`. **Ключевое (по итогам арх-ревью):** группировка по (Scope, Kind), а НЕ
  только по Scope — консистентно с write path (`dedup()` фильтрует `SearchLexical` по `Kinds:
  [kind]`): запись одного Kind никогда не supersede'ит запись другого Kind в том же scope, даже
  при идентичном content (episodic-наблюдение и semantic-факт — легитимно разные сущности).
  Выживает бо́льшая `Importance`, при равенстве — новее по `CreatedAt` (`pickDedupWinner`), при
  полном равенстве — детерминированный тай-брейк в пользу «чемпиона». `sweepGroup`: members[i] —
  «чемпион», каждый непроигравший j>i сравнивается с ним; проигравший → `Supersede(loser,winner)`
  + `processed[loser]=true` (больше не участвует ни как чемпион, ни как претендент), чемпион при
  необходимости переключается на победителя — цепочка похожих сходится к одной выжившей за проход.
  Деградация §15: ошибка `Supersede` на паре → `log.Printf` + продолжение; наружу только ошибка
  `AllForReindex`. **Проверено (факты):** `CGO_ENABLED=1 go build/vet/test -tags sqlite_fts5
  -count=1 ./...` — всё зелёное (10 пакетов; maint — 7 dedup-тестов поверх ЖИВОГО sqlite-store:
  supersede в пользу важного/нового, тай-брейк по CreatedAt, НЕ дедупит разные scope, НЕ дедупит
  разные Kind в одном scope, цепочка из трёх → одна выжившая, непохожие не трогаются, повторный
  Sweep идемпотентен — 0 новых supersede). Арх-ревью go-reviewer — соответствует §11/§7.2/§6/§9/§15;
  единственное отклонение (группировка только по Scope) устранено доработкой (→ Scope+Kind). Code
  review high (3 finder-угла) — **0 корректностных багов** (порядок аргументов Supersede,
  детерминизм champion-switch и тай-брейка, edge-кейсы пустого/одиночного входа — доказаны чистыми).
  Симплификация: схлопнул группировку с (упорядоч. слайс + idx-map + мёртвые поля `dedupGroup.scope/
  kind`) до `map[dedupGroupKey][]Memory`; повторный пересчёт SimHash в O(n²) оставлен осознанно
  (инкапсуляция simhash в write, фоновый нечастый джоб). **Verify неприменим:** dedup не подключён
  к рантайму (планировщик/wiring — 4.3), покрыт интеграц. тестами. **Границы:** `cmd/memoryd`,
  `config`, `mcp`, `recall`, `embed`, `store` не тронуты (store — только вызовы `AllForReindex`/
  `Supersede`); в `write` — только новый экспорт-обёртка.
- **4.1 (Decay)** ✅ — `Store.ListEpisodicForDecay(notAccessedSince, limit)` (episodic без
  `superseded_by`, `COALESCE(accessed_at, created_at) < notAccessedSince`, сортировка по этой
  дате ↑, `limit<=0` без лимита; Tags/Links не грузятся — как у ListEpisodicForConsolidation;
  отражён в §6). `maint.Decayer{store, *write.Writer}` + `NewDecayer`; `Decay(ctx, demoteAfter,
  archiveAfter, limit) (DecayResult{Scanned,Demoted,Archived}, error)`: кандидаты idle>demoteAfter;
  если idle>=archiveAfter И importance<=1 → архив self-supersede-тумстоуном `Supersede(id,id)`
  (как forget), иначе importance−1 (не ниже 1) через `Writer.Update` (переиспользует
  clampImportance). Пороги не хардкодятся — параметры (дефолты задаст планировщик 4.3).
  Пер-запись деградация: ошибка Store/Writer логируется, прогон продолжается. **Ключевая
  проверка:** `Writer.Update` трогает только `updated_at`, НЕ `accessed_at` — decay-часы
  (по accessed_at/created_at) не сбрасываются, запись доходит до архива за несколько прогонов.
- **Проверено (факты):** `CGO_ENABLED=1 go build/test -tags sqlite_fts5 ./...` — все пакеты
  зелёные (store, maint — новые тесты включ.); арх-ревью go-reviewer — «соответствует дизайну»
  (было замечание: DESIGN §6 не синхронизирован — исправлено, `ListEpisodicForDecay` добавлен
  в листинг Store); /code-review (high) — багов нет; /simplify — код чист, правок нет. `verify`
  неприменим: decay ещё не подключён к рантайму (планировщик/wiring — 4.3), покрыт юнит-тестами.

**Следующий шаг:** 4.1 и 4.2 закрыты — весь maint-слой (Decay/Consolidate/Reindex/DedupSweep)
готов как пакетные типы. Разблокирована **4.3 (Планировщик джобов)** — обе её зависимости (4.1,
4.2) готовы; туда подключится периодический прогон `Decayer`/`Consolidator`/`Reindexer`/
`DedupSweeper` (+ дефолты порогов decay). Параллельно — **Этап 5: 5.1 (импортёр файловой памяти)**
разблокирован, **5.2** ждёт 5.1. Порядок — tasks/README.md. ollama+bge-m3 у пользователя есть.

**Как применять:** обновляй этот файл по мере закрытия задач/этапов. Отмечай, что реально
собрано/протестировано/запущено (с фактами), а не только «сделано».
