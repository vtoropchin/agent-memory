// synccmd.go — подкоманда `memo sync` (Этап 19, DESIGN.md §12 «CLI memo sync и memo doctor», §4.1):
// разовый прогон целей синхронизации с общей базой, статус без сети и явные
// операции над схемой общей базы (применение, ротация base_id) с подтверждением.
// Правил синхронизации здесь нет: прогон — runSync (syncrun.go), схема — store.Hub.
// Настройка целей — в морде (раздел «Синхронизация»), не здесь.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"agentmemory/internal/config"
	"agentmemory/internal/memory/store"
	memsync "agentmemory/internal/memory/sync"
)

// syncSetupHint — куда идти, если целей нет или нужной не нашлось.
const syncSetupHint = "настройте её в memo web, раздел «Синхронизация»"

// runSyncCmd разбирает флаги `memo sync` и выполняет один из режимов: прогон
// (по умолчанию), -status, -apply-schema или -rotate-base. stdin читается только
// для подтверждения операций над схемой без -yes.
func runSyncCmd(args []string, stdin io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "синхронизация проектов локальной памяти с общими базами команды.")
		fmt.Fprintln(out, "Использование: memo sync [-project X]              — разовый прогон всех целей (или одной)")
		fmt.Fprintln(out, "               memo sync -status [-project X]      — состояние целей без прогона (или memo sync status)")
		fmt.Fprintln(out, "               memo sync -apply-schema -project X  — создать/обновить схему общей базы")
		fmt.Fprintln(out, "               memo sync -rotate-base -project X   — новая идентичность общей базы после восстановления из копии")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "путь к TOML-конфигу или каталогу проекта с agentmemory.toml (опционально; если задан — обязан существовать)")
	dbPath := fs.String("db", "", "путь к файлу SQLite, переопределяет store.path из конфига (опционально)")
	project := fs.String("project", "", "только эта цель (имя проекта без project:)")
	status := fs.Bool("status", false, "показать состояние целей из локальной БД, без прогона и без сети")
	applySchema := fs.Bool("apply-schema", false, "создать/обновить схему общей базы цели -project (спрашивает подтверждение)")
	rotateBase := fs.Bool("rotate-base", false, "сменить base_id общей базы цели -project после восстановления из копии (спрашивает подтверждение)")
	yes := fs.Bool("yes", false, "не спрашивать подтверждение для -apply-schema/-rotate-base (неинтерактивный запуск)")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора/-h

	switch rest := fs.Args(); {
	case len(rest) == 1 && rest[0] == "status":
		*status = true
	case len(rest) > 0:
		return fmt.Errorf("лишние аргументы %q (см. memo sync -h)", rest)
	}
	modes := 0
	for _, on := range []bool{*status, *applySchema, *rotateBase} {
		if on {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("-status, -apply-schema и -rotate-base взаимоисключающие")
	}
	if (*applySchema || *rotateBase) && *project == "" {
		return errors.New("-apply-schema и -rotate-base требуют -project")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("загрузка конфига: %w", err)
	}
	if *dbPath != "" {
		cfg.Store.Path = *dbPath
	}
	loadDotEnvForConfig(*configPath, "memo sync")

	st, err := openStore(cfg.Store)
	if err != nil {
		return fmt.Errorf("открытие хранилища %q: %w", cfg.Store.Target(), err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Printf("memo sync: закрытие хранилища: %v", err)
		}
	}()
	local, ok := store.SyncLocalOf(st)
	if !ok {
		return errors.New("хранилище уже общее (store.driver = postgres) — синхронизация не нужна")
	}
	envDir, err := syncEnvDir(cfg.Store)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	d := hubDialer{local: local, envDir: envDir}
	switch {
	case *status:
		return printSyncStatus(ctx, out, local, envDir, *project)
	case *applySchema:
		return syncApplySchema(ctx, stdin, out, d, *project, *yes)
	case *rotateBase:
		return syncRotateBase(ctx, stdin, out, d, *project, *yes)
	default:
		return syncRunTargets(ctx, out, local, envDir, *project, authorName(ctx, st))
	}
}

// syncTargets — цели прогона/статуса: все или одна project (нет её — ошибка с
// подсказкой).
func syncTargets(ctx context.Context, local *store.SyncLocal, project string) ([]store.SyncTarget, error) {
	if project == "" {
		return local.Targets(ctx)
	}
	t, err := local.Target(ctx, project)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("цели %q нет — %s", project, syncSetupHint)
	}
	if err != nil {
		return nil, err
	}
	return []store.SyncTarget{t}, nil
}

// syncRunTargets — разовый прогон целей под тем же lease, что у фона: построчный
// итог, ошибка — если упала хоть одна цель. Цель с выключенными тумблерами (её
// прогон Syncer заканчивает сразу) и цель, которую сейчас прогоняет другой
// процесс, пропускаются без ошибки.
func syncRunTargets(ctx context.Context, out io.Writer, local *store.SyncLocal, envDir, project, name string) error {
	targets, err := syncTargets(ctx, local, project)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintf(out, "целей синхронизации нет — %s\n", syncSetupHint)
		return nil
	}
	failed := 0
	for _, t := range targets {
		// Прогон и при выключенных тумблерах: что тогда делать (сброс разметки,
		// итог в sync_state) решает Syncer, а не доставка.
		res, err := runSync(ctx, local, envDir, t.Project, name)
		switch {
		case err == nil && !t.Push && !t.Pull:
			fmt.Fprintf(out, "⏭ %s: пропущен — «Отправлять» и «Загружать» выключены\n", t.Project)
		case errors.Is(err, errSyncBusy):
			fmt.Fprintf(out, "⏭ %s: пропущен — %v\n", t.Project, err)
		case err != nil:
			failed++
			fmt.Fprintf(out, "❌ %s: %s; ошибка: %v\n", t.Project, formatSyncResult(res), err)
		default:
			fmt.Fprintf(out, "✅ %s: %s\n", t.Project, formatSyncResult(res))
		}
	}
	if failed > 0 {
		return fmt.Errorf("синхронизация не удалась для %d из %d целей", failed, len(targets))
	}
	return nil
}

// formatSyncResult — итог прогона одной строкой; отложенное и отброшенные связи
// — только если есть.
func formatSyncResult(r memsync.Result) string {
	s := fmt.Sprintf("отправлено %d, получено %d, конфликтов %d", r.Pushed, r.Pulled, r.Conflicts)
	if r.Deferred > 0 {
		s += fmt.Sprintf(", отложено до следующего прогона %d", r.Deferred)
	}
	if r.DroppedLinks > 0 {
		s += fmt.Sprintf(", отброшено связей без адресата %d", r.DroppedLinks)
	}
	return s
}

// printSyncStatus печатает цели и их состояние из локальной БД — без сети и без
// пароля (только задан ли он).
func printSyncStatus(ctx context.Context, out io.Writer, local *store.SyncLocal, envDir, project string) error {
	targets, err := syncTargets(ctx, local, project)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		fmt.Fprintf(out, "целей синхронизации нет — %s\n", syncSetupHint)
		return nil
	}
	for _, t := range targets {
		ls, err := loadSyncLocalStatus(ctx, local, envDir, t)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "%s: отправлять — %s, загружать — %s; общая база %s; пароль: %s\n",
			t.Project, yesNo(t.Push), yesNo(t.Pull), syncTargetAddr(t), ls.password)
		fmt.Fprintf(out, "  %s\n", ls.lastRun())
		fmt.Fprintf(out, "  последний успех: %s; конфликтов всего: %d; неотправленных: %d\n",
			formatSyncTime(ls.state.LastOKAt), ls.state.ConflictsTotal, ls.pending)
	}
	return nil
}

// syncLocalStatus — то, что о цели известно локально, без сети: общее для
// `memo sync -status` и секции doctor.
type syncLocalStatus struct {
	state    memsync.State
	pending  int
	password string // «задан» | «не задан (<переменная> в <файл>)» | ошибка чтения .env
}

func loadSyncLocalStatus(ctx context.Context, local *store.SyncLocal, envDir string, t store.SyncTarget) (syncLocalStatus, error) {
	var ls syncLocalStatus
	var err error
	if ls.state, err = local.State(ctx, t.Project); err != nil {
		return ls, err
	}
	if ls.pending, err = local.PendingCount(ctx, memsync.Scope(t.Project), memsync.Kinds()); err != nil {
		return ls, err
	}
	switch pw, err := syncPassword(envDir, t.PasswordEnv); {
	case err != nil:
		ls.password = err.Error()
	case pw != "":
		ls.password = "задан"
	default:
		ls.password = fmt.Sprintf("не задан (%s в %s/.env)", t.PasswordEnv, envDir)
	}
	return ls, nil
}

// lastRun — время и итог последнего прогона одной фразой.
func (ls syncLocalStatus) lastRun() string {
	st := ls.state
	if st.LastRunAt == 0 {
		return "прогонов ещё не было"
	}
	if st.LastError != "" {
		return fmt.Sprintf("последний прогон %s — ошибка: %s", formatSyncTime(st.LastRunAt), st.LastError)
	}
	return fmt.Sprintf("последний прогон %s — успешно (отправлено %d, получено %d, конфликтов %d)",
		formatSyncTime(st.LastRunAt), st.LastPushed, st.LastPulled, st.LastConflicts)
}

// syncTargetAddr — адрес общей базы цели без пароля.
func syncTargetAddr(t store.SyncTarget) string {
	addr := fmt.Sprintf("%s@%s:%d/%s", t.User, t.Host, t.Port, t.DBName)
	if t.SSLMode != "" {
		addr += " (sslmode=" + t.SSLMode + ")"
	}
	return addr
}

func formatSyncTime(ts int64) string {
	if ts == 0 {
		return "не было"
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04:05")
}

func yesNo(b bool) string {
	if b {
		return "да"
	}
	return "нет"
}

// describeHubSchema — состояние схемы общей базы относительно бинаря.
func describeHubSchema(s store.HubSchema, project string) string {
	switch {
	case s.Empty:
		return fmt.Sprintf("схемы нет — примените: memo sync -apply-schema -project %s", project)
	case s.Ahead():
		return fmt.Sprintf("версия %d новее этой версии memo (%d) — обновите memo", s.Version, s.Supported)
	case !s.Current():
		return fmt.Sprintf("версия %d старее этой версии memo (%d) — примените: memo sync -apply-schema -project %s", s.Version, s.Supported, project)
	default:
		return fmt.Sprintf("версия %d — совпадает", s.Version)
	}
}

// openSyncHub открывает общую базу цели project и читает состояние её схемы.
func openSyncHub(ctx context.Context, d hubDialer, project string) (*store.Hub, store.HubSchema, error) {
	hub, err := d.open(ctx, project)
	if errors.Is(err, store.ErrNotFound) {
		return nil, store.HubSchema{}, fmt.Errorf("цели %q нет — %s", project, syncSetupHint)
	}
	if err != nil {
		return nil, store.HubSchema{}, err
	}
	s, err := hub.Schema(ctx)
	if err != nil {
		hub.Close()
		return nil, store.HubSchema{}, err
	}
	return hub, s, nil
}

// errSyncDeclined — операция над общей базой не подтверждена.
var errSyncDeclined = errors.New("отменено: подтверждение не получено (для неинтерактивного запуска — -yes)")

// syncApplySchema создаёт или обновляет схему общей базы цели — явная операция
// с подтверждением (DESIGN.md §4.1). Актуальная схема — ничего не делает, схема
// новее бинаря — ошибка.
func syncApplySchema(ctx context.Context, stdin io.Reader, out io.Writer, d hubDialer, project string, yes bool) error {
	ctx, cancel := context.WithTimeout(ctx, syncRunTimeout)
	defer cancel()
	hub, s, err := openSyncHub(ctx, d, project)
	if err != nil {
		return err
	}
	defer hub.Close()
	fmt.Fprintf(out, "%s: схема общей базы: %s\n", project, describeHubSchema(s, project))
	switch {
	case s.Ahead():
		return errors.New("схему новее этой версии memo применять нельзя — обновите memo")
	case s.Current():
		fmt.Fprintln(out, "схема актуальна — применять нечего")
		return nil
	}
	if !yes && !confirm(stdin, out, fmt.Sprintf("Создать/обновить схему общей базы проекта %s до версии %d? Это DDL на общей базе команды", project, s.Supported)) {
		return errSyncDeclined
	}
	if err := hub.ApplySchema(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "схема общей базы применена: версия %d\n", s.Supported)
	return nil
}

// syncRotateBase выдаёт общей базе цели новый base_id после восстановления из
// копии (DESIGN.md §4.1): у всех клиентов курсор сбросится и набор сверится
// целиком. Только на актуальной схеме, с подтверждением.
func syncRotateBase(ctx context.Context, stdin io.Reader, out io.Writer, d hubDialer, project string, yes bool) error {
	ctx, cancel := context.WithTimeout(ctx, syncRunTimeout)
	defer cancel()
	hub, s, err := openSyncHub(ctx, d, project)
	if err != nil {
		return err
	}
	defer hub.Close()
	if !s.Current() {
		return fmt.Errorf("схема общей базы: %s", describeHubSchema(s, project))
	}
	if !yes && !confirm(stdin, out, fmt.Sprintf("Сменить идентичность общей базы проекта %s? Нужно только после восстановления базы из резервной копии: все клиенты сверят набор целиком", project)) {
		return errSyncDeclined
	}
	id, err := hub.RotateBase(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "base_id общей базы обновлён: %s — клиенты сверят набор при следующем прогоне\n", id)
	return nil
}

// confirm задаёт вопрос и ждёт «y»/«да» строкой из in; всё прочее, в том числе
// конец ввода (stdin не терминал), — отказ.
func confirm(in io.Reader, out io.Writer, question string) bool {
	fmt.Fprintf(out, "%s [y/N]: ", question)
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "д", "да":
		return true
	}
	fmt.Fprintln(out)
	return false
}
