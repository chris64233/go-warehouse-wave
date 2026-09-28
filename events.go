package gowarehousewave

import "time"

// 事件类型。每次分配变化都以不可变事件追加到日志。
const (
	evBatchRegistered  = "batch_registered"
	evWaveCreated      = "wave_created"
	evShortageReported = "shortage_reported"
	evShortageFailed   = "shortage_rearrange_failed"
	evPickConfirmed    = "pick_confirmed"
	evWaveCompleted    = "wave_completed"
	evWaveCancelled    = "wave_cancelled"
)

// BatchRegistered 登记一个库存批次（含现存数量与有效期/入库时间）。
type BatchRegistered struct {
	Batch Batch `json:"batch"`
}

// WaveCreated 波次成立：冻结订单行并产生 v1 全量分配。
type WaveCreated struct {
	WaveID     string        `json:"wave_id"`
	ExternalID string        `json:"external_id"`
	Lines      []OrderLine   `json:"lines"`
	Entries    []*AllocEntry `json:"entries"`
}

// ShortageReported 缺货重排成功：原行保留并记账 ShortQty，追加下一版分配。
type ShortageReported struct {
	WaveID     string        `json:"wave_id"`
	EntryID    string        `json:"entry_id"`
	ShortQty   int64         `json:"short_qty"`
	NewVersion int           `json:"new_version"`
	NewEntries []*AllocEntry `json:"new_entries"`
}

// ShortageFailed 重排尝试失败的审计记录：不改变任何库存与波次状态。
type ShortageFailed struct {
	WaveID   string `json:"wave_id"`
	EntryID  string `json:"entry_id"`
	ShortQty int64  `json:"short_qty"`
	Reason   string `json:"reason"`
}

// PickConfirmed 拣货确认：从指定批次确认拣出数量。
type PickConfirmed struct {
	WaveID  string `json:"wave_id"`
	EntryID string `json:"entry_id"`
	Qty     int64  `json:"qty"`
}

// WaveCompleted 波次全部完成（所有版本行均无剩余占用）。
type WaveCompleted struct {
	WaveID string `json:"wave_id"`
}

// WaveCancelled 波次取消；应用时把仍有剩余占用的行标记为释放。
type WaveCancelled struct {
	WaveID string `json:"wave_id"`
}

// Event 是事件日志中的一条不可变记录。
type Event struct {
	Seq       int64             `json:"seq"`
	Type      string            `json:"type"`
	At        time.Time         `json:"at"`
	Batch     *BatchRegistered  `json:"batch_registered,omitempty"`
	Created   *WaveCreated      `json:"wave_created,omitempty"`
	Shortage  *ShortageReported `json:"shortage_reported,omitempty"`
	ShortFail *ShortageFailed   `json:"shortage_rearrange_failed,omitempty"`
	Pick      *PickConfirmed    `json:"pick_confirmed,omitempty"`
	Completed *WaveCompleted    `json:"wave_completed,omitempty"`
	Cancelled *WaveCancelled    `json:"wave_cancelled,omitempty"`
}
