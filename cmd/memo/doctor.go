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

// closableStore — store.Store, дополненный Close(): store.Open возвращает
// неэкспортированный конкретный тип, поэтому его нельзя назвать напрямую вне
// пакета store, но он реализует Close() error помимо интерфейса Store.
// databaseSection нужно и то, и другое — передать Store дальше в секции 3/4 и
// закрыть его в конце buildDoctorReport.
type closableStore interface {
	store.Store
	Close() error
}

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
// сводит их в общий вердикт. Строго read-only: секция «БД» проверяет
// существование файла ДО store.Open (Open создал бы файл и применил схему —
// это было бы записью), остальные секции читают только через Store.
func buildDoctorReport(ctx context.Context, cfg config.Config, configPath string, live bool) doctorReport {
	var sections []section

	sections = append(sections, configSection(cfg, configPath))

	dbSec, st := databaseSection(cfg.Store.Path)
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

// databaseSection — секция 2: существование и размер файла БД, открытие
// хранилища, quick_check и journal_mode (через store.Health). Проверка
// существования файла ЛОКАЛЬНАЯ (os.Stat, до store.Open) — store.Open создаёт
// отсутствующий файл и накатывает схему, что было бы записью и противоречило
// бы read-only инварианту doctor; поэтому при отсутствующем файле store.Open
// вовсе не вызывается, а секция сразу помечается ❌. Возвращает открытый Store
// (nil, если файл отсутствует или Open не удался) — вызывающая сторона
// закрывает его и передаёт дальше в секции 3/4.
func databaseSection(path string) (section, closableStore) {
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

	st, err := store.Open(path)
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
// (defaulting base_url на провайдер-специфичный дефолт, ollamaDefaultBaseURL/
// openaiDefaultBaseURL — те же константы main.go). Таймаут пробы — 5s
// (probeTimeout), отдельный дочерний context от ctx команды.
func embedderLivenessSection(ctx context.Context, cfg config.EmbedderConfig) section {
	const name = "Embedder (живость)"
	switch cfg.Provider {
	case "", "noop":
		return section{Name: name, Status: statusOK, Detail: "векторный слой выключен конфигом"}
	case "ollama":
		baseURL := cfg.BaseURL
		if baseURL == "" {
			baseURL = ollamaDefaultBaseURL
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
			baseURL = openaiDefaultBaseURL
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
// store.Open — тем же путём, что применит сам store.Open.
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
