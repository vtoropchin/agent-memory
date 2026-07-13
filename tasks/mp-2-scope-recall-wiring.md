# Задача MP-2 — Разрешение scope в recall/save и связка в memo

**Статус:** ✅ выполнено 2026-07-11. `mcp.NewServer` теперь принимает `config.ScopeConfig`;
`handleSave` подставляет `SaveScope()` при пустом входном scope (Writer.Save сам
возвращает `ErrEmptyScope`, если и дефолт пуст); `handleRecall` разрешает набор scope
через `resolveRecallScopes` (приоритет: `in.Scopes` > `in.Scope` > `cfg.RecallScopes()`);
схема `memory_recall` получила поле `scopes` (back-compat с одиночным `scope`);
`cmd/memo` пробрасывает `cfg.Scope`; `config.example.toml` дополнен секцией `[scope]`.
Новые тесты `internal/mcp/server_test.go`: `TestResolveRecallScopes` (5 подслучаев),
`TestSaveDefaultScopeFromInstance` (3 подслучая). Билд и `go test -tags sqlite_fts5
-count=1 ./...` зелёные целиком (все пакеты PASS).
**Область:** `internal/mcp`, `cmd/memo`, `internal/memory/recall` (только проброс)
**Зависимости:** MP-1 (Query.Scopes, ScopeConfig, хелперы)

## Контекст

MP-1 добавил `Query.Scopes`, предикат набора scope в store и `ScopeConfig` с хелперами
`SaveScope()`/`RecallScopes()`. Теперь — разрешение scope на входе инструментов
(DESIGN.md §2.1): дефолт инстанса из конфига, с приоритетом явного значения из вызова.
Модель: recall по умолчанию = `project:<name>` + `user:*`, save по умолчанию =
`project:<name>`.

## Что сделать

1. **Проброс конфига scope в mcp.** `mcp.NewServer` (или его зависимости) получает
   `config.ScopeConfig` (или уже готовые `SaveScope()`/`RecallScopes()`), чтобы хендлеры
   могли разрешать дефолты. `cmd/memo` передаёт `cfg.Scope`.
2. **handleSave** (`internal/mcp/save.go`): если во входе scope пуст — подставить
   `cfg.SaveScope()`. Если и он пуст (project не задан) — вернуть доменную ошибку
   `ErrEmptyScope` как сейчас (мультипроектность требует настроенного project). Явный scope
   из вызова — использовать как есть.
3. **handleRecall** (`internal/mcp/recall.go` + `schema.go`): добавить в схему `memory_recall`
   опциональное поле `scopes` (массив строк) рядом с существующим `scope` (одиночный,
   back-compat). Разрешение (реализовать в хендлере):
   - непустой `scopes` из вызова → `Query.Scopes = scopes`;
   - иначе непустой `scope` → `Query.Scopes = [scope]` (или оставить `Query.Scope`);
   - иначе → `Query.Scopes = cfg.RecallScopes()` (дефолт `project + user:*`);
   - если и дефолт пуст (project не задан) → без фильтра (adhoc-режим).
   Описание полей в схеме — упомянуть модель «свой проект + user:*» и что пустое поле =
   дефолт инстанса.
4. **recall.Recaller** — только пробросить `Query.Scopes` в `store.SearchLexical`/
   `SearchVector` (store уже умеет по MP-1/3.2). Никакой scope-логики в recall не хардкодить.
5. **config.example.toml** — секция `[scope]` с комментариями (project, shared, что пустой
   project выключает мультипроектность).

### Координация с 3.4

Если 3.4 (гибридный recall) ещё не выполнена — она просто передаёт `q` в `SearchVector`,
scope уже разрешён здесь; конфликта нет. Если 3.4 уже выполнена — убедиться, что векторный
путь тоже получает `Query.Scopes` (он идёт через тот же `q`).

## Тесты

- `mcp/server_test.go`: recall без scope при заданном `project` → в `Query.Scopes`
  попадают `project:<name>` и `user:*` (проверить через фейковый store, фиксирующий Query);
  явный `scopes` перекрывает дефолт; save без scope пишет в `project:<name>`; save без
  scope при пустом project → ошибка.

## Границы

Не трогать: `store` (готово в MP-1/3.2), `write`-домен (кроме того, что scope приходит
готовым), `embed`, `reason`, `maint`.

## Критерий приёмки

- Билд чист; `CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./...` зелёный целиком.
- e2e (оркестратор): два инстанса memo с разными `project` над одним файлом БД — recall
  каждого видит свой проект и `user:*`, но не чужой `project:*`.
