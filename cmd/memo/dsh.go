// dsh.go — специфика третьего поддерживаемого агентского приложения,
// DeepSeek Harness (`dsh`, ключ -agent dsh) в `memo init`. В отличие от Claude
// Code и opencode, у dsh нет проектного MCP-конфига: серверы монтируются в
// дерево плагинов Cordis патч-слоями (`$DSH_HOME/cordis.patch.yml`,
// `$DSH_HOME/profiles/<профиль>/cordis.patch.yml` или `--patch <файл>`), а
// формат записи — YAML с JS-выражениями (`!!js`), а не JSON. Поэтому init не
// мержит запись в чужой файл, а генерирует проектный оверлей целиком
// (writeDshOverlay); подключить его к профилю dsh пользователь должен сам —
// что именно для этого сделать, печатает dshOverlayHint (подсказки на этом
// шаге харнесс не даёт: разовое `--patch` и вставка блока в patch-слой профиля
// — разные действия). Блок оверлея обрамлён маркерами с путём проекта: копию,
// вставленную в patch-слой, повторный init находит и обновляет сам
// (syncDshProfileCopies) — иначе смена провайдеров или -hooks молча не
// доезжала бы до харнесса. Подключить блок через `include` Cordis нельзя:
// плагины внутри include резолвятся от каталога файла, а не от установки dsh
// (проверено: «failed to import»).
//
// Пути в оверлее и в командах хуков абсолютные: dsh запускает MCP-процесс с
// рабочим каталогом процесса харнесса (у Desktop-приложения, по lsof живого
// процесса, это «/»), а хук — с каталогом сессии (может быть подкаталогом
// проекта), поэтому относительный `agentmemory.toml` не разрешается. -config
// при этом — каталог проекта, а не путь к agentmemory.toml: только в этой форме
// config.Load разрешает относительный store.path (`-store ./mem.db`) от
// каталога проекта, а не от рабочего каталога процесса.
//
// Хуки (ambient-режим, §12) у dsh есть — через мост
// `@deepseek-ai/dsh-hooks-claude-code`: он читает файл формата Claude Code
// (ключ `hooks`) и поднимает события SessionStart/UserPromptSubmit, а
// `${CLAUDE_PROJECT_DIR}` подставляет сам. Конфиг хуков init пишет в проекте
// (mergeDshHooks), а строка моста с путём к нему попадает в тот же оверлей.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Артефакты dsh внутри каталога проекта: patch-оверлей Cordis и конфиг хуков
// (формат Claude Code — его читает мост). Каталог .dsh — тот же, из которого
// харнесс читает проектные скилы (проектный корень ранга 100).
const (
	dshOverlayName = "memo.cordis.yml"
	dshHooksName   = "memo-hooks.json"
)

// Плагины dsh, которые монтирует оверлей: MCP-клиент (запись сервера memo) и
// мост хуков Claude Code (ambient-режим). Оба поставляются вместе с харнессом
// и резолвятся из его установки, отдельная установка не нужна.
const (
	dshMCPClientPlugin = "@deepseek-ai/dsh-mcp-client"
	dshHooksPlugin     = "@deepseek-ai/dsh-hooks-claude-code"
)

// dshServerName — имя сервера в записи MCP-клиента: инструменты видны модели
// как mcp__memo__<инструмент>. Строки оверлея (id memo и memo-hooks) у всех
// проектов одинаковы, поэтому оверлеи РАЗНЫХ проектов в одном профиле не
// складываются, а перекрывают друг друга: загрузчик Cordis оставляет
// последнюю строку с тем же id (проверено на живом харнессе — `--patch`
// поверх слоя профиля молча переключал сессию на память другого проекта; об
// этом предупреждает dshOverlayHint).
const dshServerName = "memo"

// dshOverlayPath / dshHooksPath — пути артефактов dsh внутри каталога проекта.
func dshOverlayPath(dir string) string { return filepath.Join(dir, ".dsh", dshOverlayName) }
func dshHooksPath(dir string) string   { return filepath.Join(dir, ".dsh", dshHooksName) }

// dshAbsDir — абсолютный путь каталога проекта для артефактов dsh. Ошибка
// filepath.Abs достижима только при нерабочем рабочем каталоге процесса; в этом
// случае возвращаем исходный путь — внятную ошибку даст запись файла, а не
// подстановка пустой строки в YAML.
func dshAbsDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// scaffoldDsh раскладывает всё, что специфично для dsh: конфиг хуков (если в
// этом прогоне выбраны хуки) и проектный patch-оверлей с записью сервера memo.
// Запись сервера и строка моста хуков живут в ОДНОМ файле — оверлее, поэтому
// обе части собираются здесь, а не двумя независимыми функциями.
//
// Возвращает подсказку о ручном подключении оверлея — doInit печатает её в
// конце вывода, после сводки.
func scaffoldDsh(dir, project, command string, cfg initConfig, out io.Writer) (string, error) {
	mappings, hooksOn := hookEventsByChoice[cfg.Hooks]
	if hooksOn {
		if err := writeDshHooks(dir, command, mappings, out); err != nil {
			return "", err
		}
	}
	block := renderDshBlock(command, dir, cfg, hooksOn)
	if err := writeDshOverlay(dir, project, block, out); err != nil {
		return "", err
	}
	if err := syncDshProfileCopies(dir, block, out); err != nil {
		return "", err
	}
	if !hooksOn {
		// Конфиг хуков прошлого прогона удаляется: строка моста могла уйти в
		// patch-слой профиля (постоянное подключение), и тогда выключить хуки
		// может только отсутствие файла — мост без конфига не регистрирует ни
		// одного хука.
		if err := os.Remove(dshHooksPath(dir)); err == nil {
			fmt.Fprintf(out, "  удалён %s (хуки не выбраны)\n", dshHooksPath(dir))
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("удаление %q: %w", dshHooksPath(dir), err)
		}
		// Строка моста хуков — часть оверлея, поэтому включить ambient-режим
		// задним числом правкой конфига нельзя: оверлей нужно перегенерировать.
		fmt.Fprintf(out, "ambient-режим (авто-recall через хуки dsh) не подключён — включить: memo init -agent dsh -hooks %s\n", hooksSessionStart)
	}
	return dshOverlayHint(dir), nil
}

// renderDshBlock собирает блок проектного patch-оверлея dsh — то, что
// подключается к профилю: запись MCP-сервера memo (всегда) и, если в этом прогоне выбраны хуки (hooksOn), строку моста
// хуков с путём к сгенерированному конфигу. command — путь к бинарю memo (тот
// же, что попадает в .mcp.json/opencode.json), cfg — параметры прогона (из него
// берутся выбранные провайдеры для env). hooksOn передаётся аргументом, а не
// вычисляется здесь второй раз: решение «есть ли хуки» принимается ровно в
// одном месте (scaffoldDsh) — иначе файл хуков и строка моста, ссылающаяся на
// него, могут разойтись.
//
// env-плейсхолдеры у dsh принципиально другие, чем у Claude Code/opencode: dsh
// вычищает из окружения MCP-процесса переменные вида *KEY*/*PASSWORD*/*SECRET*/
// *TOKEN* и DSH_*, поэтому экспортированный в шелле ключ доезжает до memo
// ТОЛЬКО через явный env в записи сервера (выражение dsh вычисляет в процессе
// харнесса). Пустая строка при незаданной переменной безопасна: memo читает
// `<dir>/.env` сам и перекрывает ею пустое значение (loadDotEnv, DESIGN.md §13).
// Блок обрамлён маркерами dshBlockStart/dshBlockEnd и заканчивается переводом
// строки.
func renderDshBlock(command, dir string, cfg initConfig, hooksOn bool) string {
	absDir := dshAbsDir(dir)

	var b strings.Builder
	fmt.Fprintf(&b, `%s
# сгенерировано memo init — правки внутри блока перезапишет следующий прогон
- insert:
    - id: memo
      name: %s
      config:
        serverName: %s
        transport: stdio
        command: %s
        args:
          - '-config'
          - %s
`,
		dshBlockStart(absDir),
		yamlQuoteSingle(dshMCPClientPlugin),
		dshServerName,
		yamlQuoteSingle(command),
		yamlQuoteSingle(absDir),
	)

	if env := dshProviderEnv(cfg); len(env) > 0 {
		b.WriteString("        env:\n")
		for _, e := range env {
			fmt.Fprintf(&b, "          %s: %s\n", e.key, e.value)
		}
	}

	if hooksOn {
		fmt.Fprintf(&b, `    - id: memo-hooks
      name: %s
      config:
        configPath: %s
`,
			yamlQuoteSingle(dshHooksPlugin),
			yamlQuoteSingle(dshHooksPath(absDir)),
		)
	}

	b.WriteString(dshBlockEnd(absDir) + "\n")
	return b.String()
}

// dshBlockStart / dshBlockEnd — строки-маркеры блока оверлея (YAML-комментарии,
// загрузчику не мешают). Путь проекта в маркере отличает блоки разных проектов
// в одном patch-слое: init обновляет только блок своего проекта.
func dshBlockStart(dir string) string { return "# >>> memo " + dshAbsDir(dir) }
func dshBlockEnd(dir string) string   { return "# <<< memo " + dshAbsDir(dir) }

// dshEnvVar — плейсхолдер переменной в env-блоке записи сервера.
type dshEnvVar struct {
	key   string
	value string
}

// dshProviderEnv — env-плейсхолдеры выбранных в этом прогоне провайдеров в
// синтаксисе dsh (`!!js`-выражение вместо shell-style "${VAR:-}" у Claude Code и
// "{env:VAR}" у opencode), в порядке реестра провайдеров — том же, что у
// providerSelectOptions в wizard.go: оверлей детерминирован без сортировки по
// имени. Реестр тот же (selectedProviders), что у buildProviderEnv; подстановка
// не кавычится — тег !!js обязан начинать значение.
func dshProviderEnv(cfg initConfig) []dshEnvVar {
	env := make([]dshEnvVar, 0, len(embedderProviders)+len(reasonerProviders))
	for _, p := range selectedProviders(cfg) {
		if p.envKey != "" {
			env = append(env, dshEnvVar{key: p.envKey, value: fmt.Sprintf("!!js process.env.%s ?? ''", p.envKey)})
		}
	}
	return env
}

// writeDshOverlay пишет проектный оверлей dsh — шапку с инструкцией и блок
// (renderDshBlock). Оверлей не мержится в чужой файл, а генерируется целиком
// (как agentmemory.toml: источник истины — параметры текущего прогона init,
// поэтому -force на него не влияет).
func writeDshOverlay(dir, project, block string, out io.Writer) error {
	path := dshOverlayPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", filepath.Dir(path), err)
	}
	header := fmt.Sprintf(`# Проектный patch-оверлей DeepSeek Harness (Cordis) — MCP-сервер memo
# для проекта %s. Сгенерирован `+"`memo init`"+`; пути абсолютные, потому что dsh
# запускает MCP-процесс с рабочим каталогом процесса харнесса.
#
# Подключение: разово — `+"`dsh web --patch <этот файл>`"+`, постоянно — блок
# ниже целиком, вместе со строками-маркерами, в patch-слой профиля
# ($DSH_HOME/cordis.patch.yml или $DSH_HOME/profiles/<профиль>/cordis.patch.yml);
# повторный memo init обновит вставленный блок сам.
`, project)
	return overwriteFile(path, header+block, out)
}

// dshHome — каталог харнесса по тем же правилам, что у dsh: $DSH_HOME, если
// задан и не пуст (с раскрытием ~), иначе ~/.dsh.
func dshHome() (string, error) {
	if home := strings.TrimSpace(os.Getenv("DSH_HOME")); home != "" {
		expanded, err := expandHomeLocal(home)
		if err != nil {
			return "", err
		}
		return filepath.Abs(expanded)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("получение домашней директории: %w", err)
	}
	return filepath.Join(userHome, ".dsh"), nil
}

// syncDshProfileCopies обновляет блок оверлея этого проекта в patch-слоях dsh
// ($DSH_HOME/cordis.patch.yml и $DSH_HOME/profiles/*/cordis.patch.yml), куда
// пользователь вставил его для постоянного подключения. Трогаются только
// слои, где уже есть блок с маркерами ЭТОГО проекта: сам init блок никуда не
// вставляет — подключение к профилю остаётся решением пользователя. Остальные
// строки слоя и его права сохраняются. Слой с незакрытым маркером не
// меняется — init предупреждает и продолжает: чужой битый файл не повод
// ронять настройку проекта.
func syncDshProfileCopies(dir, block string, out io.Writer) error {
	home, err := dshHome()
	if err != nil {
		fmt.Fprintf(out, "warning: каталог dsh не определён, вставленные копии оверлея не проверены: %v\n", err)
		return nil
	}
	layers, err := filepath.Glob(filepath.Join(home, "profiles", "*", "cordis.patch.yml"))
	if err != nil {
		return fmt.Errorf("поиск patch-слоёв dsh: %w", err)
	}
	for _, path := range append([]string{filepath.Join(home, "cordis.patch.yml")}, layers...) {
		raw, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("чтение %q: %w", path, err)
		}
		updated, found, err := replaceDshBlock(string(raw), dir, block)
		if err != nil {
			fmt.Fprintf(out, "warning: %s: %v — блок не обновлён\n", path, err)
			continue
		}
		if !found || updated == string(raw) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("чтение прав %q: %w", path, err)
		}
		if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
			return fmt.Errorf("запись %q: %w", path, err)
		}
		fmt.Fprintf(out, "обновлён блок memo в %s\n", path)
	}
	return nil
}

// replaceDshBlock заменяет в тексте patch-слоя блок проекта dir (строки от
// dshBlockStart до dshBlockEnd включительно) на block. found — был ли блок;
// маркер начала без маркера конца — ошибка (замена «до конца файла» съела бы
// строки пользователя).
func replaceDshBlock(text, dir, block string) (updated string, found bool, err error) {
	lines := strings.SplitAfter(text, "\n")
	start := -1
	for i, line := range lines {
		switch strings.TrimRight(line, "\r\n") {
		case dshBlockStart(dir):
			start = i
		case dshBlockEnd(dir):
			if start < 0 {
				continue
			}
			return strings.Join(lines[:start], "") + block + strings.Join(lines[i+1:], ""), true, nil
		}
	}
	if start >= 0 {
		return "", false, fmt.Errorf("маркер %q без парного %q", dshBlockStart(dir), dshBlockEnd(dir))
	}
	return text, false, nil
}

// dshHookCommand собирает команду хука для конфига dsh: тот же `memo hook`, но
// -config — абсолютный путь к каталогу проекта (см. шапку файла), и -format
// json. Оба отличия вынужденные и проверены на живом харнессе:
//   - якорь ${CLAUDE_PROJECT_DIR:-.} (hookConfigAnchor у Claude Code) не
//     годится: мост заменяет ${CLAUDE_PROJECT_DIR} на каталог сессии (он может
//     быть подкаталогом проекта), а `:-`-форму фолбэка не понимает вовсе —
//     подстановка не сработала бы, и переменная уехала бы в memo литералом;
//   - мост читает контекст только из hookSpecificOutput.additionalContext:
//     обычный stdout он не использует (проверено: с форматом text бриф в
//     сессию не попадал), а text — дефолт, с которым хук работает у Claude Code.
func dshHookCommand(memoPath, dir, event string) string {
	return fmt.Sprintf("%q hook -event %s -config %q -format %s",
		memoPath, event, dshAbsDir(dir), hookFormatJSON)
}

// writeDshHooks собирает конфиг хуков dsh (<dir>/.dsh/memo-hooks.json) —
// формат Claude Code (ключ `hooks`), который читает мост
// dsh-hooks-claude-code. Файл целиком наш (на него ссылается строка моста в
// оверлее), поэтому пишется с нуля, а не мержится: источник истины — параметры
// текущего прогона init, как у agentmemory.toml и оверлея. Мерж здесь оставлял
// бы «залипшие» события: повторный init с суженным -hooks (или вовсе none) не
// выключал бы хук, вписанный прошлым прогоном. Свои хуки пользователю следует
// держать в отдельном файле и подключать своим мостом — этот принадлежит memo.
func writeDshHooks(dir, memoPath string, mappings []hookEventMapping, out io.Writer) error {
	hooksSection := map[string][]hookMatcherEntry{}
	events := make([]string, 0, len(mappings))
	for _, m := range mappings {
		hooksSection[m.ClaudeEvent] = []hookMatcherEntry{{
			Matcher: "",
			Hooks:   []map[string]any{{"type": "command", "command": dshHookCommand(memoPath, dir, m.MemoEvent)}},
		}}
		events = append(events, m.ClaudeEvent)
	}
	// Сортированный порядок событий — детерминированный файл и вывод
	// (порядок в map случаен).
	sort.Strings(events)

	content, err := json.MarshalIndent(map[string]any{"hooks": hooksSection}, "", "  ")
	if err != nil {
		return fmt.Errorf("сборка конфига хуков %q: %w", dshHooksPath(dir), err)
	}

	path := dshHooksPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", filepath.Dir(path), err)
	}
	if err := overwriteFile(path, string(append(content, '\n')), out); err != nil {
		return err
	}
	fmt.Fprintf(out, "  хуки: %s\n", strings.Join(events, ", "))
	return nil
}

// dshOverlayHint — что `memo init` печатает в конце вывода: у dsh нет
// проектного MCP-конфига, поэтому оверлей надо подключить к профилю вручную.
// Пути печатаются абсолютными (init мог запускаться из другого каталога), а
// patch-слои — в форме $DSH_HOME из документации dsh. Предупреждение про
// перекрытие — следствие одинаковых id строк у всех проектов (см.
// dshServerName).
func dshOverlayHint(dir string) string {
	absDir := dshAbsDir(dir)
	overlay := dshOverlayPath(absDir)
	return fmt.Sprintf(`dsh не читает проектный MCP-конфиг — запись сервера memo лежит в оверлее %s.
подключи её к профилю dsh:
  разово:      dsh web --patch %s
  постоянно:   блок оверлея целиком, от строки "%s" до "%s", —
               в patch-слой профиля:
               $DSH_HOME/cordis.patch.yml (все профили) или
               $DSH_HOME/profiles/<профиль>/cordis.patch.yml (один профиль;
               Desktop-приложение использует профиль desktop). Профиль с HMR
               подхватит правку без перезапуска, а повторный memo init обновит
               вставленный блок сам.
в один профиль подключай оверлей ОДНОГО проекта: строки оверлеев разных проектов
перекрывают друг друга (действует последний слой, --patch — поверх профиля), и
сессия молча пойдёт в память другого проекта.
`, overlay, shellQuoteSingle(overlay), dshBlockStart(absDir), dshBlockEnd(absDir))
}
