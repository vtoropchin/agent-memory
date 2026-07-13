// cobra.go — дерево команд memo на github.com/spf13/cobra (задача 11.1).
//
// Единственная роль cobra здесь — маршрутизация по имени подкоманды; разбор
// ФЛАГОВ каждой подкоманды остаётся полностью на стороне уже существующих
// runInit/runWeb/runRecall/runSave/runHook/runDoctor/runExport/runImport и
// серверного run() (main.go) — все они и до этой задачи разбирали свои флаги
// собственным stdlib flag.FlagSet(name, flag.ExitOnError). Поэтому на КАЖДОЙ
// команде здесь стоит DisableFlagParsing=true: если бы cobra/pflag сами
// парсили флаги, поведение неминуемо разошлось бы с уже протестированным
// (иной синтаксис длинных флагов, другая обработка позиционных аргументов
// после флагов у `recall "<запрос>"`/`import file.jsonl`, другой -h). Вместо
// этого cobra получает "сырые" args после имени подкоманды и передаёт их как
// есть в соответствующую runX — то есть сохраняет байт-в-байт поведение,
// которое требовала задача 11.1, а не пытается его повторить через pflag.
//
// Компромисс: "-config — persistent-флаг корня, наследуется подкомандами" из
// задачи реализован НЕ через cobra.PersistentFlags (это потребовало бы
// включить парсинг pflag и вернуло бы риск расхождения выше), а тем, что
// каждая подкоманда и без того независимо принимает свой -config флагом
// собственного FlagSet — с точки зрения пользователя результат тот же (-config
// работает одинаково везде), но механизм — конвенция, а не общая cobra-flag-
// секция.
//
// exitCode — единственный канал, которым RunE сообщает наружу желаемый код
// выхода процесса: у подкоманд разные контракты (hook — всегда 0 несмотря на
// ошибку; doctor — код из отчёта; recall/save/init/export/import/web — 0/1 по
// наличию ошибки), которые нельзя выразить одним правилом cobra "ошибка ->
// os.Exit(1)". RunE поэтому всегда возвращает nil cobra (SilenceUsage/
// SilenceErrors подавляют её собственную печать usage/ошибки — они уже
// напечатаны/учтены внутри) и пишет код в *exitCode. Такой разрез вместе с
// тем, что stdin/stdout/stderr/exitCode передаются параметрами, а не берутся
// из os.Stdin/os.Stdout/os.Exit напрямую, делает всё дерево тестируемым без
// реального завершения процесса теста (см. cobra_test.go).
package main

import (
	"fmt"
	"io"
	"log"

	"github.com/spf13/cobra"
)

// newRootCmd строит дерево команд memo. root без подкоманды — серверный
// путь run() (обратная совместимость с уже установленными .mcp.json,
// вызывающими `memo -config <path>` без подкоманды).
func newRootCmd(stdin io.Reader, stdout, stderr io.Writer, exitCode *int) *cobra.Command {
	root := &cobra.Command{
		Use:                "memo",
		Short:              "сервер долговременной памяти для ИИ-агентов (MCP поверх SQLite)",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Справка верхнего уровня — перехватывается до серверного пути:
			// сервер ещё не стартовал, поэтому печать в stdout здесь
			// безопасна и не портит протокол MCP.
			if len(args) >= 1 && isHelpArg(args[0]) {
				writeUsage(stdout)
				return nil
			}
			if err := run(args); err != nil {
				fmt.Fprintf(stderr, "memo: %v\n", err)
				*exitCode = 1
			}
			return nil
		},
	}
	// Подкоманда `completion` (авто-генерация shell-автодополнения) — не часть
	// документированного набора подкоманд memo (DESIGN.md §5/§13); отключаем,
	// чтобы `memo -h` не показывал незадокументированную возможность.
	root.CompletionOptions.DisableDefaultCmd = true

	// cobra авто-регистрирует свою собственную подкоманду "help", которая иначе
	// перехватывала бы литерал "help" ДО того, как он попал бы в root.RunE
	// (Find матчит её по имени, разбор флагов тут ни при чём) — и печатала бы
	// сгенерированную cobra справку вместо writeUsage(). Заменяем её, чтобы
	// "memo help" вело себя как "memo -h" (isHelpArg, см. root.RunE) —
	// то же самое, что было в диспетчере main() до задачи 11.1.
	root.SetHelpCommand(&cobra.Command{
		Use:    "help",
		Hidden: true,
		Run: func(cmd *cobra.Command, args []string) {
			writeUsage(stdout)
		},
	})

	root.AddCommand(
		newForwardingCmd("init", "установить память в проект (MCP-конфиг, скил, agentmemory.toml)", exitCode, stderr, "memo init",
			func(args []string) error { return runInit(args, stdout) }),
		newForwardingCmd("web", "HTTP-морда для просмотра и поиска памяти", exitCode, stderr, "memo web",
			func(args []string) error { return runWeb(args, stdout) }),
		newForwardingCmd("recall", "одноразовый recall из shell/скрипта/хука агента", exitCode, stderr, "memo recall",
			func(args []string) error { return runRecall(args, stdout) }),
		newForwardingCmd("save", "одноразовое сохранение факта в память", exitCode, stderr, "memo save",
			func(args []string) error { return runSave(args, stdin, stdout) }),
		newHookCmd(stdin, stdout),
		newDoctorCmd(stdout, stderr, exitCode),
		newForwardingCmd("export", "бэкап/перенос памяти в переносимый JSONL", exitCode, stderr, "memo export",
			func(args []string) error { return runExport(args, stdout) }),
		newForwardingCmd("import", "восстановление памяти из JSONL-бэкапа", exitCode, stderr, "memo import",
			func(args []string) error { return runImport(args, stdout) }),
	)
	return root
}

// newForwardingCmd строит подкоманду, чей контракт — "0 при успехе, 1 при
// ошибке, ошибка печатается в stderr с префиксом logPrefix" (recall, save,
// init, web, export, import): общий для шести из восьми подкоманд, поэтому
// вынесен один раз, а не повторён в каждой команде отдельно. hook и doctor —
// единственные с другим контрактом выхода, у них свои конструкторы ниже.
func newForwardingCmd(use, short string, exitCode *int, stderr io.Writer, logPrefix string, runX func(args []string) error) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              short,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runX(args); err != nil {
				fmt.Fprintf(stderr, "%s: %v\n", logPrefix, err)
				*exitCode = 1
			}
			return nil
		},
	}
}

// newHookCmd строит подкоманду `hook` — ЖЕЛЕЗНЫЙ инвариант hook.go: любая
// ошибка → пустой stdout, exit 0 (хук не должен ломать сессию агента).
// exitCode здесь никогда не трогается (остаётся 0, с которым стартовал
// процесс) — в отличие от newForwardingCmd, ошибка runHook только логируется.
func newHookCmd(stdin io.Reader, stdout io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:                "hook",
		Short:              "хук для .claude/settings.json (session-start|prompt-submit): любая ошибка → пустой stdout, exit 0",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runHook(args, stdin, stdout); err != nil {
				log.Printf("memo hook: %v", err)
			}
			return nil
		},
	}
}

// newDoctorCmd строит подкоманду `doctor` — контракт выхода отдельный от
// остальных: код берётся из самого отчёта (0 здоров/деградирован, 1 есть ❌),
// а не из факта наличия err (err у runDoctor — только ошибки, из-за которых
// отчёт вообще нельзя построить: битые флаги/конфиг, это тоже exit 1).
func newDoctorCmd(stdout, stderr io.Writer, exitCode *int) *cobra.Command {
	return &cobra.Command{
		Use:                "doctor",
		Short:              "read-only диагностика: в каком режиме реально работает память",
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		Args:               cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := runDoctor(args, stdout)
			if err != nil {
				fmt.Fprintf(stderr, "memo doctor: %v\n", err)
				*exitCode = 1
				return nil
			}
			*exitCode = code
			return nil
		},
	}
}
