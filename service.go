package gowarehousewave

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// CreateWaveRequest 创建波次请求。WaveID 为外部波次号，承担幂等键。
type CreateWaveRequest struct {
	WaveID string
	Lines  []OrderLine
}

// ReportShortageRequest 缺货上报。Version 必须是拣货员回执所依据的当前分配版本。
type ReportShortageRequest struct {
	WaveID  string
	EntryID string
	// Version 回执依据的分配版本；与当前版本不一致返回 ErrStaleVersion。
	Version int
	// SKU 可选校验：非空时必须与明细 SKU 一致。
	SKU string
	// Qty 该批次上实际短缺的数量（相对未拣余量）。
	Qty int
}

// ConfirmPickRequest 拣货确认。Version 必须是当前分配版本，旧版本回执被拒绝。
type ConfirmPickRequest struct {
	WaveID  string
	EntryID string
	Version int
	SKU     string
	Qty     int
}

// plannedEntry 试算阶段产出的一条候选分配，不落任何状态。
type plannedEntry struct {
	orderID string
	sku     string
	batchID string
	qty     int
	reason  string
}

// Service 波次库存分配服务。所有方法均为并发安全：
// 库存与波次状态在同一把互斥锁内变更，多波次并发争用同一批次时
// 要么先到者完整占用、要么后到者整体失败，不会超卖或产生部分占用。
type Service struct {
	mu       sync.Mutex
	batches  map[string]*batch
	waves    map[string]*wave
	entrySeq int
	journal  *Journal
	now      func() time.Time
}

// NewService 创建纯内存服务（分配变化仍可通过 List/Get 查询，无落盘）。
func NewService() *Service {
	return &Service{
		batches: map[string]*batch{},
		waves:   map[string]*wave{},
		now:     time.Now,
	}
}

// RegisterBatch 登记一个库存批次。
func (s *Service) RegisterBatch(in BatchInput) error {
	if in.ID == "" || in.SKU == "" {
		return ErrInvalidRequest
	}
	if in.OnHand <= 0 {
		return ErrInvalidQty
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.batches[in.ID]; ok {
		return ErrBatchExists
	}
	ev := event{Type: evBatchRegistered, TS: s.now(), Batch: &in}
	if err := s.commit(ev); err != nil {
		return err
	}
	return nil
}

// CreateWave 冻结订单行集合并尝试整体取得库存。
//
// 幂等：同一 WaveID 携带相同订单集合重复提交时返回既有分配；
// 订单集合不同则返回 ErrWaveConflict。库存不足时返回 ErrInsufficientStock，
// 波次不成立、不产生任何中间占用。
func (s *Service) CreateWave(req CreateWaveRequest) (*WaveDetail, error) {
	lines, err := normalizeLines(req.Lines)
	if err != nil {
		return nil, err
	}
	if req.WaveID == "" {
		return nil, fmt.Errorf("%w: empty wave id", ErrInvalidRequest)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if w, ok := s.waves[req.WaveID]; ok {
		if !sameLineSet(w.lines, lines) {
			return nil, ErrWaveConflict
		}
		return s.waveDetail(w), nil
	}

	plan, err := s.plan(lines, "")
	if err != nil {
		return nil, err
	}

	now := s.now()
	newEntries := make([]eventEntry, 0, len(plan))
	ids := make([]string, 0, len(plan))
	for _, p := range plan {
		id := s.nextEntryID()
		ids = append(ids, id)
		newEntries = append(newEntries, eventEntry{
			ID: id, OrderID: p.orderID, SKU: p.sku,
			BatchID: p.batchID, Qty: p.qty, Reason: ReasonInitial,
		})
	}
	ev := event{
		Type: evWaveCreated, TS: now, WaveID: req.WaveID,
		Version: 1, Reason: ReasonInitial, Lines: lines,
		Entries: newEntries, VersionEntryIDs: ids,
	}
	if err := s.commit(ev); err != nil {
		return nil, err
	}
	return s.waveDetail(s.waves[req.WaveID]), nil
}

// ReportShortage 报告某条当前版本明细的实际短缺。
//
// 原分配记录完整保留，系统在新版本中：短缺数量核销该批次账面库存，
// 其余未拣明细原样结转，缺口按 FEFO 从仍可用库存重新分配。
// 重排失败（库存不足）时返回 ErrInsufficientStock，波次停留原状态，
// 已确认拣出的数量始终不动，不会重新分给别人。
func (s *Service) ReportShortage(req ReportShortageRequest) (*AllocationVersion, error) {
	if req.Qty <= 0 {
		return nil, ErrInvalidQty
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	w, e, err := s.locateActiveEntry(req.WaveID, req.EntryID, req.Version, req.SKU)
	if err != nil {
		return nil, err
	}
	if req.Qty > e.remaining() {
		return nil, fmt.Errorf("%w: shortage %d exceeds remaining %d", ErrInvalidQty, req.Qty, e.remaining())
	}

	// 缺口需求行（订单行、SKU 与短缺明细一致），排除短缺批次本身：
	// 该批次账面上只应有（未拣余量 - 短缺）件实物，已通过 carry 保留。
	gap := []OrderLine{{OrderID: e.orderID, SKU: e.sku, Qty: req.Qty}}
	plan, err := s.plan(gap, e.batchID)
	if err != nil {
		return nil, err
	}

	oldVersion := w.versions[w.currentVersion-1]
	oldIDs := append([]string(nil), oldVersion.entryIDs...)

	newEntries := make([]eventEntry, 0, len(oldIDs)+len(plan))
	newIDs := make([]string, 0, cap(newEntries))
	addNew := func(orderID, sku, batchID string, qty int, reason string) {
		id := s.nextEntryID()
		newIDs = append(newIDs, id)
		newEntries = append(newEntries, eventEntry{
			ID: id, OrderID: orderID, SKU: sku, BatchID: batchID,
			Qty: qty, Reason: reason,
		})
	}
	for _, id := range oldIDs {
		old := w.entries[id]
		if old.id == e.id {
			if carry := e.remaining() - req.Qty; carry > 0 {
				addNew(old.orderID, old.sku, old.batchID, carry, ReasonCarry)
			}
			for _, p := range plan {
				addNew(p.orderID, p.sku, p.batchID, p.qty, ReasonShortage)
			}
			continue
		}
		if r := old.remaining(); r > 0 {
			addNew(old.orderID, old.sku, old.batchID, r, ReasonCarry)
		}
	}

	now := s.now()
	ev := event{
		Type: evShortageReallocated, TS: now, WaveID: req.WaveID,
		Version:            w.currentVersion + 1,
		Reason:             ReasonShortage,
		ShortageEntryID:    e.id,
		ShortageQty:        req.Qty,
		SupersededEntryIDs: oldIDs,
		Entries:            newEntries,
		VersionEntryIDs:    newIDs,
	}
	if err := s.commit(ev); err != nil {
		return nil, err
	}
	return s.versionView(w, w.currentVersion), nil
}

// ConfirmPick 确认拣货。旧版本回执（版本号不匹配）返回 ErrStaleVersion，
// 不得覆盖新分配；确认数量从未拣余量中扣减并永久出库，取消也不会回收。
func (s *Service) ConfirmPick(req ConfirmPickRequest) (*AllocationEntry, error) {
	if req.Qty <= 0 {
		return nil, ErrInvalidQty
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	w, e, err := s.locateActiveEntry(req.WaveID, req.EntryID, req.Version, req.SKU)
	if err != nil {
		return nil, err
	}
	if req.Qty > e.remaining() {
		return nil, fmt.Errorf("%w: pick %d exceeds remaining %d", ErrInvalidQty, req.Qty, e.remaining())
	}

	ev := event{
		Type: evPickConfirmed, TS: s.now(), WaveID: req.WaveID,
		Version: w.currentVersion, EntryID: e.id, Qty: req.Qty,
	}
	if err := s.commit(ev); err != nil {
		return nil, err
	}
	view := w.entries[e.id].toView()
	return &view, nil
}

// CancelWave 取消波次：释放当前版本上尚未确认拣出的全部占用。
//
// 已确认拣出的数量保留出库；旧版本明细在重排时已释放过，不再处理，
// 因而每份库存至多释放一次。重复取消同一波次为幂等空操作。
func (s *Service) CancelWave(waveID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.waves[waveID]
	if !ok {
		return ErrWaveNotFound
	}
	if w.status == WaveStatusCancelled {
		return nil
	}
	if w.status == WaveStatusCompleted {
		return ErrWaveNotActive
	}
	ev := event{Type: evWaveCancelled, TS: s.now(), WaveID: waveID}
	return s.commit(ev)
}

// GetWave 查询波次全量分配明细（含每个历史版本）。
func (s *Service) GetWave(waveID string) (*WaveDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.waves[waveID]
	if !ok {
		return nil, ErrWaveNotFound
	}
	return s.waveDetail(w), nil
}

// GetBatch 查询单个库存批次的账面视图。
func (s *Service) GetBatch(batchID string) (*Batch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.batches[batchID]
	if !ok {
		return nil, ErrBatchNotFound
	}
	v := b.toView()
	return &v, nil
}

// ListBatches 按批次号排序列出全部批次。
func (s *Service) ListBatches() []Batch {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.batches))
	for id := range s.batches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Batch, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.batches[id].toView())
	}
	return out
}

// ---- 内部逻辑（调用方须持锁）----

// locateActiveEntry 定位“当前版本上仍有效”的明细，统一处理版本竞争。
func (s *Service) locateActiveEntry(waveID, entryID string, version int, sku string) (*wave, *entry, error) {
	w, ok := s.waves[waveID]
	if !ok {
		return nil, nil, ErrWaveNotFound
	}
	if w.status != WaveStatusPending && w.status != WaveStatusPicking {
		return nil, nil, ErrWaveNotActive
	}
	e, ok := w.entries[entryID]
	if !ok {
		return nil, nil, ErrEntryNotFound
	}
	if e.version != w.currentVersion || version != w.currentVersion {
		return nil, nil, fmt.Errorf("%w: receipt version %d, current version %d", ErrStaleVersion, version, w.currentVersion)
	}
	if e.status != EntryActive {
		return nil, nil, ErrEntryNotActive
	}
	if sku != "" && sku != e.sku {
		return nil, nil, ErrSKUMismatch
	}
	return w, e, nil
}

// plan 按冻结后的行顺序逐行分配，同 SKU 批次按 FEFO
// （有效期较早优先，其次入库时间较早，再按批次号兜底）。
// 纯试算：失败时不改动任何库存与波次状态。
func (s *Service) plan(lines []OrderLine, excludeBatch string) ([]plannedEntry, error) {
	scratch := map[string]int{} // 批次试算剩余量，惰性初始化为 available()
	avail := func(b *batch) int {
		if v, ok := scratch[b.id]; ok {
			return v
		}
		return b.available()
	}
	candidates := func(sku string) []*batch {
		var cs []*batch
		for _, b := range s.batches {
			if b.sku != sku || b.id == excludeBatch || avail(b) <= 0 {
				continue
			}
			cs = append(cs, b)
		}
		sort.Slice(cs, func(i, j int) bool {
			a, c := cs[i], cs[j]
			if !a.expiry.Equal(c.expiry) {
				return a.expiry.Before(c.expiry)
			}
			if !a.receivedAt.Equal(c.receivedAt) {
				return a.receivedAt.Before(c.receivedAt)
			}
			return a.id < c.id
		})
		return cs
	}

	var plan []plannedEntry
	for _, ln := range lines {
		need := ln.Qty
		for _, b := range candidates(ln.SKU) {
			if need == 0 {
				break
			}
			take := need
			if a := avail(b); take > a {
				take = a
			}
			scratch[b.id] = avail(b) - take
			plan = append(plan, plannedEntry{
				orderID: ln.OrderID, sku: ln.SKU, batchID: b.id,
				qty: take, reason: ReasonInitial,
			})
			need -= take
		}
		if need > 0 {
			return nil, fmt.Errorf("%w: sku %s short %d", ErrInsufficientStock, ln.SKU, need)
		}
	}
	return plan, nil
}

func (s *Service) nextEntryID() string {
	s.entrySeq++
	return fmt.Sprintf("E%06d", s.entrySeq)
}

func (s *Service) waveDetail(w *wave) *WaveDetail {
	d := &WaveDetail{
		WaveID:         w.id,
		Status:         w.status,
		CurrentVersion: w.currentVersion,
		Lines:          append([]OrderLine(nil), w.lines...),
		Entries:        s.versionEntries(w, w.currentVersion),
	}
	d.Versions = make([]AllocationVersion, 0, len(w.versions))
	for i := range w.versions {
		d.Versions = append(d.Versions, *s.versionView(w, i+1))
	}
	return d
}

func (s *Service) versionEntries(w *wave, v int) []AllocationEntry {
	ver := w.versions[v-1]
	out := make([]AllocationEntry, 0, len(ver.entryIDs))
	for _, id := range ver.entryIDs {
		out = append(out, w.entries[id].toView())
	}
	return out
}

func (s *Service) versionView(w *wave, v int) *AllocationVersion {
	ver := w.versions[v-1]
	return &AllocationVersion{
		WaveID:    w.id,
		Version:   ver.version,
		Reason:    ver.reason,
		CreatedAt: ver.createdAt,
		Entries:   s.versionEntries(w, v),
	}
}

// normalizeLines 校验订单行，并按 (OrderID, SKU) 排序冻结；同键合并数量。
func normalizeLines(lines []OrderLine) ([]OrderLine, error) {
	if len(lines) == 0 {
		return nil, fmt.Errorf("%w: empty order lines", ErrInvalidRequest)
	}
	idx := map[string]int{}
	out := make([]OrderLine, 0, len(lines))
	for _, ln := range lines {
		if ln.OrderID == "" || ln.SKU == "" {
			return nil, fmt.Errorf("%w: order id and sku are required", ErrInvalidRequest)
		}
		if ln.Qty <= 0 {
			return nil, fmt.Errorf("%w: non-positive qty for %s/%s", ErrInvalidQty, ln.OrderID, ln.SKU)
		}
		key := ln.OrderID + "\x00" + ln.SKU
		if i, ok := idx[key]; ok {
			out[i].Qty += ln.Qty
			continue
		}
		idx[key] = len(out)
		out = append(out, OrderLine{OrderID: ln.OrderID, SKU: ln.SKU, Qty: ln.Qty})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OrderID != out[j].OrderID {
			return out[i].OrderID < out[j].OrderID
		}
		return out[i].SKU < out[j].SKU
	})
	return out, nil
}

// sameLineSet 比较冻结后的订单集合（数量必须一致）。
func sameLineSet(a, b []OrderLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
