package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agentmemory/internal/memory"
	"agentmemory/internal/memory/write"
)

// TestTaskBlockingThroughTools — полный цикл блокировки через инструменты
// (задача 15.4): save A, save B с blocked_by A → update B doing → ошибка с
// упоминанием A → memory_get(A) показывает «Блокирует: B», memory_get(B) —
// «Заблокирована (ждёт): A» → recall помечает B ⛔ → update A done → update B
// doing ок; unlinks снимает связь.
func TestTaskBlockingThroughTools(t *testing.T) {
	ctx := context.Background()
	srv := newTestServer(t)
	const scope = "project:agentmemory"

	aRes, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: string(memory.KindTask), Content: "Решить, как хранить блокировки.", Scope: scope, Summary: "Решение по блокировкам",
	})
	if err != nil {
		t.Fatalf("handleSave(A): %v", err)
	}
	a := extractID(t, textOf(aRes))

	bRes, _, err := srv.handleSave(ctx, nil, saveInput{
		Kind: string(memory.KindTask), Content: "Реализовать блокировки после решения.", Scope: scope, Summary: "Реализация блокировок",
		Links: []linkEntry{{Dst: a, Rel: memory.RelBlockedBy}},
	})
	if err != nil {
		t.Fatalf("handleSave(B): %v", err)
	}
	b := extractID(t, textOf(bRes))

	doing, done := string(memory.StatusDoing), string(memory.StatusDone)
	_, _, err = srv.handleUpdate(ctx, nil, updateInput{ID: b, Status: &doing})
	if err == nil || !strings.Contains(err.Error(), "заблокирована") || !strings.Contains(err.Error(), "Решение по блокировкам ("+a+")") {
		t.Fatalf("update B doing при живом блокере: err = %v, want ошибку с перечнем блокеров", err)
	}

	getA, _, err := srv.handleGet(ctx, nil, getInput{ID: a})
	if err != nil {
		t.Fatalf("handleGet(A): %v", err)
	}
	if out := textOf(getA); !strings.Contains(out, "Блокирует:\n- [todo] (task) Реализация блокировок ("+b+")") {
		t.Errorf("memory_get(A) должен показать «Блокирует: B»:\n%s", out)
	}
	getB, _, err := srv.handleGet(ctx, nil, getInput{ID: b})
	if err != nil {
		t.Fatalf("handleGet(B): %v", err)
	}
	if out := textOf(getB); !strings.Contains(out, "Заблокирована (ждёт):\n- [todo] (task) Решение по блокировкам ("+a+")") || strings.Contains(out, "Блокирует:") {
		t.Errorf("memory_get(B) должен показать «Заблокирована (ждёт): A» и не показывать «Блокирует»:\n%s", out)
	}

	recallRes, _, err := srv.handleRecall(ctx, nil, recallInput{Query: "блокировки решение", Scope: scope})
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	if md := textOf(recallRes); !strings.Contains(md, "⛔ Реализация блокировок (ждёт: Решение по блокировкам)") {
		t.Errorf("recall должен пометить B маркером ⛔ с именем блокера:\n%s", md)
	}

	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: a, Status: &done}); err != nil {
		t.Fatalf("handleUpdate(A done): %v", err)
	}
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: b, Status: &doing}); err != nil {
		t.Fatalf("handleUpdate(B doing) после done блокера: %v", err)
	}
	getB, _, err = srv.handleGet(ctx, nil, getInput{ID: b})
	if err != nil {
		t.Fatalf("handleGet(B): %v", err)
	}
	if out := textOf(getB); strings.Contains(out, "Заблокирована") {
		t.Errorf("после done блокера секции «Заблокирована» быть не должно:\n%s", out)
	}

	// unlinks снимает ребро: связь исчезает из links записи.
	if _, _, err := srv.handleUpdate(ctx, nil, updateInput{ID: b, Unlinks: []linkEntry{{Dst: a, Rel: memory.RelBlockedBy}}}); err != nil {
		t.Fatalf("handleUpdate(unlinks): %v", err)
	}
	m, err := srv.store.Get(ctx, b)
	if err != nil {
		t.Fatalf("Get(B): %v", err)
	}
	if len(m.Links) != 0 {
		t.Errorf("после unlinks у B не должно остаться рёбер: %+v", m.Links)
	}

	// blocked_by на факт — читаемая ошибка write.ErrBlockerNotTask.
	factRes, _, err := srv.handleSave(ctx, nil, saveInput{Kind: string(memory.KindSemantic), Content: "Факт о блокировках.", Scope: scope})
	if err != nil {
		t.Fatalf("handleSave(факт): %v", err)
	}
	fact := extractID(t, textOf(factRes))
	_, _, err = srv.handleUpdate(ctx, nil, updateInput{ID: b, Links: []linkEntry{{Dst: fact, Rel: memory.RelBlockedBy}}})
	if err == nil || !strings.Contains(err.Error(), write.ErrBlockerNotTask.Error()) {
		t.Errorf("blocked_by на факт: err = %v, want ErrBlockerNotTask", err)
	}
}

// TestBlockingSchema: схемы принимают rel=blocked_by в links и поле unlinks в
// memory_update; неизвестный rel в unlinks отвергается normalizeLinks.
func TestBlockingSchema(t *testing.T) {
	updateResolved, err := updateInputSchema().Resolve(nil)
	if err != nil {
		t.Fatalf("updateInputSchema().Resolve: %v", err)
	}
	var args any
	if err := json.Unmarshal([]byte(`{"id":"01J8Z000000000000000000A","links":[{"id":"01J8Z000000000000000000B","rel":"blocked_by"}],"unlinks":["01J8Z000000000000000000C",{"id":"01J8Z000000000000000000D","rel":"blocked_by"}]}`), &args); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := updateResolved.Validate(args); err != nil {
		t.Errorf("memory_update Validate(links blocked_by + unlinks): %v, want nil", err)
	}
	if _, err := normalizeLinks([]linkEntry{{Dst: "x", Rel: "blocks"}}); err == nil || !strings.Contains(err.Error(), "blocked_by") {
		t.Errorf("normalizeLinks(rel=blocks) = %v, want ошибку с перечнем, включающим blocked_by", err)
	}
	in := unmarshalUpdateInput(t, `{"id":"x","unlinks":[{"id":"y","rel":"blocked_by"}]}`)
	if len(in.Unlinks) != 1 || in.Unlinks[0].Dst != "y" || in.Unlinks[0].Rel != memory.RelBlockedBy {
		t.Errorf("unlinks не разобрались: %+v", in.Unlinks)
	}
}
