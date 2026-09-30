# go-warehouse-wave

仓库拣货波次（wave）库存分配与缺货重排能力。以外部波次号作幂等键，支持
FEFO 批次分配、缺货后的版本化重排、版本校验的拣货确认、取消时只释放未确认
占用，并以追加式 JSONL 事件日志持久化每一次分配变化。

## 核心能力

| 能力 | 方法 | 说明 |
| --- | --- | --- |
| 库存批次登记 | `RegisterBatch` | 登记批次号、SKU、数量、有效期与入库时间 |
| 波次创建 | `CreateWave` | 冻结订单行集合，按 FEFO 整体取得库存；外部波次号幂等 |
| 缺货报告 | `ReportShortage` | 保留原分配，为缺口生成**下一版**分配 |
| 拣货确认 | `ConfirmPick` | 按当前分配版本校验回执，确认数量永久出库 |
| 波次取消 | `CancelWave` | 只释放当前版本上尚未确认的占用，重复取消幂等 |
| 分配明细查询 | `GetWave` / `GetBatch` / `ListBatches` | 波次全部版本明细与批次账面 |

## 快速开始

```go
s, err := gowarehousewave.OpenService("data/waves.jsonl") // 持久化模式
if err != nil { /* ... */ }
defer s.Close()

// 也可以使用纯内存模式：s := gowarehousewave.NewService()

s.RegisterBatch(gowarehousewave.BatchInput{
    ID: "B001", SKU: "A", OnHand: 10,
    Expiry:     time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
    ReceivedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
})

wave, err := s.CreateWave(gowarehousewave.CreateWaveRequest{
    WaveID: "W-EXT-1001", // 外部波次号 = 幂等键
    Lines:  []gowarehousewave.OrderLine{{OrderID: "O1", SKU: "A", Qty: 8}},
})
```

### 缺货重排

拣货员发现批次实盘短缺时上报（必须带上回执所依据的版本号）：

```go
v2, err := s.ReportShortage(gowarehousewave.ReportShortageRequest{
    WaveID: "W-EXT-1001", EntryID: wave.Entries[0].ID,
    Version: 1, Qty: 2,
})
```

系统会：

1. **保留 v1 全部记录**（明细置为 `superseded`，不删除、不改写）；
2. 核销短缺批次的账面库存（盘亏，不回到可用池）；
3. 未拣明细（扣除短缺量）作为 `carry` 结转到 v2；
4. 缺口按 FEFO 从**仍可用**的批次补位（`shortage` 明细）；
5. 若库存不足，整体返回 `ErrInsufficientStock`，波次停在原版本/原状态。

### 拣货确认与版本竞争

```go
_, err = s.ConfirmPick(gowarehousewave.ConfirmPickRequest{
    WaveID: wave.WaveID, EntryID: v2.Entries[0].ID,
    Version: v2.Version, Qty: 1,
})
```

- 旧版本回执（明细已被重排取代，或回执版本号落后）返回 `ErrStaleVersion`，
  不允许覆盖新分配；
- 确认数量从占用转为已拣出（永久出库），取消、重排都不会回收它；
- 全部订单行数量跨版本累计拣出后，波次自动置为 `completed`。

### 取消

`CancelWave` 释放当前版本全部**未确认**占用回可用池；已确认拣出的数量
保留。取消幂等（重复调用为空操作），取消后拣货/缺货操作返回
`ErrWaveNotActive`。

## 关键设计与不变量

### 1. 整体成立，杜绝部分占用

`CreateWave` / `ReportShortage` 先在试算结构（scratch）上按 FEFO 模拟分配，
只有所有订单行都满足后才落状态；试算失败不触碰任何批次账面。因此不会出现
“部分订单已占货、其余失败”的中间结果。

**FEFO 顺序**：有效期较早 → 入库时间较早 → 批次号（确定性兜底）。

### 2. 并发不超卖、不丢占用

全部状态变更在同一把 `sync.Mutex` 内完成（试算 + 提交均持锁），多波次并发
争用同一批次时序列化裁决：先到者整体占用，后到者要么拿到剩余库存、要么整体
失败。批次账面恒等式：

```
OnHand = Available + Held + Picked
```

缺货核销同时调减 `OnHand` 与 `Held`；拣货确认 `Held → Picked`；取消
`Held → Available`。任何路径后该式恒成立。

### 3. 外部波次号幂等

同一 `WaveID` 重复提交：订单集合（按 `OrderID, SKU` 冻结排序后的数量集合）
相同时返回**原分配**（不重新试算）；内容不同返回 `ErrWaveConflict`。

### 4. 版本化重排，已拣数量不动

- 每次成功的缺货重排分配版本号 +1；旧版本完整保留可审计。
- 结转只携带**未拣余量**，已确认拣出的数量留在历史明细上，既不会被再次
  分配给别人，也不会被取消释放。
- 重排是原子的：补位试算失败时不核销、不结转、不升版本。

### 5. 每份库存只释放一次

旧版本明细在重排时就把未拣余量释放（转入新版本占用或核销），取消只遍历
**当前版本**的 active 明细，因此同一份库存不可能在重排与取消中被释放两次。

## 持久化

`OpenService(path)` 使用追加式 **JSON Lines** 事件日志：

- 每次分配变化（批次登记 / 建波 / 缺货重排 / 拣货确认 / 取消）写一条事件，
  `Write` 后 `fsync`，再更新内存状态；落盘失败则内存不动。
- 打开日志时顺序重放全部事件恢复状态；历史明细（含 superseded）一并恢复。

事件类型见 `journal.go` 中的 `batch_registered` / `wave_created` /
`shortage_reallocated` / `pick_confirmed` / `wave_cancelled`。

## API 参考

### 入参 / 视图

- `BatchInput`：`ID, SKU, OnHand, Expiry, ReceivedAt`
- `OrderLine`：`OrderID, SKU, Qty`（同一订单行重复出现会合并数量）
- `CreateWaveRequest`：`WaveID, Lines`
- `ReportShortageRequest` / `ConfirmPickRequest`：
  `WaveID, EntryID, Version, SKU(可选校验), Qty`
- `WaveDetail`：`Status, CurrentVersion, Lines, Versions(全部历史), Entries(当前版本)`
- `AllocationEntry`：`ID, Version, OrderID, SKU, BatchID, Qty, Picked, Reason, Status`
- `Batch`：`OnHand, Available, Held, Picked, Status`

### 波次 / 明细状态

- 波次：`pending` →（部分拣出）`picking` →（全部拣出）`completed`；
  或任意活动态 → `cancelled`。
- 明细：`active` → `superseded`（被新版本取代）/ `completed` / `cancelled`。
- 明细来源：`initial`（初版）/ `carry`（重排结转）/ `shortage`（缺口补位）。

### 错误

`errors.Is` 可判定：`ErrBatchExists`、`ErrBatchNotFound`、`ErrWaveNotFound`、
`ErrWaveConflict`、`ErrInsufficientStock`、`ErrInvalidQty`、
`ErrInvalidRequest`、`ErrEntryNotFound`、`ErrStaleVersion`、
`ErrWaveNotActive`、`ErrEntryNotActive`、`ErrSKUMismatch`。

## 测试

```bash
go test -race ./...
```

覆盖：FEFO 顺序与跨批拆分、库存不足原子回滚、幂等返回原分配、同号内容
冲突、同号并发重复提交只占用一次、20 波次并发争用不超卖、缺货核销与补位、
重排失败回滚且已拣数量不可再分配、v1→v2→v3 连续重排、旧版本回执拒绝、
取消只释放未拣占用、拣货/取消并发账实守恒，以及 JSONL 落盘后重启重放一致。
