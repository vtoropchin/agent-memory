package memory

import "sort"

// Хелперы графа связей для иерархии задач (DESIGN.md §14 Этап 15, задача 15.3).
// Чистые функции над уже загруженными Memory/Links, без I/O: обратный обход
// («кто ссылается на меня») строится потребителем из Store.Get(id) +
// Store.Neighbors(id) — соседи приходят со своими Links, поэтому направление и
// rel каждого ребра восстановимы, а отдельный «входящий» запрос к хранилищу не
// нужен, пока множества соседей малы.
//
// Направление иерархии зафиксировано: ребро rel=part_of идёт от ДОЧЕРНЕЙ записи
// к РОДИТЕЛЬСКОЙ (задача part_of истории, история part_of эпика). Старые данные
// с перевёрнутыми рёбрами не чинятся — это конвенция для новых записей.

// ParentOf возвращает id родителя m — цель первого ребра с Rel == RelPartOf.
// У задачи один родитель; если рёбер part_of несколько, берётся первое (порядок
// Links стабилен: store отдаёт их отсортированными по (Dst, Rel)). Нет part_of —
// ok == false.
func ParentOf(m Memory) (id string, ok bool) {
	for _, l := range m.Links {
		if l.Rel == RelPartOf {
			return l.Dst, true
		}
	}
	return "", false
}

// ChildrenOf возвращает те из neighbors, у кого есть ребро {Dst: id, Rel:
// RelPartOf} — то есть задачи, объявившие себя частью id. Прочие rel (related,
// causes, …) и рёбра на другие записи не считаются; порядок neighbors
// сохраняется. Учитываются только записи Kind == KindTask: у прочих kind part_of
// — тематическая связь (факт «часть темы»), а не иерархия доски, и в прогресс
// родителя они входить не должны. Ожидает соседей с загруженными Links
// (Store.Neighbors).
func ChildrenOf(id string, neighbors []Memory) []Memory {
	var children []Memory
	for _, n := range neighbors {
		if n.Kind != KindTask {
			continue
		}
		for _, l := range n.Links {
			if l.Rel == RelPartOf && l.Dst == id {
				children = append(children, n)
				break
			}
		}
	}
	return children
}

// Progress считает прогресс родителя по детям: total — живые дети
// (SupersededBy == nil; тумстоуны не считаются ни в числителе, ни в
// знаменателе), done — из них со Status == StatusDone.
func Progress(children []Memory) (done, total int) {
	for _, c := range children {
		if c.SupersededBy != nil {
			continue
		}
		total++
		if c.Status == StatusDone {
			done++
		}
	}
	return done, total
}

// SortByStage упорядочивает задачи как колонки доски (DESIGN.md §14 Этап 15):
// по стадии в порядке Statuses, внутри стадии — по Importance по убыванию,
// затем по UpdatedAt по убыванию (свежее выше), затем по Id (детерминизм при
// полном равенстве). Порядок списков детей в memory_get и в карточке
// web-морды — один и тот же, поэтому правило живёт здесь, а не в каждом
// адаптере; доска (пакет board) переиспользует его же. Сортировка на месте,
// стабильная.
func SortByStage(tasks []Memory) {
	rank := make(map[Status]int, len(Statuses))
	for i, st := range Statuses {
		rank[st] = i
	}
	sort.SliceStable(tasks, func(i, j int) bool {
		ri, rj := rank[tasks[i].Status], rank[tasks[j].Status]
		if ri != rj {
			return ri < rj
		}
		if tasks[i].Importance != tasks[j].Importance {
			return tasks[i].Importance > tasks[j].Importance
		}
		if tasks[i].UpdatedAt != tasks[j].UpdatedAt {
			return tasks[i].UpdatedAt > tasks[j].UpdatedAt
		}
		return tasks[i].Id < tasks[j].Id
	})
}
