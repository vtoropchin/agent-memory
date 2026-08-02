package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"agentmemory/internal/memory"
)

// TestLinkEntryUnmarshalJSON проверяет разбор обеих форм элемента links (задача
// 9.3, обратная совместимость обязательна): строка-id (back-compat, Rel остаётся
// пустым) и объект {"id":.., "rel":..}.
func TestLinkEntryUnmarshalJSON(t *testing.T) {
	t.Run("строка — back-compat", func(t *testing.T) {
		var e linkEntry
		if err := json.Unmarshal([]byte(`"01J8Z000000000000000000A"`), &e); err != nil {
			t.Fatalf("UnmarshalJSON: %v", err)
		}
		if e.Dst != "01J8Z000000000000000000A" || e.Rel != "" {
			t.Errorf("got %+v, want Dst=id, Rel=\"\"", e)
		}
	})

	t.Run("объект с rel", func(t *testing.T) {
		var e linkEntry
		if err := json.Unmarshal([]byte(`{"id":"01J8Z000000000000000000A","rel":"causes"}`), &e); err != nil {
			t.Fatalf("UnmarshalJSON: %v", err)
		}
		if e.Dst != "01J8Z000000000000000000A" || e.Rel != "causes" {
			t.Errorf("got %+v, want Dst=id, Rel=causes", e)
		}
	})

	t.Run("объект без rel — Rel пуст, дефолт не здесь", func(t *testing.T) {
		var e linkEntry
		if err := json.Unmarshal([]byte(`{"id":"01J8Z000000000000000000A"}`), &e); err != nil {
			t.Fatalf("UnmarshalJSON: %v", err)
		}
		if e.Dst != "01J8Z000000000000000000A" || e.Rel != "" {
			t.Errorf("got %+v, want Rel=\"\" (дефолт делает write.resolveLinks, не разбор)", e)
		}
	})

	t.Run("смешанный массив", func(t *testing.T) {
		var entries []linkEntry
		raw := `["01AAAAAAAAAAAAAAAAAAAAAAAA", {"id":"01BBBBBBBBBBBBBBBBBBBBBBBB","rel":"part_of"}]`
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if len(entries) != 2 {
			t.Fatalf("len(entries) = %d, want 2", len(entries))
		}
		if entries[0].Dst != "01AAAAAAAAAAAAAAAAAAAAAAAA" || entries[0].Rel != "" {
			t.Errorf("entries[0] = %+v, want строковая форма (Rel пуст)", entries[0])
		}
		if entries[1].Dst != "01BBBBBBBBBBBBBBBBBBBBBBBB" || entries[1].Rel != "part_of" {
			t.Errorf("entries[1] = %+v, want объектная форма rel=part_of", entries[1])
		}
	})

	t.Run("некорректный элемент — ошибка, не паника", func(t *testing.T) {
		var e linkEntry
		if err := json.Unmarshal([]byte(`42`), &e); err == nil {
			t.Fatal("ожидалась ошибка на число вместо строки/объекта")
		}
	})
}

// TestNormalizeLinks проверяет валидацию rel (DESIGN.md §4, задача 9.3):
// неизвестное значение — ошибка с перечнем допустимых (fail-fast, без молчаливого
// фолбэка в related); пустой Rel и известные значения проходят как есть.
func TestNormalizeLinks(t *testing.T) {
	t.Run("пустой Rel проходит как есть", func(t *testing.T) {
		out, err := normalizeLinks([]linkEntry{{Dst: "a"}})
		if err != nil {
			t.Fatalf("normalizeLinks: %v", err)
		}
		if len(out) != 1 || out[0].Dst != "a" || out[0].Rel != "" {
			t.Errorf("out = %+v, want Dst=a, Rel=\"\"", out)
		}
	})

	t.Run("известный Rel проходит как есть", func(t *testing.T) {
		out, err := normalizeLinks([]linkEntry{{Dst: "a", Rel: memory.RelCauses}})
		if err != nil {
			t.Fatalf("normalizeLinks: %v", err)
		}
		if len(out) != 1 || out[0].Rel != memory.RelCauses {
			t.Errorf("out = %+v, want Rel=%q", out, memory.RelCauses)
		}
	})

	t.Run("неизвестный Rel — ошибка с перечнем допустимых", func(t *testing.T) {
		_, err := normalizeLinks([]linkEntry{{Dst: "a", Rel: "cuases"}})
		if err == nil {
			t.Fatal("ожидалась ошибка на опечатку в rel")
		}
		for _, want := range linkRelValues {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %q, ожидалось упоминание допустимого значения %q", err.Error(), want)
			}
		}
	})

	t.Run("пусто на входе — пусто на выходе", func(t *testing.T) {
		out, err := normalizeLinks(nil)
		if err != nil || out != nil {
			t.Errorf("normalizeLinks(nil) = (%v, %v), want (nil, nil)", out, err)
		}
	})
}

// TestSaveInputSchemaValidatesLinks проверяет саму опубликованную JSON-схему
// memory_save (не только Go-разбор): её резолвит и валидирует ровно тот же путь,
// что MCP SDK применяет к сырым аргументам вызова инструмента (applySchema →
// Resolved.Validate) до того, как они попадут в наш обработчик. oneOf строки и
// объекта должен принять back-compat строку, типизированный объект и смешанный
// массив, и отклонить элемент, не подходящий ни под одну из форм.
func TestSaveInputSchemaValidatesLinks(t *testing.T) {
	resolved, err := saveInputSchema().Resolve(nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	valid := []string{
		`{"kind":"semantic","content":"c","scope":"s","links":["01ARZ3NDEKTSV4RRFFQ69G5FAV"]}`,
		`{"kind":"semantic","content":"c","scope":"s","links":[{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","rel":"causes"}]}`,
		`{"kind":"semantic","content":"c","scope":"s","links":["01ARZ3NDEKTSV4RRFFQ69G5FAV",{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","rel":"part_of"}]}`,
		`{"kind":"semantic","content":"c","scope":"s"}`, // links вообще не передан
	}
	for _, raw := range valid {
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("json.Unmarshal(%s): %v", raw, err)
		}
		if err := resolved.Validate(v); err != nil {
			t.Errorf("Validate(%s): %v, want nil (валидный вход)", raw, err)
		}
	}

	invalid := `{"kind":"semantic","content":"c","scope":"s","links":[42]}`
	var v any
	if err := json.Unmarshal([]byte(invalid), &v); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := resolved.Validate(v); err == nil {
		t.Errorf("Validate(%s) = nil, want ошибку (элемент — ни строка, ни объект)", invalid)
	}
}

// TestKindEnumIncludesDebt проверяет, что "debt" (техдолг, четвёртый memory.Kind
// наравне с semantic/episodic/procedural — согласовано в задаче про сущность
// техдолга) присутствует в enum поля kind/kinds обеих схем: memory_save (kind)
// и memory_recall (kinds). Схема memory_recall.kinds фильтрует по типу — без
// "debt" в её enum отфильтровать записи техдолга через строгий JSON Schema
// валидатор было бы невозможно.
func TestKindEnumIncludesDebt(t *testing.T) {
	saveResolved, err := saveInputSchema().Resolve(nil)
	if err != nil {
		t.Fatalf("saveInputSchema().Resolve: %v", err)
	}
	var saveArgs any
	if err := json.Unmarshal([]byte(`{"kind":"debt","content":"c","scope":"s"}`), &saveArgs); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := saveResolved.Validate(saveArgs); err != nil {
		t.Errorf("memory_save schema Validate(kind=debt): %v, want nil", err)
	}

	recallResolved, err := recallInputSchema().Resolve(nil)
	if err != nil {
		t.Fatalf("recallInputSchema().Resolve: %v", err)
	}
	var recallArgs any
	if err := json.Unmarshal([]byte(`{"query":"q","kinds":["debt"]}`), &recallArgs); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := recallResolved.Validate(recallArgs); err != nil {
		t.Errorf("memory_recall schema Validate(kinds=[debt]): %v, want nil", err)
	}
}
