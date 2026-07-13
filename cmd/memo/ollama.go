// ollama.go — диагностика окружения ollama для интерактивного `memo init` при
// выборе embedder-provider=ollama (задача 11.1, цикл исправлений). Различает
// happy/no-models/not-running/not-installed (resolveOllamaEmbedState) и несёт
// текст рекомендаций/инструкций под конкретную ОС — вся логика здесь чистая
// (кроме сетевого вызова fetchOllamaModels из providers.go и инъецируемого
// lookPath), поэтому тестируется без huh/реального ollama (см. ollama_test.go).
// wizard.go (promptOllamaEmbedder) — единственный вызывающий, отвечает только
// за рендер huh-виджетов поверх этой логики.
package main

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// recommendedOllamaEmbedModel — одна рекомендуемая модель эмбеддинга ollama:
// имя (== имя тега для `ollama pull <name>`), краткое обоснование выбора и
// примерный размер загрузки — для сообщения пользователю в состояниях
// no-models/not-installed.
type recommendedOllamaEmbedModel struct {
	Name       string
	Reason     string
	ApproxSize string
}

// recommendedOllamaEmbedModels — рекомендуемые модели эмбеддинга ollama, в
// порядке приоритета (первая — bge-m3, дефолт проекта, см. defaultOllamaModel
// в init.go). Единый источник и для классификатора (classifyOllamaEmbedModels
// — эти модели предпочтительны при показе select happy-состояния), и для
// текста рекомендаций (printOllamaNoModelsHelp/printOllamaNotInstalledHelp).
var recommendedOllamaEmbedModels = []recommendedOllamaEmbedModel{
	{
		Name:       "bge-m3",
		Reason:     "многоязычная (включая русский), 1024-dim, сильный retrieval — дефолт проекта",
		ApproxSize: "1.2 ГБ",
	},
	{
		Name:       "nomic-embed-text",
		Reason:     "лёгкая и быстрая, 768-dim, хороша для английского, экономна по ресурсам",
		ApproxSize: "275 МБ",
	},
	{
		Name:       "mxbai-embed-large",
		Reason:     "1024-dim, топ качество на MTEB (английский), тяжелее",
		ApproxSize: "670 МБ",
	},
}

// knownEmbeddingModelBases — курируемый список базовых имён известных моделей
// эмбеддинга ollama (без тега вида ":latest"/":567m" — см. matchesEmbeddingBase),
// включая три рекомендованные (recommendedOllamaEmbedModels) и остальные
// распространённые. Известное ограничение (не решается здесь): нераспознанная
// кастомная/малоизвестная embedding-модель попадёт в состояние no-models —
// escape для этого случая — флаг -embedder-model или -no-input (задача 11.1).
var knownEmbeddingModelBases = []string{
	"bge-m3",
	"bge-large",
	"bge-small",
	"nomic-embed-text",
	"mxbai-embed-large",
	"all-minilm",
	"snowflake-arctic-embed", // покрывает и snowflake-arctic-embed2 (hasPrefix)
	"granite-embedding",
	"paraphrase-multilingual",
	"embeddinggemma",
	"qwen3-embedding",
}

// matchesEmbeddingBase проверяет, соответствует ли name (имя установленной
// модели ollama, возможно с тегом ":<tag>", напр. "bge-m3:latest") базовому
// known — точное совпадение части до ":" либо её префикс по known (покрывает
// варианты вроде "snowflake-arctic-embed2").
func matchesEmbeddingBase(name, known string) bool {
	base, _, _ := strings.Cut(name, ":")
	return base == known || strings.HasPrefix(base, known)
}

// isKnownEmbeddingModel проверяет имя установленной модели против
// knownEmbeddingModelBases.
func isKnownEmbeddingModel(name string) bool {
	for _, base := range knownEmbeddingModelBases {
		if matchesEmbeddingBase(name, base) {
			return true
		}
	}
	return false
}

// classifyOllamaEmbedModels фильтрует installed (имена реально установленных
// моделей ollama, как их вернула fetchOllamaModels) до распознанных моделей
// эмбеддинга: рекомендованные (recommendedOllamaEmbedModels) — первыми, в
// порядке приоритета, остальные распознанные (knownEmbeddingModelBases) — в
// порядке installed. Чат-модели и нераспознанные имена отсеиваются. Пустой
// installed или отсутствие распознанных — nil, не ошибка.
func classifyOllamaEmbedModels(installed []string) []string {
	var out []string
	seen := make(map[string]bool, len(installed))

	for _, rec := range recommendedOllamaEmbedModels {
		for _, name := range installed {
			if seen[name] || !matchesEmbeddingBase(name, rec.Name) {
				continue
			}
			out = append(out, name)
			seen[name] = true
		}
	}
	for _, name := range installed {
		if seen[name] || !isKnownEmbeddingModel(name) {
			continue
		}
		out = append(out, name)
		seen[name] = true
	}
	return out
}

// ollamaEmbedState — результат диагностики окружения ollama при выборе
// embedder-provider=ollama в интерактивном `memo init` (resolveOllamaEmbedState,
// задача 11.1).
type ollamaEmbedState int

const (
	// ollamaStateHappy — сервер отвечает, среди установленных моделей есть
	// хотя бы одна распознанная модель эмбеддинга.
	ollamaStateHappy ollamaEmbedState = iota
	// ollamaStateNoModels — сервер отвечает, но распознанных моделей
	// эмбеддинга нет (моделей нет вовсе либо есть только чат-модели).
	ollamaStateNoModels
	// ollamaStateNotRunning — сервер не отвечает, но бинарь ollama найден в
	// PATH.
	ollamaStateNotRunning
	// ollamaStateNotInstalled — сервер не отвечает, и бинаря ollama в PATH
	// нет.
	ollamaStateNotInstalled
)

// resolveOllamaEmbedState пробует получить список установленных моделей
// ollama (fetchOllamaModels) и классифицирует окружение в одно из 4 состояний.
// lookPath инъецируется (в проде — exec.LookPath) для тестируемости без
// зависимости от реального PATH.
//
// Возвращаемый []string зависит от состояния: ollamaStateHappy — распознанные
// модели эмбеддинга (classifyOllamaEmbedModels, непусто по определению этого
// состояния); ollamaStateNoModels — сырой список установленных моделей
// (возможно пустой либо только чат-модели — вызывающий использует его для
// сообщения о том, что уже установлено); ollamaStateNotRunning/
// ollamaStateNotInstalled — nil. Возвращаемая ошибка — исходная ошибка
// fetchOllamaModels (nil при happy/no-models, ollamaStateNotRunning/
// ollamaStateNotInstalled её оборачивают только неявно, сама ошибка не
// оборачивается — вызывающий сам решает, показывать ли её текст).
func resolveOllamaEmbedState(ctx context.Context, baseURL string, lookPath func(string) (string, error)) (ollamaEmbedState, []string, error) {
	fetchCtx, cancel := context.WithTimeout(ctx, fetchModelsTimeout)
	defer cancel()

	installed, err := fetchOllamaModels(fetchCtx, baseURL)
	if err != nil {
		if _, lookErr := lookPath("ollama"); lookErr != nil {
			return ollamaStateNotInstalled, nil, err
		}
		return ollamaStateNotRunning, nil, err
	}

	embedModels := classifyOllamaEmbedModels(installed)
	if len(embedModels) == 0 {
		return ollamaStateNoModels, installed, nil
	}
	return ollamaStateHappy, embedModels, nil
}

// ollamaInstallInstructions возвращает инструкцию установки ollama под goos
// (runtime.GOOS: "darwin" | "linux" | "windows"; иначе — общая ссылка на
// страницу загрузки).
func ollamaInstallInstructions(goos string) string {
	switch goos {
	case "darwin":
		return "установи ollama: `brew install ollama` (или скачай приложение: https://ollama.com/download/mac), затем запусти `ollama serve` или само приложение."
	case "linux":
		return "установи ollama: `curl -fsSL https://ollama.com/install.sh | sh`, затем запусти `ollama serve`."
	case "windows":
		return "скачай и установи ollama: https://ollama.com/download/windows — после установки сервер запускается автоматически."
	default:
		return "скачай ollama: https://ollama.com/download."
	}
}

// ollamaStartInstructions возвращает инструкцию запуска уже установленного,
// но не отвечающего ollama-сервера под goos (состояние ollamaStateNotRunning).
func ollamaStartInstructions(goos string) string {
	switch goos {
	case "darwin", "windows":
		return "запусти приложение Ollama (или `ollama serve` в терминале)."
	case "linux":
		return "`ollama serve` (или `systemctl --user start ollama` / `sudo systemctl start ollama`)."
	default:
		return "`ollama serve`."
	}
}

// printOllamaNoModelsHelp печатает в out рекомендации по установке модели
// эмбеддинга для состояния ollamaStateNoModels: список рекомендованных
// моделей с обоснованием и командой `ollama pull`, плюс (если есть) то, что
// уже установлено, но не распознано как модель эмбеддинга.
func printOllamaNoModelsHelp(out io.Writer, installed []string) {
	fmt.Fprintln(out, "ollama отвечает, но распознанных моделей эмбеддинга среди установленных нет.")
	fmt.Fprintln(out, "рекомендуемые модели эмбеддинга:")
	for _, m := range recommendedOllamaEmbedModels {
		fmt.Fprintf(out, "  - %s (%s, ~%s): ollama pull %s\n", m.Name, m.Reason, m.ApproxSize, m.Name)
	}
	if len(installed) > 0 {
		fmt.Fprintf(out, "уже установлены (не для эмбеддинга): %s\n", strings.Join(installed, ", "))
	}
}

// printOllamaNotRunningHelp печатает в out причину недоступности сервера и
// инструкцию запуска под goos для состояния ollamaStateNotRunning (бинарь
// ollama найден в PATH, но сервер не отвечает).
func printOllamaNotRunningHelp(out io.Writer, fetchErr error, goos string) {
	fmt.Fprintf(out, "ollama установлена, но сервер не отвечает (%v).\n", fetchErr)
	fmt.Fprintln(out, ollamaStartInstructions(goos))
}

// printOllamaNotInstalledHelp печатает в out инструкцию установки ollama под
// goos и рекомендуемую модель для последующего `ollama pull` — для состояния
// ollamaStateNotInstalled (бинаря ollama нет в PATH).
func printOllamaNotInstalledHelp(out io.Writer, fetchErr error, goos string) {
	fmt.Fprintf(out, "ollama не найдена (%v).\n", fetchErr)
	fmt.Fprintln(out, ollamaInstallInstructions(goos))
	// Источник — recommendedOllamaEmbedModels[0] (тот же список, что даёт top
	// рекомендации в no-models), а не defaultOllamaModel (дефолт конфига в
	// init.go, отдельная роль) — чтобы рекомендация не расходилась между
	// состояниями диагностики.
	fmt.Fprintf(out, "после установки: ollama pull %s (рекомендуемая модель по умолчанию)\n", recommendedOllamaEmbedModels[0].Name)
}
