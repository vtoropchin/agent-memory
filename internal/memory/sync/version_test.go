package sync

import (
	"reflect"
	"testing"

	"agentmemory/internal/memory"
)

func ptr(s string) *string { return &s }

func TestSameCanonical(t *testing.T) {
	base := memory.Memory{
		Id: "r1", Kind: memory.KindTask, Scope: "project:X", Content: "c", UpdatedAt: 5,
		Tags:  []string{"a", "b"},
		Links: []memory.Link{{Dst: "d1"}, {Dst: "d2", Rel: memory.RelPartOf}},
	}
	eq := base
	eq.Summary, eq.Source, eq.SupersededBy = ptr(""), ptr(""), ptr("")
	eq.Tags = []string{"b", "a"}
	eq.Links = []memory.Link{{Dst: "d2", Rel: memory.RelPartOf}, {Dst: "d1", Rel: memory.RelRelated}}
	at := int64(9)
	eq.AccessedAt, eq.AccessCount = &at, 7
	if !same(base, eq) {
		t.Fatal("канонически равные версии сочтены разными")
	}
	if diff := base; same(base, func() memory.Memory { diff.Links = diff.Links[:1]; return diff }()) {
		t.Fatal("разные связи сочтены равными")
	}
}

// TestSameCoversEveryField — каждое поле Memory, кроме локальных сигналов
// ранжирования, участвует в сравнении: новое поле, забытое в same, молча
// терялось бы при синхронизации (ничья с отличием в нём считалась бы равенством).
func TestSameCoversEveryField(t *testing.T) {
	local := map[string]bool{"AccessedAt": true, "AccessCount": true}
	typ := reflect.TypeOf(memory.Memory{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		var m memory.Memory
		f := reflect.ValueOf(&m).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("x")
		case reflect.Int, reflect.Int64:
			f.SetInt(1)
		case reflect.Pointer:
			v := reflect.New(f.Type().Elem())
			if v.Elem().Kind() == reflect.String {
				v.Elem().SetString("x")
			}
			f.Set(v)
		case reflect.Slice:
			f.Set(reflect.Append(f, reflect.Zero(f.Type().Elem())))
		default:
			t.Fatalf("поле %s: тип %s не поддержан тестом", name, f.Kind())
		}
		if got := same(memory.Memory{}, m); got != local[name] {
			t.Errorf("поле %s: same = %v, want %v", name, got, local[name])
		}
	}
}
