package gowarehousewave

import (
	"sort"
)

// allocate 按 FEFO（有效期较早优先；相同则入库时间较早优先）为一组需求行
// 计算全量批次分配。要么所有行都被完整满足并返回明细，要么返回
// ErrInsufficientStock 且不返回任何明细——调用方据此保证“全有或全无”。
//
// onHand: 各批次现存实物数量；holds: 各批次此前已被占用的数量。
// 分配只读取这两份快照并构造新明细，不修改任何状态。
func allocate(lines []OrderLine, batches map[string]*Batch, holds map[string]int64, version int, idgen func() string) ([]*AllocEntry, error) {
	// 按 SKU 汇总需求，并保留确定的行消费顺序。需求行已由 normalizeLines 去重。
	demand := map[string]int64{}
	for _, l := range lines {
		demand[l.SKU] += l.Qty
	}
	lineOrder := lines

	// 每个 SKU 一份按 FEFO 排好序的可用批次列表。
	bySKU := map[string][]*Batch{}
	for _, b := range batches {
		avail := b.OnHand - holds[b.ID]
		if avail <= 0 {
			continue
		}
		bySKU[b.SKU] = append(bySKU[b.SKU], b)
	}
	for sku := range bySKU {
		list := bySKU[sku]
		sort.SliceStable(list, func(i, j int) bool {
			if !list[i].ExpiresAt.Equal(list[j].ExpiresAt) {
				return list[i].ExpiresAt.Before(list[j].ExpiresAt)
			}
			if !list[i].ReceivedAt.Equal(list[j].ReceivedAt) {
				return list[i].ReceivedAt.Before(list[j].ReceivedAt)
			}
			return list[i].ID < list[j].ID
		})
	}

	// 先做总量可行性检查，尽早失败、且不留半成品。
	for sku, need := range demand {
		var free int64
		for _, b := range bySKU[sku] {
			free += b.OnHand - holds[b.ID]
		}
		if free < need {
			return nil, ErrInsufficientStock
		}
	}

	// 批次游标：所有需求行共享同一 FEFO 序列，先到的行先吃较早批次。
	cursor := map[string]int{}
	var entries []*AllocEntry
	for _, l := range lineOrder {
		need := l.Qty
		list := bySKU[l.SKU]
		idx := cursor[l.SKU]
		for need > 0 {
			// 跳过已被前序行耗尽的批次。
			for idx < len(list) {
				b := list[idx]
				if b.OnHand-holds[b.ID]-consumed(entries, b.ID) > 0 {
					break
				}
				idx++
			}
			if idx >= len(list) {
				// 可行性检查本应拦住；此处属保护性失败。
				return nil, ErrInsufficientStock
			}
			b := list[idx]
			used := holds[b.ID] + consumed(entries, b.ID)
			take := b.OnHand - used
			if take > need {
				take = need
			}
			entries = append(entries, &AllocEntry{
				ID:      idgen(),
				Version: version,
				LineID:  l.LineID,
				OrderID: l.OrderID,
				SKU:     l.SKU,
				BatchID: b.ID,
				Qty:     take,
			})
			need -= take
			if b.OnHand-used-take <= 0 {
				idx++
			}
		}
		cursor[l.SKU] = idx
	}
	return entries, nil
}

// consumed 统计本次分配中已落在某批次上的数量。
func consumed(entries []*AllocEntry, batchID string) int64 {
	var n int64
	for _, e := range entries {
		if e.BatchID == batchID {
			n += e.Qty
		}
	}
	return n
}

// sortLines 按 (OrderID, LineID, SKU) 稳定排序订单行，使幂等比较与分配顺序确定。
func sortLines(l []OrderLine) {
	sort.SliceStable(l, func(i, j int) bool {
		if l[i].OrderID != l[j].OrderID {
			return l[i].OrderID < l[j].OrderID
		}
		if l[i].LineID != l[j].LineID {
			return l[i].LineID < l[j].LineID
		}
		return l[i].SKU < l[j].SKU
	})
}
