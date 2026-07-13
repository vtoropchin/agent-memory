package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/config"
	"agentmemory/internal/memory"
	"agentmemory/internal/memory/store"
)

// newDoctorTestStore открывает scratch-хранилище во временном каталоге теста
// и возвращает его путь (для config.Config.Store.Path) вместе с открытым
// store.Store для наполнения тестовыми записями. Возвращает путь, а не сам
// объект Store — doctor открывает БД самостоятельно через store.Open.
func newDoctorTestStore(t *testing.T) (path string, st store.Store) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "memory.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return path, s
}

// putDoctorMemory кладёт в st одну живую запись памяти с разумными дефолтами.
func putDoctorMemory(t *testing.T, st store.Store, mutate func(*memory.Memory)) {
	t.Helper()
	m := memory.Memory{
		Id:         ulid.Make().String(),
		Kind:       memory.KindSemantic,
		Scope:      "project:agentmemory",
		Content:    "содержимое по умолчанию",
		Importance: 3,
		CreatedAt:  1000,
		UpdatedAt:  1000,
	}
	if mutate != nil {
		mutate(&m)
	}
	if err := st.Put(context.Background(), m); err != nil {
		t.Fatalf("Put: %v", err)
	}
}

// ollamaTestServer поднимает httptest-сервер, отвечающий на /api/embed валидным
// эмбеддингом фиксированной размерности (тот же формат ответа, что и у
// TestNewEmbedder_OllamaAvailable в main_test.go).
func ollamaTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2,0.3]]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sectionByName находит секцию отчёта по имени или проваливает тест.
func sectionByName(t *testing.T, sections []section, name string) section {
	t.Helper()
	for _, s := range sections {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("секция %q не найдена среди %d секций", name, len(sections))
	return section{}
}

func TestBuildDoctorReport_HealthyStore(t *testing.T) {
	dbPath, st := newDoctorTestStore(t)
	putDoctorMemory(t, st, nil)
	putDoctorMemory(t, st, func(m *memory.Memory) { m.Kind = memory.KindEpisodic })

	// Симулирует завершённый reindex под ту же модель/dim, что в конфиге ниже
	// (httptest-сервер отвечает вектором длины 3) — без этого EmbedMeta пуст
	// (reindex ни разу не пробегал), и секция «Векторный слой» законно
	// показала бы ⚠️ «индекс ещё не создан», что для ЭТОГО теста (всё ✅) не
	// нужно — такой сценарий отдельно покрыт TestVectorLayerSection_*.
	if err := st.ResetVectorIndex(context.Background(), "bge-m3", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	srv := ollamaTestServer(t)
	cfg := config.Default()
	cfg.Store.Path = dbPath
	cfg.Scope.Project = "agentmemory"
	cfg.Embedder = config.EmbedderConfig{Provider: "ollama", BaseURL: srv.URL, Model: "bge-m3"}
	cfg.Reasoner = config.ReasonerConfig{Provider: "noop"}

	report := buildDoctorReport(context.Background(), cfg, "agentmemory.toml", false)

	for _, sec := range report.Sections {
		if sec.Name == "Итог" {
			continue
		}
		if sec.Status != statusOK {
			t.Errorf("секция %q = %s (%s), хочу ok", sec.Name, sec.Status, sec.Detail)
		}
	}
	if report.Verdict != "здоров" {
		t.Errorf("Verdict = %q, хочу «здоров»", report.Verdict)
	}
	if report.ExitCode != 0 {
		t.Errorf("ExitCode = %d, хочу 0", report.ExitCode)
	}
}

func TestVectorLayerSection_ModelMismatchWarns(t *testing.T) {
	_, st := newDoctorTestStore(t)
	putDoctorMemory(t, st, nil)

	// embed_meta зафиксирован под другую модель/dim, чем в конфиге —
	// имитирует reindex, который ещё не догнал смену модели embedder'а.
	if err := st.ResetVectorIndex(context.Background(), "old-model", 5); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	cfg := config.EmbedderConfig{Provider: "ollama", Model: "bge-m3"}
	sec := vectorLayerSection(context.Background(), st, cfg)

	if sec.Status != statusWarn {
		t.Fatalf("Status = %s, хочу warn; detail=%q", sec.Status, sec.Detail)
	}
}

func TestVectorLayerSection_NoIndexNoopIsOK(t *testing.T) {
	_, st := newDoctorTestStore(t)

	sec := vectorLayerSection(context.Background(), st, config.EmbedderConfig{Provider: "noop"})
	if sec.Status != statusOK {
		t.Fatalf("Status = %s, хочу ok; detail=%q", sec.Status, sec.Detail)
	}
}

func TestVectorLayerSection_NoIndexButEnabledWarns(t *testing.T) {
	_, st := newDoctorTestStore(t)

	sec := vectorLayerSection(context.Background(), st, config.EmbedderConfig{Provider: "ollama", Model: "bge-m3"})
	if sec.Status != statusWarn {
		t.Fatalf("Status = %s, хочу warn; detail=%q", sec.Status, sec.Detail)
	}
}

func TestVectorLayerSection_StoreUnavailable(t *testing.T) {
	sec := vectorLayerSection(context.Background(), nil, config.EmbedderConfig{Provider: "ollama"})
	if sec.Status != statusFail {
		t.Fatalf("Status = %s, хочу fail", sec.Status)
	}
}

func TestEmbedderLivenessSection_OllamaDown(t *testing.T) {
	closedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedSrv.Close()

	sec := embedderLivenessSection(context.Background(), config.EmbedderConfig{Provider: "ollama", BaseURL: closedSrv.URL, Model: "bge-m3"})
	if sec.Status != statusFail {
		t.Fatalf("Status = %s, хочу fail; detail=%q", sec.Status, sec.Detail)
	}
}

func TestEmbedderLivenessSection_Noop(t *testing.T) {
	sec := embedderLivenessSection(context.Background(), config.EmbedderConfig{Provider: "noop"})
	if sec.Status != statusOK {
		t.Fatalf("Status = %s, хочу ok", sec.Status)
	}
}

func TestBuildDoctorReport_OllamaDownIsBroken(t *testing.T) {
	dbPath, st := newDoctorTestStore(t)
	putDoctorMemory(t, st, nil)

	closedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedSrv.Close()

	cfg := config.Default()
	cfg.Store.Path = dbPath
	cfg.Embedder = config.EmbedderConfig{Provider: "ollama", BaseURL: closedSrv.URL, Model: "bge-m3"}

	report := buildDoctorReport(context.Background(), cfg, "", false)

	embedderSec := sectionByName(t, report.Sections, "Embedder (живость)")
	if embedderSec.Status != statusFail {
		t.Errorf("секция Embedder = %s, хочу fail", embedderSec.Status)
	}
	if report.Verdict != "сломан" {
		t.Errorf("Verdict = %q, хочу «сломан»", report.Verdict)
	}
	if report.ExitCode != 1 {
		t.Errorf("ExitCode = %d, хочу 1 (есть ❌ секция — по спеке задачи 9.1)", report.ExitCode)
	}
}

func TestDatabaseSection_MissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.db")

	sec, st := databaseSection(missing)
	if sec.Status != statusFail {
		t.Fatalf("Status = %s, хочу fail", sec.Status)
	}
	if st != nil {
		t.Fatalf("store не nil при отсутствующем файле")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("databaseSection не должен создавать файл БД, но файл существует (err=%v)", err)
	}
}

func TestRunDoctor_MissingDBFileExitsNonZero(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.db")
	tomlPath := writeMinimalConfig(t, missing)

	var buf bytes.Buffer
	code, err := runDoctor([]string{"-config", tomlPath}, &buf)
	if err != nil {
		t.Fatalf("runDoctor: %v", err)
	}
	if code != 1 {
		t.Errorf("exitCode = %d, хочу 1 (файл БД отсутствует → секция «БД» ❌)", code)
	}
}

func TestRunDoctor_JSONOutputValid(t *testing.T) {
	dbPath, st := newDoctorTestStore(t)
	putDoctorMemory(t, st, nil)
	tomlPath := writeMinimalConfig(t, dbPath)

	var buf bytes.Buffer
	if _, err := runDoctor([]string{"-config", tomlPath, "-json"}, &buf); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}

	var report doctorReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("вывод -json не распарсился: %v\nout: %s", err, buf.String())
	}
	if len(report.Sections) != 7 {
		t.Errorf("len(Sections) = %d, хочу 7 (Конфиг/БД/Записи/Вектор/Embedder/Reasoner/Итог)", len(report.Sections))
	}
	for _, sec := range report.Sections {
		if sec.Name == "" {
			t.Errorf("секция с пустым Name: %+v", sec)
		}
	}
}

// writeMinimalConfig пишет минимальный TOML-конфиг с указанным store.path и
// возвращает путь к нему.
func writeMinimalConfig(t *testing.T, dbPath string) string {
	t.Helper()
	tomlPath := filepath.Join(t.TempDir(), "agentmemory.toml")
	content := "[store]\npath = " + `"` + dbPath + `"` + "\n"
	if err := os.WriteFile(tomlPath, []byte(content), 0o644); err != nil {
		t.Fatalf("запись конфига: %v", err)
	}
	return tomlPath
}

// TestDoctorReadOnly_DBUnchanged проверяет, что doctor не вносит в БД ничего
// сверх неизбежных механизмов самого SQLite в режиме WAL. store.Open/Close
// (используемые doctor'ом, как и любым другим read-путём memoryd) сами по
// себе трогают файл: SQLite делает "courtesy checkpoint" при закрытии
// последнего соединения (sqlite.org/wal.html) — переносит накопленные
// WAL-фреймы в основной файл, даже если за сессию не было ни одной записи;
// это меняет mtime и байты файла независимо от doctor. Поэтому сравнение
// "было/стало" вокруг самого doctor всегда покажет расхождение — это баг
// теста, а не doctor. Правильная проверка: взять ДВЕ идентичные копии
// устоявшейся (уже прочекпоинченной) БД, прогнать doctor над одной и
// нейтральный "пустой" open+close (тот же неизбежный checkpoint-механизм,
// но без единого SELECT) над другой — если doctor не пишет ничего сверх
// этого неизбежного минимума, обе копии совпадут побайтово.
func TestDoctorReadOnly_DBUnchanged(t *testing.T) {
	dbPath, st := newDoctorTestStore(t)
	putDoctorMemory(t, st, nil)
	if closer, ok := st.(interface{ Close() error }); ok {
		closer.Close()
	}
	// Прогрев: гоняем WAL до устоявшегося (прочекпоинченного) состояния перед
	// снятием копий, чтобы обе копии ниже стартовали с идентичного, чистого
	// baseline (без хвоста WAL-фреймов от Put выше).
	primer, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("прогревающий store.Open: %v", err)
	}
	if err := primer.Close(); err != nil {
		t.Fatalf("прогревающий Close: %v", err)
	}

	doctorCopy := filepath.Join(t.TempDir(), "doctor.db")
	baselineCopy := filepath.Join(t.TempDir(), "baseline.db")
	copyFile(t, dbPath, doctorCopy)
	copyFile(t, dbPath, baselineCopy)

	tomlPath := writeMinimalConfig(t, doctorCopy)
	var buf bytes.Buffer
	if _, err := runDoctor([]string{"-config", tomlPath}, &buf); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}

	baselineStore, err := store.Open(baselineCopy)
	if err != nil {
		t.Fatalf("baseline store.Open: %v", err)
	}
	if err := baselineStore.Close(); err != nil {
		t.Fatalf("baseline Close: %v", err)
	}

	doctorContent, err := os.ReadFile(doctorCopy)
	if err != nil {
		t.Fatalf("ReadFile doctorCopy: %v", err)
	}
	baselineContent, err := os.ReadFile(baselineCopy)
	if err != nil {
		t.Fatalf("ReadFile baselineCopy: %v", err)
	}

	if !bytes.Equal(doctorContent, baselineContent) {
		t.Errorf("содержимое БД после doctor расходится с нейтральным open+close (read-only инвариант нарушен): doctor %d байт, baseline %d байт", len(doctorContent), len(baselineContent))
	}
}

// copyFile копирует src в dst побайтово (для read-only теста выше — снятие
// независимых копий устоявшейся БД).
func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("чтение %s: %v", src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("запись %s: %v", dst, err)
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{0, "0 Б"},
		{512, "512 Б"},
		{2048, "2.0 КБ"},
		{5 * 1024 * 1024, "5.0 МБ"},
	}
	for _, tt := range tests {
		if got := humanSize(tt.bytes); got != tt.want {
			t.Errorf("humanSize(%d) = %q, хочу %q", tt.bytes, got, tt.want)
		}
	}
}
