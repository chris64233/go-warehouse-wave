package gowarehousewave

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// RegisterBatchInput 登记库存批次入参。
type RegisterBatchInput struct {
	BatchID    string
	SKU        string
	OnHand     int64
	ExpiresAt  time.Time
	ReceivedAt time.Time
}

// CreateWaveInput 创建波次入参。ExternalID 为外部波次号，承担幂等键。
type CreateWaveInput struct {
	ExternalID string
	Lines      []OrderLine
}

// ReportShortageInput 缺货报告入参。Version 为拣货员持有的分配版本。
type ReportShortageInput struct {
	WaveID   string
	EntryID  string
	Version  int
	ShortQty int64
}

// ConfirmPickInput 拣货确认入参。Version 为该回执基于的分配版本。
type ConfirmPickInput struct {
	WaveID  string
	EntryID string
	Version int
	Qty     int64
}

// Service 是波次库存分配领域服务。所有方法可被多 goroutine 并发调用；
// 内部用单一互斥锁串行化状态变更，配合事件日志保证并发争用下不超卖。
type Service struct {
	mu      sync.RWMutex
	journal Journal
	now     func() time.Time
	newID   func() string

	batches     map[string]*Batch
	waves       map[string]*Wave  // 内部 ID → 波次
	externalIdx map[string]string // 外部波次号 → 内部 ID
	entryToWave map[string]string // 分配行 ID → 波次 ID
}

// NewService 构造服务。若 journalPath 为空则使用纯内存日志（主要用于测试），
// 否则使用 append-only 文件日志并先回放已有事件重建状态。
func NewService(journalPath string) (*Service, error) {
	var j Journal
	if journalPath == "" {
		j = newMemoryJournal()
	} else {
		fj, err := openFileJournal(journalPath)
		if err != nil {
			return nil, err
		}
		j = fj
	}
	s := &Service{
		journal:     j,
		now:         time.Now,
		newID:       randomID,
		batches:     map[string]*Batch{},
		waves:       map[string]*Wave{},
		externalIdx: map[string]string{},
		entryToWave: map[string]string{},
	}
	if err := s.replay(); err != nil {
		j.Close()
		return nil, err
	}
	return s, nil
}

// Close 刷新并关闭底层日志。
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journal.Close()
}

// ---------- 库存批次登记 ----------

// RegisterBatch 登记或更新一个库存批次的现存数量与时效信息。
// 重新登记时现存数量不得低于该批次已被承诺的数量，否则会破坏不超卖不变量。
func (s *Service) RegisterBatch(in RegisterBatchInput) (*Batch, error) {
	if in.BatchID == "" || in.SKU == "" || in.OnHand < 0 {
		return nil, fmt.Errorf("%w: batch id/sku required and on-hand must not be negative", ErrInvalidArgument)
	}
	b := &Batch{
		ID:         in.BatchID,
		SKU:        in.SKU,
		OnHand:     in.OnHand,
		ExpiresAt:  in.ExpiresAt.UTC(),
		ReceivedAt: in.ReceivedAt.UTC(),
	}
	s.mu.Lock()
	if old, ok := s.batches[b.ID]; ok && old.SKU != b.SKU {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: cannot change sku of existing batch %s", ErrInvalidArgument, b.ID)
	}
	if committed := s.holdsForLocked(b.ID); in.OnHand < committed {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: on-hand %d below committed %d for batch %s", ErrInvalidArgument, in.OnHand, committed, b.ID)
	}
	if err := s.commitLocked(evBatchRegistered, &BatchRegistered{Batch: *b}); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	created := cloneBatch(s.batches[b.ID])
	s.mu.Unlock()
	return created, nil
}

// ---------- 波次创建 ----------

// CreateWave 冻结订单行集合并按 FEFO 全量分配库存。
// 外部波次号幂等：同号重复提交相同订单集合返回原波次；内容变化返回 ErrConflict。
// 库存不足时返回 ErrInsufficientStock，波次不成立，不产生任何占用。
func (s *Service) CreateWave(in CreateWaveInput) (*Wave, error) {
	if in.ExternalID == "" || len(in.Lines) == 0 {
		return nil, fmt.Errorf("%w: external id and at least one line required", ErrInvalidArgument)
	}
	frozen, err := normalizeLines(in.Lines, s.newID)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.externalIdx[in.ExternalID]; ok {
		existing := s.waves[id]
		if !sameLineSet(existing.Lines, frozen) {
			return nil, ErrConflict
		}
		return cloneWave(existing), nil
	}

	entries, err := allocate(frozen, s.batches, s.globalHoldsLocked(), 1, s.newID)
	if err != nil {
		return nil, err
	}

	wc := &WaveCreated{WaveID: s.newID(), ExternalID: in.ExternalID, Lines: frozen, Entries: entries}
	if err := s.commitLocked(evWaveCreated, wc); err != nil {
		return nil, err
	}
	return cloneWave(s.waves[wc.WaveID]), nil
}

// ---------- 缺货报告与重排 ----------

// ReportShortage 报告某条当前版本分配行实际短缺 shortQty。
// 原分配记录完整保留：仅对原行记账 ShortQty，再从仍可用库存中按 FEFO
// 生成下一版分配。重排失败返回错误，波次保持待处理、原占用不变。
func (s *Service) ReportShortage(in ReportShortageInput) (*Wave, error) {
	if in.ShortQty <= 0 {
		return nil, fmt.Errorf("%w: short qty must be positive", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	w, e, err := s.resolveEntryLocked(in.WaveID, in.EntryID)
	if err != nil {
		return nil, err
	}
	if in.Version != w.CurrentVersion {
		return nil, fmt.Errorf("%w: receipt version %d is older than current version %d", ErrVersionConflict, in.Version, w.CurrentVersion)
	}
	if in.ShortQty > e.Remaining() {
		return nil, fmt.Errorf("%w: short qty %d exceeds remaining %d", ErrInvalidArgument, in.ShortQty, e.Remaining())
	}

	// 试算下一版：原行的缺口被视为该批次的新损坏量，且不允许再从该批次取。
	holds := s.rearrangeViewLocked(e.BatchID)
	gap := []OrderLine{{LineID: e.LineID, OrderID: e.OrderID, SKU: e.SKU, Qty: in.ShortQty}}
	newEntries, err := allocate(gap, s.batches, holds, w.CurrentVersion+1, s.newID)
	if err != nil {
		// 重排失败：记录审计事件，但不改变任何库存与分配状态。
		_ = s.commitLocked(evShortageFailed, &ShortageFailed{
			WaveID: w.ID, EntryID: e.ID, ShortQty: in.ShortQty, Reason: err.Error(),
		})
		return nil, err
	}
	for _, ne := range newEntries {
		ne.ReplacesLine = e.ID
		ne.SourceBatch = e.BatchID
	}

	sr := &ShortageReported{
		WaveID: w.ID, EntryID: e.ID, ShortQty: in.ShortQty,
		NewVersion: w.CurrentVersion + 1, NewEntries: newEntries,
	}
	// 损坏量由 shortage 事件的投影统一记账（实时提交与日志回放一致）。
	if err := s.commitLocked(evShortageReported, sr); err != nil {
		return nil, err
	}
	return cloneWave(s.waves[w.ID]), nil
}

// ---------- 拣货确认 ----------

// ConfirmPick 确认从指定批次实际拣出 qty。回执必须基于当前分配版本，
// 旧版本回执一律拒绝；确认数量不得超过该行剩余占用。
func (s *Service) ConfirmPick(in ConfirmPickInput) (*Wave, error) {
	if in.Qty <= 0 {
		return nil, fmt.Errorf("%w: pick qty must be positive", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	w, e, err := s.resolveEntryLocked(in.WaveID, in.EntryID)
	if err != nil {
		return nil, err
	}
	if in.Version != w.CurrentVersion {
		return nil, fmt.Errorf("%w: receipt version %d is older than current version %d", ErrVersionConflict, in.Version, w.CurrentVersion)
	}
	if in.Qty > e.Remaining() {
		return nil, fmt.Errorf("%w: pick qty %d exceeds remaining %d", ErrInvalidArgument, in.Qty, e.Remaining())
	}

	if err := s.commitLocked(evPickConfirmed, &PickConfirmed{
		WaveID: w.ID, EntryID: e.ID, Qty: in.Qty,
	}); err != nil {
		return nil, err
	}
	// 全部版本行均无剩余占用则波次完成。
	if w.Status == StatusPending && totalRemaining(w) == 0 {
		if err := s.commitLocked(evWaveCompleted, &WaveCompleted{WaveID: w.ID}); err != nil {
			return nil, err
		}
	}
	return cloneWave(w), nil
}

// ---------- 取消 ----------

// CancelWave 取消波次：只释放尚未确认（也未报缺）的占用，每条只释放一次，
// 已经确认拣出与已报缺损坏的数量绝不释放。重复取消返回 ErrWaveClosed。
func (s *Service) CancelWave(waveID string) (*Wave, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, err := s.lookupWaveLocked(waveID)
	if err != nil {
		return nil, err
	}
	if w.Status != StatusPending {
		return nil, fmt.Errorf("%w: wave %s", ErrWaveClosed, w.Status)
	}
	if err := s.commitLocked(evWaveCancelled, &WaveCancelled{WaveID: w.ID}); err != nil {
		return nil, err
	}
	return cloneWave(s.waves[w.ID]), nil
}

// ---------- 查询 ----------

// GetWave 按内部 ID 或外部波次号查询波次及其全部版本分配明细。
func (s *Service) GetWave(id string) (*Wave, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lookupWaveLocked(id)
}

// ListAllocations 返回波次当前仍待拣货的分配明细。部分报缺后，原行的剩余
// 与下一版补缺行同属有效分配，都会返回（行上的 Version 字段用于区分）。
func (s *Service) ListAllocations(waveID string) ([]*AllocEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, err := s.lookupWaveLocked(waveID)
	if err != nil {
		return nil, err
	}
	out := make([]*AllocEntry, 0, len(w.Entries))
	for _, e := range w.Entries {
		if e.Remaining() > 0 {
			out = append(out, cloneEntry(e))
		}
	}
	return out, nil
}

// AllocationHistory 返回波次所有版本的全部分配明细（含已报缺/已完成的历史行）。
func (s *Service) AllocationHistory(waveID string) ([]*AllocEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, err := s.lookupWaveLocked(waveID)
	if err != nil {
		return nil, err
	}
	out := make([]*AllocEntry, len(w.Entries))
	for i, e := range w.Entries {
		out[i] = cloneEntry(e)
	}
	return out, nil
}

// GetBatch 查询批次及其当前可用量（现存登记量 − 已承诺量）。
func (s *Service) GetBatch(batchID string) (*Batch, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, 0, fmt.Errorf("%w: batch %s", ErrNotFound, batchID)
	}
	avail := b.OnHand - s.holdsForLocked(batchID)
	return cloneBatch(b), avail, nil
}

// ---------- 内部状态操作 ----------

func (s *Service) lookupWaveLocked(id string) (*Wave, error) {
	if wid, ok := s.externalIdx[id]; ok {
		id = wid
	}
	w, ok := s.waves[id]
	if !ok {
		return nil, fmt.Errorf("%w: wave %s", ErrNotFound, id)
	}
	return cloneWave(w), nil
}

func (s *Service) resolveEntryLocked(waveID, entryID string) (*Wave, *AllocEntry, error) {
	if wid, ok := s.externalIdx[waveID]; ok {
		waveID = wid
	}
	w, ok := s.waves[waveID]
	if !ok {
		return nil, nil, fmt.Errorf("%w: wave %s", ErrNotFound, waveID)
	}
	if w.Status == StatusCancelled {
		return nil, nil, fmt.Errorf("%w: wave cancelled", ErrWaveClosed)
	}
	if owner, ok := s.entryToWave[entryID]; !ok || owner != w.ID {
		return nil, nil, fmt.Errorf("%w: entry %s", ErrNotFound, entryID)
	}
	for _, e := range w.Entries {
		if e.ID == entryID {
			if e.Remaining() <= 0 {
				return nil, nil, fmt.Errorf("%w: entry %s", ErrEntryClosed, entryID)
			}
			return w, e, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: entry %s", ErrNotFound, entryID)
}

// globalHoldsLocked 汇总每个批次已被承诺、不能再分配的数量。
// 对任何未释放的分配行，其原始 Qty 全部计入：已确认部分是已出库的实物，
// 已报缺部分是确认损坏，其余是在持占用；取消波次中仅 Released 行归还。
func (s *Service) globalHoldsLocked() map[string]int64 {
	return s.committedByBatchLocked()
}

// committedByBatchLocked 返回各批次已承诺数量。
func (s *Service) committedByBatchLocked() map[string]int64 {
	committed := map[string]int64{}
	for _, w := range s.waves {
		for _, e := range w.Entries {
			committed[e.BatchID] += committedQty(e)
		}
	}
	return committed
}

// committedQty 是一条分配行已不可再分配的数量：未释放行整笔 Qty 都已承诺；
// 已释放行只保留其已确认拣出与已报缺损坏的部分（释放的剩余已归还库存）。
func committedQty(e *AllocEntry) int64 {
	if e.Released {
		return e.ConfirmedQty + e.ShortQty
	}
	return e.Qty
}

// holdsForLocked 返回单个批次的已承诺数量（查询路径）。
func (s *Service) holdsForLocked(batchID string) int64 {
	var n int64
	for _, w := range s.waves {
		for _, e := range w.Entries {
			if e.BatchID == batchID {
				n += committedQty(e)
			}
		}
	}
	return n
}

// rearrangeViewLocked 构造缺货重排时的库存视图：缺口来源批次整体排除
// （缺口确认损坏，且不允许用同批次回填），其余批次照常扣除已承诺量。
func (s *Service) rearrangeViewLocked(sourceBatch string) map[string]int64 {
	holds := s.committedByBatchLocked()
	if b, ok := s.batches[sourceBatch]; ok {
		holds[sourceBatch] = b.OnHand // 令该批次可用量为 0
	}
	return holds
}

// commitLocked 先持久化事件再应用状态：事件落盘失败则状态不变。
func (s *Service) commitLocked(typ string, payload any) error {
	ev := &Event{Type: typ, At: s.now().UTC()}
	switch typ {
	case evBatchRegistered:
		ev.Batch = payload.(*BatchRegistered)
	case evWaveCreated:
		ev.Created = payload.(*WaveCreated)
	case evShortageReported:
		ev.Shortage = payload.(*ShortageReported)
	case evShortageFailed:
		ev.ShortFail = payload.(*ShortageFailed)
	case evPickConfirmed:
		ev.Pick = payload.(*PickConfirmed)
	case evWaveCompleted:
		ev.Completed = payload.(*WaveCompleted)
	case evWaveCancelled:
		ev.Cancelled = payload.(*WaveCancelled)
	default:
		return fmt.Errorf("unknown event type %q", typ)
	}
	if err := s.journal.Append(ev); err != nil {
		return err
	}
	s.apply(ev)
	return nil
}

// apply 把单个事件作用于内存状态。replay 与实时提交共用同一套投影逻辑。
func (s *Service) apply(ev *Event) {
	switch ev.Type {
	case evBatchRegistered:
		b := ev.Batch.Batch
		s.batches[b.ID] = &b
	case evWaveCreated:
		c := ev.Created
		now := ev.At
		w := &Wave{
			ID: c.WaveID, ExternalID: c.ExternalID, Status: StatusPending,
			Lines: cloneLines(c.Lines), Entries: []*AllocEntry{},
			CurrentVersion: 1, CreatedAt: now, UpdatedAt: now,
		}
		for _, e := range c.Entries {
			cp := cloneEntry(e)
			w.Entries = append(w.Entries, cp)
			s.entryToWave[cp.ID] = w.ID
		}
		s.waves[w.ID] = w
		s.externalIdx[w.ExternalID] = w.ID
	case evShortageReported:
		r := ev.Shortage
		w := s.waves[r.WaveID]
		for _, e := range w.Entries {
			if e.ID == r.EntryID {
				e.ShortQty += r.ShortQty
			}
		}
		for _, ne := range r.NewEntries {
			cp := cloneEntry(ne)
			w.Entries = append(w.Entries, cp)
			s.entryToWave[cp.ID] = w.ID
		}
		w.CurrentVersion = r.NewVersion
		w.UpdatedAt = ev.At
	case evShortageFailed:
		// 纯审计事件：无状态变化。
	case evPickConfirmed:
		p := ev.Pick
		w := s.waves[p.WaveID]
		for _, e := range w.Entries {
			if e.ID == p.EntryID {
				e.ConfirmedQty += p.Qty
			}
		}
		w.UpdatedAt = ev.At
	case evWaveCompleted:
		if w := s.waves[ev.Completed.WaveID]; w != nil {
			w.Status = StatusCompleted
			w.UpdatedAt = ev.At
		}
	case evWaveCancelled:
		w := s.waves[ev.Cancelled.WaveID]
		w.Status = StatusCancelled
		for _, e := range w.Entries {
			if e.Remaining() > 0 {
				e.Released = true // 幂等标记：每份剩余占用只释放一次
			}
		}
		w.UpdatedAt = ev.At
	}
}

// replay 回放日志重建全部状态，然后做一次状态自愈：若进程在
// pick_confirmed 与 wave_completed 两条事件之间崩溃，恢复后依据明细
// 把已无剩余占用的待处理波次补记为完成。
func (s *Service) replay() error {
	if err := s.journal.Replay(func(ev *Event) {
		s.apply(ev)
	}); err != nil {
		return err
	}
	for _, w := range s.waves {
		if w.Status == StatusPending && totalRemaining(w) == 0 {
			w.Status = StatusCompleted
		}
	}
	return nil
}

// ---------- 辅助函数 ----------

func totalRemaining(w *Wave) int64 {
	var n int64
	for _, e := range w.Entries {
		n += e.Remaining()
	}
	return n
}

// normalizeLines 校验并冻结订单行：补全系统行号、按 (OrderID,LineID,SKU) 排序。
func normalizeLines(lines []OrderLine, newID func() string) ([]OrderLine, error) {
	out := make([]OrderLine, len(lines))
	ids := map[string]bool{}
	for i, l := range lines {
		if l.SKU == "" || l.Qty <= 0 {
			return nil, fmt.Errorf("%w: line %d sku required and qty must be positive", ErrInvalidArgument, i)
		}
		l2 := l
		if l2.LineID == "" {
			l2.LineID = "auto-" + newID()
		}
		key := l2.OrderID + "\x00" + l2.LineID
		if ids[key] {
			return nil, fmt.Errorf("%w: duplicated order line %s/%s", ErrInvalidArgument, l2.OrderID, l2.LineID)
		}
		ids[key] = true
		out[i] = l2
	}
	sortLines(out)
	return out, nil
}

// sameLineSet 比较两次提交的订单集合是否一致（忽略顺序）。
func sameLineSet(a, b []OrderLine) bool {
	if len(a) != len(b) {
		return false
	}
	// 两边都已按 normalizeLines 排序，逐字段比较即可。
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneBatch(b *Batch) *Batch {
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

func cloneEntry(e *AllocEntry) *AllocEntry {
	cp := *e
	return &cp
}

func cloneLines(l []OrderLine) []OrderLine {
	out := make([]OrderLine, len(l))
	copy(out, l)
	return out
}

func cloneWave(w *Wave) *Wave {
	cp := *w
	cp.Lines = cloneLines(w.Lines)
	cp.Entries = make([]*AllocEntry, len(w.Entries))
	for i, e := range w.Entries {
		cp.Entries[i] = cloneEntry(e)
	}
	return &cp
}

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败在实际系统中不可恢复；退化为时间戳兜底避免空 ID。
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
