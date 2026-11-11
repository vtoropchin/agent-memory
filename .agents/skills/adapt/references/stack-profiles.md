# Профили стеков — подсказки для заполнения таблицы соответствий

Это **подсказки, а не готовые ответы**: значения проверяются по фактическому коду проекта.
Строка профиля, противоречащая тому, что видно в репозитории, проигрывает репозиторию.

## 1. Распознавание

| Признак в корне | Стек | Где обычно живут команды |
|---|---|---|
| `go.mod` | Go | `Makefile`, `go test ./...` |
| `pyproject.toml` / `setup.cfg` / `requirements.txt` | Python | `pyproject.toml` (scripts, tool.*), `tox.ini`, `Makefile` |
| `package.json` + `tsconfig.json` | TypeScript | `package.json` → `scripts` |
| `package.json` без `tsconfig.json` | JavaScript | там же |
| `pom.xml` / `build.gradle(.kts)` | Java / Kotlin | maven/gradle-таски |
| `*.csproj` / `*.sln` | C# | `dotnet build/test` |
| `composer.json` | PHP | `composer.json` → `scripts` (часто `app:check-build`, `test`, `cs`) |
| `Cargo.toml` | Rust | `cargo test`, `cargo clippy` |
| `Package.swift` / `*.xcodeproj` | Swift | `swift test`, `xcodebuild`, свой `Scripts/build.sh` |
| `CMakeLists.txt` / `Makefile` + `*.c/*.cpp` | C/C++ (часто встраиваемое) | `cmake --build`, `ctest`, прошивочные скрипты |
| `Gemfile` | Ruby | `rake`, `rspec` |
| `mix.exs` | Elixir | `mix test`, `mix credo` |

Монорепа (`workspaces`, `go.work`, gradle-модули, `packages/`, `apps/`) меняет ответ на «что такое
компонент»: компонент = пакет workspace, а не каталог.

## 2. Таблица соответствий по стекам

Колонки — строки таблицы из `manual-adaptation.md`, шаг 1.

| Стек | Компонент (единица развёртывания) | Порт | Структура через границу | `Main` | Ограничение видимости |
|---|---|---|---|---|---|
| Go | пакет / модуль | `interface`, объявленный **потребителем** | `struct` без методов | `func main` + `cmd/<app>` | экспорт по регистру имени, `internal/` |
| Python | пакет (каталог с `__init__.py`), дистрибутив | `Protocol` / ABC | `@dataclass(frozen=True)` | `__main__.py` / фабрика приложения | `_имя`, `__all__`, отсутствие ре-экспорта |
| TypeScript | пакет workspace / модуль | `interface` / `type` функции | `readonly`-объект, `type` | `main.ts` / `composition-root.ts` | `export` только из `index.ts`, `package.json#exports` |
| Java | модуль сборки / JPMS-модуль | `interface` | `record` | `main` + конфигурация DI | `public` только у API, `module-info.java` |
| Kotlin | gradle-модуль | `interface` / `fun`-тип | `data class` | `fun main` / DI-граф | `internal`, ограниченная видимость модуля |
| C# | проект (`.csproj`) / сборка | `interface` | `record` | `Program.cs` + регистрация DI | `internal`, `InternalsVisibleTo` |
| PHP | composer-пакет / слой каталогов | `interface` | `readonly class` / DTO | контейнер + провайдеры, точка входа | `private`/`final`, отсутствие autoload-экспорта |
| Rust | крейт workspace | `trait` | `struct` c `#[derive(Clone)]` | `fn main` в бинарном крейте | `pub(crate)`, границы крейта |
| Swift | таргет SPM / модуль | `protocol` | `struct` | `App` / `main.swift` + фабрика | `internal` по умолчанию, `public` точечно |
| C/C++ | единица компоновки / статическая библиотека, таргет CMake | заголовок с указателями на функции / абстрактный класс | `struct` POD | `main()` + сборка зависимостей | заголовки в `include/` vs приватные `src/`, `static` |
| Ruby | pack (`packwerk`) / gem | duck typing + явный модуль-контракт | `Struct`/`Data.define` | инициализатор приложения | `private_constant`, публичный API pack |
| Elixir | приложение OTP / контекст | `@behaviour` | struct модуля | `Application.start/2` + supervision tree | `@moduledoc false`, `defp`, `boundary` |

| Стек | Подмена в тестах | Проверка направления импортов | Анализ графа | Время и случайность |
|---|---|---|---|---|
| Go | подстановка интерфейса аргументом/полем | `go-arch-lint`, `depguard`, `internal/` | `go list -deps`, `go mod graph` | `Clock`, `IDGen` интерфейсами |
| Python | аргумент конструктора (не `monkeypatch`) | `import-linter` контракты, ruff banned-api | `grimp`, `pydeps` | порт `Clock`, инъекция `uuid4` |
| TypeScript | передача реализации в фабрику | `dependency-cruiser`, `eslint-plugin-boundaries` | `dependency-cruiser --output-type dot`, `madge` | порт `Clock`/`Random` |
| Java/Kotlin | конструктор + тестовая реализация | ArchUnit / Konsist, JPMS | `jdeps`, gradle `dependencies` | `Clock` из JDK как порт |
| C# | конструктор + фейк | NetArchTest, ArchUnitNET | ссылки проектов, `dotnet list reference` | `TimeProvider` как порт |
| PHP | конструктор + фейк | `deptrac`, `phparkitect`, `phpat` | `deptrac` граф | порт `ClockInterface` (PSR-20) |
| Rust | дженерик по трейту или `Box<dyn Trait>` | границы крейтов (компилятор) | `cargo tree`, `cargo-modules` | трейт `Clock` |
| Swift | инъекция протокола | тест по `import` в исходниках таргета, границы таргетов | граф зависимостей SPM | протокол `Clock` |
| C/C++ | подмена реализации при компоновке | ограничение include-путей, таргеты CMake | `include-what-you-use`, граф CMake | HAL-порт таймера |
| Ruby | инъекция объекта | `packwerk check` | `packwerk` граф | порт `Clock` |
| Elixir | `@behaviour` + конфиг реализации | `boundary`, `mix xref graph` | `mix xref graph --format dot` | порт `Clock` |

## 3. Что подчеркнуть в `AGENTS.md`/`CLAUDE.md` по семействам

- **Статически типизированные с явными интерфейсами** (Java, C#, Kotlin, Swift, Rust, Go) — набор
  применим буквально; сильнее всего работает принуждение через видимость и единицы сборки;
  метрика A (абстрактность) осмысленна.
- **Динамически типизированные** (Python, Ruby, JS, PHP отчасти) — порт часто существует как
  соглашение о вызове; A занижена и мало что значит, опирайтесь на ADP/SDP и организацию модулей;
  принуждение — линтер импортов и тесты. Записать это в `AGENTS.md`/`CLAUDE.md`, иначе ревьюер будет
  требовать явных объявлений портов там, где идиома их не требует.
- **Функциональные** (Elixir, F#, Scala-FP, функциональный TS) — порт = запись функций,
  передаваемая аргументом; `Main` = композиция функций; правило зависимостей действует так же.
- **Веб-стеки с богатыми фреймворками** (Laravel/Symfony, Spring, Django, Rails, NestJS) — главный
  риск: базовые классы, аннотации и активные записи ORM в ядре. Явно записать: доменная модель и
  persistence-модель — **разные типы**, маппинг живёт в шлюзе. Это единственное правило, которое
  окупается на таких проектах чаще всего.
- **Встраиваемые и системные** (C/C++, Rust на железе, прошивки) — добавляются HAL и OSAL
  (см. скил `details-and-deferral`); критерий успеха — сборка и прогон тестов **вне целевого
  железа**; условная компиляция заменяется подстановкой реализаций на этапе компоновки.
- **Мобильные** (Swift/SwiftUI, Kotlin/Android) — UI-фреймворк как деталь; presenter/view-model
  по «Скромному объекту»; ядро без импортов UIKit/SwiftUI/Android SDK.

## 4. Частые ложные срабатывания по стекам (фаза 7)

| Стек | Что ревьюер примет за нарушение | Как погасить в `AGENTS.md`/`CLAUDE.md` |
|---|---|---|
| Go | интерфейс объявлен у потребителя, а не рядом с реализацией | «интерфейсы объявляются потребителем — это идиома, не нарушение DIP» |
| Python | отсутствие явных ABC | «порты выражаются `Protocol`/структурной типизацией; отдельные объявления не требуются» |
| TypeScript | структурная типизация вместо `implements` | то же |
| Rust | «мало абстракций» из-за дженериков | «подстановка через дженерики по трейтам — статический полиморфизм, DIP соблюдён» |
| Java/Spring | аннотации в адаптерах | «аннотации допустимы вне ядра; в ядре запрещены» |
| PHP/Laravel | фасады и хелперы | «фасады разрешены только в слое доставки и адаптерах» |
| Go/Rust | тесты рядом с исходником | «расположение тестов — идиома языка, не связано с границами» |
| любой | сгенерированный код (protobuf, ORM-миграции, DI-контейнер) | перечислить каталоги генерации как исключения |
