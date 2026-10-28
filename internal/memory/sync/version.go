package sync

import (
	"slices"

	"agentmemory/internal/memory"
)

// inSet — входит ли запись в набор синхронизации scope: тот же scope и вид из syncKinds.
func inSet(m memory.Memory, scope string) bool {
	return m.Scope == scope && slices.Contains(syncKinds, m.Kind)
}

// same — равны ли версии записи во всём, что синхронизируется: поля, теги и
// связи (DESIGN.md §7.1). Сравнение в канонической форме — nil и пустая строка
// у необязательных полей равны, теги и связи — множества, пустой rel —
// related: иначе ложные расхождения давали бы лишние применения и сбросы
// векторов. accessed_at/access_count — локальные сигналы, не сравниваются.
func same(a, b memory.Memory) bool {
	return a.Id == b.Id && a.Kind == b.Kind && a.Scope == b.Scope &&
		a.Content == b.Content && deref(a.Summary) == deref(b.Summary) &&
		a.Importance == b.Importance && a.CreatedAt == b.CreatedAt && a.UpdatedAt == b.UpdatedAt &&
		deref(a.SupersededBy) == deref(b.SupersededBy) && deref(a.Source) == deref(b.Source) &&
		a.ExpiresAt == b.ExpiresAt && a.Status == b.Status && a.Type == b.Type &&
		a.Branch == b.Branch && a.Workflow == b.Workflow &&
		a.Author == b.Author && a.UpdatedBy == b.UpdatedBy &&
		sameSet(a.Tags, b.Tags, func(t string) string { return t }) &&
		sameSet(a.Links, b.Links, linkKey)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// linkKey — каноническое представление связи для сравнения и дедупликации.
func linkKey(l memory.Link) string {
	rel := l.Rel
	if rel == "" {
		rel = memory.RelRelated
	}
	return l.Dst + "\x00" + rel
}

func sameSet[T any](a, b []T, key func(T) string) bool {
	as := make(map[string]bool, len(a))
	for _, x := range a {
		as[key(x)] = true
	}
	bs := make(map[string]bool, len(b))
	for _, x := range b {
		k := key(x)
		if !as[k] {
			return false
		}
		bs[k] = true
	}
	return len(as) == len(bs)
}

// mergeLinks собирает связи записи, которые получит принимающая сторона:
// из own (связи стороны-источника версии) — рёбра, для которых take(dst)
// истинно, затем из keep (связи принимающей стороны) — рёбра, для которых
// take(dst) ложно. Так LWW заменяет связи внутри набора, а рёбра вне набора,
// которые другая сторона не видит, остаются на месте (DESIGN.md §7.1).
// Повторы рёбер схлопываются.
func mergeLinks(own []memory.Link, ownTake func(dst string) bool, keep []memory.Link, keepTake func(dst string) bool) []memory.Link {
	var out []memory.Link
	seen := map[string]bool{}
	add := func(l memory.Link) {
		if k := linkKey(l); !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	for _, l := range own {
		if ownTake(l.Dst) {
			add(l)
		}
	}
	for _, l := range keep {
		if keepTake(l.Dst) {
			add(l)
		}
	}
	return out
}
