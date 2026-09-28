package gowarehousewave

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 同一原始批次先后发现更多损坏：缺口可连续重排，版本递增、原行保留。
func TestShortage_ChainedRearrangements(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1)
	regBatch(t, s, "B", "X", 10, t2, t1)
	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	orig := w.Entries[0]

	// 第一次：A 缺 4 → B 补 4（v2）。
	w2, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: orig.ID, Version: 1, ShortQty: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	var gap1 *AllocEntry
	for _, e := range w2.Entries {
		if e.Version == 2 {
			gap1 = e
		}
	}

	// 第二次：原行剩余的 6 在 A 上也全部损坏，持 v2 回执再报缺 → v3，B 恰好还剩 6。
	w3, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: orig.ID, Version: 2, ShortQty: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if w3.CurrentVersion != 3 {
		t.Fatalf("expected v3, got v%d", w3.CurrentVersion)
	}
	hist, _ := s.AllocationHistory(w.ID)
	if len(hist) != 3 {
		t.Fatalf("expected 3 versioned entries, got %d", len(hist))
	}
	// 原行累计记账 10 短缺、完整保留；A 的 10 件全部不可再分。
	if orig2 := findEntry(hist, orig.ID); orig2.ShortQty != 10 || orig2.Remaining() != 0 {
		t.Fatalf("original entry: %+v", orig2)
	}
	if a := avail(t, s, "A"); a != 0 {
		t.Fatalf("A avail=%d want 0", a)
	}
	if a := avail(t, s, "B"); a != 0 {
		t.Fatalf("B fully committed to gaps, avail=%d want 0", a)
	}
	// gap1 与新 gap2 都待拣，共 10。
	var active int64
	for _, e := range hist {
		if e.Status() == EntryActive {
			active += e.Remaining()
		}
	}
	if active != 10 {
		t.Fatalf("active remaining=%d want 10", active)
	}
	// 原行已关闭，再次报缺/确认应被拒绝。
	if _, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: orig.ID, Version: 3, ShortQty: 1,
	}); !errors.Is(err, ErrEntryClosed) {
		t.Fatalf("closed entry shortage: %v", err)
	}
	_ = gap1
}

// 多个订单行共享同一 FEFO 批次序列：排序靠前的行先消费较早批次。
func TestFEFO_MultipleLinesConsumeInOrder(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 5, t1, t1)
	regBatch(t, s, "B", "X", 5, t2, t1)
	// 故意乱序提交两行。
	w := mustCreate(t, s, "W1",
		line("o2", "l9", "X", 5),
		line("o1", "l1", "X", 5),
	)
	byLine := map[string]string{}
	qtyByLineBatch := map[string]int64{}
	for _, e := range w.Entries {
		byLine[e.LineID] = e.BatchID
		qtyByLineBatch[e.LineID+"/"+e.BatchID] += e.Qty
	}
	if byLine["l1"] != "A" || byLine["l9"] != "B" {
		t.Fatalf("FEFO consumption order wrong: %+v", byLine)
	}
	if qtyByLineBatch["l1/A"] != 5 || qtyByLineBatch["l9/B"] != 5 {
		t.Fatalf("line/batch quantities wrong: %+v", qtyByLineBatch)
	}
	// 冻结后的订单行集合按确定顺序保存。
	if w.Lines[0].LineID != "l1" || w.Lines[1].LineID != "l9" {
		t.Fatalf("frozen lines not normalized: %+v", w.Lines)
	}
}

// 行状态与波次 ActiveHolds 的汇总语义。
func TestEntryAndWaveStatusHelpers(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1)
	regBatch(t, s, "B", "X", 10, t2, t1)
	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	if w.Entries[0].Status() != EntryActive {
		t.Fatal("new entry should be active")
	}
	if holds := w.ActiveHolds(); holds["A"] != 10 {
		t.Fatalf("active holds=%v", holds)
	}

	orig := w.Entries[0]
	w2, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: orig.ID, Version: 1, ShortQty: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	var oldNow, gapNow *AllocEntry
	for _, e := range w2.Entries {
		if e.ID == orig.ID {
			oldNow = e
		} else {
			gapNow = e
		}
	}
	if oldNow.Status() != EntryShort {
		t.Fatalf("fully shorted entry status=%s", oldNow.Status())
	}
	if gapNow.Status() != EntryActive {
		t.Fatalf("gap entry status=%s", gapNow.Status())
	}
	if holds := w2.ActiveHolds(); holds["B"] != 10 || holds["A"] != 0 {
		t.Fatalf("holds after shortage=%v", holds)
	}

	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: gapNow.ID, Version: 2, Qty: 10,
	}); err != nil {
		t.Fatal(err)
	}
	done, _ := s.GetWave(w.ID)
	if done.Status != StatusCompleted {
		t.Fatalf("status=%s", done.Status)
	}
	for _, e := range done.Entries {
		if e.ID == gapNow.ID && e.Status() != EntryConfirmed {
			t.Fatalf("gap picked entry status=%s", e.Status())
		}
	}
}

// 部分拣出 + 部分报缺的行取消：只释放真正剩余的部分。
func TestCancel_PartialConfirmPartialShort(t *testing.T) {
	s := testService(t)
	regBatch(t, s, "A", "X", 10, t1, t1)
	regBatch(t, s, "B", "X", 10, t2, t1)
	w := mustCreate(t, s, "W1", line("o1", "l1", "X", 10))
	orig := w.Entries[0]
	// 确认 3、报缺 3（重排到 B），原行剩 4。
	if _, err := s.ConfirmPick(ConfirmPickInput{
		WaveID: w.ID, EntryID: orig.ID, Version: 1, Qty: 3,
	}); err != nil {
		t.Fatal(err)
	}
	w2, err := s.ReportShortage(ReportShortageInput{
		WaveID: w.ID, EntryID: orig.ID, Version: 1, ShortQty: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var gap *AllocEntry
	for _, e := range w2.Entries {
		if e.Version == 2 {
			gap = e
		}
	}
	// 取消：原行释放 4，补缺行释放 3；已确认 3 与已损坏 3 不释放。
	cw, err := s.CancelWave(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range cw.Entries {
		if !e.Released {
			t.Fatalf("entry %s should be released", e.ID)
		}
		if e.Status() != EntryReleased {
			t.Fatalf("entry %s status=%s", e.ID, e.Status())
		}
	}
	if a := avail(t, s, "A"); a != 4 {
		t.Fatalf("A released avail=%d want 4", a)
	}
	if a := avail(t, s, "B"); a != 10 {
		t.Fatalf("B gap released back, avail=%d want 10", a)
	}
	_ = gap
}

// 日志文件尾部出现损坏行时，回放返回错误而非静默吞掉（已完整落盘的前缀事件仍有效）。
func TestFileJournal_CorruptTailReplayErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wave.log")
	func() {
		s, _ := NewService(path)
		defer s.Close()
		regBatch(t, s, "A", "X", 10, t1, t1)
	}()
	// 追加一行无法解析的内容，模拟写入中途掉电。
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not-json\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if _, err := NewService(path); err == nil {
		t.Fatal("expected replay error on corrupt journal tail")
	}
}

func findEntry(entries []*AllocEntry, id string) *AllocEntry {
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	return nil
}
