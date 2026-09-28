package gowarehousewave

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------- 测试辅助 ----------

func testService(t *testing.T) *Service {
	t.Helper()
	s, err := NewService("")
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func regBatch(t *testing.T, s *Service, id, sku string, qty int64, expiry, received time.Time) {
	t.Helper()
	if _, err := s.RegisterBatch(RegisterBatchInput{
		BatchID: id, SKU: sku, OnHand: qty, ExpiresAt: expiry, ReceivedAt: received,
	}); err != nil {
		t.Fatalf("RegisterBatch %s: %v", id, err)
	}
}

func mustCreate(t *testing.T, s *Service, ext string, lines ...OrderLine) *Wave {
	t.Helper()
	w, err := s.CreateWave(CreateWaveInput{ExternalID: ext, Lines: lines})
	if err != nil {
		t.Fatalf("CreateWave %s: %v", ext, err)
	}
	return w
}

func line(order, lid, sku string, qty int64) OrderLine {
	return OrderLine{OrderID: order, LineID: lid, SKU: sku, Qty: qty}
}

func avail(t *testing.T, s *Service, batchID string) int64 {
	t.Helper()
	_, a, err := s.GetBatch(batchID)
	if err != nil {
		t.Fatalf("GetBatch %s: %v", batchID, err)
	}
	return a
}

// entriesByBatch 取波次全部历史明细，按批次汇总当前在持剩余。
func remainingByBatch(w *Wave) map[string]int64 {
	out := map[string]int64{}
	for _, e := range w.Entries {
		out[e.BatchID] += e.Remaining()
	}
	return out
}

var (
	t1 = time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	t3 = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
)

// ---------- 1. FEFO 顺序 ----------

func TestFEFO_ExpiryBeforeReceiveTime(t *testing.T) {
	s := testService(t)
	// b1 有效期更早，但入库更晚；有效期应优先于入库时间。
	regBatch(t, s, "b1", "SKU1", 10, t1, t3)
	regBatch(t, s, "b2", "SKU1", 10, t2, t1)

	w := mustCreate(t, s, "W1", line("o1", "l1", "SKU1", 5))
	if got := w.Entries; len(got) != 1 || got[0].BatchID != "b1" {
		t.Fatalf("expected earliest expiry batch b1, got %+v", got)
	}
}

func TestFEFO_SameExpiryEarlierReceivedFirst(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "b1", "SKU1", 10, t2, t3) // 有效期相同，入库晚
	regBatch(t, s, "b2", "SKU1", 10, t2, t1) // 入库早

	w := mustCreate(t, s, "W1", line("o1", "l1", "SKU1", 5))
	if w.Entries[0].BatchID != "b2" {
		t.Fatalf("expected earlier received batch b2, got %s", w.Entries[0].BatchID)
	}
}

func TestFEFO_SplitsAcrossBatchesInOrder(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "b1", "SKU1", 4, t1, t1)
	regBatch(t, s, "b2", "SKU1", 4, t2, t1)
	regBatch(t, s, "b3", "SKU1", 4, t3, t1)

	w := mustCreate(t, s, "W1", line("o1", "l1", "SKU1", 10))
	got := map[string]int64{}
	for _, e := range w.Entries {
		got[e.BatchID] += e.Qty
	}
	if got["b1"] != 4 || got["b2"] != 4 || got["b3"] != 2 {
		t.Fatalf("expected 4/4/2 split, got %v", got)
	}
}

// ---------- 2. 全有或全无 ----------

func TestCreate_AllOrNothingOnInsufficientStock(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "bX", "X", 20, t1, t1) // X 充足
	regBatch(t, s, "bY", "Y", 3, t1, t1)  // Y 只有 3，需要 5

	_, err := s.CreateWave(CreateWaveInput{ExternalID: "W1", Lines: []OrderLine{
		line("o1", "l1", "X", 8),
		line("o1", "l2", "Y", 5),
	}})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("expected ErrInsufficientStock, got %v", err)
	}
	// 失败后不得残留任何占用：两个批次可用量不变，波次不存在。
	if a := avail(t, s, "bX"); a != 20 {
		t.Fatalf("bX leaked holds: avail=%d", a)
	}
	if a := avail(t, s, "bY"); a != 3 {
		t.Fatalf("bY leaked holds: avail=%d", a)
	}
	if _, err := s.GetWave("W1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed wave must not exist, got err=%v", err)
	}
}

// ---------- 3. 外部波次号幂等与冲突 ----------

func TestCreate_IdempotentSameContent(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "b1", "X", 100, t1, t1)
	lines := []OrderLine{line("o1", "l1", "X", 10)}

	w1 := mustCreate(t, s, "W1", lines...)
	// 改变提交顺序也应视为相同集合。
	w2 := mustCreate(t, s, "W1", lines...)
	if w1.ID != w2.ID || w2.CurrentVersion != 1 {
		t.Fatalf("idempotent resubmit must return same wave: %s vs %s", w1.ID, w2.ID)
	}
	if len(w2.Entries) != 1 || avail(t, s, "b1") != 90 {
		t.Fatalf("duplicate submission must not double-allocate")
	}
}

func TestCreate_ConflictOnChangedContent(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "b1", "X", 100, t1, t1)
	mustCreate(t, s, "W1", line("o1", "l1", "X", 10))

	_, err := s.CreateWave(CreateWaveInput{
		ExternalID: "W1",
		Lines:      []OrderLine{line("o1", "l1", "X", 11)}, // 数量变了
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}

	_, err = s.CreateWave(CreateWaveInput{
		ExternalID: "W1",
		Lines:      []OrderLine{line("o1", "l1", "X", 10), line("o1", "l2", "X", 1)},
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("adding a line must conflict, got %v", err)
	}
}

func TestCreate_ConcurrentSameExternalIDSingleAllocation(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "b1", "X", 100, t1, t1)

	const n = 32
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			w, err := s.CreateWave(CreateWaveInput{
				ExternalID: "W1", Lines: []OrderLine{line("o1", "l1", "X", 10)},
			})
			if err == nil {
				ids[i] = w.ID
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()

	var first string
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent resubmit %d failed: %v", i, err)
		}
		if first == "" {
			first = ids[i]
		} else if ids[i] != first {
			t.Fatalf("concurrent resubmits produced different waves")
		}
	}
	if a := avail(t, s, "b1"); a != 90 {
		t.Fatalf("concurrent duplicate submissions allocated %d instead of 10", 100-a)
	}
}

// ---------- 4. 并发争用不超卖 ----------

func TestCreate_ConcurrentContentionNoOversell(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "b1", "X", 6, t1, t1)
	regBatch(t, s, "b2", "X", 4, t2, t1)

	const n = 20
	const need = 6
	var wg sync.WaitGroup
	successes := make(chan string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			w, err := s.CreateWave(CreateWaveInput{
				ExternalID: fmt.Sprintf("W-%d", i),
				Lines:      []OrderLine{line("o", "l", "X", need)},
			})
			if err == nil {
				if len(w.Entries) != 1 {
					t.Errorf("wave %d split unexpectedly: %+v", i, w.Entries)
				}
				successes <- w.ID
			} else if !errors.Is(err, ErrInsufficientStock) {
				t.Errorf("wave %d unexpected err: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(successes)

	count := 0
	for range successes {
		count++
	}
	if count != 1 {
		t.Fatalf("exactly one 6-unit wave may win over 10 total units, got %d", count)
	}
	// 占用后仍可分配的余量为 4。
	if a := avail(t, s, "b1") + avail(t, s, "b2"); a != 4 {
		t.Fatalf("remaining available should be 4, got %d", a)
	}
}

// ---------- 5. 缺货重排：成功路径 ----------

func TestShortage_RearrangeToNextVersion(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1)
	regBatch(t, s, "B", "X", 10, t2, t1)

	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	old := w.Entries[0]
	if old.BatchID != "A" || w.CurrentVersion != 1 {
		t.Fatalf("initial allocation wrong: %+v", old)
	}

	// A 批次实物短缺 4，应从 B 重排，波次升版到 2。
	w2, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: old.ID, Version: 1, ShortQty: 4,
	})
	if err != nil {
		t.Fatalf("ReportShortage: %v", err)
	}
	if w2.CurrentVersion != 2 || w2.Status != StatusPending {
		t.Fatalf("expected pending v2, got %s v%d", w2.Status, w2.CurrentVersion)
	}

	hist, err := s.AllocationHistory(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("original allocation must be preserved: %d entries", len(hist))
	}
	// 原行保留：记账 4 短缺，剩余 6 仍待拣。
	var orig, gap *AllocEntry
	for _, e := range hist {
		switch e.ID {
		case old.ID:
			orig = e
		default:
			gap = e
		}
	}
	if orig.ShortQty != 4 || orig.Remaining() != 6 {
		t.Fatalf("original entry not preserved correctly: %+v", orig)
	}
	if gap.BatchID != "B" || gap.Qty != 4 || gap.Version != 2 ||
		gap.ReplacesLine != old.ID || gap.SourceBatch != "A" {
		t.Fatalf("rearranged entry wrong: %+v", gap)
	}
	// A 的 4 个缺口视为损坏，不能再分给别人：A 可用 0，B 可用 6。
	if a := avail(t, s, "A"); a != 0 {
		t.Fatalf("short units must not be reusable, A avail=%d", a)
	}
	if a := avail(t, s, "B"); a != 6 {
		t.Fatalf("B avail=%d want 6", a)
	}

	// 旧行剩余 6 与补缺 4 全部按 v2 回执确认后波次完成。
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: orig.ID, Version: 2, Qty: 6,
	}); err != nil {
		t.Fatalf("confirm remaining on old entry: %v", err)
	}
	done, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: gap.ID, Version: 2, Qty: 4,
	})
	if err != nil {
		t.Fatalf("confirm gap entry: %v", err)
	}
	if done.Status != StatusCompleted {
		t.Fatalf("expected completed, got %s", done.Status)
	}
}

// ---------- 6. 缺货重排失败：波次保持待处理 ----------

func TestShortage_RearrangeFailureKeepsPending(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1) // 只有 A，无替代批次

	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	old := w.Entries[0]

	_, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: old.ID, Version: 1, ShortQty: 4,
	})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("expected ErrInsufficientStock, got %v", err)
	}

	after, err := s.GetWave(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusPending || after.CurrentVersion != 1 {
		t.Fatalf("wave must stay pending v1, got %s v%d", after.Status, after.CurrentVersion)
	}
	if len(after.Entries) != 1 || after.Entries[0].ShortQty != 0 {
		t.Fatalf("failed rearrangement must not mutate original entry: %+v", after.Entries)
	}
	// 原占用完好：仍可按原分配拣出全部 10。
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: old.ID, Version: 1, Qty: 10,
	}); err != nil {
		t.Fatalf("original allocation must remain pickable: %v", err)
	}
	// 失败尝试应留下审计事件。
	if data := readJournal(t, s); !contains(data, evShortageFailed) {
		t.Fatalf("expected %s audit event in journal", evShortageFailed)
	}
}

// ---------- 7. 旧版本回执不能覆盖新分配 ----------

func TestStaleVersionReceiptsRejected(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1)
	regBatch(t, s, "B", "X", 10, t2, t1)
	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	old := w.Entries[0]

	w2, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: old.ID, Version: 1, ShortQty: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var gap *AllocEntry
	for _, e := range w2.Entries {
		if e.ID != old.ID {
			gap = e
		}
	}

	// 旧版本拣货回执：即使落在仍有剩余的原行上也必须拒绝。
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: old.ID, Version: 1, Qty: 1,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale pick on old entry: %v", err)
	}
	// 旧版本缺货回执同样拒绝（旧行、新行都一样）。
	if _, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: gap.ID, Version: 1, ShortQty: 1,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale shortage on new entry: %v", err)
	}
	// 当前版本回执正常。
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: old.ID, Version: 2, Qty: 7,
	}); err != nil {
		t.Fatalf("current version pick on old entry: %v", err)
	}
}

// ---------- 8. 取消：只释放未确认占用，每份只释放一次 ----------

func TestCancel_ReleasesOnlyUnconfirmedOnce(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1)
	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	e := w.Entries[0]

	// 先确认拣出 6，再取消：仅 4 归还库存。
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: e.ID, Version: 1, Qty: 6,
	}); err != nil {
		t.Fatal(err)
	}
	cancelled, err := s.CancelWave(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != StatusCancelled {
		t.Fatalf("status=%s", cancelled.Status)
	}
	entry := cancelled.Entries[0]
	if !entry.Released || entry.ConfirmedQty != 6 {
		t.Fatalf("entry state wrong: %+v", entry)
	}
	if a := avail(t, s, "A"); a != 4 {
		t.Fatalf("only unconfirmed 4 may be released, avail=%d", a)
	}

	// 已确认拣出的 6 绝不能再分给别人：余量之上的新波次必须失败。
	if _, err := s.CreateWave(CreateWaveInput{
		ExternalID: "W2", Lines: []OrderLine{line("o2", "l1", "X", 5)},
	}); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("confirmed stock must not be reallocated, err=%v", err)
	}
	mustCreate(t, s, "W3", line("o3", "l1", "X", 4))

	// 取消后拒绝一切回执与重复取消。
	if _, err := s.CancelWave(w.ID); !errors.Is(err, ErrWaveClosed) {
		t.Fatalf("double cancel: %v", err)
	}
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: e.ID, Version: 1, Qty: 1,
	}); !errors.Is(err, ErrWaveClosed) {
		t.Fatalf("pick after cancel: %v", err)
	}
}

// ---------- 9. 确认/取消竞争：任何交错下不变量成立 ----------

func TestConfirmCancelRaceInvariants(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		s := testService(t)
		regBatch(t, s, "A", "X", 10, t1, t1)
		w := mustCreate(t, s, fmt.Sprintf("W%d", iter), line("o1", "l1", "X", 10))
		e := w.Entries[0]

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		var pickErr, cancelErr error
		go func() {
			defer wg.Done()
			<-start
			_, pickErr = s.ConfirmPick(ConfirmPickInput{
				WaveID: w.ID, EntryID: e.ID, Version: 1, Qty: 6,
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, cancelErr = s.CancelWave(w.ID)
		}()
		close(start)
		wg.Wait()

		got, _ := s.GetWave(w.ID)
		var confirmed int64
		for _, en := range got.Entries {
			confirmed += en.ConfirmedQty
		}
		switch {
		case cancelErr == nil && pickErr == nil:
			// 确认先于取消：6 已出库，4 释放。
			if got.Status != StatusCancelled || confirmed != 6 {
				t.Fatalf("iter %d: pick-then-cancel state wrong: %+v", iter, got)
			}
			if a := avail(t, s, "A"); a != 4 {
				t.Fatalf("iter %d: avail=%d want 4", iter, a)
			}
		case cancelErr == nil && errors.Is(pickErr, ErrWaveClosed):
			// 取消先于确认：全部释放。
			if confirmed != 0 || avail(t, s, "A") != 10 {
				t.Fatalf("iter %d: cancel-then-pick state wrong confirmed=%d avail=%d",
					iter, confirmed, avail(t, s, "A"))
			}
		default:
			t.Fatalf("iter %d: unexpected combination pickErr=%v cancelErr=%v", iter, pickErr, cancelErr)
		}
	}
}

// ---------- 10. 已确认库存不会再次分配 ----------

func TestConfirmedStockRemovedFromPool(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1)
	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	e := w.Entries[0]
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: e.ID, Version: 1, Qty: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if a := avail(t, s, "A"); a != 0 {
		t.Fatalf("fully picked batch avail=%d want 0", a)
	}
	if _, err := s.CreateWave(CreateWaveInput{
		ExternalID: "W2", Lines: []OrderLine{line("o2", "l1", "X", 1)},
	}); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("picked units must never be reallocated, err=%v", err)
	}
}

// ---------- 11. 取消释放后库存可重新分配 ----------

func TestCancel_MakesStockAvailableAgain(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 5, t1, t1)
	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 5))
	if a := avail(t, s, "A"); a != 0 {
		t.Fatalf("avail after create=%d want 0", a)
	}
	if _, err := s.CancelWave(w.ID); err != nil {
		t.Fatal(err)
	}
	if a := avail(t, s, "A"); a != 5 {
		t.Fatalf("avail after cancel=%d want 5", a)
	}
	mustCreate(t, s, "W2", line("o2", "l1", "X", 5))
}

// ---------- 12. 参数校验与批次下限 ----------

func TestRegister_Validation(t *testing.T) {
	s := testService(t)
	if _, err := s.RegisterBatch(RegisterBatchInput{BatchID: "", SKU: "X", OnHand: 1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty id: %v", err)
	}
	if _, err := s.RegisterBatch(RegisterBatchInput{BatchID: "b", SKU: "X", OnHand: -1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative onhand: %v", err)
	}
	regBatch(t, s, "A", "X", 10, t1, t1)
	mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	// 不得把现存调到已承诺量之下，也不得改 SKU。
	if _, err := s.RegisterBatch(RegisterBatchInput{BatchID: "A", SKU: "X", OnHand: 9}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("shrink below committed: %v", err)
	}
	if _, err := s.RegisterBatch(RegisterBatchInput{BatchID: "A", SKU: "Y", OnHand: 10}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("change sku: %v", err)
	}
}

func TestConfirm_OverRemainingRejected(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1)
	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: w.Entries[0].ID, Version: 1, Qty: 11,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("over-pick: %v", err)
	}
}

// ---------- 13. 文件日志持久化与重启恢复 ----------

func TestFileJournal_RestoreState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wave.log")

	func() {
		s, err := NewService(path)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		regBatch(t, s, "A", "X", 10, t1, t1)
		regBatch(t, s, "B", "X", 10, t2, t1)
		w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
		// A 短缺 4，B 可补足，重排成功。
		if _, err := s.ReportShortage(ReportShortageInput{
			WaveID: w.ID, EntryID: w.Entries[0].ID, Version: 1, ShortQty: 4,
		}); err != nil {
			t.Fatal(err)
		}
	}()

	// 重新打开：状态必须从日志完整重建。
	s2, err := NewService(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	if a := avail(t, s2, "A"); a != 0 {
		t.Fatalf("after restart A avail=%d want 0", a)
	}
	if a := avail(t, s2, "B"); a != 6 {
		t.Fatalf("after restart B avail=%d want 6", a)
	}
	w, err := s2.GetWave("W1")
	if err != nil {
		t.Fatal(err)
	}
	if w.CurrentVersion != 2 || w.Status != StatusPending {
		t.Fatalf("restored wave v%d %s", w.CurrentVersion, w.Status)
	}
	if len(w.Entries) != 2 || len(w.Lines) != 1 {
		t.Fatalf("restored entries/lines lost: %+v", w)
	}
	// 幂等键同样恢复：同号同内容返回原波次，改内容冲突。
	if again := mustCreate(t, s2, "W1", line("o1", "l1", "X", 10)); again.ID != w.ID {
		t.Fatal("idempotency index not restored")
	}
	if _, err := s2.CreateWave(CreateWaveInput{
		ExternalID: "W1", Lines: []OrderLine{line("o1", "l1", "X", 9)},
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("restored conflict check: %v", err)
	}
	// 恢复后业务可继续推进。
	hist, _ := s2.AllocationHistory(w.ID)
	var orig, gap *AllocEntry
	for _, e := range hist {
		if e.BatchID == "A" {
			orig = e
		} else {
			gap = e
		}
	}
	if _, err := s2.ConfirmPick(ConfirmPickInput{WaveID: w.ID, EntryID: orig.ID, Version: 2, Qty: 6}); err != nil {
		t.Fatal(err)
	}
	done, err := s2.ConfirmPick(ConfirmPickInput{WaveID: w.ID, EntryID: gap.ID, Version: 2, Qty: 4})
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != StatusCompleted {
		t.Fatalf("completed after restart: %s", done.Status)
	}
}

func TestFileJournal_CompletedSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wave.log")
	func() {
		s, _ := NewService(path)
		defer s.Close()
		regBatch(t, s, "A", "X", 3, t1, t1)
		w := mustCreate(t, s, "W1", line("o1", "l1", "X", 3))
		if _, err := s.ConfirmPick(ConfirmPickInput{
			WaveID: w.ID, EntryID: w.Entries[0].ID, Version: 1, Qty: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}()
	s2, err := NewService(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	w, err := s2.GetWave("W1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != StatusCompleted {
		t.Fatalf("status=%s want completed", w.Status)
	}
	if _, err := s2.CancelWave(w.ID); !errors.Is(err, ErrWaveClosed) {
		t.Fatalf("completed wave cannot be cancelled: %v", err)
	}
}

// ---------- 辅助 ----------

func readJournal(t *testing.T, s *Service) string {
	t.Helper()
	j, ok := s.journal.(*memoryJournal)
	if !ok {
		return ""
	}
	out := ""
	_ = j.Replay(func(ev *Event) { out += ev.Type + "\n" })
	return out
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
