// Тесты диагностики готовности PostgreSQL в пробе `memo init` (задача 12.1,
// DESIGN.md §15): предупреждения о недостающем pgvector/праве CREATE не валят
// пробу, печатаются ровно один раз (doInit), а форматирование текста
// (storeReadinessWarnings) проверяется без сети — на фейковом
// store.PostgresReadinessChecker, по образцу стабов probeStoreConnection в
// init_store_test.go.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agentmemory/internal/memory/store"
)

// fakeReadinessChecker — минимальная реализация store.PostgresReadinessChecker
// для юнит-теста storeReadinessWarnings: не требует поднимать все ~20 методов
// store.Store (см. комментарий у storeReadinessWarnings, зачем она принимает
// именно этот узкий интерфейс).
type fakeReadinessChecker struct {
	readiness store.PostgresReadiness
	err       error
}

func (f fakeReadinessChecker) CheckPostgresReadiness(context.Context) (store.PostgresReadiness, error) {
	return f.readiness, f.err
}

func TestStoreReadinessWarnings(t *testing.T) {
	cases := []struct {
		name        string
		checker     fakeReadinessChecker
		wantCount   int
		wantSubstrs []string
	}{
		{
			name:      "готовая база — без предупреждений",
			checker:   fakeReadinessChecker{readiness: store.PostgresReadiness{VectorExtension: true, CanCreate: true}},
			wantCount: 0,
		},
		{
			name:        "нет pgvector",
			checker:     fakeReadinessChecker{readiness: store.PostgresReadiness{VectorExtension: false, CanCreate: true}},
			wantCount:   1,
			wantSubstrs: []string{"pgvector"},
		},
		{
			// "CREATE" НЕ годится маркером здесь (по итогам ревью): текст
			// предупреждения про pgvector сам упоминает "CREATE EXTENSION" —
			// подстрока "CREATE" не отличила бы это предупреждение от
			// pgvector-warning, если бы условия в storeReadinessWarnings
			// перепутались местами. "GRANT" встречается только в тексте
			// про CanCreate.
			name:        "нет права CREATE",
			checker:     fakeReadinessChecker{readiness: store.PostgresReadiness{VectorExtension: true, CanCreate: false}},
			wantCount:   1,
			wantSubstrs: []string{"GRANT"},
		},
		{
			name:        "ни pgvector, ни права CREATE",
			checker:     fakeReadinessChecker{readiness: store.PostgresReadiness{VectorExtension: false, CanCreate: false}},
			wantCount:   2,
			wantSubstrs: []string{"pgvector", "GRANT"},
		},
		{
			name:        "сам диагностический запрос упал — тоже предупреждение, не паника",
			checker:     fakeReadinessChecker{err: errors.New("нет доступа к pg_extension")},
			wantCount:   1,
			wantSubstrs: []string{"нет доступа к pg_extension"},
		},
		{
			// Доработка задачи 12.1 по итогам ревью: таймаут/отмена самого
			// диагностического запроса — отдельная формулировка ("сервер не
			// ответил"), а не общее "не удалось проверить готовность БД",
			// которое уводит пользователя к правам/расширениям, хотя причина
			// в другом (см. комментарий у storeReadinessWarnings).
			name:        "таймаут диагностического запроса — отдельная формулировка",
			checker:     fakeReadinessChecker{err: context.DeadlineExceeded},
			wantCount:   1,
			wantSubstrs: []string{"не ответил"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := storeReadinessWarnings(context.Background(), tc.checker)
			if len(got) != tc.wantCount {
				t.Fatalf("storeReadinessWarnings = %v (%d), хочу %d предупреждений", got, len(got), tc.wantCount)
			}
			for i, sub := range tc.wantSubstrs {
				if !strings.Contains(got[i], sub) {
					t.Errorf("warning[%d] = %q, не содержит %q", i, got[i], sub)
				}
			}
		})
	}
}

// TestDoInit_PostgresReadinessWarningsPrintedOnce проверяет, что
// предупреждения о готовности (не про доступность — это отдельная ошибка)
// печатаются в out ровно по разу каждое и не валят doInit: неготовая, но
// доступная база — успешный init с предупреждениями в выводе.
func TestDoInit_PostgresReadinessWarningsPrintedOnce(t *testing.T) {
	isolateHomeForTest(t)
	dir := t.TempDir()
	stubStoreProbeWithWarnings(t, []string{
		"расширение PostgreSQL 'vector' (pgvector) не установлено — тест",
		"у роли нет права CREATE в целевой схеме — тест",
	}, nil)

	var out strings.Builder
	err := doInit(initConfig{
		Project:          "myproj",
		Dir:              dir,
		ReasonerProvider: "noop",
		StoreDriver:      storeDriverPostgres,
		PostgresHost:     "postgres.lan",
	}, &out)
	if err != nil {
		t.Fatalf("doInit: %v", err)
	}

	text := out.String()
	for _, want := range []string{"pgvector", "CREATE"} {
		if strings.Count(text, want) != 1 {
			t.Errorf("вывод должен упоминать %q ровно один раз, вывод:\n%s", want, text)
		}
	}
}
