// Подкоманда `memo doctor [-config path] [-json] [-live]` — read-only
// диагностика фактического режима работы памяти (DESIGN.md §15, задача 9.1).
// Система построена на принципе «деградация, а не падение»: memo может
// молча работать в урезанном режиме (ollama лежит → recall без вектора; ключ
// DeepSeek не подхватился → без сводок; embed_meta от другой модели → reindex
// ещё не догнал), и без этой команды это видно только по stderr работающего
// MCP-процесса. doctor отвечает на вопрос «всё ли хорошо» одной командой и
// НИЧЕГО не чинит и не пишет в БД — единственные побочные эффекты это сетевые
// пробы живости Embedder/Reasoner (последняя — только под -live).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/reason"
	"agentmemory/internal/memory/store"
)

// probeTimeout — таймаут сетевых проб живости Embedder/Reasoner (задача 9.1:
// «~5s»). Отдельный дочерний context.WithTimeout на каждую пробу — общий ctx
// команды (context.Background(), doctor не слушает сигналы — однократный
// быстрый прогон) таймаута не имеет.
const probeTimeout = 5 * time.Second

// status — состояние одной секции отчёта doctor.
type status int

const (
	statusOK status = iota
	statusWarn
	statusFail
)

// icon — человекочитаемый маркер секции (Markdown-вывод).
func (s status) icon() string {
	switch s {
	case statusOK:
		return "✅"
	case statusWarn:
		return "⚠️"
	case statusFail:
		return "❌"
	default:
		return "?"
	}
}

// String — машиночитаемое имя статуса (используется MarshalJSON).
func (s status) String() string {
	switch s {
	case statusOK:
		return "ok"
	case statusWarn:
		return "warn"
	case statusFail:
		return "fail"
	default:
		return "unknown"
	}
}

// MarshalJSON сериализует статус словом ("ok"/"warn"/"fail"), а не числом —
// -json предназначен для скриптов, число было бы менее читаемо/устойчиво к
// перестановке констант.
func (s status) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// UnmarshalJSON — обратное к MarshalJSON (используется тестами, гоняющими
// -json вывод через encoding/json туда и обратно).
func (s *status) UnmarshalJSON(data []byte) error {
	var word string
	if err := json.Unmarshal(data, &word); err != nil {
		return err
	}
	switch word {
	case "ok":
		*s = statusOK
	case "warn":
		*s = statusWarn
	case "fail":
		*s = statusFail
	default:
		return fmt.Errorf("doctor: неизвестный статус %q", word)
	}
	return nil
}

// section — одна проверка отчёта doctor: имя, статус, одна строка деталей.
type section struct {
	Name   string `json:"name"`
	Status status `json:"status"`
	Detail string `json:"detail"`
}

// doctorReport — полный отчёт `memo doctor` (человекочитаемый и -json
// вывод строятся из одной и той же структуры).
type doctorReport struct {
	Sections []section `json:"sections"`
	Verdict  string    `json:"verdict"`  // "здоров" | "деградирован (N предупреждений)" | "сломан"
	ExitCode int       `json:"exitCode"` // 0 — здоров/деградирован, 1 — есть ❌ секция
}

// runDoctor разбирает флаги подкоманды `doctor`, строит отчёт и печатает его в
// out (Markdown-подобный человекочитаемый список секций или -json). Возвращает
// желаемый код выхода процесса (0 — здоров/деградирован, 1 — сломан) отдельно
// от err (err — только ошибки, из-за которых отчёт вообще нельзя построить:
// битые флаги, невалидный конфиг-файл — как у остальных подкоманд, это
// приводит к exit 1 через main.go). Вынесена из диспетчера для тестируемости
// (по образцу runInit/runWeb/runRecall).
func runDoctor(args []string, out io.Writer) (exitCode int, err error) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "read-only диагностика: в каком режиме реально работает память и всё ли с ней хорошо.")
		fmt.Fprintln(out, "Использование: memo doctor [-config path] [-db path] [-json] [-live]")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "путь к TOML-конфигу (опционально)")
	dbPath := fs.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	jsonOut := fs.Bool("json", false, "печатать машиночитаемый JSON вместо человекочитаемого списка секций")
	live := fs.Bool("live", false, "выполнить пробный запрос к Reasoner (DeepSeek) — стоит денег, по умолчанию выключено")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора/-h

	cfg, err := config.Load(*configPath)
	if err != nil {
		return 1, fmt.Errorf("загрузка конфига: %w", err)
	}
	if *dbPath != "" {
		cfg.Store.Path = *dbPath
	}
	// .env рядом с конфигом — до проверки env-ключей Reasoner/Embedder(openai) в
	// секциях 5/6, симметрично серверному режиму и recall/save (задача 5.4).
	loadDotEnvForConfig(*configPath, "memo doctor")

	report := buildDoctorReport(context.Background(), cfg, *configPath, *live)

	if *jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return 1, fmt.Errorf("doctor: сериализация JSON: %w", err)
		}
		return report.ExitCode, nil
	}

	for _, sec := range report.Sections {
		fmt.Fprintf(out, "%s %s: %s\n", sec.Status.icon(), sec.Name, sec.Detail)
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Итог: %s\n", report.Verdict)

	return report.ExitCode, nil
}

// buildDoctorReport выполняет все 7 секций диагностики по порядку задачи 9.1 и
// сводит их в общий вердикт. Read-only-гарантия у секции «БД» НЕ симметрична
// между бэкендами: для sqlite databaseSection проверяет существование файла ДО
// store.OpenSQLite и не даёт создать отсутствующий файл — но store.OpenSQLite
// при открытии СУЩЕСТВУЮЩЕГО файла всё равно применяет baseSchema+migrate
// (идемпотентные, но настоящие DDL/запись), поэтому для sqlite read-only здесь
// означает только «не создаём отсутствующую БД», а не «вообще не пишем». Для
// postgres такого локального файла нет вовсе, зато read-only получается
// настоящим: postgresDatabaseSection открывает соединение через
// store.OpenPostgresReadOnly, а не через общую фабрику store.Open (см. её
// комментарий) — там нет CREATE/ALTER, только Ping и current_schema().
// Остальные секции читают только через Store.
func buildDoctorReport(ctx context.Context, cfg config.Config, configPath string, live bool) doctorReport {
	var sections []section

	sections = append(sections, configSection(cfg, configPath))

	dbSec, st := storeSection(cfg.Store)
	sections = append(sections, dbSec)
	if st != nil {
		defer st.Close()
	}

	sections = append(sections, recordsSection(ctx, st))
	sections = append(sections, vectorLayerSection(ctx, st, cfg.Embedder))
	sections = append(sections, embedderLivenessSection(ctx, cfg.Embedder))
	sections = append(sections, reasonerSection(ctx, cfg.Reasoner, live))

	verdict, exitCode, warnings := summarize(sections)
	sections = append(sections, section{
		Name:   "Итог",
		Status: verdictStatus(warnings, exitCode),
		Detail: verdict,
	})

	return doctorReport{Sections: sections, Verdict: verdict, ExitCode: exitCode}
}

// summarize сводит секции в одну строку вердикта и код выхода: 0 — «здоров»
// (все ✅) либо «деградирован (N предупреждений)» (есть ⚠️, нет ❌); 1 — «сломан»
// (есть хотя бы одна ❌) — DESIGN.md §15, задача 9.1.
func summarize(sections []section) (verdict string, exitCode int, warnings int) {
	broken := false
	for _, sec := range sections {
		switch sec.Status {
		case statusFail:
			broken = true
		case statusWarn:
			warnings++
		}
	}
	if broken {
		return "сломан", 1, warnings
	}
	if warnings > 0 {
		// Формат буквально по ТЗ (задача 9.1): "деградирован (N предупреждений)" —
		// без склонения по числам (упрощение, не грамматическая ошибка ценой
		// хрупкости сравнения строк в тестах/скриптах).
		return fmt.Sprintf("деградирован (%d предупреждений)", warnings), 0, warnings
	}
	return "здоров", 0, warnings
}

// verdictStatus — статус итоговой строки-секции, зеркалит семантику exitCode:
// сломан → ❌, есть предупреждения → ⚠️, иначе ✅. Сама итоговая секция не
// участвует в подсчёте (summarize вызывается до её добавления).
func verdictStatus(warnings, exitCode int) status {
	if exitCode != 0 {
		return statusFail
	}
	if warnings > 0 {
		return statusWarn
	}
	return statusOK
}

// configSection — секция 1: путь к конфигу и его валидность (уже проверена
// вызывающим кодом через config.Load до вызова buildDoctorReport — если бы
// конфиг был невалиден, runDoctor вернул бы ошибку раньше), а также задан ли
// [scope] project — пустое значение означает выключенную мультипроектность
// (DESIGN.md §2.1) и является предупреждением, а не поломкой: adhoc-режим
// полностью поддерживается.
func configSection(cfg config.Config, configPath string) section {
	var detail strings.Builder
	if configPath == "" {
		detail.WriteString("файл не указан, используются дефолты")
	} else {
		detail.WriteString(fmt.Sprintf("файл %s", configPath))
	}

	st := statusOK
	if cfg.Scope.Project == "" {
		detail.WriteString("; [scope] project не задан — мультипроектность выключена")
		st = statusWarn
	} else {
		detail.WriteString(fmt.Sprintf("; [scope] project=%q", cfg.Scope.Project))
	}

	return section{Name: "Конфиг", Status: st, Detail: detail.String()}
}

// storeSection — секция 2, диспетчер по store.driver: "" и "sqlite" уходят в
// databaseSection (файл на диске, историческая read-only проверка через
// os.Stat до открытия), "postgres" — в postgresDatabaseSection (нет
// локального файла, чьё существование можно проверить заранее). Неизвестный
// driver сюда дойти не должен (config.Load уже отклоняет такой конфиг через
// StoreConfig.Validate), но секция остаётся defensive, а не паникует.
func storeSection(cfg config.StoreConfig) (section, store.Store) {
	switch cfg.Driver {
	case "", "sqlite":
		sec, st := databaseSection(cfg.Path)
		return applyLegacyURIWarning(cfg.Path, sec), st
	case "postgres":
		return postgresDatabaseSection(cfg)
	default:
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("неизвестный store.driver %q", cfg.Driver)}, nil
	}
}

// applyLegacyURIWarning — ревизия по итогам ревью задачи 12.2
// (tasks/12.2-sqlite-uri-escaping.md): экранирование "%" в sqliteDSN
// (store.escapeSQLiteURIPath) поменяло, КАКОЙ физический файл открывает
// store.OpenSQLite для путей, содержащих валидную последовательность "%XX"
// (пример: "profile%41.db" раньше SQLite декодировал в "profileA.db" —
// другой, отдельный файл; теперь открывается буквально "profile%41.db" — см.
// доккомментарий store.SQLiteLegacyURIPath, там же вся механика percent-
// декодирования). Старая база по декодированному пути при этом никуда не
// переносится и не удаляется — тихо становится сиротой, и ни OpenSQLite (не
// роняет ошибку — просто создаёт новый пустой файл), ни остальные секции
// doctor (проверяют уже открытый st, т.е. новый файл) этого не заметят.
// databaseSection делает os.Stat по буквальному пути ДО открытия — тем же
// приёмом, что и здесь, но он не знает о "старом" пути вовсе.
//
// Почему не в самом OpenSQLite/при обычном старте memo, а только здесь: при
// каждом старте (в отличие от однократного read-only прогона doctor) это
// значило бы ещё одно read-only open+запрос к ВТОРОМУ файлу на КАЖДЫЙ запуск
// memo/MCP-сервера ради узкого случая ("%XX" в пути — редкость), да ещё и по
// пути, который сам же memo больше никогда не откроет как рабочий — лишняя
// работа и задержка старта без соразмерной пользы. doctor уже read-only
// диагностика, вызывается по требованию, и для неё лишний проверочный запрос
// — обычное дело (см. остальные секции).
//
// Проверка "старого" пути идёт через nonEmptySQLiteFile (init.go, тот же
// пакет main, задача 12.1 доработок) — тот же read-only помощник, что и у
// подсказки `memo init` про `memo migrate`, с таймаутом sqliteMemoryCountTimeout,
// а не probeTimeout: probeTimeout откалиброван под СЕТЕВЫЕ пробы живости
// Embedder/Reasoner (см. её комментарий), природа же этой проверки — локальный
// файловый ввод-вывод, который не должен незаметно делить таймаут с сетевым.
//
// rawPath — тот же нераскрытый cfg.Store.Path, что получает databaseSection.
// sec — уже готовая секция «БД» (её Detail/Status не переопределяются, а
// дополняются: если декодирование ничего не меняет для rawPath — подавляющее
// большинство путей без "%" — sec возвращается как есть). Диагностика мягкая:
// любая ошибка самой проверки (раскрытие "~", повреждённый файл, нет прав,
// таймаут) просто оставляет sec без изменений — тот же принцип, что у
// nonEmptySQLiteFile (не превращать диагностику предупреждения в повод
// завалить весь отчёт). Статус повышается до statusWarn только если секция
// была statusOK — если она уже statusFail (например, буквальный путь не
// существует вовсе — вероятный симптом именно этой подмены), реальная
// проблема правдивее описывается как Fail, а не Warn; предупреждение при этом
// всё равно добавляется в Detail, чтобы объяснить вероятную причину.
func applyLegacyURIWarning(rawPath string, sec section) section {
	expanded, err := expandHomeLocal(rawPath)
	if err != nil {
		return sec
	}
	legacy := store.SQLiteLegacyURIPath(expanded)
	if legacy == expanded {
		return sec
	}

	has, _ := nonEmptySQLiteFile(legacy)
	if !has {
		return sec
	}

	// Готовая команда `memo migrate` вместо голого упоминания её имени —
	// doctor знает оба пути (старый и текущий), а формат "sqlite:<путь>"
	// формирует formatMigrateEndpoint (migrate.go), уже используемая для той
	// же подсказки в `memo init` (doInit, init.go): один источник синтаксиса
	// вместо третьего независимого способа собрать ту же строку.
	// shellQuoteSingle — та же причина, что и там: путь может содержать $,
	// обратные кавычки и другие спецсимволы шелла.
	migrateCmd := fmt.Sprintf("memo migrate %s %s",
		shellQuoteSingle(formatMigrateEndpoint(config.StoreConfig{Driver: storeDriverSQLite, Path: legacy})),
		shellQuoteSingle(formatMigrateEndpoint(config.StoreConfig{Driver: storeDriverSQLite, Path: expanded})),
	)
	sec.Detail = fmt.Sprintf(
		"%s; ⚠ раньше memo открывал по этому пути другой файл — %s (percent-декодирование %%XX в прежнем, неэкранирующем коде) — и в нём есть накопленная память; перенеси её: `%s` (или поправь store.path)",
		sec.Detail, legacy, migrateCmd,
	)
	if sec.Status == statusOK {
		sec.Status = statusWarn
	}
	return sec
}

// databaseSection — секция 2 для sqlite: существование и размер файла БД,
// открытие хранилища, quick_check и journal_mode (через store.Health). Проверка
// существования файла ЛОКАЛЬНАЯ (os.Stat, до store.OpenSQLite) — Open создаёт
// отсутствующий файл и накатывает схему, что было бы записью и противоречило
// бы read-only инварианту doctor; поэтому при отсутствующем файле
// store.OpenSQLite вовсе не вызывается, а секция сразу помечается ❌.
// Возвращает открытый Store (nil, если файл отсутствует или Open не удался) —
// вызывающая сторона закрывает его и передаёт дальше в секции 3/4.
func databaseSection(path string) (section, store.Store) {
	expanded, err := expandHomeLocal(path)
	if err != nil {
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("раскрытие пути %q: %v", path, err)}, nil
	}

	info, statErr := os.Stat(expanded)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("файл %s не найден", expanded)}, nil
		}
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("проверка файла %s: %v", expanded, statErr)}, nil
	}

	st, err := store.OpenSQLite(path)
	if err != nil {
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("файл %s (%s); открытие: %v", expanded, humanSize(info.Size()), err)}, nil
	}

	health, err := st.Health(context.Background())
	if err != nil {
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("файл %s (%s); проверка целостности: %v", expanded, humanSize(info.Size()), err)}, st
	}

	if health.QuickCheck != "ok" {
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("файл %s (%s); quick_check сообщает о проблеме: %s", expanded, humanSize(info.Size()), health.QuickCheck)}, st
	}

	detail := fmt.Sprintf("файл %s (%s); quick_check: ok; journal_mode=%s", expanded, humanSize(info.Size()), health.JournalMode)
	if !strings.EqualFold(health.JournalMode, "wal") {
		return section{Name: "БД", Status: statusWarn, Detail: detail + " (ожидался wal)"}, st
	}
	return section{Name: "БД", Status: statusOK, Detail: detail}, st
}

// postgresDatabaseSection — секция 2 для store.driver=postgres: локального файла,
// чьё существование можно проверить заранее (как для sqlite выше), у postgres нет —
// поэтому вместо os.Stat-проверки ДО открытия здесь read-only инвариант doctor
// обеспечивается тем, КАКИМ путём открывается соединение: store.OpenPostgresReadOnly
// (а не общая фабрика openStore/store.Open, которую использует весь остальной
// cmd/memo) не применяет схему — только Ping и чтение current_schema(). Обычный
// store.Open делает это как часть init() при КАЖДОМ открытии (CREATE TABLE/INDEX IF
// NOT EXISTS, DROP+CREATE TRIGGER, CREATE OR REPLACE FUNCTION) — то есть на свежей БД
// молча накатил бы всю прод-схему памяти в рамках диагностики, а на существующей —
// как минимум переприменил бы триггеры; см. store.OpenPostgresReadOnly для деталей.
// resolvePostgresPassword — то же подхватывание MEMO_POSTGRES_PASSWORD, что у
// остального cmd/memo (openStore). journal_mode в детали не идёт — это
// sqlite-специфичное поле HealthInfo, для postgres оно неприменимо (реализация
// openPostgres/Health решает, чем его заполнять).
//
// После Health — та же диагностика ГОТОВНОСТИ, что печатает `memo init`
// (pgvector/право CREATE, задача 12.1, storeReadinessWarnings/
// storeReadinessWarningsFor в init.go): доступность и готовность — разные
// вещи (см. комментарий у probeStoreConnectionLive), и дрейф между ними может
// случиться уже ПОСЛЕ первого удачного `memo init` — администратор отозвал
// GRANT CREATE, снёс расширение vector — а `memo init` с этого момента больше
// не запускается на готовой базе. doctor — единственное место, которое
// перепроверяет это на уже работающей базе. storeReadinessWarningsFor
// переиспользуется как есть (не копия форматирования): init.go и doctor.go —
// один пакет main, второй набор текстов предупреждений означал бы, что их
// пришлось бы держать синхронизированными вручную. Неготовность — statusWarn
// (не statusFail): тот же принцип деградации, что у `memo init` (DESIGN.md
// §15) — база вполне доступна и рабочая, просто не всё готово к тому, чего
// memo попробует добиться при следующем старте/reindex.
func postgresDatabaseSection(cfg config.StoreConfig) (section, store.Store) {
	target := cfg.Target()

	st, err := store.OpenPostgresReadOnly(resolvePostgresPassword(cfg))
	if err != nil {
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("%s: открытие: %v", target, err)}, nil
	}

	health, err := st.Health(context.Background())
	if err != nil {
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("%s; проверка целостности: %v", target, err)}, st
	}

	if health.QuickCheck != "ok" {
		return section{Name: "БД", Status: statusFail, Detail: fmt.Sprintf("%s; quick_check сообщает о проблеме: %s", target, health.QuickCheck)}, st
	}

	detail := fmt.Sprintf("%s; quick_check: ok", target)

	// Отдельный дедлайн (probeTimeout — та же константа, что у прочих сетевых
	// проб doctor'а: embedderLivenessSection/reasonerSection), а не
	// context.Background() без срока: без него зависший диагностический запрос
	// повесил бы весь прогон `memo doctor` бесконечно.
	readinessCtx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	// "готовность: ..." печатается ВСЕГДА, когда диагностика реально вызвана —
	// и в чистом случае тоже (не только при warnings) — иначе тест на этот
	// путь (TestPostgresDoctorReadOnly_SchemaUnchanged) не отличил бы
	// "диагностика вызвана и ничего не нашла" от "блок диагностики вырезан из
	// кода целиком": в обоих случаях warnings был бы пуст, а detail —
	// неотличим от прежнего вида без этой строки.
	if warnings := storeReadinessWarningsFor(readinessCtx, st); len(warnings) > 0 {
		return section{Name: "БД", Status: statusWarn, Detail: detail + "; готовность: " + strings.Join(warnings, "; ")}, st
	}
	return section{Name: "БД", Status: statusOK, Detail: detail + "; готовность: ok"}, st
}

// recordsSection — секция 3: сводные счётчики памяти (Store.Stats). st==nil
// означает, что предыдущая секция «БД» не смогла открыть хранилище — записи
// проверить нечем.
func recordsSection(ctx context.Context, st store.Store) section {
	if st == nil {
		return section{Name: "Записи", Status: statusFail, Detail: "недоступно: хранилище не открыто (см. секцию «БД»)"}
	}

	stats, err := st.Stats(ctx)
	if err != nil {
		return section{Name: "Записи", Status: statusFail, Detail: fmt.Sprintf("Stats: %v", err)}
	}

	return section{
		Name:   "Записи",
		Status: statusOK,
		Detail: fmt.Sprintf("всего живых: %d (по kind: %s; по scope: %s); архивных (superseded): %d",
			stats.Total, formatCounts(stats.ByKind), formatCounts(stats.ByScope), stats.Superseded),
	}
}

// formatCounts форматирует карту счётчиков в компактную строку "a=1, b=2" в
// детерминированном (отсортированном по ключу) порядке — иначе вывод doctor
// прыгал бы между запусками (map итерируется в Go в случайном порядке).
func formatCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "нет"
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

// vectorLayerSection — секция 4: состояние векторного индекса. Сравнивает
// embed_meta (модель+dim, зафиксированные последним reindex — источник истины,
// DESIGN.md §9) с моделью из [embedder] конфига и считает очередь на досчёт
// (ListMissingVectors). st==nil — хранилище недоступно, проверить нечем.
func vectorLayerSection(ctx context.Context, st store.Store, cfg config.EmbedderConfig) section {
	const name = "Векторный слой"
	if st == nil {
		return section{Name: name, Status: statusFail, Detail: "недоступно: хранилище не открыто (см. секцию «БД»)"}
	}

	model, dim, err := st.EmbedMeta(ctx)
	if err != nil {
		return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("чтение embed_meta: %v", err)}
	}

	missing, err := st.ListMissingVectors(ctx, 0)
	if err != nil {
		return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("подсчёт очереди на досчёт вектора: %v", err)}
	}
	waitingSuffix := fmt.Sprintf("; %d записей ждут вектора", len(missing))

	vectorEnabled := cfg.Provider != "" && cfg.Provider != "noop"

	switch {
	case model == "" && dim == 0 && !vectorEnabled:
		return section{Name: name, Status: statusOK, Detail: "индекса нет — векторный слой выключен конфигом"}
	case model == "" && dim == 0:
		return section{Name: name, Status: statusWarn, Detail: "индекс ещё не создан (reindex ещё не пробегал)" + waitingSuffix}
	case !vectorEnabled:
		return section{Name: name, Status: statusWarn, Detail: fmt.Sprintf("индекс модели %s (dim=%d) существует, но embedder сейчас выключен конфигом — recall на FTS5", model, dim)}
	case model == cfg.Model:
		return section{Name: name, Status: statusOK, Detail: fmt.Sprintf("модель %s (dim=%d) совпадает с конфигом%s", model, dim, waitingSuffix)}
	default:
		return section{Name: name, Status: statusWarn, Detail: fmt.Sprintf("индекс модели %s (dim=%d) расходится с конфигом (%s) — reindex перегонит при следующем старте%s", model, dim, cfg.Model, waitingSuffix)}
	}
}

// embedderLivenessSection — секция 5: пробный эмбеддинг тем же путём, что
// фабрика newEmbedder (embed.NewOllama/embed.NewOpenAI), но БЕЗ отката на Noop
// — в отличие от newEmbedder, doctor'у нужна причина ошибки, а не тихая
// деградация. Извлечение общего probe-хелпера из newEmbedder признано
// нецелесообразным: newEmbedder логирует разные сообщения на разных причинах
// сбоя (недоступный сервер vs пустой ключ) и по контракту никогда не
// возвращает ошибку конструктора наружу — сливать эти две разные вещи в один
// хелпер усложнило бы обе стороны сильнее, чем экономит небольшое повторение
// (defaulting base_url на провайдер-специфичный дефолт, defaultOllamaBaseURL/
// defaultOpenAIBaseURL — те же константы init.go, единый источник истины с
// newEmbedder и `memo init`). Таймаут пробы — 5s
// (probeTimeout), отдельный дочерний context от ctx команды.
func embedderLivenessSection(ctx context.Context, cfg config.EmbedderConfig) section {
	const name = "Embedder (живость)"
	switch cfg.Provider {
	case "", "noop":
		return section{Name: name, Status: statusOK, Detail: "векторный слой выключен конфигом"}
	case "ollama":
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = defaultOllamaBaseURL
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		e, err := embed.NewOllama(probeCtx, baseURL, cfg.Model)
		if err != nil {
			return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("ollama %s недоступна или модель %q не подтянута (%v) — recall работает на FTS5", baseURL, cfg.Model, err)}
		}
		return section{Name: name, Status: statusOK, Detail: fmt.Sprintf("модель %s, dim=%d", e.Model(), e.Dim())}
	case "openai":
		key := os.Getenv(openaiAPIKeyEnv)
		if key == "" {
			return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("%s пуст — recall работает на FTS5", openaiAPIKeyEnv)}
		}
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = defaultOpenAIBaseURL
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		e, err := embed.NewOpenAI(probeCtx, baseURL, cfg.Model, key)
		if err != nil {
			return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("сервер %s недоступен или ключ/модель невалидны (%v) — recall работает на FTS5", baseURL, err)}
		}
		return section{Name: name, Status: statusOK, Detail: fmt.Sprintf("модель %s, dim=%d", e.Model(), e.Dim())}
	default:
		return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("неизвестный embedder.provider %q", cfg.Provider)}
	}
}

// reasonerSection — секция 6: ключ в env/.env найден? provider известен?
// Живой запрос к DeepSeek НЕ выполняется по умолчанию (стоит денег) — только
// под -live, минимальным пробным запросом с таймаутом probeTimeout. Пустой
// ключ/модель — предупреждение (Reasoner=Noop — полноценно поддерживаемая
// деградация, DESIGN.md §15), а не поломка.
func reasonerSection(ctx context.Context, cfg config.ReasonerConfig, live bool) section {
	const name = "Reasoner"
	switch cfg.Provider {
	case "", "noop":
		return section{Name: name, Status: statusOK, Detail: "LLM-слой выключен конфигом (суммаризация/консолидация недоступны)"}
	case "deepseek":
		key := os.Getenv(deepseekAPIKeyEnv)
		if key == "" {
			return section{Name: name, Status: statusWarn, Detail: fmt.Sprintf("%s не найден (env/.env) — суммаризация/консолидация деградируют до Noop", deepseekAPIKeyEnv)}
		}
		if cfg.Model == "" {
			return section{Name: name, Status: statusWarn, Detail: "reasoner.model пуст — суммаризация/консолидация деградируют до Noop"}
		}
		if !live {
			return section{Name: name, Status: statusOK, Detail: fmt.Sprintf("provider=deepseek, ключ найден, model=%s (живой запрос не выполнялся — используйте -live)", cfg.Model)}
		}

		r, err := reason.NewDeepSeek(key, cfg.Model)
		if err != nil {
			return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("создание клиента: %v", err)}
		}
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		if _, err := r.Complete(probeCtx, "Ответь одним словом: ок."); err != nil {
			return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("живой запрос к DeepSeek не удался: %v — суммаризация/консолидация деградируют до Noop", err)}
		}
		return section{Name: name, Status: statusOK, Detail: fmt.Sprintf("provider=deepseek, живой запрос успешен, model=%s", cfg.Model)}
	default:
		return section{Name: name, Status: statusFail, Detail: fmt.Sprintf("неизвестный reasoner.provider %q", cfg.Provider)}
	}
}

// expandHomeLocal раскрывает ведущий "~" в путь до домашней директории
// пользователя — маленький дубль неэкспортированного store.expandHome:
// заводить ради одной строки экспорт из store (или общий util-пакет) избыточно
// (граница задачи — не трогать internal-пакеты, кроме одобренного
// store.Health), а doctor'у нужно посмотреть на файл БД (os.Stat) РАНЬШЕ
// store.OpenSQLite — тем же путём, что применит сам store.OpenSQLite.
func expandHomeLocal(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("получение домашней директории: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
	}
	return path, nil
}

// humanSize форматирует размер файла в байтах в компактную человекочитаемую
// строку (Б/КБ/МБ/ГБ, шаг 1024).
func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d Б", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	units := []string{"КБ", "МБ", "ГБ", "ТБ"}
	return fmt.Sprintf("%.1f %s", float64(bytes)/float64(div), units[exp])
}
