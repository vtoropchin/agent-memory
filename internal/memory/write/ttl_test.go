package write

import (
	"context"
	"errors"
	"testing"

	"agentmemory/internal/memory"
)

// TestSaveTTLSetsExpiresAt проверяет, что SaveInput.TTL (задача 9.4) разбирается
// как Go-длительность от текущего момента (в тесте now=1000, см.
// newTestWriterWithEmbedder) и пишется в Memory.ExpiresAt = now + duration.
func TestSaveTTLSetsExpiresAt(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-ttl", Content: "факт с сроком годности", TTL: "72h"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := int64(1000) + int64(72*3600)
	if got.ExpiresAt != want {
		t.Errorf("ExpiresAt = %d, want %d (now=1000 + 72h)", got.ExpiresAt, want)
	}
}

// TestSaveEmptyTTLIsEternal проверяет, что пустой TTL (по умолчанию) даёт вечную
// запись — ExpiresAt=0, поведение не отличается от кода до задачи 9.4.
func TestSaveEmptyTTLIsEternal(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-ttl", Content: "вечный факт"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExpiresAt != 0 {
		t.Errorf("ExpiresAt = %d, want 0 (вечная)", got.ExpiresAt)
	}
}

// TestSaveInvalidTTLFailsFast проверяет, что невалидная строка TTL (не парсится
// time.ParseDuration, либо парсится в неположительную длительность) возвращает
// ErrInvalidTTL и НЕ вставляет запись (fail-fast, ошибка раньше дедупа/вставки).
func TestSaveInvalidTTLFailsFast(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	for _, tc := range []struct {
		name string
		ttl  string
	}{
		{"мусорная строка", "завтра"},
		{"отсутствие юнита", "72"},
		{"нулевая длительность", "0h"},
		{"отрицательная длительность", "-1h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-ttl-invalid", Content: "факт " + tc.name, TTL: tc.ttl})
			if !errors.Is(err, ErrInvalidTTL) {
				t.Fatalf("err = %v, want ErrInvalidTTL", err)
			}
		})
	}

	cands, err := st.SearchLexical(ctx, memory.Query{Text: "факт", Scope: "s-ttl-invalid", Limit: 10})
	if err != nil {
		t.Fatalf("SearchLexical: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("SearchLexical = %+v, want пусто (невалидный ttl не должен был вставить запись)", cands)
	}
}

// TestUpdateTTLExtends проверяет, что UpdateInput.TTL продлевает срок жизни:
// новый ExpiresAt считается от ТЕКУЩЕГО момента (now в тесте), а не от исходного
// created_at/ExpiresAt записи.
func TestUpdateTTLExtends(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-ttl-update", Content: "факт для продления", TTL: "1h"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	ttl := "48h"
	if err := w.Update(ctx, UpdateInput{Id: id, TTL: &ttl}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := int64(1000) + int64(48*3600)
	if got.ExpiresAt != want {
		t.Errorf("ExpiresAt после продления = %d, want %d (now=1000 + 48h)", got.ExpiresAt, want)
	}
}

// TestUpdateTTLZeroClears проверяет задокументированный сентинел "0" (и, тем же
// путём, что и Summary, пустую строку) — UpdateInput.TTL снимает срок жизни,
// запись становится вечной (ExpiresAt=0), даже если до этого TTL был установлен.
func TestUpdateTTLZeroClears(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-ttl-clear", Content: "факт со сроком, который снимут", TTL: "1h"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	for _, sentinel := range []string{"0", ""} {
		t.Run("sentinel="+sentinel, func(t *testing.T) {
			// каждый под-тест начинает с TTL="1h" заново, чтобы проверить снятие независимо.
			ttlSet := "1h"
			if err := w.Update(ctx, UpdateInput{Id: id, TTL: &ttlSet}); err != nil {
				t.Fatalf("Update (установка): %v", err)
			}
			s := sentinel
			if err := w.Update(ctx, UpdateInput{Id: id, TTL: &s}); err != nil {
				t.Fatalf("Update (снятие): %v", err)
			}
			got, err := st.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.ExpiresAt != 0 {
				t.Errorf("ExpiresAt = %d, want 0 (снято сентинелом %q)", got.ExpiresAt, sentinel)
			}
		})
	}
}

// TestUpdateNilTTLDoesNotChange проверяет, что не переданный TTL (nil) не трогает
// уже установленный ExpiresAt — как и остальные поля UpdateInput.
func TestUpdateNilTTLDoesNotChange(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-ttl-nil", Content: "факт, TTL которого не должны тронуть", TTL: "72h"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	before, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	newImportance := 5
	if err := w.Update(ctx, UpdateInput{Id: id, Importance: &newImportance}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if after.ExpiresAt != before.ExpiresAt {
		t.Errorf("ExpiresAt изменился = %d, want %d (TTL не передавался в Update)", after.ExpiresAt, before.ExpiresAt)
	}
}

// TestUpdateInvalidTTLFailsFast — невалидный TTL (не "0"/"") в Update возвращает
// ErrInvalidTTL и не применяет никаких изменений записи.
func TestUpdateInvalidTTLFailsFast(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t)

	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "s-ttl-update-invalid", Content: "факт"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	ttl := "не длительность"
	if err := w.Update(ctx, UpdateInput{Id: id, TTL: &ttl}); !errors.Is(err, ErrInvalidTTL) {
		t.Fatalf("err = %v, want ErrInvalidTTL", err)
	}

	got, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExpiresAt != 0 {
		t.Errorf("ExpiresAt = %d, want 0 (невалидный ttl не должен был применить изменения)", got.ExpiresAt)
	}
}
