package gowarehousewave

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// 每次分配变化落盘一条 event（JSON Lines，追加写）。
const (
	evBatchRegistered     = "batch_registered"
	evWaveCreated         = "wave_created"
	evShortageReallocated = "shortage_reallocated"
	evPickConfirmed       = "pick_confirmed"
	evWaveCancelled       = "wave_cancelled"
)

// eventEntry 事件内嵌的分配明细载荷。
type eventEntry struct {
	ID      string `json:"id"`
	OrderID string `json:"order_id"`
	SKU     string `json:"sku"`
	BatchID string `json:"batch_id"`
	Qty     int    `json:"qty"`
	Reason  string `json:"reason"`
}

// event 分配变化事件。字段按事件类型取用，未用字段在 JSON 中省略。
type event struct {
	Type    string      `json:"type"`
	TS      time.Time   `json:"ts"`
	Batch   *BatchInput `json:"batch,omitempty"`
	WaveID  string      `json:"wave_id,omitempty"`
	Version int         `json:"version,omitempty"`
	Reason  string      `json:"reason,omitempty"`

	// wave_created
	Lines []OrderLine `json:"lines,omitempty"`

	// wave_created / shortage_reallocated：新版本明细与其内部顺序
	Entries         []eventEntry `json:"entries,omitempty"`
	VersionEntryIDs []string     `json:"version_entry_ids,omitempty"`

	// shortage_reallocated
	ShortageEntryID    string   `json:"shortage_entry_id,omitempty"`
	ShortageQty        int      `json:"shortage_qty,omitempty"`
	SupersededEntryIDs []string `json:"superseded_entry_ids,omitempty"`

	// pick_confirmed
	EntryID string `json:"entry_id,omitempty"`
	Qty     int    `json:"qty,omitempty"`
}

// Journal 追加式分配变化日志。每次 commit 同步落盘（Create + Write + Sync），
// 进程重启后通过 Open 重放完整恢复内存状态。
type Journal struct {
	f *os.File
}

// OpenService 打开（必要时创建）path 对应的 JSONL 日志，
// 重放历史事件后返回带持久化能力的服务。
func OpenService(path string) (*Service, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create journal dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}

	s := NewService()
	s.journal = &Journal{f: f}
	if err := s.replay(f); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return nil, fmt.Errorf("seek journal: %w", err)
	}
	return s, nil
}

// Close 关闭底层日志文件。
func (s *Service) Close() error {
	if s.journal == nil {
		return nil
	}
	err := s.journal.f.Close()
	s.journal = nil
	return err
}

// replay 顺序读取并应用日志中的全部事件。
func (s *Service) replay(r io.Reader) error {
	sc := bufio.NewScanner(r)
	// 单事件可能较大，放开 Scanner 的 token 上限。
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			return fmt.Errorf("journal replay decode: %w", err)
		}
		if err := s.apply(ev); err != nil {
			return fmt.Errorf("journal replay apply %q: %w", ev.Type, err)
		}
	}
	return sc.Err()
}

// commit 先持久化事件、再更新内存状态。落盘失败则内存不动，
// 保证“已返回成功的分配变化”必然可在重启后恢复。
func (s *Service) commit(ev event) error {
	if s.journal != nil {
		data, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		if _, err := s.journal.f.Write(data); err != nil {
			return fmt.Errorf("write journal: %w", err)
		}
		if err := s.journal.f.Sync(); err != nil {
			return fmt.Errorf("sync journal: %w", err)
		}
	}
	return s.apply(ev)
}

// apply 将一条事件推进内存状态。建波/重排事件里的批次占用调整
// 由试算保证成立，这里直接记账，不再做库存不足判断。
func (s *Service) apply(ev event) error {
	switch ev.Type {
	case evBatchRegistered:
		b := ev.Batch
		if _, ok := s.batches[b.ID]; ok {
			return ErrBatchExists
		}
		s.batches[b.ID] = &batch{
			id: b.ID, sku: b.SKU, onHand: b.OnHand,
			expiry: b.Expiry, receivedAt: b.ReceivedAt,
		}

	case evWaveCreated:
		if _, ok := s.waves[ev.WaveID]; ok {
			return ErrWaveConflict
		}
		w := &wave{
			id:             ev.WaveID,
			status:         WaveStatusPending,
			currentVersion: 1,
			lines:          append([]OrderLine(nil), ev.Lines...),
			entries:        map[string]*entry{},
			createdAt:      ev.TS,
		}
		for _, ee := range ev.Entries {
			e := newEventEntry(ev.WaveID, 1, ee)
			w.entries[e.id] = e
			s.batches[e.batchID].held += e.qty
		}
		w.versions = []*allocVersion{{
			version: 1, reason: ev.Reason, createdAt: ev.TS,
			entryIDs: append([]string(nil), ev.VersionEntryIDs...),
		}}
		s.waves[w.id] = w
		s.advanceEntrySeq()

	case evShortageReallocated:
		w, ok := s.waves[ev.WaveID]
		if !ok {
			return ErrWaveNotFound
		}
		nv := ev.Version
		// 旧版本全部有效明细失去占用权。
		for _, id := range ev.SupersededEntryIDs {
			old := w.entries[id]
			if old.status != EntryActive {
				return fmt.Errorf("replay: superseded entry %s in status %s", id, old.status)
			}
			old.status = EntrySuperseded
			s.batches[old.batchID].held -= old.remaining()
		}
		// 短缺批次核销实物（盘亏，不回到 available）。
		short := w.entries[ev.ShortageEntryID]
		s.batches[short.batchID].onHand -= ev.ShortageQty
		// 新版本（结转 + 补位）重新占用。
		for _, ee := range ev.Entries {
			e := newEventEntry(ev.WaveID, nv, ee)
			w.entries[e.id] = e
			s.batches[e.batchID].held += e.qty
		}
		w.currentVersion = nv
		w.versions = append(w.versions, &allocVersion{
			version: nv, reason: ev.Reason, createdAt: ev.TS,
			entryIDs: append([]string(nil), ev.VersionEntryIDs...),
		})
		s.advanceEntrySeq()

	case evPickConfirmed:
		w := s.waves[ev.WaveID]
		e := w.entries[ev.EntryID]
		if ev.Qty > e.remaining() {
			return fmt.Errorf("replay: pick %d exceeds remaining of %s", ev.Qty, e.id)
		}
		b := s.batches[e.batchID]
		e.picked += ev.Qty
		b.held -= ev.Qty
		b.picked += ev.Qty
		if e.remaining() == 0 {
			e.status = EntryCompleted
		}
		w.status = WaveStatusPicking
		if s.waveFullyPicked(w) {
			w.status = WaveStatusCompleted
		}

	case evWaveCancelled:
		w := s.waves[ev.WaveID]
		cur := w.versions[w.currentVersion-1]
		for _, id := range cur.entryIDs {
			e := w.entries[id]
			if e.status != EntryActive {
				continue
			}
			if r := e.remaining(); r > 0 {
				s.batches[e.batchID].held -= r
			}
			e.status = EntryCancelled
		}
		w.status = WaveStatusCancelled

	default:
		return fmt.Errorf("unknown event type %q", ev.Type)
	}
	return nil
}

func newEventEntry(waveID string, version int, ee eventEntry) *entry {
	return &entry{
		id: ee.ID, waveID: waveID, version: version,
		orderID: ee.OrderID, sku: ee.SKU, batchID: ee.BatchID,
		qty: ee.Qty, reason: ee.Reason, status: EntryActive,
	}
}

// waveFullyPicked 全部订单行需求数量均已跨版本拣出。
func (s *Service) waveFullyPicked(w *wave) bool {
	picked := map[string]int{} // orderID\x00sku -> 累计拣出
	for _, e := range w.entries {
		if e.picked > 0 {
			picked[e.orderID+"\x00"+e.sku] += e.picked
		}
	}
	for _, ln := range w.lines {
		if picked[ln.OrderID+"\x00"+ln.SKU] < ln.Qty {
			return false
		}
	}
	return true
}

// advanceEntrySeq 重放时让内存自增序号不小于日志中出现过的最大 entry 编号，
// 保证重放后新建明细的 ID 不与历史冲突。
func (s *Service) advanceEntrySeq() {
	for id := range s.allEntryIDs() {
		var n int
		for _, ch := range id {
			if ch >= '0' && ch <= '9' {
				n = n*10 + int(ch-'0')
			}
		}
		if n > s.entrySeq {
			s.entrySeq = n
		}
	}
}

func (s *Service) allEntryIDs() map[string]struct{} {
	out := map[string]struct{}{}
	for _, w := range s.waves {
		for id := range w.entries {
			out[id] = struct{}{}
		}
	}
	return out
}
