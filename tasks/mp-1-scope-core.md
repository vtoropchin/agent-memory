# Задача MP-1 — Ядро модели scope (набор scope в поиске + идентичность проекта)

**Статус:** ✅ выполнена (2026-07-11). Билд `-tags sqlite_fts5` чист; `go test` весь зелёный
(добавлен `TestSearchLexicalScopes` + `config_test.go`); арх-ревью — соответствует §2.1,
отклонений нет; code review — 0 находок; симплификация — код чист. Разблокирует 3.2/3.4/3.6/5.2/MP-2.
**Область:** `internal/memory/types.go`, `internal/memory/store`, `internal/config`, DESIGN.md
**Зависимости:** нет

## Контекст

Мультипроектность (DESIGN.md §2.1): один общий файл БД, проекты разделены scope, модель —
**изоляция проектов + общий слой `user:*`**. Сейчас поиск фильтрует по ОДИНОЧНОМУ scope
(`store/sqlite.go`: `if q.Scope != "" { "m.scope = ?" }`), а recall без scope не фильтрует —
это утечка контекста между проектами. Нужно перейти на **набор scope** (`scope IN (...)`)
и завести идентичность проекта в конфиге. Это foundational-задача: векторный поиск (3.2)
и recall (3.4/mp-2) наследуют предикат отсюда, поэтому она идёт до них.

## Что сделать

1. **`internal/memory/types.go`** — добавить в `Query` поле `Scopes []string` (набор
   пространств для фильтра; пусто → откат на одиночный `Scope`; оба пусты → без фильтра).
   Комментарий со ссылкой на §2.1. Поле `Scope` НЕ удалять (его использует одиночный
   write-путь/дедуп).
2. **`internal/memory/store/sqlite.go`** — единый хелпер предиката scope, например
   `scopePredicate(scopes []string, scope string) (clause string, args []any)`:
   - `len(scopes) > 0` → `m.scope IN (?, ?, …)` со всеми scopes;
   - иначе `scope != ""` → `m.scope = ?`;
   - иначе → пустой clause (без фильтра).
   Применить его в `SearchLexical` ВМЕСТО текущего одиночного условия. (Метод
   `SearchVector` появится в 3.2 — он обязан использовать ЭТОТ ЖЕ хелпер; здесь не
   реализуется.) Интерфейс `Store` не менять.
3. **`internal/config`** — секция scope:
   ```toml
   [scope]
   project = "myproject"   # → project:myproject; пусто = мультипроектность выключена
   shared  = ["user:*"]    # пространства, всегда добавляемые к recall по умолчанию
   ```
   Тип `ScopeConfig{Project string; Shared []string}`; в `Config`. Дефолт (`Default()`):
   `Project = ""`, `Shared = ["user:*"]`. Хелперы-методы:
   - `SaveScope() string` → `"project:"+Project`, если `Project != ""`, иначе `""`.
   - `RecallScopes() []string` → `append(["project:"+Project], Shared...)`, если
     `Project != ""`, иначе `nil`.
   Разрешение приоритета (явное > дефолт) реализуется в mcp — это задача MP-2; здесь только
   конфиг и хелперы + их юнит-тест.
4. **DESIGN.md** — §2.1 уже описывает модель; при необходимости синхронизировать §4/§13
   (упомянуть `[scope]` в примере конфига §13).

## Тесты

- `store_test.go`: `SearchLexical` с `Query.Scopes = ["project:a","user:*"]` возвращает
  записи обоих scope и НЕ возвращает `project:b`; с одиночным `Scope` — прежнее поведение
  (регрессия); без scope — без фильтра.
- `config`: `SaveScope()`/`RecallScopes()` при заданном и пустом `Project`; парсинг
  `[scope]` из TOML поверх дефолтов (`shared` перезаписывается корректно).

## Границы

Не трогать: `recall`, `mcp`, `cmd/memoryd`, `write`, `embed`, `reason`, `maint`.
Дефолтную подстановку scope в вызовах инструментов делает MP-2, не эта задача.

## Критерий приёмки

- Билд чист; `CGO_ENABLED=1 go test -tags sqlite_fts5 -count=1 ./...` зелёный целиком
  (существующие тесты не ломаются — одиночный `Scope` работает по-прежнему).
