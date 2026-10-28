package write

import (
	"context"
	"testing"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/embed"
	"agentmemory/internal/memory/store"
)

// authorOf читает запись и возвращает её поля авторства.
func authorOf(t *testing.T, st store.Store, id string) (author, updatedBy string) {
	t.Helper()
	m, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return m.Author, m.UpdatedBy
}

// TestWriterAuthor: Save ставит Author и UpdatedBy = имя Writer'а; Update,
// Forget и слияние дубля меняют только UpdatedBy; Touch не трогает ничего;
// Writer без имени поля не ставит и не затирает (Этап 19, DESIGN.md §4).
func TestWriterAuthor(t *testing.T) {
	ctx := context.Background()
	_, st := newTestWriter(t)
	anna := New(st, embed.NewNoop(), WithAuthor("  Анна  "))
	boris := New(st, embed.NewNoop(), WithAuthor("Борис"))
	anonymous := New(st, embed.NewNoop())
	scope := "project:authors"

	id, _, err := anna.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: scope, Content: "факт, созданный Анной про авторство"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if a, u := authorOf(t, st, id); a != "Анна" || u != "Анна" {
		t.Errorf("после Save: author=%q updated_by=%q, want Анна/Анна (имя без пробелов)", a, u)
	}

	t.Run("Update меняет только UpdatedBy", func(t *testing.T) {
		imp := 5
		if err := boris.Update(ctx, UpdateInput{Id: id, Importance: &imp}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if a, u := authorOf(t, st, id); a != "Анна" || u != "Борис" {
			t.Errorf("после Update: author=%q updated_by=%q, want Анна/Борис", a, u)
		}
	})

	t.Run("Writer без имени не затирает поля", func(t *testing.T) {
		imp := 4
		if err := anonymous.Update(ctx, UpdateInput{Id: id, Importance: &imp}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if a, u := authorOf(t, st, id); a != "Анна" || u != "Борис" {
			t.Errorf("после Update без имени: author=%q updated_by=%q, want Анна/Борис", a, u)
		}
	})

	t.Run("Touch не трогает авторство", func(t *testing.T) {
		if err := st.Touch(ctx, id); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		if a, u := authorOf(t, st, id); a != "Анна" || u != "Борис" {
			t.Errorf("после Touch: author=%q updated_by=%q, want Анна/Борис", a, u)
		}
	})

	t.Run("слияние дубля — правка существующей записи", func(t *testing.T) {
		got, merged, err := anna.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: scope, Content: "факт, созданный Анной про авторство"})
		if err != nil {
			t.Fatalf("Save дубля: %v", err)
		}
		if !merged || got != id {
			t.Fatalf("Save дубля: id=%s merged=%v, want слияние в %s", got, merged, id)
		}
		if a, u := authorOf(t, st, id); a != "Анна" || u != "Анна" {
			t.Errorf("после слияния: author=%q updated_by=%q, want Анна/Анна", a, u)
		}
	})

	t.Run("Forget меняет только UpdatedBy", func(t *testing.T) {
		if err := boris.Forget(ctx, id, ""); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if a, u := authorOf(t, st, id); a != "Анна" || u != "Борис" {
			t.Errorf("после Forget: author=%q updated_by=%q, want Анна/Борис", a, u)
		}
	})

	t.Run("Forget без имени не затирает UpdatedBy", func(t *testing.T) {
		other, _, err := boris.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: scope, Content: "другой факт Бориса"})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if err := anonymous.Forget(ctx, other, ""); err != nil {
			t.Fatalf("Forget: %v", err)
		}
		if a, u := authorOf(t, st, other); a != "Борис" || u != "Борис" {
			t.Errorf("после Forget без имени: author=%q updated_by=%q, want Борис/Борис", a, u)
		}
	})

	t.Run("Save без имени оставляет поля пустыми", func(t *testing.T) {
		nobody, _, err := anonymous.Save(ctx, SaveInput{Kind: memory.KindTask, Scope: scope, Summary: "задача", Content: "задача без автора"})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		if a, u := authorOf(t, st, nobody); a != "" || u != "" {
			t.Errorf("Save без имени: author=%q updated_by=%q, want пусто", a, u)
		}
	})
}
