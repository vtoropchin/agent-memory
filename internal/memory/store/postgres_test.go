package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/oklog/ulid/v2"

	"agentmemory/internal/memory"
)

// pgTestDSNEnv — переменная окружения с DSN тестовой БД PostgreSQL (задача C2).
// Формат — обычный postgres:// connection string, напр.
// "postgres://memo:memo@postgres.lan:5432/memo?sslmode=disable". Не задана —
// все тесты этого файла пропускаются (newTestPgStore -> t.Skip), поэтому
// `make test` без живого сервера остаётся зелёным.
const pgTestDSNEnv = "MEMO_TEST_POSTGRES_DSN"

// newTestPgStore открывает pgStore на свежесозданной, уникальной для этого вызова
// схеме PostgreSQL и регистрирует её удаление в t.Cleanup.
//
// Стратегия изоляции — схема-на-прогон через search_path, а не префиксы id в общей
// public-схеме:
//   - база `memo` на postgres.lan — общая, командная (см. задачу C2): любой тест,
//     пишущий в public наравне с боевыми/чужими тестовыми данными, рискует их
//     задеть (List/Stats/AllForExport без фильтра увидят чужие записи, Neighbors
//     пересечётся по id и т.д.) — префиксы id это НЕ изолируют, они разделяют
//     только "мои" строки от "чужих" внутри одних и тех же таблиц, но не спасают
//     от Stats/List/AllForExport, которые по контракту не принимают такой фильтр;
//   - векторный слой (ResetVectorIndex/PutVector/SearchVector/SemanticNeighbors)
//     живёт в ОДНОЙ таблице memories_vec на весь backend с фиксированной
//     размерностью (embed_meta) — параллельные тесты с разными dim физически не
//     могут ужиться в общей public-схеме одновременно, префиксами id это не решить;
//   - отдельная схема на вызов даёт КАЖДОМУ тесту (и каждому параллельному прогону
//     `go test`/CI) полностью независимый набор memories/tags/links/schema_version/
//     embed_meta/memories_vec — тот же уровень изоляции, что t.TempDir()+файл даёт
//     sqlite-тестам (newTestStore).
//
// Изоляция достигается БЕЗ единой правки postgres.go/config.go: search_path —
// обычный query-параметр connection string, который pgconn (jackc/pgx/v5) трактует
// как run-time параметр и отправляет в startup-пакете при установлении КАЖДОГО
// физического соединения пула — поэтому достаточно добавить его в DSN перед
// sql.Open, ничего не меняя в openPostgres/pgStore.init (проверено вручную:
// разные соединения одного *sql.DB видят один и тот же search_path). config.StoreConfig
// не тронут: у неё нет и не должно быть понятия "схема" — это чисто тестовый приём.
//
// search_path здесь — РОВНО одна схема, БЕЗ ",public" (см. newTestPgStoreWithVector
// и её комментарий про инцидент с ResetVectorIndex): для всех методов, кроме
// векторного слоя, это ничего не ограничивает (тип vector им не нужен), зато
// гарантирует, что ни один unqualified-запрос физически не может задеть public —
// он либо резолвится в нашей схеме, либо (если объекта там ещё нет) падает с
// "relation does not exist", а не тихо находит одноимённый объект в public.
func newTestPgStore(t *testing.T) *pgStore {
	t.Helper()
	return newTestPgStoreSchema(t, false)
}

// newTestPgStoreWithVector — как newTestPgStore, но для тестов, которым нужен
// векторный слой (вызывают ResetVectorIndex): search_path включает ",public" следом
// за схемой, чтобы стал виден тип vector (расширение pgvector — объект уровня БД,
// singleton: оно уже установлено в public на этом сервере, CREATE EXTENSION IF NOT
// EXISTS в ResetVectorIndex — no-op и НЕ переустанавливает его в нашей схеме, а без
// public в search_path тип vector не резолвится вовсе).
//
// До фикса ловушки DROP-по-search_path (см. развёрнутый комментарий у
// ResetVectorIndex в postgres_vector.go) этому хелперу требовалась заглушка —
// пустая memories_vec, предсозданная в тестовой схеме ДО открытия соединения с
// public в search_path, — чтобы голый `DROP TABLE IF EXISTS memories_vec` не
// нашёл и не снёс public.memories_vec (что на постгрес.lan реально один раз
// случилось при разработке этой изоляции). Теперь ResetVectorIndex/hasVecTable/
// PutVector и все остальные обращения к memories_vec в драйвере схема-
// квалифицированы явно (pgStore.schema, зафиксированная через current_schema() в
// pgStore.init) и резолвятся в СВОЮ схему независимо от того, что ещё есть в
// search_path — заглушка стала не нужна, ",public" в search_path остаётся только
// ради видимости типа vector.
func newTestPgStoreWithVector(t *testing.T) *pgStore {
	t.Helper()
	return newTestPgStoreSchema(t, true)
}

func newTestPgStoreSchema(t *testing.T, withVector bool) *pgStore {
	t.Helper()
	s := &pgStore{db: newTestPgSchemaDB(t, withVector), now: defaultNow}
	if err := s.init(context.Background()); err != nil {
		t.Fatalf("init тестовой схемы: %v", err)
	}
	return s
}

// newTestPgSchemaDB создаёт свежую уникальную схему PostgreSQL (см. newTestPgStore
// про изоляцию), регистрирует её удаление и возвращает пул соединений с search_path на
// неё — БЕЗ init: схема пуста, это нужно тестам открытия общей базы без автомиграции
// (postgres_hub_test.go).
func newTestPgSchemaDB(t *testing.T, withVector bool) *sql.DB {
	t.Helper()
	baseDSN := os.Getenv(pgTestDSNEnv)
	if baseDSN == "" {
		t.Skipf("%s не задан — пропуск postgres-тестов store. Пример:\n  %s=postgres://memo:memo@postgres.lan:5432/memo?sslmode=disable make test", pgTestDSNEnv, pgTestDSNEnv)
	}

	ctx := context.Background()
	// Префикс буквой (не цифрой) — ULID может начинаться с цифры, а неэкранированный
	// идентификатор Postgres обязан начинаться с буквы/подчёркивания. Строчные буквы —
	// Postgres всё равно свернул бы неэкранированный идентификатор к нижнему регистру.
	schema := "memo_test_" + strings.ToLower(ulid.Make().String())

	bootstrap, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("открытие bootstrap-соединения (%s): %v", pgTestDSNEnv, err)
	}
	defer bootstrap.Close()
	if _, err := bootstrap.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("создание тестовой схемы %s: %v", schema, err)
	}
	// Регистрируется ДО открытия основного соединения: t.Cleanup вызывает функции в
	// порядке LIFO, поэтому закрытие db (регистрируется ниже) отработает раньше
	// удаления схемы — соединения, использующие схему, уже закрыты к моменту DROP.
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		cleanup, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Errorf("открытие соединения для уборки схемы %s: %v", schema, err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
			t.Errorf("удаление тестовой схемы %s (общая база memo не должна копить мусор): %v", schema, err)
		}
	})

	searchPath := schema
	if withVector {
		// ",public" — только ради видимости типа vector (см. комментарий у
		// newTestPgStoreWithVector); memories_vec заглушки больше не требует —
		// ResetVectorIndex резолвит её в pgStore.schema явно, не по search_path.
		searchPath = schema + ",public"
	}

	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("разбор %s: %v", pgTestDSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", searchPath)
	u.RawQuery = q.Encode()

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("открытие тестового соединения (схема %s): %v", schema, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// setNow подменяет источник текущего времени у store для детерминированных тестов
// Supersede/Touch/TTL. Parity-тесты работают через интерфейс Store и поэтому не
// имеют прямого доступа к приватному полю now конкретной реализации — этот хелпер
// закрывает разницу через переключение по типу (единственное место в тестах,
// которое знает о конкретных типах sqliteStore/pgStore).
func setNow(t *testing.T, s Store, now func() int64) {
	t.Helper()
	switch st := s.(type) {
	case *sqliteStore:
		st.now = now
	case *pgStore:
		st.now = now
	default:
		t.Fatalf("setNow: неизвестная реализация Store %T", s)
	}
}

// runParity прогоняет один и тот же сценарий fn против sqlite- и postgres-реализаций
// Store с одинаковыми входными данными и одинаковыми ожиданиями внутри fn (задача
// C2: "самое ценное — не повторить тесты, а поймать расхождения между двумя
// реализациями"). Если fn проходит на sqlite, но падает на postgres (или наоборот) —
// это и есть найденное расхождение. postgres-прогон пропускается через t.Skip
// внутри newTestPgStore, если pgTestDSNEnv не задана.
func runParity(t *testing.T, name string, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run(name+"/sqlite", func(t *testing.T) {
		fn(t, newTestStore(t))
	})
	t.Run(name+"/postgres", func(t *testing.T) {
		fn(t, newTestPgStore(t))
	})
}

// runParityWithVector — как runParity, но открывает postgres-хранилище через
// newTestPgStoreWithVector: нужна только сценариям, которые вызывают
// ResetVectorIndex (см. её комментарий про изоляцию public/memories_vec) — обычный
// newTestPgStore держит search_path без public и не годится для реального
// векторного слоя (тип vector не резолвится).
func runParityWithVector(t *testing.T, name string, fn func(t *testing.T, s Store)) {
	t.Helper()
	t.Run(name+"/sqlite", func(t *testing.T) {
		fn(t, newTestStore(t))
	})
	t.Run(name+"/postgres", func(t *testing.T) {
		fn(t, newTestPgStoreWithVector(t))
	})
}

func TestParityPutGetRoundtrip(t *testing.T) {
	runParity(t, "PutGetRoundtrip", parityPutGetRoundtrip)
}

func parityPutGetRoundtrip(t *testing.T, s Store) {
	ctx := context.Background()

	id := newID(t)
	m := memory.Memory{
		Id:         id,
		Kind:       memory.KindSemantic,
		Scope:      "project:parity",
		Content:    "Проект на Go, PostgreSQL и pgvector через jackc/pgx",
		Summary:    strPtr("Go + PostgreSQL"),
		Importance: 4,
		CreatedAt:  1000,
		UpdatedAt:  1000,
		AccessedAt: int64Ptr(1500),
		Source:     strPtr("session-1"),
		Tags:       []string{"go", "stack"},
	}

	otherID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: otherID, Kind: memory.KindSemantic, Scope: "project:parity",
		Content: "другая запись", Importance: 3, CreatedAt: 999, UpdatedAt: 999,
	}); err != nil {
		t.Fatalf("Put other: %v", err)
	}
	m.Links = []memory.Link{{Dst: otherID, Rel: memory.RelRelated}}

	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != m.Content {
		t.Errorf("Content = %q, want %q", got.Content, m.Content)
	}
	if got.Summary == nil || *got.Summary != *m.Summary {
		t.Errorf("Summary = %v, want %v", got.Summary, m.Summary)
	}
	if got.Source == nil || *got.Source != *m.Source {
		t.Errorf("Source = %v, want %v", got.Source, m.Source)
	}
	if got.AccessedAt == nil || *got.AccessedAt != 1500 {
		t.Errorf("AccessedAt = %v, want 1500", got.AccessedAt)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "go" || got.Tags[1] != "stack" {
		t.Errorf("Tags = %v, want [go stack]", got.Tags)
	}
	if len(got.Links) != 1 || got.Links[0].Dst != otherID || got.Links[0].Rel != memory.RelRelated {
		t.Errorf("Links = %v", got.Links)
	}

	// Upsert: обновляем content, ожидаем что старые tags/links полностью заменяются.
	m.Content = "обновлённый факт"
	m.Tags = []string{"updated"}
	m.Links = nil
	if err := s.Put(ctx, m); err != nil {
		t.Fatalf("Put update: %v", err)
	}
	got2, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got2.Content != "обновлённый факт" {
		t.Errorf("Content после update = %q", got2.Content)
	}
	if len(got2.Tags) != 1 || got2.Tags[0] != "updated" {
		t.Errorf("Tags после update = %v, want [updated]", got2.Tags)
	}
	if len(got2.Links) != 0 {
		t.Errorf("Links после update = %v, want empty", got2.Links)
	}
}

func TestParityPutRejectsEmptyOrInvalidId(t *testing.T) {
	runParity(t, "PutRejectsEmptyOrInvalidId", func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := s.Put(ctx, memory.Memory{Kind: memory.KindSemantic, Scope: "x", Content: "c"}); err == nil {
			t.Error("Put с пустым Id должен вернуть ошибку")
		}
		if err := s.Put(ctx, memory.Memory{Id: "not-a-ulid", Kind: memory.KindSemantic, Scope: "x", Content: "c"}); err == nil {
			t.Error("Put с некорректным ULID должен вернуть ошибку")
		}
	})
}

func TestParityGetNotFound(t *testing.T) {
	runParity(t, "GetNotFound", func(t *testing.T, s Store) {
		ctx := context.Background()
		_, err := s.Get(ctx, newID(t))
		if err == nil {
			t.Fatal("Get несуществующей записи должен вернуть ошибку")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("err = %v, want errors.Is(err, ErrNotFound)", err)
		}
	})
}

// TestParityExpiresAtNullSemantics — самое тонкое место контракта Put (см. комментарий
// в sqliteStore.Put/pgStore.Put): ExpiresAt=0 обязан храниться как SQL NULL, а не
// литеральный 0 — иначе metaFilters/metaFiltersPG (m.expires_at IS NULL OR
// m.expires_at > ?now) сочтут запись просроченной ещё в эпоху unix 0, и она
// немедленно пропадёт из SearchLexical/SearchVector на любом backend.
func TestParityExpiresAtNullSemantics(t *testing.T) {
	runParity(t, "ExpiresAtNullSemantics", func(t *testing.T, s Store) {
		ctx := context.Background()
		setNow(t, s, func() int64 { return 1000 })

		eternal := newID(t)
		if err := s.Put(ctx, memory.Memory{
			Id: eternal, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "вечная запись без срока годности", CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 0,
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}

		got, err := s.Get(ctx, eternal)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ExpiresAt != 0 {
			t.Errorf("ExpiresAt = %d, want 0", got.ExpiresAt)
		}

		cands, err := s.SearchLexical(ctx, memory.Query{Text: "вечная запись", Scope: "project:parity", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		found := false
		for _, c := range cands {
			if c.Memory.Id == eternal {
				found = true
			}
		}
		if !found {
			t.Fatalf("SearchLexical не нашёл вечную запись (ExpiresAt=0 должен храниться как NULL, не литеральный 0): %+v", cands)
		}
	})
}

func TestParitySearchLexicalBasic(t *testing.T) {
	runParity(t, "SearchLexicalBasic", paritySearchLexicalBasic)
}

func paritySearchLexicalBasic(t *testing.T, s Store) {
	ctx := context.Background()

	semID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: semID, Kind: memory.KindSemantic, Scope: "project:parity",
		Content: "Проект использует PostgreSQL и pgvector для поиска", Importance: 4,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put sem: %v", err)
	}

	procID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: procID, Kind: memory.KindProcedural, Scope: "project:parity",
		Content: "Сборка через make build, тесты через make test", Importance: 3,
		CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("Put proc: %v", err)
	}

	otherScopeID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: otherScopeID, Kind: memory.KindSemantic, Scope: "project:other",
		Content: "PostgreSQL в другом проекте", Importance: 5,
		CreatedAt: 3, UpdatedAt: 3,
	}); err != nil {
		t.Fatalf("Put other scope: %v", err)
	}

	supersededID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: supersededID, Kind: memory.KindSemantic, Scope: "project:parity",
		Content: "PostgreSQL устаревший факт", Importance: 3,
		CreatedAt: 4, UpdatedAt: 4,
	}); err != nil {
		t.Fatalf("Put superseded: %v", err)
	}
	if err := s.Supersede(ctx, supersededID, semID); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("находит по content и фильтрует по scope", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "PostgreSQL", Scope: "project:parity", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 {
			t.Fatalf("candidates = %d, want 1 (нашли: %+v)", len(cands), cands)
		}
		if cands[0].Memory.Id != semID {
			t.Errorf("candidate.Id = %q, want %q", cands[0].Memory.Id, semID)
		}
		if cands[0].Source != "lexical" {
			t.Errorf("Source = %q, want lexical", cands[0].Source)
		}
	})

	t.Run("фильтрует по kind", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{
			Text: "make", Scope: "project:parity", Kinds: []string{"procedural"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 || cands[0].Memory.Id != procID {
			t.Fatalf("candidates = %+v, want procID", cands)
		}
	})

	t.Run("исключает superseded", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "устаревший", Scope: "project:parity", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 0 {
			t.Fatalf("candidates = %+v, want пусто (запись superseded)", cands)
		}
	})

	t.Run("кривой запрос не паникует и не ошибается", func(t *testing.T) {
		weird := []string{
			`"unterminated`,
			`col: OR NOT AND (`,
			`-- ; DROP TABLE memories`,
			`""""`,
			"   ",
			"",
		}
		for _, q := range weird {
			if _, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:parity", Limit: 10}); err != nil {
				t.Errorf("SearchLexical(%q) вернул ошибку: %v", q, err)
			}
		}
	})
}

func TestParitySearchLexicalScopes(t *testing.T) {
	runParity(t, "SearchLexicalScopes", paritySearchLexicalScopes)
}

func paritySearchLexicalScopes(t *testing.T, s Store) {
	ctx := context.Background()

	aID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: aID, Kind: memory.KindSemantic, Scope: "project:a",
		Content: "заметка про golang в проекте a", Importance: 3,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put a: %v", err)
	}

	userID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: userID, Kind: memory.KindSemantic, Scope: "user:*",
		Content: "заметка про golang общая", Importance: 3,
		CreatedAt: 2, UpdatedAt: 2,
	}); err != nil {
		t.Fatalf("Put user: %v", err)
	}

	bID := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: bID, Kind: memory.KindSemantic, Scope: "project:b",
		Content: "заметка про golang в проекте b", Importance: 3,
		CreatedAt: 3, UpdatedAt: 3,
	}); err != nil {
		t.Fatalf("Put b: %v", err)
	}

	t.Run("набор scopes возвращает записи из всех перечисленных scope и не возвращает прочие", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{
			Text: "golang", Scopes: []string{"project:a", "user:*"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(cands) != 2 || !got[aID] || !got[userID] {
			t.Fatalf("candidates = %+v, want [a user]", cands)
		}
		if got[bID] {
			t.Errorf("candidates содержат запись из project:b, не входящего в Scopes")
		}
	})

	t.Run("Scopes имеет приоритет над Scope", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{
			Text: "golang", Scope: "project:b", Scopes: []string{"project:a"}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 || cands[0].Memory.Id != aID {
			t.Fatalf("candidates = %+v, want [a] (Scopes приоритетнее Scope)", cands)
		}
	})
}

func TestParitySearchLexicalTagsFilter(t *testing.T) {
	runParity(t, "SearchLexicalTagsFilter", paritySearchLexicalTagsFilter)
}

// paritySearchLexicalTagsFilter проверяет OR-семантику Query.Tags (задача 8.2):
// запись проходит, если имеет хотя бы один из перечисленных тегов.
func paritySearchLexicalTagsFilter(t *testing.T, s Store) {
	ctx := context.Background()

	stackID, goID, bothID, noneID := newID(t), newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id   string
		tags []string
	}{
		{stackID, []string{"стек"}},
		{goID, []string{"go"}},
		{bothID, []string{"стек", "go"}},
		{noneID, nil},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "заметка про инструменты проекта", Importance: 3,
			CreatedAt: 1, UpdatedAt: 1, Tags: tc.tags,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
	}

	t.Run("один тег", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "заметка", Tags: []string{"стек"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(got) != 2 || !got[stackID] || !got[bothID] {
			t.Fatalf("candidates = %+v, want [stackID bothID]", cands)
		}
	})

	t.Run("несколько тегов — OR", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "заметка", Tags: []string{"стек", "go"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		got := map[string]bool{}
		for _, c := range cands {
			got[c.Memory.Id] = true
		}
		if len(got) != 3 || !got[stackID] || !got[goID] || !got[bothID] {
			t.Fatalf("candidates = %+v, want [stackID goID bothID]", cands)
		}
	})

	t.Run("несуществующий тег — пусто", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "заметка", Tags: []string{"нет-такого-тега"}, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 0 {
			t.Fatalf("candidates = %+v, want пусто", cands)
		}
	})
}

func TestParitySearchLexicalCreatedAtAndCombinedFilters(t *testing.T) {
	runParity(t, "SearchLexicalCreatedAtAndCombinedFilters", paritySearchLexicalCreatedAtAndCombinedFilters)
}

func paritySearchLexicalCreatedAtAndCombinedFilters(t *testing.T, s Store) {
	ctx := context.Background()

	old, mid, recent := newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id string
		ts int64
	}{
		{old, 100},
		{mid, 200},
		{recent, 300},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "хронологическая заметка", Importance: 3,
			CreatedAt: tc.ts, UpdatedAt: tc.ts,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
	}

	t.Run("CreatedAfter/CreatedBefore включают границу", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "хронологическая", CreatedAfter: 200, CreatedBefore: 200, Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 || cands[0].Memory.Id != mid {
			t.Fatalf("candidates = %+v, want [mid]", cands)
		}
	})

	// комбинация scope+kind+tag+даты: запись должна пройти ВСЕ условия сразу (AND
	// между разными видами фильтров, задача 8.2).
	match := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: match, Kind: memory.KindProcedural, Scope: "project:parity",
		Content: "комбинированный рецепт сборки", Importance: 3,
		CreatedAt: 200, UpdatedAt: 200, Tags: []string{"стек"},
	}); err != nil {
		t.Fatalf("Put match: %v", err)
	}
	wrongKind := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: wrongKind, Kind: memory.KindSemantic, Scope: "project:parity",
		Content: "комбинированный факт", Importance: 3,
		CreatedAt: 200, UpdatedAt: 200, Tags: []string{"стек"},
	}); err != nil {
		t.Fatalf("Put wrongKind: %v", err)
	}
	wrongTag := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: wrongTag, Kind: memory.KindProcedural, Scope: "project:parity",
		Content: "комбинированный рецепт без нужного тега", Importance: 3,
		CreatedAt: 200, UpdatedAt: 200, Tags: []string{"другое"},
	}); err != nil {
		t.Fatalf("Put wrongTag: %v", err)
	}

	t.Run("scope+kind+tag+даты — AND между видами фильтров", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{
			Text: "комбинированный", Scope: "project:parity",
			Kinds: []string{"procedural"}, Tags: []string{"стек"},
			CreatedAfter: 100, Limit: 10,
		})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 1 || cands[0].Memory.Id != match {
			t.Fatalf("candidates = %+v, want [match]", cands)
		}
	})
}

func TestParitySearchLexicalExpiryFilter(t *testing.T) {
	runParity(t, "SearchLexicalExpiryFilter", paritySearchLexicalExpiryFilter)
}

// paritySearchLexicalExpiryFilter проверяет, что общий слой фильтров TTL (задача
// 9.4) скрывает записи с истёкшим сроком, оставляя вечные и ещё не истёкшие
// видимыми — и что граница expires_at == now уже трактуется как просроченная
// (строгое ">", не ">="), одинаково на обоих backend.
func paritySearchLexicalExpiryFilter(t *testing.T, s Store) {
	ctx := context.Background()
	setNow(t, s, func() int64 { return 1000 })

	eternal, future, boundary, expired := newID(t), newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id        string
		expiresAt int64
	}{
		{eternal, 0},
		{future, 1001},
		{boundary, 1000},
		{expired, 999},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "запись с сроком годности", Importance: 3,
			CreatedAt: 1, UpdatedAt: 1, ExpiresAt: tc.expiresAt,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
	}

	cands, err := s.SearchLexical(ctx, memory.Query{Text: "срока годности", Scope: "project:parity", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Memory.Id] = true
	}
	if len(got) != 2 || !got[eternal] || !got[future] {
		t.Fatalf("candidates = %+v, want [eternal future] (boundary/expired должны быть скрыты)", cands)
	}
}

// TestParitySearchLexicalScoreDirection проверяет направление Score («больше =
// релевантнее», DESIGN.md §8): запись, совпадающая по большему числу слов запроса,
// должна получить более высокий Score, чем запись, совпадающая только по одному —
// одинаково для sqlite (-bm25) и postgres (ts_rank).
func TestParitySearchLexicalScoreDirection(t *testing.T) {
	runParity(t, "SearchLexicalScoreDirection", func(t *testing.T, s Store) {
		ctx := context.Background()

		strong, weak := newID(t), newID(t)
		if err := s.Put(ctx, memory.Memory{
			Id: strong, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "черепаха ниндзя пицца скейтборд", Importance: 3,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("Put strong: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{
			Id: weak, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "черепаха плавает в пруду", Importance: 3,
			CreatedAt: 2, UpdatedAt: 2,
		}); err != nil {
			t.Fatalf("Put weak: %v", err)
		}

		cands, err := s.SearchLexical(ctx, memory.Query{Text: "черепаха ниндзя пицца", Scope: "project:parity", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		if len(cands) != 2 {
			t.Fatalf("candidates = %+v, want 2", cands)
		}
		if cands[0].Memory.Id != strong {
			t.Fatalf("cands[0].Id = %q, want %q (запись с бОльшим числом совпавших слов должна идти первой)", cands[0].Memory.Id, strong)
		}
		if cands[0].Score <= cands[1].Score {
			t.Errorf("Score = [%f %f], want убывание (больше — релевантнее)", cands[0].Score, cands[1].Score)
		}
	})
}

func TestParityList(t *testing.T) {
	runParity(t, "List", parityList)
}

func parityList(t *testing.T, s Store) {
	ctx := context.Background()

	var ids []string
	for i := 0; i < 5; i++ {
		id := newID(t)
		ids = append(ids, id)
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindSemantic, Scope: "project:parity", Content: "запись",
			CreatedAt: int64(i), UpdatedAt: int64(i), Tags: []string{"t" + id[:1]},
		}); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	wantOrder := make([]string, len(ids))
	for i, id := range ids {
		wantOrder[len(ids)-1-i] = id
	}

	t.Run("пагинация: created_at DESC, total не зависит от страницы", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:parity"}, Limit: 2, Offset: 0})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 5 {
			t.Errorf("total = %d, want 5", total)
		}
		if len(items) != 2 || items[0].Id != wantOrder[0] || items[1].Id != wantOrder[1] {
			t.Fatalf("items = %+v, want [%q %q]", items, wantOrder[0], wantOrder[1])
		}
		if len(items[0].Tags) != 1 {
			t.Errorf("items[0].Tags = %v, want заполнены", items[0].Tags)
		}
		if items[0].Links != nil {
			t.Errorf("items[0].Links = %v, want nil (List не грузит Links)", items[0].Links)
		}

		page2, total2, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:parity"}, Limit: 2, Offset: 2})
		if err != nil {
			t.Fatalf("List страница 2: %v", err)
		}
		if total2 != 5 {
			t.Errorf("total (страница 2) = %d, want 5", total2)
		}
		if len(page2) != 2 || page2[0].Id != wantOrder[2] || page2[1].Id != wantOrder[3] {
			t.Fatalf("page2 = %+v, want [%q %q]", page2, wantOrder[2], wantOrder[3])
		}
	})

	t.Run("Offset за пределами total даёт пустой items и корректный total", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:parity"}, Limit: 10, Offset: 100})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 5 {
			t.Errorf("total = %d, want 5", total)
		}
		if len(items) != 0 {
			t.Errorf("items = %+v, want пусто", items)
		}
	})

	t.Run("отрицательный Offset трактуется как 0, Limit<=0 — дефолт", func(t *testing.T) {
		items, _, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:parity"}, Limit: 0, Offset: -5})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(items) != 5 {
			t.Errorf("items = %+v, want 5 (Offset<0 -> 0, Limit<=0 -> дефолт)", items)
		}
	})
}

func TestParityListFiltersAndSupersededVisibility(t *testing.T) {
	runParity(t, "ListFiltersAndSupersededVisibility", parityListFiltersAndSupersededVisibility)
}

func parityListFiltersAndSupersededVisibility(t *testing.T, s Store) {
	ctx := context.Background()

	aSem, aProc, bSem := newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: aSem, Kind: memory.KindSemantic, Scope: "project:parity-a", Content: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put aSem: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: aProc, Kind: memory.KindProcedural, Scope: "project:parity-a", Content: "c", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put aProc: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: bSem, Kind: memory.KindSemantic, Scope: "project:parity-b", Content: "c", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put bSem: %v", err)
	}

	t.Run("фильтр по scope и kind", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:parity-a"}, Kinds: []string{"semantic"}, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 1 || len(items) != 1 || items[0].Id != aSem {
			t.Fatalf("items/total = %+v/%d, want [aSem]/1", items, total)
		}
	})

	superseded, replacement := newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: replacement, Kind: memory.KindSemantic, Scope: "project:parity-a", Content: "замена", CreatedAt: 4, UpdatedAt: 4}); err != nil {
		t.Fatalf("Put replacement: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: superseded, Kind: memory.KindSemantic, Scope: "project:parity-a", Content: "устаревшая", CreatedAt: 5, UpdatedAt: 5}); err != nil {
		t.Fatalf("Put superseded: %v", err)
	}
	if err := s.Supersede(ctx, superseded, replacement); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("по умолчанию superseded исключены, IncludeSuperseded включает их", func(t *testing.T) {
		items, total, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:parity-a"}, Limit: 10})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if total != 3 {
			t.Fatalf("total = %d, want 3", total)
		}
		for _, m := range items {
			if m.Id == superseded {
				t.Errorf("items содержат superseded запись: %+v", items)
			}
		}

		itemsAll, totalAll, err := s.List(ctx, BrowseFilter{Scopes: []string{"project:parity-a"}, Limit: 10, IncludeSuperseded: true})
		if err != nil {
			t.Fatalf("List (IncludeSuperseded): %v", err)
		}
		if totalAll != 4 || len(itemsAll) != 4 {
			t.Fatalf("itemsAll/totalAll = %+v/%d, want 4/4", itemsAll, totalAll)
		}
	})
}

func TestParityStats(t *testing.T) {
	runParity(t, "Stats", parityStats)
}

func parityStats(t *testing.T, s Store) {
	ctx := context.Background()

	aSem, aProc, bSem, superseded := newID(t), newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: aSem, Kind: memory.KindSemantic, Scope: "project:parity-a", Content: "c", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put aSem: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: aProc, Kind: memory.KindProcedural, Scope: "project:parity-a", Content: "c", CreatedAt: 2, UpdatedAt: 2}); err != nil {
		t.Fatalf("Put aProc: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: bSem, Kind: memory.KindSemantic, Scope: "project:parity-b", Content: "c", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put bSem: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: superseded, Kind: memory.KindSemantic, Scope: "project:parity-a", Content: "c", CreatedAt: 4, UpdatedAt: 4}); err != nil {
		t.Fatalf("Put superseded: %v", err)
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Total != 4 || stats.Superseded != 0 {
		t.Fatalf("stats = %+v, want Total=4 Superseded=0", stats)
	}
	if stats.ByScope["project:parity-a"] != 3 || stats.ByScope["project:parity-b"] != 1 {
		t.Errorf("ByScope = %v, want project:parity-a=3 project:parity-b=1", stats.ByScope)
	}
	if stats.ByKind["semantic"] != 3 || stats.ByKind["procedural"] != 1 {
		t.Errorf("ByKind = %v, want semantic=3 procedural=1", stats.ByKind)
	}

	if err := s.Supersede(ctx, superseded, aSem); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	stats, err = s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats после supersede: %v", err)
	}
	if stats.Total != 3 || stats.Superseded != 1 {
		t.Fatalf("stats после supersede = %+v, want Total=3 Superseded=1", stats)
	}
}

func TestParityAllForExport(t *testing.T) {
	runParity(t, "AllForExport", parityAllForExport)
}

// parityAllForExport проверяет, что AllForExport возвращает ВСЕ записи (включая
// тумстоуны), отсортированные по id ASC, вместе с Tags/Links — контракт, отдельный
// от List (постранично, без тумстоунов и без Links) и AllForReindex (без тумстоунов).
func parityAllForExport(t *testing.T, s Store) {
	ctx := context.Background()

	replacement := newID(t)
	if err := s.Put(ctx, memory.Memory{Id: replacement, Kind: memory.KindSemantic, Scope: "project:parity", Content: "замена", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("Put replacement: %v", err)
	}
	live := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: live, Kind: memory.KindSemantic, Scope: "project:parity", Content: "живая запись",
		CreatedAt: 2, UpdatedAt: 2, Tags: []string{"go"},
		Links: []memory.Link{{Dst: replacement, Rel: memory.RelRelated}},
	}); err != nil {
		t.Fatalf("Put live: %v", err)
	}
	tombstoned := newID(t)
	if err := s.Put(ctx, memory.Memory{Id: tombstoned, Kind: memory.KindSemantic, Scope: "project:parity", Content: "устаревшая", CreatedAt: 3, UpdatedAt: 3}); err != nil {
		t.Fatalf("Put tombstoned: %v", err)
	}
	if err := s.Supersede(ctx, tombstoned, replacement); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	all, err := s.AllForExport(ctx, []string{"project:parity"})
	if err != nil {
		t.Fatalf("AllForExport: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("AllForExport = %+v, want 3 записи (включая тумстоун)", all)
	}
	// Порядок — id ASC.
	for i := 1; i < len(all); i++ {
		if all[i].Id < all[i-1].Id {
			t.Fatalf("AllForExport не отсортирован по id ASC: %+v", all)
		}
	}
	byID := map[string]memory.Memory{}
	for _, m := range all {
		byID[m.Id] = m
	}
	tomb, ok := byID[tombstoned]
	if !ok {
		t.Fatalf("AllForExport не содержит тумстоун %q: %+v", tombstoned, all)
	}
	if tomb.SupersededBy == nil || *tomb.SupersededBy != replacement {
		t.Errorf("тумстоун.SupersededBy = %v, want %q", tomb.SupersededBy, replacement)
	}
	liveGot := byID[live]
	if len(liveGot.Tags) != 1 || liveGot.Tags[0] != "go" {
		t.Errorf("AllForExport не подгрузил Tags: %+v", liveGot)
	}
	if len(liveGot.Links) != 1 || liveGot.Links[0].Dst != replacement {
		t.Errorf("AllForExport не подгрузил Links: %+v", liveGot)
	}
}

func TestParityAllForReindex(t *testing.T) {
	runParity(t, "AllForReindex", func(t *testing.T, s Store) {
		ctx := context.Background()

		kept, superseded, replacement := newID(t), newID(t), newID(t)
		if err := s.Put(ctx, memory.Memory{Id: replacement, Kind: memory.KindSemantic, Scope: "project:parity", Content: "замена", CreatedAt: 2, UpdatedAt: 2}); err != nil {
			t.Fatalf("Put replacement: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{
			Id: kept, Kind: memory.KindSemantic, Scope: "project:parity", Content: "живая запись",
			CreatedAt: 1, UpdatedAt: 1, Tags: []string{"go"},
			Links: []memory.Link{{Dst: replacement, Rel: memory.RelRelated}},
		}); err != nil {
			t.Fatalf("Put kept: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{Id: superseded, Kind: memory.KindSemantic, Scope: "project:parity", Content: "устаревшая", CreatedAt: 3, UpdatedAt: 3}); err != nil {
			t.Fatalf("Put superseded: %v", err)
		}
		if err := s.Supersede(ctx, superseded, replacement); err != nil {
			t.Fatalf("Supersede: %v", err)
		}

		all, err := s.AllForReindex(ctx)
		if err != nil {
			t.Fatalf("AllForReindex: %v", err)
		}
		got := map[string]memory.Memory{}
		for _, m := range all {
			got[m.Id] = m
		}
		if len(all) != 2 {
			t.Fatalf("AllForReindex = %+v, want 2 записи (kept, replacement)", all)
		}
		if _, ok := got[superseded]; ok {
			t.Errorf("AllForReindex вернул superseded запись: %+v", all)
		}
		keptGot, ok := got[kept]
		if !ok {
			t.Fatalf("AllForReindex не содержит %q: %+v", kept, all)
		}
		if len(keptGot.Tags) != 1 || keptGot.Tags[0] != "go" {
			t.Errorf("AllForReindex не подгрузил Tags: %+v", keptGot)
		}
		if len(keptGot.Links) != 1 || keptGot.Links[0].Dst != replacement {
			t.Errorf("AllForReindex не подгрузил Links: %+v", keptGot)
		}
	})
}

func TestParityNeighbors(t *testing.T) {
	runParity(t, "Neighbors", func(t *testing.T, s Store) {
		ctx := context.Background()

		a, b, c := newID(t), newID(t), newID(t)
		if err := s.Put(ctx, memory.Memory{Id: a, Kind: memory.KindSemantic, Scope: "s", Content: "A", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatalf("Put a: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{
			Id: b, Kind: memory.KindSemantic, Scope: "s", Content: "B", CreatedAt: 2, UpdatedAt: 2,
			Links: []memory.Link{{Dst: a, Rel: memory.RelRelated}},
		}); err != nil {
			t.Fatalf("Put b: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{
			Id: c, Kind: memory.KindSemantic, Scope: "s", Content: "C", CreatedAt: 3, UpdatedAt: 3,
			Links: []memory.Link{{Dst: a, Rel: memory.RelRelated}},
		}); err != nil {
			t.Fatalf("Put c: %v", err)
		}

		neighbors, err := s.Neighbors(ctx, a)
		if err != nil {
			t.Fatalf("Neighbors: %v", err)
		}
		if len(neighbors) != 2 {
			t.Fatalf("neighbors = %+v, want 2 (b и c, оба ссылаются на a)", neighbors)
		}
		ids := map[string]bool{}
		for _, n := range neighbors {
			ids[n.Id] = true
		}
		if !ids[b] || !ids[c] {
			t.Errorf("neighbors ids = %v, want содержать %q и %q", ids, b, c)
		}

		if err := s.Supersede(ctx, b, a); err != nil {
			t.Fatalf("Supersede: %v", err)
		}
		neighbors, err = s.Neighbors(ctx, a)
		if err != nil {
			t.Fatalf("Neighbors после supersede: %v", err)
		}
		if len(neighbors) != 1 || neighbors[0].Id != c {
			t.Fatalf("neighbors после supersede b = %+v, want [c] (b — тумстоун)", neighbors)
		}
	})
}

// TestParityNeighborsLoadsLinks: Neighbors отдаёт соседей с их Links (и Tags) в
// обе стороны ребра — по ним потребитель различает направление и rel (задача
// 15.3: дети эпика = соседи с ребром {Dst: эпик, Rel: part_of}, memory.ChildrenOf).
func TestParityNeighborsLoadsLinks(t *testing.T) {
	runParity(t, "NeighborsLoadsLinks", func(t *testing.T, s Store) {
		ctx := context.Background()

		epic, story, related, far := newID(t), newID(t), newID(t), newID(t)
		if err := s.Put(ctx, memory.Memory{Id: far, Kind: memory.KindSemantic, Scope: "s", Content: "далёкая", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatalf("Put far: %v", err)
		}
		// related — исходящий сосед эпика (эпик -> related); у него своя связь на
		// far, которая должна прийти целиком, хотя far эпику не сосед.
		if err := s.Put(ctx, memory.Memory{
			Id: related, Kind: memory.KindSemantic, Scope: "s", Content: "смежный факт", CreatedAt: 2, UpdatedAt: 2,
			Tags:  []string{"ctx"},
			Links: []memory.Link{{Dst: far, Rel: memory.RelRelated}},
		}); err != nil {
			t.Fatalf("Put related: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{
			Id: epic, Kind: memory.KindTask, Scope: "s", Content: "эпик", Status: memory.StatusTodo, Type: memory.TaskTypeEpic, CreatedAt: 3, UpdatedAt: 3,
			Links: []memory.Link{{Dst: related, Rel: memory.RelRelated}},
		}); err != nil {
			t.Fatalf("Put epic: %v", err)
		}
		// story — входящий сосед (story -> epic, part_of: дочь -> родитель).
		if err := s.Put(ctx, memory.Memory{
			Id: story, Kind: memory.KindTask, Scope: "s", Content: "история", Status: memory.StatusDoing, Type: memory.TaskTypeStory, CreatedAt: 4, UpdatedAt: 4,
			Links: []memory.Link{{Dst: epic, Rel: memory.RelPartOf}},
		}); err != nil {
			t.Fatalf("Put story: %v", err)
		}

		neighbors, err := s.Neighbors(ctx, epic)
		if err != nil {
			t.Fatalf("Neighbors: %v", err)
		}
		if len(neighbors) != 2 {
			t.Fatalf("neighbors = %+v, want 2 (story и related)", neighbors)
		}
		byID := map[string]memory.Memory{}
		for _, n := range neighbors {
			byID[n.Id] = n
		}
		st, ok := byID[story]
		if !ok {
			t.Fatalf("story %q нет среди соседей: %+v", story, neighbors)
		}
		if len(st.Links) != 1 || st.Links[0].Dst != epic || st.Links[0].Rel != memory.RelPartOf {
			t.Errorf("Links входящего соседа = %+v, want [{%s part_of}]", st.Links, epic)
		}
		rl, ok := byID[related]
		if !ok {
			t.Fatalf("related %q нет среди соседей: %+v", related, neighbors)
		}
		if len(rl.Links) != 1 || rl.Links[0].Dst != far || rl.Links[0].Rel != memory.RelRelated {
			t.Errorf("Links исходящего соседа = %+v, want [{%s related}] (все его исходящие, не только к якорю)", rl.Links, far)
		}
		if len(rl.Tags) != 1 || rl.Tags[0] != "ctx" {
			t.Errorf("Tags соседа = %v, want [ctx]", rl.Tags)
		}

		children := memory.ChildrenOf(epic, neighbors)
		if len(children) != 1 || children[0].Id != story {
			t.Errorf("ChildrenOf(epic) = %+v, want [story]", children)
		}
	})
}

func TestParitySupersedeTouch(t *testing.T) {
	runParity(t, "SupersedeTouch", func(t *testing.T, s Store) {
		ctx := context.Background()

		oldID, newIDVal := newID(t), newID(t)
		if err := s.Put(ctx, memory.Memory{Id: oldID, Kind: memory.KindSemantic, Scope: "s", Content: "старое", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatalf("Put old: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{Id: newIDVal, Kind: memory.KindSemantic, Scope: "s", Content: "новое", CreatedAt: 2, UpdatedAt: 2}); err != nil {
			t.Fatalf("Put new: %v", err)
		}

		setNow(t, s, func() int64 { return 12345 })
		if err := s.Supersede(ctx, oldID, newIDVal); err != nil {
			t.Fatalf("Supersede: %v", err)
		}
		got, err := s.Get(ctx, oldID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.SupersededBy == nil || *got.SupersededBy != newIDVal {
			t.Errorf("SupersededBy = %v, want %q", got.SupersededBy, newIDVal)
		}
		if got.UpdatedAt != 12345 {
			t.Errorf("UpdatedAt = %d, want 12345", got.UpdatedAt)
		}
		if err := s.Supersede(ctx, newID(t), newIDVal); err == nil {
			t.Error("Supersede несуществующего oldID должен вернуть ошибку")
		}

		eventID := newID(t)
		if err := s.Put(ctx, memory.Memory{Id: eventID, Kind: memory.KindEpisodic, Scope: "s", Content: "событие", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatalf("Put event: %v", err)
		}
		setNow(t, s, func() int64 { return 42 })
		if err := s.Touch(ctx, eventID); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		gotEvent, err := s.Get(ctx, eventID)
		if err != nil {
			t.Fatalf("Get event: %v", err)
		}
		if gotEvent.AccessedAt == nil || *gotEvent.AccessedAt != 42 || gotEvent.AccessCount != 1 {
			t.Errorf("после Touch: AccessedAt=%v AccessCount=%d, want 42/1", gotEvent.AccessedAt, gotEvent.AccessCount)
		}
		setNow(t, s, func() int64 { return 43 })
		if err := s.Touch(ctx, eventID); err != nil {
			t.Fatalf("Touch 2: %v", err)
		}
		gotEvent, err = s.Get(ctx, eventID)
		if err != nil {
			t.Fatalf("Get event 2: %v", err)
		}
		if gotEvent.AccessedAt == nil || *gotEvent.AccessedAt != 43 || gotEvent.AccessCount != 2 {
			t.Errorf("после Touch 2: AccessedAt=%v AccessCount=%d, want 43/2", gotEvent.AccessedAt, gotEvent.AccessCount)
		}
		if err := s.Touch(ctx, newID(t)); err == nil {
			t.Error("Touch несуществующей записи должен вернуть ошибку")
		}
	})
}

func TestParityVectorDegradation(t *testing.T) {
	runParity(t, "VectorDegradation", parityVectorDegradation)
}

// parityVectorDegradation проверяет деградацию векторного слоя ДО первого
// ResetVectorIndex (DESIGN.md §15: Embedder=noop, индекс ни разу не создавался) —
// пустой результат без ошибки, а не панику/ошибку "таблица не существует".
func parityVectorDegradation(t *testing.T, s Store) {
	ctx := context.Background()

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: "project:parity",
		Content: "c", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	cands, err := s.SearchVector(ctx, []float32{1, 0, 0}, memory.Query{Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector без vec-таблицы: %v", err)
	}
	if cands != nil {
		t.Errorf("SearchVector = %+v, want nil", cands)
	}

	neighbors, err := s.SemanticNeighbors(ctx, id, 3)
	if err != nil {
		t.Fatalf("SemanticNeighbors без vec-таблицы: %v", err)
	}
	if neighbors != nil {
		t.Errorf("SemanticNeighbors = %+v, want nil", neighbors)
	}

	status, err := s.VectorStatus(ctx, id)
	if err != nil {
		t.Fatalf("VectorStatus без vec-таблицы: %v", err)
	}
	if status != (VectorStatus{}) {
		t.Errorf("VectorStatus = %+v, want нулевое значение", status)
	}

	missing, err := s.ListMissingVectors(ctx, 0)
	if err != nil {
		t.Fatalf("ListMissingVectors без vec-таблицы: %v", err)
	}
	if missing != nil {
		t.Errorf("ListMissingVectors = %+v, want nil", missing)
	}

	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta: %v", err)
	}
	if model != "" || dim != 0 {
		t.Errorf("EmbedMeta = (%q, %d), want (\"\", 0)", model, dim)
	}

	if err := s.PutVector(ctx, id, []float32{1, 0, 0}); err == nil {
		t.Error("PutVector без ResetVectorIndex должен вернуть ошибку")
	}
}

func TestParityVectorLayer(t *testing.T) {
	runParityWithVector(t, "VectorLayer", parityVectorLayer)
}

// parityVectorLayer проверяет ResetVectorIndex/PutVector/SearchVector с ТОЧНЫМИ
// числовыми ожиданиями по Score (не только по порядку) — обе реализации используют
// одну и ту же метрику (cosine) с формулой Score=1/(1+distance), поэтому значения
// обязаны совпадать (с точностью до float32) независимо от backend. near/far —
// специально подобранные ортогональные/сонаправленные векторы с "круглой"
// дистанцией (0, 1-cos45°, 1), чтобы ловить малейшее формульное расхождение между
// драйверами, а не просто "score убывает".
func parityVectorLayer(t *testing.T, s Store) {
	ctx := context.Background()

	if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	near, mid, far := newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id  string
		vec []float32
		ts  int64
	}{
		{near, []float32{1, 0, 0}, 1},
		{mid, []float32{1, 1, 0}, 2},
		{far, []float32{0, 1, 0}, 3},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "запись " + tc.id, CreatedAt: tc.ts, UpdatedAt: tc.ts,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
		if err := s.PutVector(ctx, tc.id, tc.vec); err != nil {
			t.Fatalf("PutVector %s: %v", tc.id, err)
		}
	}

	cands, err := s.SearchVector(ctx, []float32{1, 0, 0}, memory.Query{Scope: "project:parity", Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if len(cands) != 3 {
		t.Fatalf("candidates = %+v, want 3", cands)
	}

	const epsilon = 1e-5
	wantScore := map[string]float64{
		near: 1.0,                           // distance 0 (совпадает с запросом)
		mid:  1 / (1 + 0.29289321881345254), // distance 1-cos(45°)
		far:  0.5,                           // distance 1 (ортогональные вектора)
	}
	wantOrder := []string{near, mid, far}
	for i, id := range wantOrder {
		if cands[i].Memory.Id != id {
			t.Fatalf("cands[%d].Id = %q, want %q (порядок: %+v)", i, cands[i].Memory.Id, id, cands)
		}
		if cands[i].Source != "vector" {
			t.Errorf("cands[%d].Source = %q, want vector", i, cands[i].Source)
		}
		diff := cands[i].Score - wantScore[id]
		if diff > epsilon || diff < -epsilon {
			t.Errorf("cands[%d] (%s).Score = %.9f, want %.9f (±%v)", i, id, cands[i].Score, wantScore[id], epsilon)
		}
	}

	// Пустой вектор запроса — деградация до пустого результата без ошибки.
	for _, vec := range [][]float32{nil, {}} {
		empty, err := s.SearchVector(ctx, vec, memory.Query{Limit: 10})
		if err != nil {
			t.Fatalf("SearchVector с пустым вектором (%v): %v", vec, err)
		}
		if empty != nil {
			t.Errorf("SearchVector с пустым вектором = %+v, want nil", empty)
		}
	}

	// Пересоздание с другой размерностью/моделью стирает старые векторы.
	if err := s.ResetVectorIndex(ctx, "other-model", 4); err != nil {
		t.Fatalf("ResetVectorIndex (2): %v", err)
	}
	model, dim, err := s.EmbedMeta(ctx)
	if err != nil {
		t.Fatalf("EmbedMeta: %v", err)
	}
	if model != "other-model" || dim != 4 {
		t.Fatalf("EmbedMeta = (%q, %d), want (other-model, 4)", model, dim)
	}
	cands, err = s.SearchVector(ctx, []float32{1, 0, 0, 0}, memory.Query{Limit: 10})
	if err != nil {
		t.Fatalf("SearchVector после пересоздания: %v", err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates = %+v, want пусто (старые векторы должны были исчезнуть)", cands)
	}
}

// TestResetVectorIndexDoesNotDropForeignSchemaVecTable — регрессия на реальный
// инцидент на postgres.lan, найденный при разработке этого файла (см. развёрнутый
// комментарий у ResetVectorIndex в postgres_vector.go): голый `DROP TABLE IF
// EXISTS memories_vec` резолвится сканированием ВСЕЙ search_path (как обычное
// чтение), а не по current_schema() (как CREATE TABLE) — поэтому если в целевой
// (текущей) схеме своей memories_vec ещё нет (обычное состояние до первого
// ResetVectorIndex), а где-то ещё по search_path есть одноимённая таблица другого
// проекта/прогона — DROP находит и удаляет ЕЁ, а CREATE следом создаёт новую в
// целевой схеме, маскируя потерю (свой store выглядит рабочим, чужой уже испорчен).
//
// Тест воспроизводит эту топологию двумя СВОИМИ одноразовыми схемами (никаких
// операций над public — задача C2 явно запрещает TRUNCATE/DROP по её объектам):
// targetSchema — та, что видит pgStore (current_schema()), foreignSchema —
// имитация "чужого" объекта дальше по search_path. На коде ДО фикса (голые
// DROP/CREATE/to_regclass без явной схемы) этот тест падает: foreignSchema.memories_vec
// и её маркерная строка исчезают после ResetVectorIndex(targetSchema). На коде
// ПОСЛЕ фикса (s.vecTable() — схема-квалифицированное имя) foreignSchema остаётся
// нетронутой независимо от её положения в search_path.
func TestResetVectorIndexDoesNotDropForeignSchemaVecTable(t *testing.T) {
	baseDSN := os.Getenv(pgTestDSNEnv)
	if baseDSN == "" {
		t.Skipf("%s не задан — пропуск postgres-тестов store. Пример:\n  %s=postgres://memo:memo@postgres.lan:5432/memo?sslmode=disable make test", pgTestDSNEnv, pgTestDSNEnv)
	}

	ctx := context.Background()
	suffix := strings.ToLower(ulid.Make().String())
	targetSchema := "memo_test_target_" + suffix
	foreignSchema := "memo_test_foreign_" + suffix

	bootstrap, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatalf("открытие bootstrap-соединения (%s): %v", pgTestDSNEnv, err)
	}
	defer bootstrap.Close()

	for _, schema := range []string{targetSchema, foreignSchema} {
		if _, err := bootstrap.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
			t.Fatalf("создание тестовой схемы %s: %v", schema, err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		cleanup, err := sql.Open("pgx", baseDSN)
		if err != nil {
			t.Errorf("открытие соединения для уборки схем %s/%s: %v", targetSchema, foreignSchema, err)
			return
		}
		defer cleanup.Close()
		for _, schema := range []string{targetSchema, foreignSchema} {
			if _, err := cleanup.ExecContext(cleanupCtx, `DROP SCHEMA `+schema+` CASCADE`); err != nil {
				t.Errorf("удаление тестовой схемы %s: %v", schema, err)
			}
		}
	})

	// "Чужая" memories_vec — имитация ранее существовавшей таблицы в другой схеме
	// той же search_path (у настоящего инцидента это была public.memories_vec).
	// Форма не обязана совпадать с "настоящей" memories_vec — важно только, что
	// DROP TABLE IF EXISTS найдёт её по имени, если резолвинг не квалифицирован
	// схемой явно.
	if _, err := bootstrap.ExecContext(ctx, `CREATE TABLE `+foreignSchema+`.memories_vec (memory_id text PRIMARY KEY)`); err != nil {
		t.Fatalf("создание чужой memories_vec в %s: %v", foreignSchema, err)
	}
	if _, err := bootstrap.ExecContext(ctx, `INSERT INTO `+foreignSchema+`.memories_vec (memory_id) VALUES ('foreign-marker')`); err != nil {
		t.Fatalf("вставка маркерной строки в %s.memories_vec: %v", foreignSchema, err)
	}

	// search_path: targetSchema первой (current_schema(), то, что видит
	// pgStore.init), foreignSchema следом. У targetSchema своей memories_vec ЕЩЁ
	// НЕТ (типичное состояние перед первым ResetVectorIndex) — ровно топология
	// инцидента: голый DROP по search_path пропускает targetSchema (там нечего
	// удалять) и добирается до foreignSchema.
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("разбор %s: %v", pgTestDSNEnv, err)
	}
	q := u.Query()
	q.Set("search_path", targetSchema+","+foreignSchema+",public")
	u.RawQuery = q.Encode()

	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatalf("открытие тестового соединения: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s := &pgStore{db: db, now: defaultNow}
	if err := s.init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}

	if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	// Ключевая проверка: чужая memories_vec (и её маркерная строка) должна
	// остаться нетронутой ResetVectorIndex, вызванным над targetSchema.
	var marker string
	err = bootstrap.QueryRowContext(ctx,
		`SELECT memory_id FROM `+foreignSchema+`.memories_vec WHERE memory_id = 'foreign-marker'`,
	).Scan(&marker)
	if err != nil {
		t.Fatalf(
			"чужая %s.memories_vec повреждена ResetVectorIndex целевой схемы %s (%v) — "+
				"значит DROP снова резолвится по всей search_path, а не по current_schema()",
			foreignSchema, targetSchema, err,
		)
	}

	// И своя memories_vec должна быть на месте, в targetSchema.
	exists, err := s.hasVecTable(ctx)
	if err != nil {
		t.Fatalf("hasVecTable: %v", err)
	}
	if !exists {
		t.Error("hasVecTable = false, want true после ResetVectorIndex")
	}
}

func TestParityVectorTagsAndScopesFilter(t *testing.T) {
	runParityWithVector(t, "VectorTagsAndScopesFilter", func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := s.ResetVectorIndex(ctx, "test-model", 2); err != nil {
			t.Fatalf("ResetVectorIndex: %v", err)
		}

		mine, other, tagged, untagged := newID(t), newID(t), newID(t), newID(t)
		for _, tc := range []struct {
			id, scope string
			tags      []string
		}{
			{mine, "project:parity", nil},
			{other, "project:other", nil},
			{tagged, "project:parity", []string{"стек"}},
			{untagged, "project:parity", nil},
		} {
			if err := s.Put(ctx, memory.Memory{
				Id: tc.id, Kind: memory.KindSemantic, Scope: tc.scope, Content: "c",
				CreatedAt: 1, UpdatedAt: 1, Tags: tc.tags,
			}); err != nil {
				t.Fatalf("Put %s: %v", tc.id, err)
			}
			if err := s.PutVector(ctx, tc.id, []float32{1, 0}); err != nil {
				t.Fatalf("PutVector %s: %v", tc.id, err)
			}
		}

		t.Run("фильтр по scope", func(t *testing.T) {
			cands, err := s.SearchVector(ctx, []float32{1, 0}, memory.Query{Scope: "project:parity", Limit: 10})
			if err != nil {
				t.Fatalf("SearchVector: %v", err)
			}
			got := map[string]bool{}
			for _, c := range cands {
				got[c.Memory.Id] = true
			}
			if got[other] {
				t.Errorf("candidates содержат запись чужого scope: %+v", cands)
			}
			if !got[mine] || !got[tagged] || !got[untagged] {
				t.Errorf("candidates = %+v, want содержать mine/tagged/untagged", cands)
			}
		})

		t.Run("фильтр по тегу", func(t *testing.T) {
			cands, err := s.SearchVector(ctx, []float32{1, 0}, memory.Query{Tags: []string{"стек"}, Limit: 10})
			if err != nil {
				t.Fatalf("SearchVector: %v", err)
			}
			if len(cands) != 1 || cands[0].Memory.Id != tagged {
				t.Fatalf("candidates = %+v, want [tagged]", cands)
			}
		})
	})
}

func TestParitySemanticNeighbors(t *testing.T) {
	runParityWithVector(t, "SemanticNeighbors", paritySemanticNeighbors)
}

func paritySemanticNeighbors(t *testing.T, s Store) {
	ctx := context.Background()
	if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
		t.Fatalf("ResetVectorIndex: %v", err)
	}

	anchor, near, far, supersededID, replacement, noVector := newID(t), newID(t), newID(t), newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id     string
		vec    []float32
		hasVec bool
		ts     int64
	}{
		{anchor, []float32{1, 0, 0}, true, 1},
		{near, []float32{1, 0.1, 0}, true, 2},
		{far, []float32{0, 1, 0}, true, 3},
		{supersededID, []float32{1, 0.05, 0}, true, 4}, // ближе near, но станет superseded
		{replacement, nil, false, 5},
		{noVector, nil, false, 6},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "запись " + tc.id, CreatedAt: tc.ts, UpdatedAt: tc.ts,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
		if tc.hasVec {
			if err := s.PutVector(ctx, tc.id, tc.vec); err != nil {
				t.Fatalf("PutVector %s: %v", tc.id, err)
			}
		}
	}
	if err := s.Supersede(ctx, supersededID, replacement); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	t.Run("порядок по возрастанию дистанции, self/superseded исключены", func(t *testing.T) {
		cands, err := s.SemanticNeighbors(ctx, anchor, 3)
		if err != nil {
			t.Fatalf("SemanticNeighbors: %v", err)
		}
		if len(cands) != 2 {
			t.Fatalf("candidates = %+v, want 2 (near, far)", cands)
		}
		wantOrder := []string{near, far}
		for i, id := range wantOrder {
			if cands[i].Memory.Id != id {
				t.Errorf("cands[%d].Id = %q, want %q (порядок: %+v)", i, cands[i].Memory.Id, id, cands)
			}
			if cands[i].Source != "vector" {
				t.Errorf("cands[%d].Source = %q, want vector", i, cands[i].Source)
			}
		}
		for i := 1; i < len(cands); i++ {
			if cands[i].Score < cands[i-1].Score {
				t.Errorf("Score (честная дистанция) не возрастает: %+v", cands)
			}
		}
		for _, c := range cands {
			if c.Memory.Id == anchor || c.Memory.Id == supersededID {
				t.Errorf("candidates содержат anchor или superseded: %+v", cands)
			}
		}
	})

	t.Run("якорь без вектора — пусто без ошибки", func(t *testing.T) {
		cands, err := s.SemanticNeighbors(ctx, noVector, 3)
		if err != nil {
			t.Fatalf("SemanticNeighbors: %v", err)
		}
		if cands != nil {
			t.Errorf("candidates = %+v, want nil", cands)
		}
	})

	t.Run("k<=0 — пусто без ошибки", func(t *testing.T) {
		cands, err := s.SemanticNeighbors(ctx, anchor, 0)
		if err != nil {
			t.Fatalf("SemanticNeighbors: %v", err)
		}
		if cands != nil {
			t.Errorf("candidates = %+v, want nil", cands)
		}
	})
}

func TestParityListMissingVectors(t *testing.T) {
	runParityWithVector(t, "ListMissingVectors", func(t *testing.T, s Store) {
		ctx := context.Background()
		if err := s.ResetVectorIndex(ctx, "test-model", 3); err != nil {
			t.Fatalf("ResetVectorIndex: %v", err)
		}

		withVec, withoutVec, superseded := newID(t), newID(t), newID(t)
		if err := s.Put(ctx, memory.Memory{Id: withVec, Kind: memory.KindSemantic, Scope: "s", Content: "с вектором", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatalf("Put withVec: %v", err)
		}
		if err := s.PutVector(ctx, withVec, []float32{1, 0, 0}); err != nil {
			t.Fatalf("PutVector: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{Id: withoutVec, Kind: memory.KindSemantic, Scope: "s", Content: "без вектора", CreatedAt: 2, UpdatedAt: 2}); err != nil {
			t.Fatalf("Put withoutVec: %v", err)
		}
		if err := s.Put(ctx, memory.Memory{Id: superseded, Kind: memory.KindSemantic, Scope: "s", Content: "устаревшая без вектора", CreatedAt: 3, UpdatedAt: 3}); err != nil {
			t.Fatalf("Put superseded: %v", err)
		}
		if err := s.Supersede(ctx, superseded, withVec); err != nil {
			t.Fatalf("Supersede: %v", err)
		}

		missing, err := s.ListMissingVectors(ctx, 0)
		if err != nil {
			t.Fatalf("ListMissingVectors: %v", err)
		}
		if len(missing) != 1 || missing[0].Id != withoutVec {
			t.Fatalf("ListMissingVectors = %+v, want ровно [%q]", missing, withoutVec)
		}
	})
}

func TestParityMaintenanceLists(t *testing.T) {
	runParity(t, "MaintenanceLists", parityMaintenanceLists)
}

// parityMaintenanceLists покрывает ListEpisodicForConsolidation/ListEpisodicForDecay/
// ListExpired — выборки для фоновых джобов (DESIGN.md §11), включая семантику
// COALESCE(accessed_at, created_at) в decay и строгую границу "< now" в ListExpired.
func parityMaintenanceLists(t *testing.T, s Store) {
	ctx := context.Background()
	const threshold = int64(1000)

	oldA, oldB, fresh, semantic := newID(t), newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: oldA, Kind: memory.KindEpisodic, Scope: "a", Content: "старое A", CreatedAt: 100, UpdatedAt: 100}); err != nil {
		t.Fatalf("Put oldA: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: oldB, Kind: memory.KindEpisodic, Scope: "b", Content: "старое B", CreatedAt: 150, UpdatedAt: 150}); err != nil {
		t.Fatalf("Put oldB: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: fresh, Kind: memory.KindEpisodic, Scope: "a", Content: "свежее", CreatedAt: threshold, UpdatedAt: threshold}); err != nil {
		t.Fatalf("Put fresh: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{Id: semantic, Kind: memory.KindSemantic, Scope: "a", Content: "старый факт", CreatedAt: 50, UpdatedAt: 50}); err != nil {
		t.Fatalf("Put semantic: %v", err)
	}

	t.Run("ListEpisodicForConsolidation: только старые episodic, сортировка scope→created_at", func(t *testing.T) {
		got, err := s.ListEpisodicForConsolidation(ctx, threshold, 0)
		if err != nil {
			t.Fatalf("ListEpisodicForConsolidation: %v", err)
		}
		wantIDs := []string{oldA, oldB}
		if len(got) != len(wantIDs) {
			t.Fatalf("len = %d, want %d (got: %+v)", len(got), len(wantIDs), got)
		}
		for i, id := range wantIDs {
			if got[i].Id != id {
				t.Errorf("got[%d].Id = %q, want %q", i, got[i].Id, id)
			}
		}
		if got[0].Tags != nil {
			t.Errorf("got[0].Tags = %v, want nil (Tags не загружаются этим методом)", got[0].Tags)
		}
	})

	staleNoAccess, staleAccessed, recentlyTouched := newID(t), newID(t), newID(t)
	if err := s.Put(ctx, memory.Memory{Id: staleNoAccess, Kind: memory.KindEpisodic, Scope: "a", Content: "давно не трогали", CreatedAt: 100, UpdatedAt: 100}); err != nil {
		t.Fatalf("Put staleNoAccess: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: staleAccessed, Kind: memory.KindEpisodic, Scope: "a", Content: "давно доступалось",
		CreatedAt: 300, UpdatedAt: 300, AccessedAt: int64Ptr(200),
	}); err != nil {
		t.Fatalf("Put staleAccessed: %v", err)
	}
	if err := s.Put(ctx, memory.Memory{
		Id: recentlyTouched, Kind: memory.KindEpisodic, Scope: "a", Content: "недавно трогали",
		CreatedAt: 50, UpdatedAt: 50, AccessedAt: int64Ptr(threshold),
	}); err != nil {
		t.Fatalf("Put recentlyTouched: %v", err)
	}

	t.Run("ListEpisodicForDecay: COALESCE(accessed_at, created_at), сортировка по возрастанию", func(t *testing.T) {
		got, err := s.ListEpisodicForDecay(ctx, threshold, 0)
		if err != nil {
			t.Fatalf("ListEpisodicForDecay: %v", err)
		}
		got2 := map[string]bool{}
		for _, m := range got {
			got2[m.Id] = true
		}
		if !got2[staleNoAccess] || !got2[staleAccessed] {
			t.Fatalf("got = %+v, want содержать staleNoAccess и staleAccessed", got)
		}
		if got2[recentlyTouched] {
			t.Errorf("got содержит recentlyTouched (accessed_at недавний): %+v", got)
		}
		// staleNoAccess (дата сравнения = created_at = 100) должен идти раньше
		// staleAccessed (дата сравнения = accessed_at = 200), несмотря на то что
		// created_at у staleAccessed (300) больше.
		idxNoAccess, idxAccessed := -1, -1
		for i, m := range got {
			if m.Id == staleNoAccess {
				idxNoAccess = i
			}
			if m.Id == staleAccessed {
				idxAccessed = i
			}
		}
		if idxNoAccess > idxAccessed {
			t.Errorf("порядок ListEpisodicForDecay = %+v, want staleNoAccess раньше staleAccessed", got)
		}
	})

	eternal, future, olderExpired, newerExpired := newID(t), newID(t), newID(t), newID(t)
	for _, tc := range []struct {
		id        string
		expiresAt int64
	}{
		{eternal, 0},
		{future, 2000},
		{olderExpired, 500},
		{newerExpired, 900},
	} {
		if err := s.Put(ctx, memory.Memory{
			Id: tc.id, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "c", CreatedAt: 1, UpdatedAt: 1, ExpiresAt: tc.expiresAt,
		}); err != nil {
			t.Fatalf("Put %s: %v", tc.id, err)
		}
	}

	t.Run("ListExpired: строго 0<expires_at<now, сортировка по возрастанию", func(t *testing.T) {
		expired, err := s.ListExpired(ctx, 1000, 0)
		if err != nil {
			t.Fatalf("ListExpired: %v", err)
		}
		if len(expired) != 2 || expired[0].Id != olderExpired || expired[1].Id != newerExpired {
			t.Fatalf("ListExpired = %+v, want [olderExpired newerExpired]", expired)
		}

		boundary := newID(t)
		if err := s.Put(ctx, memory.Memory{
			Id: boundary, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "c", CreatedAt: 1, UpdatedAt: 1, ExpiresAt: 1000,
		}); err != nil {
			t.Fatalf("Put boundary: %v", err)
		}
		expired2, err := s.ListExpired(ctx, 1000, 0)
		if err != nil {
			t.Fatalf("ListExpired (после boundary): %v", err)
		}
		for _, m := range expired2 {
			if m.Id == boundary {
				t.Errorf("ListExpired вернул boundary (expires_at == now), want строго < now")
			}
		}
	})
}

func TestPgHealth(t *testing.T) {
	s := newTestPgStore(t)
	info, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	// QuickCheck/JournalMode — намеренно НЕ сравниваются со sqlite (DESIGN.md §4,
	// задача C2: разрешённое расхождение), но контракт "QuickCheck==ok при здоровом
	// соединении" общий для обоих backend (см. postgresDatabaseSection).
	if info.QuickCheck != "ok" {
		t.Errorf("QuickCheck = %q, want ok", info.QuickCheck)
	}
	if info.JournalMode == "" {
		t.Error("JournalMode пуст, want содержательное описание сервера (версия/роль)")
	}
}

// TestPgCheckPostgresReadiness — задача 12.1 (доработки по итогам ревью):
// на готовой тестовой БД оба признака готовности должны быть true, без
// открытия второго соединения — только read-only SELECT поверх уже открытого
// s.db. Обязательно newTestPgStoreWithVector (а не newTestPgStore): проверка
// VectorExtension теперь резолвит имя типа vector через to_regtype по
// search_path СОЕДИНЕНИЯ (см. доккомментарий у PostgresReadiness.VectorExtension
// и CheckPostgresReadiness — фикс пункта C ревью: pg_extension врал "готово"
// для схемы без public в search_path, хотя CREATE TABLE ... vector(N) там же
// падает); newTestPgStore, в отличие от newTestPgStoreWithVector, держит
// search_path БЕЗ ",public" именно для того, чтобы unqualified-запросы не
// задевали public — тип vector (установленный в public, см. её комментарий)
// там принципиально не резолвится, и это ПРАВИЛЬНОЕ поведение проверки, а не
// повод её обходить. CanCreate — тестовая схема только что создана самим
// тестом, поэтому его роль ей владеет и имеет CREATE.
//
// Отсутствующие pgvector/право CREATE здесь намеренно НЕ проверяются:
// воспроизвести их пришлось бы либо снятием привилегий/удалением расширения
// на ОБЩЕЙ командной базе (нарушает "общая база не чистится" — задача прямо
// это запрещает), либо через отдельную роль/БД, которых MEMO_TEST_POSTGRES_DSN
// не гарантирует; форматирование предупреждений по каждому false-случаю
// проверяется юнит-тестами cmd/memo (storeReadinessWarnings,
// init_readiness_test.go) на фейковом PostgresReadinessChecker — без сети
// вовсе.
func TestPgCheckPostgresReadiness(t *testing.T) {
	s := newTestPgStoreWithVector(t)
	readiness, err := s.CheckPostgresReadiness(context.Background())
	if err != nil {
		t.Fatalf("CheckPostgresReadiness: %v", err)
	}
	if !readiness.VectorExtension {
		t.Error("VectorExtension = false, want true — pgvector установлен в public, а search_path (newTestPgStoreWithVector) его видит")
	}
	if !readiness.CanCreate {
		t.Error("CanCreate = false, want true — тестовая роль владеет только что созданной ею схемой")
	}
}

// TestPgCheckPostgresReadiness_VectorNotInSearchPath — прямая регрессия
// пункта C ревью задачи 12.1: pgvector установлен на сервере (в public), но
// search_path этого соединения его не видит (newTestPgStore, в отличие от
// newTestPgStoreWithVector, держит search_path из ОДНОЙ тестовой схемы, без
// ",public" — см. её комментарий). Старая проверка (pg_extension, каталог
// уровня БД) сказала бы здесь "готово", хотя ResetVectorIndex/CREATE TABLE ...
// vector(N) в этой же схеме упали бы с "type vector does not exist" —
// to_regtype обязана честно вернуть VectorExtension=false в этом случае.
func TestPgCheckPostgresReadiness_VectorNotInSearchPath(t *testing.T) {
	s := newTestPgStore(t)
	readiness, err := s.CheckPostgresReadiness(context.Background())
	if err != nil {
		t.Fatalf("CheckPostgresReadiness: %v", err)
	}
	if readiness.VectorExtension {
		t.Error("VectorExtension = true, want false — search_path этого соединения не включает public, тип vector там не резолвится, хотя pg_extension сказал бы обратное")
	}
	if !readiness.CanCreate {
		t.Error("CanCreate = false, want true — тестовая роль владеет только что созданной ею схемой")
	}
}

func TestPgClose(t *testing.T) {
	s := newTestPgStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// После Close пул соединений закрыт — операция должна вернуть ошибку, а не
	// повиснуть/запаниковать.
	if _, err := s.Get(context.Background(), newID(t)); err == nil {
		t.Error("Get после Close должен вернуть ошибку")
	}
}

// TestSearchLexicalStemmingRussianPostgres — postgres-аналог stemming_test.go
// (задача 8.3): здесь стемминг делает сам сервер (to_tsvector('russian', ...),
// DESIGN.md §4 — разрешённое расхождение в МЕХАНИЗМЕ, но не в наблюдаемом
// поведении), поэтому сценарии не переиспользованы через runParity, а продублированы
// напрямую под postgres — сравнивать здесь нечего, важно лишь что snowball-стеммер
// сервера даёт те же практические результаты, что и Go-стеммер sqlite.
func TestSearchLexicalStemmingRussianPostgres(t *testing.T) {
	s := newTestPgStore(t)
	ctx := context.Background()

	t.Run("перефразировка другой словоформой находится", func(t *testing.T) {
		id := newID(t)
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindSemantic, Scope: "project:parity",
			Content: "Проект использует хранилище PostgreSQL, тесты гоняются через make test", Importance: 4,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		for _, q := range []string{"проектом", "хранилища", "тестами"} {
			t.Run(q, func(t *testing.T) {
				cands, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:parity", Limit: 10})
				if err != nil {
					t.Fatalf("SearchLexical(%q): %v", q, err)
				}
				found := false
				for _, c := range cands {
					if c.Memory.Id == id {
						found = true
					}
				}
				if !found {
					t.Errorf("SearchLexical(%q) не нашёл запись по стеммированной форме (кандидаты: %+v)", q, cands)
				}
			})
		}
	})

	t.Run("точные англ. идентификаторы/фразы находятся как раньше", func(t *testing.T) {
		id := newID(t)
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindProcedural, Scope: "project:parity",
			Content: "CGO_ENABLED=1 go build -tags sqlite_fts5 ./...; make build; SearchLexical живёт в store", Importance: 3,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		for _, q := range []string{"sqlite_fts5", "SearchLexical"} {
			t.Run(q, func(t *testing.T) {
				cands, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:parity", Limit: 10})
				if err != nil {
					t.Fatalf("SearchLexical(%q): %v", q, err)
				}
				found := false
				for _, c := range cands {
					if c.Memory.Id == id {
						found = true
					}
				}
				if !found {
					t.Errorf("SearchLexical(%q) не нашёл запись (кандидаты: %+v)", q, cands)
				}
			})
		}
	})

	t.Run("словообразовательная граница: собирать/компилировать не мостятся к «Сборка»", func(t *testing.T) {
		id := newID(t)
		if err := s.Put(ctx, memory.Memory{
			Id: id, Kind: memory.KindProcedural, Scope: "project:parity",
			Content: "Сборка требует CGO и build-тег для FTS5", Importance: 3,
			CreatedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
		for _, q := range []string{"собирать", "компилировать"} {
			t.Run(q, func(t *testing.T) {
				cands, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:parity", Limit: 10})
				if err != nil {
					t.Fatalf("SearchLexical(%q): %v", q, err)
				}
				for _, c := range cands {
					if c.Memory.Id == id {
						t.Errorf("SearchLexical(%q) нашёл запись про «Сборка» — snowball-стеммер Postgres тоже не должен мостить словообразовательную пару", q)
					}
				}
			})
		}
	})
}

// TestSearchLexicalTSQuerySpecialCharsPostgres — безопасность sanitizeTSQuery
// (postgres_query.go) против пользовательского текста со спецсимволами языка
// операторов tsquery (& | ! :* скобки кавычки, задача C2). splitWords токенизирует
// текст ДО передачи в plainto_tsquery (границы слов — буквы/цифры Unicode), поэтому
// такие символы становятся просто разделителями слов, а не операторами — запрос не
// падает и не меняет логику (OR между словами запроса, как обычно).
func TestSearchLexicalTSQuerySpecialCharsPostgres(t *testing.T) {
	s := newTestPgStore(t)
	ctx := context.Background()

	id := newID(t)
	if err := s.Put(ctx, memory.Memory{
		Id: id, Kind: memory.KindSemantic, Scope: "project:parity",
		Content: "запись содержит только слово молоток", Importance: 3,
		CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	weird := []string{
		"молоток & дрель",
		"молоток | дрель",
		"молоток :* дрель",
		"!молоток",
		"(молоток)",
		`"молоток"`,
		"молоток' OR '1'='1",
	}
	for _, q := range weird {
		t.Run(q, func(t *testing.T) {
			cands, err := s.SearchLexical(ctx, memory.Query{Text: q, Scope: "project:parity", Limit: 10})
			if err != nil {
				t.Fatalf("SearchLexical(%q) вернул ошибку: %v", q, err)
			}
			found := false
			for _, c := range cands {
				if c.Memory.Id == id {
					found = true
				}
			}
			if !found {
				t.Errorf("SearchLexical(%q) не нашёл запись со словом «молоток» — операторные символы должны трактоваться как разделители слов (OR-семантика между словами запроса)", q)
			}
		})
	}

	t.Run("отрицательный контроль: операторные символы не подменяют логику матчинга", func(t *testing.T) {
		cands, err := s.SearchLexical(ctx, memory.Query{Text: "дрель & перфоратор", Scope: "project:parity", Limit: 10})
		if err != nil {
			t.Fatalf("SearchLexical: %v", err)
		}
		for _, c := range cands {
			if c.Memory.Id == id {
				t.Errorf("SearchLexical(дрель & перфоратор) неожиданно нашёл запись без этих слов: %+v", cands)
			}
		}
	})
}

// TestParityGetMany — паритет GetMany (задача 15.4), сценарий — parityGetMany
// (getmany_test.go).
func TestParityGetMany(t *testing.T) {
	runParity(t, "GetMany", parityGetMany)
}

// TestParityListTasks — паритет ListTasks (задача 15.5), сценарий —
// parityListTasks (listtasks_test.go).
func TestParityListTasks(t *testing.T) {
	runParity(t, "ListTasks", parityListTasks)
}
