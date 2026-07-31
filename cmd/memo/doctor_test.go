package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	s, err := store.OpenSQLite(path)
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

// TestStoreSection_LegacyURIWarning — ревизия по итогам ревью задачи 12.2:
// путь, содержащий валидный "%XX" (тут — "%41"), теперь открывается
// OpenSQLite буквально ("profile%41.db"), а раньше (до экранирования "%" в
// sqliteDSN) SQLite декодировал бы его в другой, физически отдельный файл
// ("profileA.db", см. store.SQLiteLegacyURIPath). Табличная сводка всех
// четырёх комбинаций "есть ли память по декодированному ('legacy') пути" x
// "создан ли буквальный файл, который теперь реально откроет OpenSQLite" —
// applyLegacyURIWarning (doctor.go) должен реагировать на первое условие и
// не трогать статус, выставленный вторым, если он уже не statusOK.
func TestStoreSection_LegacyURIWarning(t *testing.T) {
	tests := []struct {
		name          string
		seedLegacy    bool // положить память в legacy-путь ("profileA.db")
		createLiteral bool // создать (пустой) буквальный файл ("profile%41.db")
		wantStatus    status
		wantMigrate   bool // ожидаем подсказку `memo migrate` в Detail
	}{
		{
			name:          "legacy с памятью, буквальный файл существует — предупреждение поднимает ok до warn",
			seedLegacy:    true,
			createLiteral: true,
			wantStatus:    statusWarn,
			wantMigrate:   true,
		},
		{
			name:          "legacy пуст, буквальный файл существует — переносить нечего, предупреждения нет",
			seedLegacy:    false,
			createLiteral: true,
			wantStatus:    statusOK,
			wantMigrate:   false,
		},
		{
			name:          "legacy с памятью, буквального файла нет — статус остаётся fail, но причина объяснена",
			seedLegacy:    true,
			createLiteral: false,
			wantStatus:    statusFail,
			wantMigrate:   true,
		},
		{
			name:          "legacy пуст, буквального файла нет — обычный fail без предупреждения",
			seedLegacy:    false,
			createLiteral: false,
			wantStatus:    statusFail,
			wantMigrate:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			legacyPath := filepath.Join(dir, "profileA.db")
			newPath := filepath.Join(dir, "profile%41.db")

			if tc.seedLegacy {
				legacySt, err := store.OpenSQLite(legacyPath)
				if err != nil {
					t.Fatalf("OpenSQLite(legacy): %v", err)
				}
				putDoctorMemory(t, legacySt, nil)
				if err := legacySt.Close(); err != nil {
					t.Fatalf("Close(legacy): %v", err)
				}
			}
			if tc.createLiteral {
				newSt, err := store.OpenSQLite(newPath)
				if err != nil {
					t.Fatalf("OpenSQLite(new): %v", err)
				}
				if err := newSt.Close(); err != nil {
					t.Fatalf("Close(new): %v", err)
				}
			}

			sec, st := storeSection(config.StoreConfig{Path: newPath})
			if st != nil {
				defer st.Close()
			}

			if sec.Status != tc.wantStatus {
				t.Errorf("Status = %s, хочу %s", sec.Status, tc.wantStatus)
			}
			hasMigrateHint := strings.Contains(sec.Detail, "memo migrate")
			if hasMigrateHint != tc.wantMigrate {
				t.Errorf("Detail = %q, наличие подсказки `memo migrate` = %v, хочу %v", sec.Detail, hasMigrateHint, tc.wantMigrate)
			}
			if tc.wantMigrate && !strings.Contains(sec.Detail, legacyPath) {
				t.Errorf("Detail = %q, хочу упоминание старого пути %q", sec.Detail, legacyPath)
			}
		})
	}
}

func TestRunDoctor_MissingDBFileExitsNonZero(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.db")
	tomlPath := writeMinimalConfig(t, t.TempDir(), missing)

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
	tomlPath := writeMinimalConfig(t, t.TempDir(), dbPath)

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

// writeMinimalConfig пишет в dir минимальный TOML-конфиг с указанным
// store.path (единственная секция — [embedder]/[reasoner] уже "noop" в
// config.Default(), лишние секции с тем же значением ничего не меняют) и
// возвращает путь к нему. dir — параметр, а не всегда свой t.TempDir():
// вызывающим (напр. findExistingSqliteMemory в init_migrate_hint_test.go)
// нужен конкретный каталог, где также лежит проверяемый sqlite-файл.
func writeMinimalConfig(t *testing.T, dir, dbPath string) string {
	t.Helper()
	tomlPath := filepath.Join(dir, "agentmemory.toml")
	content := fmt.Sprintf("[store]\npath = %q\n", dbPath)
	if err := os.WriteFile(tomlPath, []byte(content), 0o644); err != nil {
		t.Fatalf("запись конфига: %v", err)
	}
	return tomlPath
}

// TestDoctorReadOnly_DBUnchanged проверяет, что doctor не вносит в БД ничего
// сверх неизбежных механизмов самого SQLite в режиме WAL. store.Open/Close
// (используемые doctor'ом, как и любым другим read-путём memo) сами по
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
	primer, err := store.OpenSQLite(dbPath)
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

	tomlPath := writeMinimalConfig(t, t.TempDir(), doctorCopy)
	var buf bytes.Buffer
	if _, err := runDoctor([]string{"-config", tomlPath}, &buf); err != nil {
		t.Fatalf("runDoctor: %v", err)
	}

	baselineStore, err := store.OpenSQLite(baselineCopy)
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

// TestPostgresDoctorReadOnly_SchemaUnchanged — postgres-аналог
// TestDoctorReadOnly_DBUnchanged: `memo doctor` против свежей, полностью пустой
// (без единой таблицы) схемы PostgreSQL не должен эту схему инициализировать. До
// фикса postgresDatabaseSection открывала Store через общую фабрику openStore/
// store.Open, которая накатывает всю схему памяти как побочный эффект открытия
// (CREATE TABLE/INDEX, триггеры search_vec) — на свежей БД doctor молча создавал бы
// прод-схему прямо во время диагностики, вопреки собственному комментарию "ничего не
// чинит и не пишет в БД" (см. пакетный комментарий doctor.go).
//
// Заодно проверяет положительный путь диагностики готовности (pgvector/право
// CREATE, storeReadinessWarningsFor, задача 12.1, доработка симплификации —
// раньше это был отдельный TestPostgresDoctorReadOnly_ReadinessDiagnostics,
// поднимавший идентичный стенд второй раз ради одной дополнительной
// проверки): на только что созданной этим же соединением схеме роль владеет
// ею (право CREATE есть), а pgvector уже установлен на тестовом сервере в
// public (см. newTestPgSchema/withPublic=true) — секция «БД» обязана
// остаться statusOK, содержать позитивный маркер "готовность: ok" (доказывает,
// что storeReadinessWarningsFor реально вызвана и ничего не нашла, а не что
// блок диагностики вырезан из postgresDatabaseSection целиком — в обоих
// случаях warnings был бы пуст, а без маркера они неотличимы) и не
// упоминать pgvector/CREATE/vector.
//
// Схема — своя, временная (newTestPgSchema, migrate_test.go — та же изоляция,
// что у store.newTestPgStoreWithVector в internal/memory/store/postgres_test.go:
// база postgres.lan/memo общая на команду, TRUNCATE/DROP по public запрещены)
// и без единой таблицы: если бы тест использовал уже проинициализированную
// схему, "doctor не создаёт таблицы" было бы неотличимо от "таблицы уже были".
//
// Отрицательный путь диагностики готовности (нет pgvector / нет CREATE)
// намеренно не воспроизводится здесь тем же способом, что и в
// internal/memory/store/postgres_test.go (TestPgCheckPostgresReadiness):
// пришлось бы либо снимать привилегии/удалять расширение на ОБЩЕЙ командной
// базе (запрещено заданием — "общая база не чистится, только read-only"),
// либо заводить отдельную роль/БД, которых MEMO_TEST_POSTGRES_DSN не
// гарантирует. Само форматирование предупреждений по каждому false-случаю и
// то, что они не валят пробу, уже покрыто без сети (TestStoreReadinessWarnings,
// init_readiness_test.go, на fakeReadinessChecker).
//
// Гейтится MEMO_TEST_POSTGRES_DSN — не задана, тест пропускается (make test без
// живого сервера остаётся зелёным).
func TestPostgresDoctorReadOnly_SchemaUnchanged(t *testing.T) {
	dsn := os.Getenv("MEMO_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("MEMO_TEST_POSTGRES_DSN не задана — пропуск интеграционного теста doctor с живым PostgreSQL")
	}
	pgCfg, password, err := parseTestPostgresDSN(dsn)
	if err != nil {
		t.Fatalf("разбор MEMO_TEST_POSTGRES_DSN: %v", err)
	}
	pgCfg.Password = password

	// withPublic=true: pgvector установлен глобально в public на тестовом
	// сервере, а диагностика готовности резолвит тип vector через
	// to_regtype('vector') строго по search_path ЭТОГО соединения — без
	// public в search_path тип не резолвился бы вовсе, и секция «БД» ложно
	// обросла бы предупреждением про отсутствующий pgvector на сервере, где
	// он на самом деле есть (см. postgres_readiness.go: комментарий у
	// PostgresReadiness.VectorExtension).
	schema, bootstrap := newTestPgSchema(t, dsn, "memo_test_doctor_", true)

	report := buildDoctorReport(context.Background(), config.Config{Store: pgCfg}, "", false)

	dbSec := sectionByName(t, report.Sections, "БД")
	if dbSec.Status != statusOK {
		t.Fatalf("секция БД = %s (%s), хочу ok (соединение с пустой, но существующей схемой само по себе валидно, CREATE есть, pgvector установлен глобально)", dbSec.Status, dbSec.Detail)
	}
	if !strings.Contains(dbSec.Detail, "готовность: ok") {
		t.Errorf("секция БД не содержит маркер \"готовность: ok\" — похоже, диагностика готовности (pgvector/CREATE) не вызывалась: %s", dbSec.Detail)
	}
	for _, unwanted := range []string{"pgvector", "CREATE", "vector"} {
		if strings.Contains(dbSec.Detail, unwanted) {
			t.Errorf("секция БД содержит %q — готовая база не должна давать предупреждений о готовности: %s", unwanted, dbSec.Detail)
		}
	}

	var tableCount int
	if err := bootstrap.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1`, schema,
	).Scan(&tableCount); err != nil {
		t.Fatalf("проверка таблиц схемы %s: %v", schema, err)
	}
	if tableCount != 0 {
		t.Errorf("после memo doctor в схеме %s появилось %d таблиц, хочу 0 (doctor не должен инициализировать схему памяти)", schema, tableCount)
	}
}
