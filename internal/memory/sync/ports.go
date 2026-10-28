// Package sync — вариант использования «синхронизация проекта с общей базой
// команды» (DESIGN.md §2.1, §4.1, §7.1, Этап 19): двусторонний обмен набором
// синхронизации проекта между локальной памятью инстанса и общей базой,
// last-write-wins по updated_at, запись целиком.
//
// Пакет объявляет свои порты (Local, Shared, Dialer) и импортирует только
// agentmemory/internal/memory и стандартную библиотеку: реализации портов живут
// в store (SQLite — локальная сторона, PostgreSQL — общая), сборка — в cmd/memo.
// Решающие правила (состав набора, LWW, слияние связей, авторство, версия
// схемы) живут здесь; адаптеры исполняют механику — транзакции, CAS-условия,
// двухпроходность FK, снятие пометок. Имя пакета совпадает со stdlib sync:
// импортёр, которому нужны оба, берёт псевдоним memsync.
package sync

import (
	"context"
	"slices"

	"agentmemory/internal/memory"
)

// syncKinds — виды памяти набора синхронизации. episodic не входит: эпизоды
// консолидируются на каждой машине своим Consolidator, общие эпизоды дали бы
// дубли фактов (DESIGN.md §2.1).
var syncKinds = []memory.Kind{memory.KindSemantic, memory.KindProcedural, memory.KindDebt, memory.KindTask}

// Kinds — виды памяти набора синхронизации (копия): статусу и doctor, чтобы
// считать неотправленное по тому же составу, что и прогон.
func Kinds() []memory.Kind { return slices.Clone(syncKinds) }

// Scope — scope набора синхронизации проекта. Прочие scope (user:*, другие
// проекты) не синхронизируются.
func Scope(project string) string { return "project:" + project }

// Cursor — позиция pull в общей базе (DESIGN.md §4.1): номер изменения (xid8)
// и id внутри него. Пустой ID — с начала XID; нулевой курсор — с начала базы.
type Cursor struct {
	XID uint64
	ID  string
}

// less — строгий порядок курсоров: по XID, затем по ID (пустой ID — меньше
// любого непустого).
func (c Cursor) less(o Cursor) bool {
	if c.XID != o.XID {
		return c.XID < o.XID
	}
	return c.ID < o.ID
}

// Remote — версия записи в общей базе: запись (с Tags/Links) и номер
// изменения, под которым она лежит.
type Remote struct {
	Memory memory.Memory
	XID    uint64
}

// Pending — версия записи в локальной базе и её пометка неотправленного
// (sync_dirty): Seq — номер пометки (0 — пометки нет), Edited — пометку
// поставила правка, а не первичная разметка (Mark).
type Pending struct {
	Memory memory.Memory
	Seq    int64
	Edited bool
}

// Guarded — запись к применению с ожидаемой версией на принимающей стороне
// (оптимистичная блокировка): версия не совпала — запись пропускается и
// разрешится следующим прогоном.
type Guarded struct {
	Memory    memory.Memory
	ExpectXID uint64 // общая сторона: change_xid, который видел интерактор; 0 — записи нет
	ExpectAt  int64  // локальная сторона: updated_at, который видел интерактор; 0 — записи нет
	ExpectSeq int64  // локальная сторона: seq пометки, который видел интерактор; 0 — пометки нет
}

// Page — страница изменений набора из общей базы. Records — изменённые записи
// по возрастанию (XID, Id); Refs — записи общей базы (любого scope и вида), на
// которые ссылаются связи Records, но которых в странице нет, — из того же
// снимка; Next — безопасный курсор после применения страницы (§4.1).
type Page struct {
	Records, Refs []Remote
	Next          Cursor
}

// Snapshot — версии записей общей базы по списку id (отсутствующие пропущены)
// и Refs их связей, как у Page; один снимок.
type Snapshot struct {
	Records, Refs []Remote
}

// Base — состояние общей базы в одном снимке: идентичность (sync_meta.base_id),
// версия схемы (0 — схемы нет), xmax снимка и наибольший change_xid.
type Base struct {
	ID      string
	Version int
	XMax    uint64
	MaxXID  uint64
}

// Target — цель синхронизации: проект и тумблеры «Отправлять» (Push) и
// «Загружать» (Pull). Параметры подключения интерактору не нужны — их знает Dialer.
type Target struct {
	Project    string
	Push, Pull bool
}

// State — итог прогонов по цели (sync_state); пишет только Syncer. Нулевое
// значение — прогонов не было.
type State struct {
	BaseID         string // base_id общей базы, к которой относится Cursor
	Cursor         Cursor // курсор pull
	PushMarked     bool   // набор размечен для текущего включения «Отправлять»
	LastRunAt      int64
	LastOKAt       int64
	LastError      string // пусто — последний прогон успешен
	LastPushed     int
	LastPulled     int
	LastConflicts  int
	ConflictsTotal int
}

// Result — итог одного прогона Run.
type Result struct {
	Project      string
	Pushed       int // записано в общую базу
	Pulled       int // применено локально из общей базы
	Conflicts    int // LWW отбросил изменение, которого другая сторона не видела
	Stamped      int // проставлено авторов при первой отправке
	Deferred     int // отложено до следующего прогона из-за гонки CAS (любая сторона)
	DroppedLinks int // отброшено рёбер, адресат которых не нашёлся
}

// Local — локальная база инстанса, ровно то, что нужно Syncer. Цели, профиль
// и lease — порт планировщика прогонов (19.6), не этот.
type Local interface {
	// State — состояние цели; нулевое, если прогонов не было.
	State(ctx context.Context, project string) (State, error)
	SaveState(ctx context.Context, project string, st State) error
	// SchemaAhead — локальная БД новее бинаря: синхронизацию не запускать.
	SchemaAhead(ctx context.Context) (bool, error)
	// Mark помечает записи scope видов kinds неотправленными (первичная
	// разметка) с новым Seq; ids == nil — весь набор. Новая пометка — с
	// Edited = false, уже стоящая сохраняет Edited: правка, сделанная при
	// выключенном «Отправлять», остаётся правкой и проигрыш LWW для неё — конфликт.
	Mark(ctx context.Context, scope string, kinds []memory.Kind, ids []string) error
	// Dirty — до limit помеченных записей scope видов kinds с Seq > after по
	// возрастанию Seq (с Tags/Links); пометки записей scope прочих видов снимает.
	Dirty(ctx context.Context, scope string, kinds []memory.Kind, after int64, limit int) ([]Pending, error)
	// Versions — записи по id (с Tags/Links, вместе с тумстоунами) и их пометки;
	// отсутствующие пропущены.
	Versions(ctx context.Context, ids []string) (map[string]Pending, error)
	// StampAuthor заполняет пустые author/updated_by записей ids именем name —
	// без смены updated_at и без новой пометки.
	StampAuthor(ctx context.Context, ids []string, name string) error
	// Apply применяет версии общей базы мимо write path (id, created_at,
	// updated_at, author, updated_by — как в источнике; accessed_at и
	// access_count — локальные) одной транзакцией: сначала записи, затем связи,
	// пометки применённых снимаются, вектор сбрасывается при смене текста.
	// Связи записи заменяются целиком. Не прошедшие CAS по ExpectAt+ExpectSeq — stale.
	Apply(ctx context.Context, recs []Guarded) (applied, stale []string, err error)
	// MarkPushed снимает пометки, если их Seq не изменился.
	MarkPushed(ctx context.Context, done []Pending) error
}

// Shared — общая база одного проекта, открытая без автомиграции.
type Shared interface {
	Base(ctx context.Context) (Base, error)
	// Pull отдаёт страницы изменений набора (scope, kinds) после курсора from,
	// по limit записей, в одном снимке; ошибка page прерывает выборку.
	Pull(ctx context.Context, scope string, kinds []memory.Kind, from Cursor, limit int, page func(Page) error) error
	// Versions — версии записей по id и Refs их связей, один снимок.
	Versions(ctx context.Context, ids []string) (Snapshot, error)
	// Push записывает пачку одной транзакцией: записи по CAS (ExpectXID == 0 —
	// только вставка новой), затем связи записанных; связи записи заменяются
	// целиком, accessed_at/access_count общей базы не трогаются. Не прошедшие
	// CAS — stale.
	Push(ctx context.Context, recs []Guarded) (written, stale []string, err error)
	Close() error
}

// Dialer открывает общую базу проекта по параметрам его цели.
type Dialer interface {
	Dial(ctx context.Context, project string) (Shared, error)
}
