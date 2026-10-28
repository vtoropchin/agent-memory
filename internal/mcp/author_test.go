package mcp

import (
	"context"
	"strings"
	"testing"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/write"
)

// TestGetShowsAuthor: memory_get показывает автора и последнего редактора
// (Этап 19), recall-бриф — нет; у записи без авторства строк нет вовсе.
func TestGetShowsAuthor(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)

	id, _, err := srv.writer.Save(ctx, write.SaveInput{Kind: memory.KindSemantic, Scope: "project:agentmemory", Content: "факт про авторство записей"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	res, _, err := srv.handleGet(ctx, nil, getInput{ID: id})
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	if out := textOf(res); strings.Contains(out, "- author:") || strings.Contains(out, "- updated_by:") {
		t.Errorf("у записи без авторства строк author/updated_by быть не должно:\n%s", out)
	}

	m, err := srv.store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	m.Author, m.UpdatedBy = "Анна", "Борис"
	if err := srv.store.Put(ctx, m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	res, _, err = srv.handleGet(ctx, nil, getInput{ID: id})
	if err != nil {
		t.Fatalf("handleGet: %v", err)
	}
	out := textOf(res)
	for _, want := range []string{"- author: Анна\n", "- updated_by: Борис\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("memory_get должен содержать %q:\n%s", want, out)
		}
	}

	rec, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "авторство записей"})
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	brief := textOf(rec)
	if !strings.Contains(brief, "факт про авторство") {
		t.Fatalf("recall должен найти запись:\n%s", brief)
	}
	if strings.Contains(brief, "Анна") || strings.Contains(brief, "Борис") {
		t.Errorf("авторство не выводится в recall-бриф:\n%s", brief)
	}
}
