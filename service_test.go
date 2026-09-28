package gowarehousewave

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---- 测试夹具 ----

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// mustRegister 登记批次，失败即终止测试。
func mustRegister(t *testing.T, s *Service, in BatchInput) {
	t.Helper()
	if err := s.RegisterBatch(in); err != nil {
		t.Fatalf("RegisterBatch(%s): %v", in.ID, err)
	}
}

func entryByBatch(es []AllocationEntry, batchID string) *AllocationEntry {
	for i := range es {
		if es[i].BatchID == batchID {
			return &es[i]
		}
	}
	return nil
}

// inventory 汇总批次视图。
func inventory(s *Service) (available, held, picked int) {
	for _, b := range s.ListBatches() {
		available += b.Available
		held += b.Held
		picked += b.Picked
	}
	return
}

// ---- 1. FEFO 分配顺序 ----

func TestFEFOOrder(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B-LATE", SKU: "A", OnHand: 10, Expiry: date(2026, 6, 1), ReceivedAt: date(2025, 1, 1)})
	mustRegister(t, s, BatchInput{ID: "B-EARLY", SKU: "A", OnHand: 10, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 2, 1)})
	mustRegister(t, s, BatchInput{ID: "B-SAME", SKU: "A", OnHand: 10, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})

	d, err := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 25}}})
	if err != nil {
		t.Fatalf("CreateWave: %v", err)
	}
	if len(d.Entries) != 3 {
		t.Fatalf("want 3 split entries across batches, got %d", len(d.Entries))
	}
	got := []string{d.Entries[0].BatchID, d.Entries[1].BatchID, d.Entries[2].BatchID}
	// 有效期最早（B-SAME 与 B-EARLY 同日，入库更早的 B-SAME 优先）→ B-EARLY → B-LATE。
	want := []string{"B-SAME", "B-EARLY", "B-LATE"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("FEFO order = %v, want %v", got, want)
		}
	}
}

// ---- 1. 库存不足时波次整体不成立，无部分占用 ----

func TestAtomicAllocationFailure(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 5, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	mustRegister(t, s, BatchInput{ID: "B2", SKU: "B", OnHand: 0 + 3, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})

	_, err := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{
		{OrderID: "O1", SKU: "A", Qty: 5},
		{OrderID: "O1", SKU: "B", Qty: 4}, // 只有 3，不足
	}})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("want ErrInsufficientStock, got %v", err)
	}
	if _, err := s.GetWave("W1"); !errors.Is(err, ErrWaveNotFound) {
		t.Fatalf("failed wave should not exist, got err=%v", err)
	}
	avail, held, picked := inventory(s)
	if held != 0 || picked != 0 {
		t.Fatalf("no occupation may remain after failure: avail=%d held=%d picked=%d", avail, held, picked)
	}
	b1, _ := s.GetBatch("B1")
	if b1.Available != 5 {
		t.Fatalf("B1 available = %d, want 5 (nothing reserved)", b1.Available)
	}
}

// ---- 2. 幂等：同号同集合返回原分配 ----

func TestIdempotentCreate(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 10, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	lines := []OrderLine{{OrderID: "O1", SKU: "A", Qty: 4}}

	d1, err := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: lines})
	if err != nil {
		t.Fatal(err)
	}
	// 把库存吃光，第二次提交仍应返回原分配而非重新试算失败。
	mustRegister(t, s, BatchInput{ID: "B2", SKU: "A", OnHand: 6, Expiry: date(2027, 1, 1), ReceivedAt: date(2025, 6, 1)})
	_, _ = s.CreateWave(CreateWaveRequest{WaveID: "W2", Lines: []OrderLine{{OrderID: "O2", SKU: "A", Qty: 12}}})

	d2, err := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 4}}})
	if err != nil {
		t.Fatalf("idempotent resubmit: %v", err)
	}
	if d2.CurrentVersion != d1.CurrentVersion || len(d2.Entries) != 1 || d2.Entries[0].ID != d1.Entries[0].ID {
		t.Fatalf("resubmit must return original allocation, d1=%+v d2=%+v", d1.Entries, d2.Entries)
	}
}

// ---- 2. 幂等冲突：同号不同订单集合 ----

func TestIdempotencyConflict(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 10, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	if _, err := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 1}}}); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 2}}})
	if !errors.Is(err, ErrWaveConflict) {
		t.Fatalf("want ErrWaveConflict, got %v", err)
	}
	_, err = s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O9", SKU: "A", Qty: 1}}})
	if !errors.Is(err, ErrWaveConflict) {
		t.Fatalf("changed order id must conflict, got %v", err)
	}
}

// ---- 2. 并发争用：不超卖、不丢占用、无部分波次 ----

func TestConcurrentContention(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 100, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			_, errs[i] = s.CreateWave(CreateWaveRequest{
				WaveID: fmt.Sprintf("W%02d", i),
				Lines:  []OrderLine{{OrderID: fmt.Sprintf("O%02d", i), SKU: "A", Qty: 10}},
			})
		}()
	}
	wg.Wait()

	ok, failed := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInsufficientStock):
			failed++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 10 || failed != 10 {
		t.Fatalf("want exactly 10 winners / 10 failures, got %d / %d", ok, failed)
	}
	avail, held, picked := inventory(s)
	if avail != 0 || held != 100 || picked != 0 {
		t.Fatalf("oversell or lost hold: avail=%d held=%d picked=%d", avail, held, picked)
	}
}

// ---- 3. 缺货重排：保留历史、缺口补位、核销盘亏 ----

func TestShortageReallocation(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B-OLD", SKU: "A", OnHand: 5, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	mustRegister(t, s, BatchInput{ID: "B-NEW", SKU: "A", OnHand: 10, Expiry: date(2027, 1, 1), ReceivedAt: date(2025, 6, 1)})

	d, err := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	old := d.Entries[0]
	if old.BatchID != "B-OLD" {
		t.Fatalf("initial allocation should pick B-OLD, got %s", old.BatchID)
	}

	// B-OLD 报告短缺 2，B-NEW 补位。
	v2, err := s.ReportShortage(ReportShortageRequest{WaveID: "W1", EntryID: old.ID, Version: 1, Qty: 2})
	if err != nil {
		t.Fatalf("ReportShortage: %v", err)
	}
	if v2.Version != 2 {
		t.Fatalf("new version = %d, want 2", v2.Version)
	}
	carry := entryByBatch(v2.Entries, "B-OLD")
	gap := entryByBatch(v2.Entries, "B-NEW")
	if carry == nil || carry.Qty != 3 || carry.Reason != ReasonCarry {
		t.Fatalf("carry entry wrong: %+v", carry)
	}
	if gap == nil || gap.Qty != 2 || gap.Reason != ReasonShortage {
		t.Fatalf("shortage backfill wrong: %+v", gap)
	}

	// 历史版本完整保留。
	dd, _ := s.GetWave("W1")
	if len(dd.Versions) != 2 || dd.Versions[0].Entries[0].Status != EntrySuperseded {
		t.Fatalf("v1 must be retained as superseded: %+v", dd.Versions)
	}

	// B-OLD：账面核销 2（onHand=3），仍持有 3；B-NEW：持有 2。
	bOld, _ := s.GetBatch("B-OLD")
	bNew, _ := s.GetBatch("B-NEW")
	if bOld.OnHand != 3 || bOld.Held != 3 || bOld.Available != 0 {
		t.Fatalf("B-OLD write-off wrong: %+v", bOld)
	}
	if bNew.Available != 8 || bNew.Held != 2 {
		t.Fatalf("B-NEW hold wrong: %+v", bNew)
	}
}

// ---- 3. 已拣数量不参与重排，重排失败原波次不动 ----

func TestShortageKeepsPickedAndFailureRollback(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 5, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	d, err := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	e0 := d.Entries[0]

	// 先确认拣出 2。
	if _, err := s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: e0.ID, Version: 1, Qty: 2}); err != nil {
		t.Fatal(err)
	}

	// 短缺 2，但没有任何可用补位库存 → 失败，波次/批次账保持不变。
	_, err = s.ReportShortage(ReportShortageRequest{WaveID: "W1", EntryID: e0.ID, Version: 1, Qty: 2})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("want ErrInsufficientStock, got %v", err)
	}
	dd, _ := s.GetWave("W1")
	if dd.CurrentVersion != 1 || dd.Status != WaveStatusPicking {
		t.Fatalf("wave must remain on v1/picking after failed realloc, v=%d status=%s", dd.CurrentVersion, dd.Status)
	}
	b1, _ := s.GetBatch("B1")
	if b1.OnHand != 5 || b1.Held != 3 || b1.Picked != 2 {
		t.Fatalf("batch ledger must be untouched: %+v", b1)
	}
	// 旧明细仍可继续拣货。
	if _, err := s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: e0.ID, Version: 1, Qty: 1}); err != nil {
		t.Fatalf("original entry should remain pickable: %v", err)
	}

	// 缺口仍无库存时，已确认的 2 件不会被再次分配给别人：
	// 另一个波次要 4 件，只能拿到 B1 上剩余可用 0（held=2,picked=3,onHand=5），必然失败。
	_, err = s.CreateWave(CreateWaveRequest{WaveID: "W2", Lines: []OrderLine{{OrderID: "O2", SKU: "A", Qty: 4}}})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("picked qty must never be reallocated, got err=%v", err)
	}
}

// ---- 3. 重排后的缺口在新批次上再次短缺，可继续生成 v3，直至无库存可补 ----

func TestMultiVersionShortage(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 5, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	mustRegister(t, s, BatchInput{ID: "B2", SKU: "A", OnHand: 3, Expiry: date(2027, 1, 1), ReceivedAt: date(2026, 1, 1)})
	mustRegister(t, s, BatchInput{ID: "B3", SKU: "A", OnHand: 5, Expiry: date(2028, 1, 1), ReceivedAt: date(2027, 1, 1)})

	d, _ := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 5}}})
	e1 := d.Entries[0]

	// v1 B1=5 全缺 → v2：B2=3、B3=2（FEFO 跨批补位）。
	v2, err := s.ReportShortage(ReportShortageRequest{WaveID: "W1", EntryID: e1.ID, Version: 1, Qty: 5})
	if err != nil {
		t.Fatal(err)
	}
	if v2.Version != 2 || len(v2.Entries) != 2 {
		t.Fatalf("v2 wrong: %+v", v2.Entries)
	}
	if e := entryByBatch(v2.Entries, "B2"); e == nil || e.Qty != 3 {
		t.Fatalf("v2 B2 entry wrong: %+v", e)
	}
	if e := entryByBatch(v2.Entries, "B3"); e == nil || e.Qty != 2 {
		t.Fatalf("v2 B3 entry wrong: %+v", e)
	}
	b2Entry := entryByBatch(v2.Entries, "B2")

	// v2 上 B2 的 3 件也全缺 → v3：B3 旧明细结转 2，B3 再补 3。
	v3, err := s.ReportShortage(ReportShortageRequest{WaveID: "W1", EntryID: b2Entry.ID, Version: 2, Qty: 3})
	if err != nil {
		t.Fatalf("v3 realloc: %v", err)
	}
	if v3.Version != 3 || len(v3.Entries) != 2 {
		t.Fatalf("v3 wrong: %+v", v3.Entries)
	}
	if totalQty(v3.Entries) != 5 {
		t.Fatalf("v3 must still cover 5 units, got %d", totalQty(v3.Entries))
	}
	dd, _ := s.GetWave("W1")
	if len(dd.Versions) != 3 {
		t.Fatalf("all three versions must be retained, got %d", len(dd.Versions))
	}

	// v3 上 B3 再缺 3：B1/B2 已核销、B3 其余被占，无可用库存 → 失败，停在 v3。
	v3B3 := entryByBatch(v3.Entries, "B3")
	_, err = s.ReportShortage(ReportShortageRequest{WaveID: "W1", EntryID: v3B3.ID, Version: 3, Qty: v3B3.Qty})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("want ErrInsufficientStock when nothing left, got %v", err)
	}
	dd2, _ := s.GetWave("W1")
	if dd2.CurrentVersion != 3 || dd2.Status != WaveStatusPending {
		t.Fatalf("wave must stay v3/picking-pending, got v%d/%s", dd2.CurrentVersion, dd2.Status)
	}
}

func totalQty(es []AllocationEntry) int {
	n := 0
	for _, e := range es {
		n += e.Qty
	}
	return n
}

// ---- 4. 旧版本回执不能覆盖新分配 ----

func TestStaleVersionReceipts(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 5, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	mustRegister(t, s, BatchInput{ID: "B2", SKU: "A", OnHand: 5, Expiry: date(2027, 1, 1), ReceivedAt: date(2026, 1, 1)})

	d, _ := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 5}}})
	old := d.Entries[0]
	v2, err := s.ReportShortage(ReportShortageRequest{WaveID: "W1", EntryID: old.ID, Version: 1, Qty: 5})
	if err != nil {
		t.Fatal(err)
	}
	// 旧明细上的拣货回执（显式带 v1）必须被拒。
	if _, err := s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: old.ID, Version: 1, Qty: 1}); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("old entry v1 receipt want ErrStaleVersion, got %v", err)
	}
	// 新明细若带旧版本号也要拒。
	if _, err := s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: v2.Entries[0].ID, Version: 1, Qty: 1}); !errors.Is(err, ErrStaleVersion) {
		t.Fatalf("new entry with stale version want ErrStaleVersion, got %v", err)
	}
	// 正确版本可拣。
	if _, err := s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: v2.Entries[0].ID, Version: 2, Qty: 5}); err != nil {
		t.Fatalf("v2 pick: %v", err)
	}
	dd, _ := s.GetWave("W1")
	if dd.Status != WaveStatusCompleted {
		t.Fatalf("wave should complete after full pick on v2, got %s", dd.Status)
	}
}

// ---- 4. 取消只释放未确认占用、每份库存只释放一次 ----

func TestCancelReleasesOnlyUnpicked(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 5, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	mustRegister(t, s, BatchInput{ID: "B2", SKU: "A", OnHand: 5, Expiry: date(2027, 1, 1), ReceivedAt: date(2026, 1, 1)})

	d, _ := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 5}}})
	e1 := d.Entries[0]
	// 先重排到 B2：B1 短缺 5，B2 补 5。
	v2, err := s.ReportShortage(ReportShortageRequest{WaveID: "W1", EntryID: e1.ID, Version: 1, Qty: 5})
	if err != nil {
		t.Fatal(err)
	}
	// 在 v2 上拣出 2。
	if _, err := s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: v2.Entries[0].ID, Version: 2, Qty: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.CancelWave("W1"); err != nil {
		t.Fatalf("CancelWave: %v", err)
	}
	// 重复取消幂等。
	if err := s.CancelWave("W1"); err != nil {
		t.Fatalf("idempotent cancel: %v", err)
	}

	b1, _ := s.GetBatch("B1")
	b2, _ := s.GetBatch("B2")
	// B1 短缺 5 已核销：onHand=0；取消不重复处理 v1。
	if b1.OnHand != 0 || b1.Held != 0 || b1.Picked != 0 {
		t.Fatalf("B1 ledger wrong after cancel: %+v", b1)
	}
	// B2：拣出 2 永久保留，剩余 3 释放回可用池（held=0, available=3, picked=2）。
	if b2.OnHand != 5 || b2.Held != 0 || b2.Picked != 2 || b2.Available != 3 {
		t.Fatalf("B2 ledger wrong after cancel: %+v", b2)
	}
	// 释放出的 3 件可被新波次取得。
	if _, err := s.CreateWave(CreateWaveRequest{WaveID: "W2", Lines: []OrderLine{{OrderID: "O2", SKU: "A", Qty: 3}}}); err != nil {
		t.Fatalf("released stock should be allocatable: %v", err)
	}
	// 已取消波次不能再确认拣货。
	_, err = s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: v2.Entries[0].ID, Version: 2, Qty: 1})
	if !errors.Is(err, ErrWaveNotActive) {
		t.Fatalf("confirm on cancelled wave want ErrWaveNotActive, got %v", err)
	}
}

// ---- 4. 取消与拣货/重排并发：按状态串行裁决，账实守恒 ----

func TestConcurrentCancelPickRace(t *testing.T) {
	s := NewService()
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 10, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	d, _ := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 10}}})
	e1 := d.Entries[0]

	const n = 10
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: e1.ID, Version: 1, Qty: 1})
		}()
		go func() {
			defer wg.Done()
			_ = s.CancelWave("W1")
		}()
	}
	wg.Wait()

	dd, _ := s.GetWave("W1")
	b1, _ := s.GetBatch("B1")
	// 守恒：拣出 + 仍持有 + 可用 == 账面。
	if b1.Picked+b1.Held+b1.Available != b1.OnHand || b1.OnHand != 10 {
		t.Fatalf("ledger not conserved: %+v wave=%s", b1, dd.Status)
	}
	switch dd.Status {
	case WaveStatusCompleted:
		if b1.Picked != 10 {
			t.Fatalf("completed but picked=%d", b1.Picked)
		}
	case WaveStatusCancelled:
		// 已拣的不可丢、未拣的必须全释放。
		if b1.Held != 0 || b1.Picked+b1.Available != 10 {
			t.Fatalf("cancelled ledger wrong: %+v", b1)
		}
	default:
		t.Fatalf("race must end in completed or cancelled, got %s", dd.Status)
	}
}

// ---- 5. 持久化：每次分配变化落盘，重启重放一致 ----

func TestJournalReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "waves.jsonl")

	s, err := OpenService(path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, BatchInput{ID: "B1", SKU: "A", OnHand: 5, Expiry: date(2026, 1, 1), ReceivedAt: date(2025, 1, 1)})
	mustRegister(t, s, BatchInput{ID: "B2", SKU: "A", OnHand: 5, Expiry: date(2027, 1, 1), ReceivedAt: date(2026, 1, 1)})
	d, _ := s.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 5}}})
	v2, _ := s.ReportShortage(ReportShortageRequest{WaveID: "W1", EntryID: d.Entries[0].ID, Version: 1, Qty: 2})
	_, _ = s.ConfirmPick(ConfirmPickRequest{WaveID: "W1", EntryID: v2.Entries[0].ID, Version: 2, Qty: 3})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := OpenService(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	dd, err := s2.GetWave("W1")
	if err != nil {
		t.Fatal(err)
	}
	if dd.CurrentVersion != 2 || dd.Status != WaveStatusPicking || len(dd.Versions) != 2 {
		t.Fatalf("replayed wave wrong: %+v", dd)
	}
	if dd.Versions[0].Entries[0].Status != EntrySuperseded {
		t.Fatalf("v1 history not preserved after replay")
	}
	if n := len(dd.Versions[1].Entries); n != 2 {
		t.Fatalf("v2 should carry 3 + backfill 2 = 2 entries, got %d", n)
	}
	b1, _ := s2.GetBatch("B1")
	b2, _ := s2.GetBatch("B2")
	if b1.OnHand != 3 || b1.Held != 0 || b1.Picked != 3 {
		t.Fatalf("B1 replayed wrong: %+v", b1)
	}
	if b2.Held != 2 || b2.Picked != 0 {
		t.Fatalf("B2 replayed wrong: %+v", b2)
	}

	// 重放后的服务可继续工作，且幂等重放同一日志不会重建波次。
	d2, err := s2.CreateWave(CreateWaveRequest{WaveID: "W1", Lines: []OrderLine{{OrderID: "O1", SKU: "A", Qty: 5}}})
	if err != nil {
		t.Fatalf("idempotency after replay: %v", err)
	}
	if d2.CurrentVersion != 2 {
		t.Fatalf("resubmit after replay must hit existing wave, got v%d", d2.CurrentVersion)
	}
}
