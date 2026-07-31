// Подкоманда `memo migrate` — перенос памяти между бэкендами хранилища
// (sqlite <-> postgres, DESIGN.md §14 Этап 12) в любую сторону: источник и
// приёмник задаются двумя явными позиционными аргументами (а не «текущим»
// -config хранилищем), потому что направление переноса между личной SQLite и
// общей командной PostgreSQL-базой должно быть однозначным и видимым прямо в
// команде — неявный вывод направления из конфига легко перепутать местами и
// случайно затереть общую базу личной. Формат аргумента: "sqlite:<путь>" или
// "postgres://user@host:port/dbname?sslmode=..." (см. parseMigrateEndpoint) —
// тот же DSN-подобный синтаксис, что уже знаком по StoreConfig.DSN/Target
// (internal/config/config.go), только с явным driver-префиксом вместо секции
// TOML. Пароль PostgreSQL никогда не принимается в самой строке (см.
// parseMigrateEndpoint) — только из env MEMO_POSTGRES_PASSWORD, как и у
// остальных секретов проекта (openStore, main.go).
//
// Переносятся ВСЕ записи через Store.AllForExport (включая тумстоуны —
// superseded_by IS NOT NULL, иначе порвутся цепочки замен) вместе с тегами и
// связями; порядок записи в приёмник решает ту же проблему FK на links.dst,
// что и `memo import` (import.go) — тем же приёмом (два прохода: сперва все
// записи без Links, затем второй проход проставляет Links, когда все id уже
// существуют в приёмнике). Векторы НЕ переносятся — производные от
// content+summary+модели эмбеддера, досчитываются штатным Reindexer/
// ListMissingVectors после переноса (DESIGN.md §9, §11) — команда явно
// сообщает об этом в выводе.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"strconv"
	"strings"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// migrateProgressEvery — как часто (в записях) печатать промежуточный прогресс
// первого прохода migrateRecords на больших переносах. AllForExport и так
// грузит весь снимок источника в память одним срезом (то же ограничение, что
// у `memo export`, — менять Store.AllForExport ради потоковой выдачи вне
// рамок этой задачи), поэтому "батчевость" здесь — про видимый прогресс
// записи в приёмник, а не про размер транзакций (Put остаётся отдельной
// транзакцией на запись, как и у Store.Put/import.go).
const migrateProgressEvery = 500

// parseMigrateEndpoint разбирает строку источника/приёмника в
// config.StoreConfig. "sqlite:<путь>" — локальный файл; "postgres://" или
// "postgresql://user@host:port/dbname?sslmode=..." — общая база. Порт и
// sslmode, не указанные в URL, берутся из config.Default().Store (5432,
// "disable") — не переопределены здесь второй раз литералами, чтобы дефолты
// не могли разойтись между конфигом и этим парсером. Пароль в URL (userinfo
// вида user:pass@) — ошибка:
// секреты в аргументах командной строки оседают в истории шелла и выводе
// `ps`, поэтому здесь действует то же правило, что и у остальных секретов
// проекта (DEEPSEEK_API_KEY/OPENAI_API_KEY/MEMO_POSTGRES_PASSWORD) — только
// env. cfg.Validate() на postgres-ветке заодно проверяет, что host/user/dbname
// не пусты.
func parseMigrateEndpoint(s string) (config.StoreConfig, error) {
	switch {
	case strings.HasPrefix(s, "sqlite:"):
		path := strings.TrimPrefix(s, "sqlite:")
		if path == "" {
			return config.StoreConfig{}, fmt.Errorf("migrate: %q: путь sqlite пуст (ожидалось sqlite:<путь>)", s)
		}
		return config.StoreConfig{Driver: "sqlite", Path: path}, nil

	case strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://"):
		u, err := url.Parse(s)
		if err != nil {
			return config.StoreConfig{}, fmt.Errorf("migrate: %q: разбор postgres-URL: %w", s, err)
		}
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				return config.StoreConfig{}, fmt.Errorf("migrate: %q: пароль в URL не поддерживается — задайте его через %s", s, memoPostgresPasswordEnv)
			}
		}
		defaults := config.Default().Store
		port := defaults.Port
		if p := u.Port(); p != "" {
			port, err = strconv.Atoi(p)
			if err != nil {
				return config.StoreConfig{}, fmt.Errorf("migrate: %q: порт %q: %w", s, p, err)
			}
		}
		sslmode := u.Query().Get("sslmode")
		if sslmode == "" {
			sslmode = defaults.SSLMode
		}
		cfg := config.StoreConfig{
			Driver:  "postgres",
			Host:    u.Hostname(),
			Port:    port,
			User:    u.User.Username(),
			DBName:  strings.TrimPrefix(u.Path, "/"),
			SSLMode: sslmode,
		}
		if err := cfg.Validate(); err != nil {
			return config.StoreConfig{}, fmt.Errorf("migrate: %q: %w", s, err)
		}
		return cfg, nil

	default:
		return config.StoreConfig{}, fmt.Errorf("migrate: %q: неизвестный формат — используйте sqlite:<путь> или postgres://user@host:port/dbname?sslmode=...", s)
	}
}

// recordCounts — сводные счётчики снимка памяти (для отчёта "прочитано"/
// "сверка" runMigrate). Тумстоуны считаются отдельно от memories, а не вместо
// них — memories включает и живые записи, и тумстоуны (см. AllForExport).
// links имеет смысл только для снимков через AllForExport (источник); снимок
// приёмника в reconcileMigrate идёт через Store.List, который Links не
// грузит, — там links остаётся нулём и в отчёт не идёт (см. reconcileMigrate).
type recordCounts struct {
	memories   int
	tombstones int
	tags       int
	links      int
}

func countRecords(records []memory.Memory) recordCounts {
	var c recordCounts
	c.memories = len(records)
	for _, m := range records {
		if m.SupersededBy != nil {
			c.tombstones++
		}
		c.tags += len(m.Tags)
		c.links += len(m.Links)
	}
	return c
}

// migrateSummary — итог переноса в приёмник (см. printMigrateSummary).
type migrateSummary struct {
	Written      int
	Skipped      int
	LinksOK      int
	LinksDropped int
	Warnings     []string
}

// migrateRecords пишет records в dst. Конфликт id (запись уже есть в
// приёмнике) — по умолчанию пропуск и счётчик Skipped (безопасно на общей
// командной базе: не затирает чужие записи молча и делает повторный запуск
// идемпотентным); overwrite=true — Put заменяет содержимое, как у `memo
// import -overwrite`.
//
// Двухпроходная запись вынесена в общий с `memo import` хелпер fkSafeWrite
// (fkwrite.go, см. его комментарий про FK на links.dst) — здесь остаётся
// только migrate-специфичный текст предупреждения о повисшей ссылке
// ("в приёмнике", в отличие от import.go) и печать промежуточного прогресса
// каждые migrateProgressEvery записей на больших переносах.
func migrateRecords(ctx context.Context, out io.Writer, dst store.Store, records []memory.Memory, overwrite bool) (migrateSummary, error) {
	res, err := fkSafeWrite(ctx, dst, records, overwrite, func(srcID, missingDst string) string {
		return fmt.Sprintf("%s: ссылка на отсутствующий в приёмнике id %s пропущена", srcID, missingDst)
	}, func(index, total int) {
		if (index+1)%migrateProgressEvery == 0 {
			fmt.Fprintf(out, "перенесено %d/%d...\n", index+1, total)
		}
	})
	if err != nil {
		return migrateSummary{}, err
	}

	return migrateSummary{
		Written:      res.Written,
		Skipped:      res.Skipped,
		LinksOK:      res.LinksOK,
		LinksDropped: res.LinksDropped,
		Warnings:     res.Warnings,
	}, nil
}

// printMigrateSummary печатает итоговую строку переноса (по образцу
// printImportSummary из import.go) и предупреждения построчно.
func printMigrateSummary(out io.Writer, sum migrateSummary) {
	fmt.Fprintf(out, "перенесено %d, пропущено (уже есть в приёмнике) %d, связей %d (пропущено %d)\n",
		sum.Written, sum.Skipped, sum.LinksOK, sum.LinksDropped)
	for _, w := range sum.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
}

// reconcileListPageSize — размер страницы при постраничном чтении приёмника в
// reconcileMigrate (см. listReceiverIDs). Достаточно крупный, чтобы не
// плодить лишние round-trip'ы на больших переносах, но не грузит весь снимок
// одним запросом.
const reconcileListPageSize = 1000

// reconcileMigrate перечитывает приёмник (тем же фильтром -scopes) ПОСЛЕ
// переноса и сверяет его с тем, что было прочитано из источника: каждый id из
// records обязан присутствовать в приёмнике (неважно, был он записан в этом
// прогоне или уже существовал и был пропущен) — иначе перенос завершился
// неполным, и это явная ошибка, а не тихая деградация (в отличие от
// LinksDropped/Skipped, которые ожидаемы и уже отражены в migrateSummary).
// Возвращает error с перечислением отсутствующих id, если расхождение есть.
//
// Сверке нужны только id — читает приёмник через Store.List (постранично,
// IncludeSuperseded: true, чтобы не потерять тумстоуны), а не AllForExport:
// AllForExport догружает Links отдельным запросом на КАЖДУЮ запись
// (loadLinks в sqlite/postgres), то есть на переносе в 50 000 записей это
// ~50 000 лишних round-trip'ов ради проверки, которой Links вообще не нужны
// (Store не даёт более дешёвого способа получить только Links-агностичный
// список — расширять сам интерфейс Store ради этого здесь не в рамках).
// Tags при этом достаются "бесплатно" — List грузит их батчем на страницу, а
// не по одной записи, поэтому попутный отчёт по тегам остаётся точным; отчёт
// по числу связей убран (Links List не грузит) — точное число уже напечатано
// раньше, в сводке migrateRecords/printMigrateSummary.
func reconcileMigrate(ctx context.Context, out io.Writer, dstTarget string, dst store.Store, records []memory.Memory, scopes []string) error {
	after, err := listReceiverIDs(ctx, dst, scopes)
	if err != nil {
		return fmt.Errorf("повторное чтение приёмника %q: %w", dstTarget, err)
	}
	afterByID := make(map[string]struct{}, len(after))
	for _, m := range after {
		afterByID[m.Id] = struct{}{}
	}

	afterCounts := countRecords(after)
	fmt.Fprintf(out, "приёмник %s после переноса (тот же фильтр -scopes): %d записей (%d тумстоунов), %d тегов\n",
		dstTarget, afterCounts.memories, afterCounts.tombstones, afterCounts.tags)

	var missing []string
	for _, m := range records {
		if _, ok := afterByID[m.Id]; !ok {
			missing = append(missing, m.Id)
		}
	}
	if len(missing) == 0 {
		fmt.Fprintln(out, "сверка: все записи источника присутствуют в приёмнике")
		return nil
	}

	const showLimit = 20
	shown := missing
	suffix := ""
	if len(shown) > showLimit {
		shown = shown[:showLimit]
		suffix = fmt.Sprintf(" (и ещё %d)", len(missing)-showLimit)
	}
	return fmt.Errorf("%d записей источника отсутствуют в приёмнике после переноса: %s%s", len(missing), strings.Join(shown, ", "), suffix)
}

// listReceiverIDs постранично вычитывает из dst все записи под фильтром
// scopes (включая тумстоуны) через Store.List — id, superseded_by и Tags, без
// Links (см. комментарий reconcileMigrate). Порядок List (created_at DESC, id
// DESC) детерминирован и не меняется между страницами, пока приёмник не
// мутирует между вызовами; reconcileMigrate вызывает эту функцию один раз
// СРАЗУ после переноса, до каких-либо иных изменений — постраничное чтение
// безопасно.
func listReceiverIDs(ctx context.Context, dst store.Store, scopes []string) ([]memory.Memory, error) {
	var all []memory.Memory
	offset := 0
	for {
		page, total, err := dst.List(ctx, store.BrowseFilter{
			Scopes:            scopes,
			IncludeSuperseded: true,
			Limit:             reconcileListPageSize,
			Offset:            offset,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		offset += len(page)
		if len(page) == 0 || offset >= total {
			break
		}
	}
	return all, nil
}

// runMigrate разбирает флаги подкоманды `migrate` и переносит записи из
// источника в приёмник. Вынесена из диспетчера для тестируемости (по образцу
// остальных подкоманд).
func runMigrate(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fmt.Fprintln(out, "перенос памяти между бэкендами хранилища (sqlite <-> postgres): записи, тумстоуны, теги и связи; векторы не переносятся.")
		fmt.Fprintln(out, "Использование: memo migrate [флаги] <источник> <приёмник>")
		fmt.Fprintln(out, "источник/приёмник: sqlite:<путь> | postgres://user@host:port/dbname?sslmode=disable")
		fmt.Fprintf(out, "  пароль postgres — только из переменной окружения %s (никогда не в самой строке)\n", memoPostgresPasswordEnv)
		fs.PrintDefaults()
	}
	dryRun := fs.Bool("dry-run", false, "посчитать и показать, что будет перенесено, ничего не записывая (приёмник не открывается вовсе)")
	overwrite := fs.Bool("overwrite", false, "заменять в приёмнике записи с конфликтующим id (по умолчанию — пропуск, как у `memo import`)")
	scopesFlag := fs.String("scopes", "", "пространства через запятую для частичного переноса (пусто — вся БД)")
	_ = fs.Parse(args) // flag.ExitOnError сам завершает процесс при ошибке разбора/-h

	if fs.NArg() != 2 {
		return fmt.Errorf("migrate: нужно ровно два позиционных аргумента — memo migrate [флаги] <источник> <приёмник>")
	}
	scopes := splitCSV(*scopesFlag)

	srcCfg, err := parseMigrateEndpoint(fs.Arg(0))
	if err != nil {
		return err
	}
	dstCfg, err := parseMigrateEndpoint(fs.Arg(1))
	if err != nil {
		return err
	}
	if srcCfg.Driver == dstCfg.Driver && srcCfg.Target() == dstCfg.Target() {
		return fmt.Errorf("migrate: источник и приёмник совпадают (%s) — переносить нечего", srcCfg.Target())
	}

	src, err := openStore(srcCfg)
	if err != nil {
		return fmt.Errorf("migrate: открытие источника %q: %w", srcCfg.Target(), err)
	}
	defer func() {
		if err := src.Close(); err != nil {
			log.Printf("memo migrate: закрытие источника: %v", err)
		}
	}()

	ctx := context.Background()
	records, err := src.AllForExport(ctx, scopes)
	if err != nil {
		return fmt.Errorf("migrate: чтение источника %q: %w", srcCfg.Target(), err)
	}

	read := countRecords(records)
	fmt.Fprintf(out, "источник %s: %d записей (%d тумстоунов), %d тегов, %d связей\n",
		srcCfg.Target(), read.memories, read.tombstones, read.tags, read.links)

	if *dryRun {
		fmt.Fprintf(out, "dry-run: приёмник %s не открывался — конфликты id в приёмнике не проверялись, реальный перенос может дать другие числа skipped/связей\n", dstCfg.Target())
		fmt.Fprintln(out, "векторы не переносятся: производные от content+summary+модели эмбеддера, досчитываются фоновым reindex после переноса (DESIGN.md §9, §11)")
		return nil
	}

	dst, err := openStore(dstCfg)
	if err != nil {
		return fmt.Errorf("migrate: открытие приёмника %q: %w", dstCfg.Target(), err)
	}
	defer func() {
		if err := dst.Close(); err != nil {
			log.Printf("memo migrate: закрытие приёмника: %v", err)
		}
	}()

	sum, err := migrateRecords(ctx, out, dst, records, *overwrite)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	printMigrateSummary(out, sum)

	if err := reconcileMigrate(ctx, out, dstCfg.Target(), dst, records, scopes); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	fmt.Fprintln(out, "векторы не переносились: производные от content+summary+модели эмбеддера — досчитаются фоновым reindex/ListMissingVectors при следующем запуске `memo`; до этого recall в приёмнике работает на лексическом поиске (штатная деградация, DESIGN.md §15)")
	return nil
}
