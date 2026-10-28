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

	t.Run("SetAuthor меняет имя последующих правок", func(t *testing.T) {
		w := New(st, embed.NewNoop(), WithAuthor("Гит"))
		mine, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: scope, Content: "факт до смены имени"})
		if err != nil {
			t.Fatalf("Save: %v", err)
		}
		w.SetAuthor("  Вера  ")
		imp := 2
		if err := w.Update(ctx, UpdateInput{Id: mine, Importance: &imp}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if a, u := authorOf(t, st, mine); a != "Гит" || u != "Вера" {
			t.Errorf("после SetAuthor: author=%q updated_by=%q, want Гит/Вера", a, u)
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

// TestWriterUpdatedAtGrows: правка ставит updated_at = max(now, прежний + 1) —
// вторая правка в ту же секунду и правка поверх версии с часов, ушедших вперёд,
// всё равно новее прежней (DESIGN.md §4, LWW синхронизации §7.1).
func TestWriterUpdatedAtGrows(t *testing.T) {
	ctx := context.Background()
	w, st := newTestWriter(t) // now зафиксирован на 1000
	id, _, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "project:clock", Content: "факт про часы синхронизации"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	updatedAt := func() int64 {
		t.Helper()
		m, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return m.UpdatedAt
	}
	for i, want := range []int64{1001, 1002} {
		imp := 4 + i
		if err := w.Update(ctx, UpdateInput{Id: id, Importance: &imp}); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got := updatedAt(); got != want {
			t.Errorf("Update %d в ту же секунду: updated_at = %d, хочу %d", i+1, got, want)
		}
	}
	if _, merged, err := w.Save(ctx, SaveInput{Kind: memory.KindSemantic, Scope: "project:clock", Content: "факт про часы синхронизации"}); err != nil || !merged {
		t.Fatalf("Save дубля: merged=%v err=%v", merged, err)
	}
	if got := updatedAt(); got != 1003 {
		t.Errorf("слияние дубля в ту же секунду: updated_at = %d, хочу 1003", got)
	}

	m, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	m.UpdatedAt = 5000 // версия с машины, чьи часы впереди
	if err := st.Put(ctx, m); err != nil {
		t.Fatal(err)
	}
	imp := 2
	if err := w.Update(ctx, UpdateInput{Id: id, Importance: &imp}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got := updatedAt(); got != 5001 {
		t.Errorf("правка поверх версии из будущего: updated_at = %d, хочу 5001", got)
	}
}
