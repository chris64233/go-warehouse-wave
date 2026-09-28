package gowarehousewave

import "time"

// 状态常量。
const (
	// WaveStatusPending 波次已成立、待拣货（含重排后的新版本）。
	WaveStatusPending = "pending"
	// WaveStatusPicking 已有部分数量确认拣出，尚未全部完成。
	WaveStatusPicking = "picking"
	// WaveStatusCompleted 全部订单行拣货完成。
	WaveStatusCompleted = "completed"
	// WaveStatusCancelled 波次取消；已确认拣出的数量保留，未确认占用已释放。
	WaveStatusCancelled = "cancelled"

	// EntryActive 当前版本上的有效占用，可拣货/上报缺货。
	EntryActive = "active"
	// EntrySuperseded 已被后续分配版本取代，仅作历史保留，不再持有库存。
	EntrySuperseded = "superseded"
	// EntryCompleted 该明细全部数量已确认拣出。
	EntryCompleted = "completed"
	// EntryCancelled 波次取消，未确认占用已释放回库存池。
	EntryCancelled = "cancelled"

	// ReasonInitial 波次创建时的初始分配版本。
	ReasonInitial = "initial"
	// ReasonShortage 缺货重排产生的版本。
	ReasonShortage = "shortage"
	// ReasonCarry 缺货重排时从旧版本原样结转的明细（换批次见 shortage）。
	ReasonCarry = "carry"
)

// BatchInput 登记库存批次的入参。
type BatchInput struct {
	// ID 批次号，全局唯一。
	ID string
	// SKU 商品编码。
	SKU string
	// OnHand 登记时的实物库存数量，必须为正。
	OnHand int
	// Expiry 批次有效期（失效时间）。
	Expiry time.Time
	// ReceivedAt 入库时间。
	ReceivedAt time.Time
}

// Batch 库存批次视图。
//
// 账面恒等式：OnHand = Available + Held + Picked。
// 缺货核销会同时调减 OnHand 与 Held（盘亏，不回到 Available）。
type Batch struct {
	ID         string
	SKU        string
	Expiry     time.Time
	ReceivedAt time.Time
	// OnHand 当前账面上的实物数量（缺货核销会调减）。
	OnHand int
	// Available 仍可分配给新波次/缺口的数量。
	Available int
	// Held 被当前版本分配占用、尚未拣出也未释放的数量。
	Held int
	// Picked 已确认拣出、永久出库的数量。
	Picked int
	// Status active / exhausted（无可用且无占用）。
	Status string
}

// OrderLine 波次中的订单行。同一订单可包含多个 SKU 行。
type OrderLine struct {
	OrderID string
	SKU     string
	Qty     int
}

// AllocationEntry 一条批次级分配明细。版本化保留：旧版本明细不会被删除或改写。
type AllocationEntry struct {
	ID      string
	WaveID  string
	Version int
	// OrderID/SKU 对应的订单行。
	OrderID string
	SKU     string
	// BatchID 分配到的库存批次。
	BatchID string
	// Qty 本明细占用的批次数量。
	Qty int
	// Picked 已确认拣出的数量。
	Picked int
	// Reason initial / shortage / carry。
	Reason string
	// Status active / superseded / completed / cancelled。
	Status string
}

// AllocationVersion 一次分配版本的完整快照。
type AllocationVersion struct {
	WaveID    string
	Version   int
	Reason    string
	CreatedAt time.Time
	// Entries 该版本下的全部分配明细。
	Entries []AllocationEntry
}

// WaveDetail 波次及其全部分配版本。
type WaveDetail struct {
	WaveID string
	Status string
	// CurrentVersion 当前生效的分配版本号，从 1 开始；每次缺货重排 +1。
	CurrentVersion int
	// Lines 创建时冻结的订单行集合（按 OrderID、SKU 排序）。
	Lines []OrderLine
	// Versions 按版本号升序的历史版本（含当前版本）。
	Versions []AllocationVersion
	// Entries 当前版本的分配明细，等价于 Versions[len-1].Entries。
	Entries []AllocationEntry
}

// ---- 内部模型 ----

// batch 库存批次聚合根。
type batch struct {
	id         string
	sku        string
	expiry     time.Time
	receivedAt time.Time
	onHand     int
	held       int
	picked     int
}

// available 可分配量 = 账面实物 - 占用 - 已拣出。
func (b *batch) available() int {
	return b.onHand - b.held - b.picked
}

func (b *batch) toView() Batch {
	st := "active"
	if b.available() == 0 && b.held == 0 {
		st = "exhausted"
	}
	return Batch{
		ID:         b.id,
		SKU:        b.sku,
		Expiry:     b.expiry,
		ReceivedAt: b.receivedAt,
		OnHand:     b.onHand,
		Available:  b.available(),
		Held:       b.held,
		Picked:     b.picked,
		Status:     st,
	}
}

// entry 批次级分配明细（内部表示）。
type entry struct {
	id      string
	waveID  string
	version int
	orderID string
	sku     string
	batchID string
	qty     int
	picked  int
	reason  string
	status  string
}

func (e *entry) remaining() int { return e.qty - e.picked }

func (e *entry) toView() AllocationEntry {
	return AllocationEntry{
		ID:      e.id,
		WaveID:  e.waveID,
		Version: e.version,
		OrderID: e.orderID,
		SKU:     e.sku,
		BatchID: e.batchID,
		Qty:     e.qty,
		Picked:  e.picked,
		Reason:  e.reason,
		Status:  e.status,
	}
}

// wave 波次聚合根。
type wave struct {
	id             string
	status         string
	currentVersion int
	lines          []OrderLine // 已冻结、已排序
	versions       []*allocVersion
	entries        map[string]*entry // 全部版本明细
	createdAt      time.Time
}

type allocVersion struct {
	version   int
	reason    string
	createdAt time.Time
	entryIDs  []string // 该版本内的明细，按创建顺序
}
