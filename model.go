package gowarehousewave

import "time"

// 波次生命周期状态。
const (
	StatusPending   = "pending"   // 待处理：尚有未完成的分配占用
	StatusCompleted = "completed" // 全部数量均已确认拣出
	StatusCancelled = "cancelled" // 波次取消，未确认占用已释放
)

// 单条分配行的状态。
const (
	EntryActive    = "active"    // 仍有占用待拣
	EntryConfirmed = "confirmed" // 全部数量已确认拣出（其中可能含报缺后的新批次确认）
	EntryShort     = "short"     // 该行曾报缺且缺口已重排，原行剩余为零（历史保留）
	EntryReleased  = "released"  // 波次取消时剩余占用被释放
)

// Batch 是一个库存批次。OnHand 为实物现存数量的登记值；
// 各波次已承诺（在持/已确认/已报缺）的数量由分配明细汇总，不直接扣减本字段。
type Batch struct {
	ID         string
	SKU        string
	OnHand     int64
	ExpiresAt  time.Time // 批次有效期，越早越优先
	ReceivedAt time.Time // 入库时间，越早越优先
}

// OrderLine 是波次冻结的订单行集合中的一行。
type OrderLine struct {
	LineID  string // 外部订单行号，可为空；为空时由系统生成
	OrderID string
	SKU     string
	Qty     int64
}

// AllocEntry 是一个版本内、单个订单行对单个库存批次的一条占用明细。
// 报缺重排不删除也不修改旧行，只追加新版本行，因此每个分配变化都可追溯。
type AllocEntry struct {
	ID           string
	Version      int
	LineID       string
	OrderID      string
	SKU          string
	BatchID      string
	Qty          int64 // 本批次数的占用数量（原始分配量，永不改变）
	ConfirmedQty int64 // 已确认拣出数量
	ShortQty     int64 // 已报告的实物短缺数量
	Released     bool  // 取消波次时剩余占用是否已释放

	// SourceOfShort/ReplacesLine 标记：本行是为了弥补 ReplacesLine 上
	// SourceBatch 批次的 ShortQty 缺口而生成的重排分配。
	ReplacesLine string
	SourceBatch  string
}

// Remaining 返回尚未确认、未报缺、未释放的占用数量。
func (e *AllocEntry) Remaining() int64 {
	if e.Released {
		return 0
	}
	return e.Qty - e.ConfirmedQty - e.ShortQty
}

// Status 汇总当前行状态。
func (e *AllocEntry) Status() string {
	switch {
	case e.Released:
		return EntryReleased
	case e.ConfirmedQty+e.ShortQty >= e.Qty:
		if e.ShortQty > 0 && e.ConfirmedQty == 0 {
			return EntryShort
		}
		return EntryConfirmed
	default:
		return EntryActive
	}
}

// Wave 是一个拣货波次及其全部版本的分配明细。
type Wave struct {
	ID             string
	ExternalID     string
	Status         string
	Lines          []OrderLine   // 创建时冻结的订单行集合
	Entries        []*AllocEntry // 按版本顺序追加的全部分配明细
	CurrentVersion int           // 当前分配版本：创建即 v1，每次成功重排 +1
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// ActiveHolds 返回仍占用库存的数量（未确认、未报缺、未释放）。
func (w *Wave) ActiveHolds() map[string]int64 {
	holds := map[string]int64{}
	for _, e := range w.Entries {
		if r := e.Remaining(); r > 0 {
			holds[e.BatchID] += r
		}
	}
	return holds
}
