// dotenv.go — загрузка .env рядом с конфигом memo (DESIGN.md §13, задача 5.4).
// Claude Code (и агентские приложения вообще) не пробрасывают .env в env
// MCP-сервера автоматически, поэтому memo сам подхватывает переменные из
// .env при старте (см. вызов loadDotEnv в run(), main.go) — так ключ
// DEEPSEEK_API_KEY, записанный `memo init`, реально долетает до reasoner'а.
package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// loadDotEnvForConfig разрешает путь к .env рядом с configPath (или в текущем
// каталоге, если configPath пуст) и подхватывает переменные окружения через
// loadDotEnv, логируя результат через log.Printf (стандартный вывод log —
// os.Stderr, что важно для one-shot CLI-подкоманд recall/save, задача 7.1: их
// stdout не должен содержать ничего, кроме брифа/результата save). label —
// префикс лог-строки ("memo", "memo recall", "memo save"). Общая
// точка входа между серверным режимом (run, main.go) и recall/save (cli.go) —
// обоим нужен DEEPSEEK_API_KEY в env до вызова newReasoner.
func loadDotEnvForConfig(configPath, label string) {
	dotenvDir := "."
	if configPath != "" {
		dotenvDir = filepath.Dir(configPath)
	}
	dotenvPath := filepath.Join(dotenvDir, ".env")
	if loaded, err := loadDotEnv(dotenvPath); err != nil {
		log.Printf("%s: загрузка %s: %v", label, dotenvPath, err)
	} else if loaded {
		log.Printf("%s: переменные окружения подхвачены из %s", label, dotenvPath)
	}
}

// loadDotEnv читает файл .env построчно (`KEY=VALUE`) и устанавливает
// переменные окружения через os.Setenv — но только те, что ещё не заданы
// непустым значением в окружении: непустой shell-env приоритетнее .env,
// а пустое или неустановленное значение (в т.ч. инжект ${VAR:-} из
// .mcp.json, см. buildMCPServerEntry в init.go) перекрывается значением из .env.
// Пустые строки и строки, начинающиеся с #, игнорируются; вокруг значения
// снимаются опциональные одинарные/двойные кавычки. Строки без "=" молча
// пропускаются.
//
// loaded=false без ошибки означает, что файла нет — это штатный случай (.env
// опционален), а не ошибка. Ошибка чтения существующего файла возвращается
// вызывающему для логирования в stderr — memo не должен из-за неё падать
// (DESIGN.md §15, деградация вместо падения).
func loadDotEnv(path string) (loaded bool, err error) {
	vars, loaded, err := parseDotEnv(path)
	if err != nil || !loaded {
		return loaded, err
	}

	for key, value := range vars {
		if existing, ok := os.LookupEnv(key); ok && existing != "" {
			continue // непустой shell-env приоритетнее .env — не перезатираем
		}
		if err := os.Setenv(key, value); err != nil {
			return true, fmt.Errorf("установка переменной %q: %w", key, err)
		}
	}
	return true, nil
}

// parseDotEnv разбирает .env по path в карту KEY->VALUE, НЕ трогая окружение
// процесса. Отделено от loadDotEnv, потому что у файла .env появился второй
// потребитель, которому нужно именно значение из файла, а не итоговое
// os.Getenv: `memo init` при повторном прогоне восстанавливает из <dir>/.env
// пароль PostgreSQL для пробы подключения (см. dotEnvPostgresPassword в
// init.go — там же, почему именно из файла, а не из окружения). Через loadDotEnv
// это сделать нельзя: она по контракту уступает непустому shell-env, то есть
// вернула бы значение из окружения, а не из файла проекта.
//
// Дубликаты ключа: побеждает первое НЕПУСТОЕ вхождение — пустое значение
// (`KEY=`) перекрывается последующим непустым. Это ровно то правило, которое
// давала построчная установка через os.Setenv до выделения этой функции:
// повторное вхождение пропускалось только тогда, когда уже установленная
// переменная непуста (`ok && existing != ""`), а `KEY=` оставляло её пустой и
// потому перекрываемой. Край не теоретический: пустая строка в .env — обычный
// след ручной правки или инжекта плейсхолдера, и настоящий ключ, дописанный
// ниже, обязан победить.
// loaded=false без ошибки — файла нет (штатный случай, .env опционален).
func parseDotEnv(path string) (vars map[string]string, loaded bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("открытие %q: %w", path, err)
	}
	defer f.Close()

	vars = map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if v, seen := vars[key]; seen && v != "" {
			continue
		}
		vars[key] = unquoteEnvValue(strings.TrimSpace(value))
	}
	if err := sc.Err(); err != nil {
		return nil, true, fmt.Errorf("чтение %q: %w", path, err)
	}
	return vars, true, nil
}

// unquoteEnvValue снимает опциональные окружающие кавычки (' или ") со
// значения .env-переменной.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
