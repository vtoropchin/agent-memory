// dsh.go — специфика третьего поддерживаемого агентского приложения,
// DeepSeek Harness (`dsh`, ключ -agent dsh) в `memo init`. В отличие от Claude
// Code и opencode, у dsh нет проектного MCP-конфига: серверы монтируются в
// дерево плагинов Cordis патч-слоями (`$DSH_HOME/cordis.patch.yml`,
// `$DSH_HOME/profiles/<профиль>/cordis.patch.yml` или `--patch <файл>`), а
// формат записи — YAML с JS-выражениями (`!!js`), а не JSON. Поэтому init не
// мержит запись в чужой файл, а генерирует проектный оверлей целиком
// (writeDshOverlay); подключить его к профилю dsh пользователь должен сам —
// что именно для этого сделать, печатает dshOverlayHint (подсказки на этом
// шаге харнесс не даёт: разовое `--patch` и вставка строк в patch-слой профиля
// — разные действия).
//
// Пути в оверлее и в командах хуков абсолютные: dsh запускает MCP-процесс с
// рабочим каталогом процесса харнесса (у Desktop-приложения, по lsof живого
// процесса, это «/»), поэтому относительный `agentmemory.toml` не разрешается.
//
// Хуки (ambient-режим, §12) у dsh есть — через мост
// `@deepseek-ai/dsh-hooks-claude-code`: он читает файл формата Claude Code
// (ключ `hooks`) и поднимает события SessionStart/UserPromptSubmit, а
// `${CLAUDE_PROJECT_DIR}` подставляет сам. Конфиг хуков init пишет в проекте
// (mergeDshHooks), а строка моста с путём к нему попадает в тот же оверлей.
package main

import (
	"encoding/json"
	"fmt"
	"io"
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
// как mcp__memo__<инструмент>. Имя уникально внутри одного дерева плагинов,
// поэтому строки оверлеев РАЗНЫХ проектов нельзя монтировать в один профиль
// одновременно: вторая строка с тем же serverName не загрузится (об этом
// предупреждает dshOverlayHint).
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
func scaffoldDsh(dir, project, command string, cfg initConfig, out io.Writer) error {
	mappings, hooksOn := hookEventsByChoice[cfg.Hooks]
	if hooksOn {
		if err := writeDshHooks(dir, command, mappings, out); err != nil {
			return err
		}
	}
	if err := writeDshOverlay(command, dir, project, cfg, hooksOn, out); err != nil {
		return err
	}
	if !hooksOn {
		// Строка моста хуков — часть оверлея, поэтому включить ambient-режим
		// задним числом правкой конфига нельзя: оверлей нужно перегенерировать.
		fmt.Fprintf(out, "ambient-режим (авто-recall через хуки dsh) не подключён — включить: memo init -agent dsh -hooks %s\n", hooksSessionStart)
	}
	fmt.Fprint(out, dshOverlayHint(dir))
	return nil
}

// renderDshOverlay собирает проектный patch-оверлей dsh: запись MCP-сервера
// memo (всегда) и, если в этом прогоне выбраны хуки (hooksOn), строку моста
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
func renderDshOverlay(command, dir, project string, cfg initConfig, hooksOn bool) string {
	absDir := dshAbsDir(dir)

	var b strings.Builder
	fmt.Fprintf(&b, `# Проектный patch-оверлей DeepSeek Harness (Cordis) — MCP-сервер memo
# для проекта %s. Сгенерирован `+"`memo init`"+`; пути абсолютные, потому что dsh
# запускает MCP-процесс с рабочим каталогом процесса харнесса.
#
# Подключение: разово — `+"`dsh web --patch <этот файл>`"+`, постоянно — строки
# ниже (начиная с "- insert:") в patch-слой профиля ($DSH_HOME/cordis.patch.yml
# или $DSH_HOME/profiles/<профиль>/cordis.patch.yml).
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
		project,
		yamlQuoteSingle(dshMCPClientPlugin),
		dshServerName,
		yamlQuoteSingle(command),
		yamlQuoteSingle(filepath.Join(absDir, "agentmemory.toml")),
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

	return b.String()
}

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

// writeDshOverlay пишет проектный оверлей dsh (renderDshOverlay) — единственный
// артефакт харнесса, который не мержится в чужой файл, а генерируется целиком
// (как agentmemory.toml: источник истины — параметры текущего прогона init,
// поэтому -force на него не влияет).
func writeDshOverlay(command, dir, project string, cfg initConfig, hooksOn bool, out io.Writer) error {
	path := dshOverlayPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("создание каталога %q: %w", filepath.Dir(path), err)
	}
	return overwriteFile(path, renderDshOverlay(command, dir, project, cfg, hooksOn), out)
}

// dshHookCommand собирает команду хука для конфига dsh: тот же `memo hook`, но
// -config — абсолютный путь к agentmemory.toml, и -format json. Оба отличия
// вынужденные и проверены на живом харнессе:
//   - якорь ${CLAUDE_PROJECT_DIR:-.} (hookConfigAnchor у Claude Code) не
//     годится: мост заменяет ${CLAUDE_PROJECT_DIR} на каталог сессии (он может
//     быть подкаталогом проекта), а `:-`-форму фолбэка не понимает вовсе —
//     подстановка не сработала бы, и переменная уехала бы в memo литералом;
//   - мост читает контекст только из hookSpecificOutput.additionalContext:
//     обычный stdout он не использует (проверено: с форматом text бриф в
//     сессию не попадал), а text — дефолт, с которым хук работает у Claude Code.
func dshHookCommand(memoPath, dir, event string) string {
	return fmt.Sprintf("%q hook -event %s -config %q -format %s",
		memoPath, event, filepath.Join(dshAbsDir(dir), "agentmemory.toml"), hookFormatJSON)
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

// dshOverlayHint — что `memo init` печатает после генерации оверлея: у dsh нет
// проектного MCP-конфига, поэтому оверлей надо подключить к профилю вручную.
// Пути печатаются абсолютными (init мог запускаться из другого каталога), а
// patch-слои — в форме $DSH_HOME из документации dsh: своего каталога харнесса
// init не знает и угадывать его не должен. Предупреждение про serverName —
// следствие того, что имя сервера одно на дерево плагинов: строки оверлеев
// двух проектов в одном профиле одновременно не уживаются.
func dshOverlayHint(dir string) string {
	absDir := dshAbsDir(dir)
	overlay := dshOverlayPath(absDir)
	return fmt.Sprintf(`dsh не читает проектный MCP-конфиг — запись сервера memo лежит в оверлее %s.
подключи её к профилю dsh:
  разово:      dsh web --patch %s
  постоянно:   строки оверлея (начиная с "- insert:") — в patch-слой профиля:
               $DSH_HOME/cordis.patch.yml (все профили) или
               $DSH_HOME/profiles/<профиль>/cordis.patch.yml (один профиль;
               Desktop-приложение использует профиль desktop). Профиль с HMR
               подхватит правку без перезапуска.
в один профиль монтируется оверлей ОДНОГО проекта: имя сервера (serverName: memo)
уникально в дереве плагинов, и вторая строка с тем же именем не загрузится.
`, overlay, shellQuoteSingle(overlay))
}
