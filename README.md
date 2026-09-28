# go-warehouse-wave

仓库拣货波次（picking wave）的库存分配与缺货重排能力：按 FEFO 分配库存批次、
版本化处理缺货重排、并发争用下保证不超卖，并以 append-only 事件日志持久化
每一次分配变化。

## 核心能力

- **库存批次登记**：批次含现存数量（OnHand）、有效期与入库时间。
- **波次创建（全有或全无）**：创建时冻结订单行集合，按
  **有效期较早 → 入库时间较早**（FEFO）的顺序为每行分配库存批次。
  任一订单行无法完整满足时，整个波次不成立，不残留任何占用。
- **外部波次号幂等**：`ExternalID` 是幂等键。同号重复提交相同订单集合
  返回原波次；订单集合发生任何变化返回 `ErrConflict`。
- **版本化缺货重排**：拣货员报告某批次短缺后，**原分配记录完整保留**
  （仅记账 `ShortQty`），再从仍可用的批次中为缺口生成下一版分配。
  重排失败时波次保持待处理（pending）、原占用不变，仅写一条失败审计事件。
- **拣货确认 / 取消的版本控制**：每份回执必须携带其基于的分配版本，
  旧版本回执一律拒绝（`ErrVersionConflict`）。取消只释放**尚未确认且未报缺**
  的占用，每条分配只释放一次；已确认拣出与已报缺损坏的数量绝不归还。
- **并发安全**：单一互斥锁串行化所有状态变更；多个波次并发创建时按获取
  锁的顺序依次全量试算，库存不足者整体失败，绝不超卖或丢失占用。
- **持久化**：所有状态变化先写 append-only JSON Lines 事件日志（每次写
  `fsync`）再更新内存；重启时回放事件重建全部状态。

## 快速开始

```go
svc, err := gowarehousewave.NewService("data/wave.log") // 传 "" 使用纯内存
if err != nil { /* 日志损坏等 */ }
defer svc.Close()

// 1. 登记库存批次
svc.RegisterBatch(gowarehousewave.RegisterBatchInput{
    BatchID: "B-001", SKU: "SKU-A", OnHand: 100,
    ExpiresAt:  expiry1,
    ReceivedAt: recv1,
})

// 2. 创建波次（外部波次号幂等）
w, err := svc.CreateWave(gowarehousewave.CreateWaveInput{
    ExternalID: "EXT-WAVE-77",
    Lines: []gowarehousewave.OrderLine{
        {OrderID: "ORD-1", LineID: "L1", SKU: "SKU-A", Qty: 10},
    },
})
// w.CurrentVersion == 1，w.Entries 为按 FEFO 得到的批次占用明细

// 3. 缺货报告（需带当前版本号）
w, err = svc.ReportShortage(gowarehousewave.ReportShortageInput{
    WaveID: w.ID, EntryID: w.Entries[0].ID, Version: w.CurrentVersion, ShortQty: 4,
})
// 原行保留并记账 4 短缺；追加 version=2 的补缺明细，CurrentVersion 变为 2

// 4. 拣货确认（必须带当前版本号，旧版本回执被拒绝）
svc.ConfirmPick(gowarehousewave.ConfirmPickInput{
    WaveID: w.ID, EntryID: e.ID, Version: w.CurrentVersion, Qty: 6,
})

// 5. 取消：仅释放未确认占用
svc.CancelWave(w.ID)
```

## API 一览

| 方法 | 说明 |
|---|---|
| `NewService(journalPath)` | 构造服务；非空路径启用文件日志并回放恢复 |
| `RegisterBatch` | 登记/更新批次；不得把现存调到已承诺量之下，不得改 SKU |
| `CreateWave` | 冻结订单行 + FEFO 全量分配；外部号幂等、内容变化报冲突 |
| `ReportShortage` | 当前版本行报缺 → 原行记账 + 下一版补缺；失败不改状态 |
| `ConfirmPick` | 当前版本行确认拣出；全部拣完波次自动完成 |
| `CancelWave` | 取消波次，仅释放剩余占用，每份只释放一次 |
| `GetWave` | 按内部 ID 或外部波次号查询波次与全量明细 |
| `ListAllocations` | 当前仍待拣货的分配明细（含部分报缺后原行的剩余） |
| `AllocationHistory` | 所有版本的全部明细（含已报缺/已确认的历史行） |
| `GetBatch` | 查询批次与当前可用量（现存 − 已承诺） |

错误哨兵（用 `errors.Is` 判断）：`ErrInvalidArgument`、`ErrNotFound`、
`ErrConflict`、`ErrVersionConflict`、`ErrInsufficientStock`、
`ErrWaveClosed`、`ErrEntryClosed`。

## 领域模型

- `Batch{ID, SKU, OnHand, ExpiresAt, ReceivedAt}`：批次主数据，OnHand 为
  登记的实物现存数量，不随分配直接扣减。
- `OrderLine{OrderID, LineID, SKU, Qty}`：订单行；波次创建时被冻结
  （校验、补全行号、按 `(OrderID, LineID, SKU)` 排序），之后永不改变。
- `AllocEntry`：一条「某版本内某订单行对某批次」的占用明细，字段包括
  `Version / Qty / ConfirmedQty / ShortQty / Released`，以及重排溯源
  `ReplacesLine / SourceBatch`。其剩余量：
  `Remaining = Qty − ConfirmedQty − ShortQty`（已释放行为 0）。
- `Wave`：含冻结订单行 `Lines`、按版本顺序**只追加**的 `Entries`、
  `CurrentVersion`（创建即 v1，每次成功重排 +1）与状态
  （pending / completed / cancelled）。

## 关键语义与不变量

1. **FEFO 分配**：同一 SKU 的批次按 `(ExpiresAt, ReceivedAt, BatchID)`
   升序消费；同一波次的订单行按确定顺序共享该 FEFO 序列，较早批次先被
   排序靠前的订单行使用。分配前先做总量可行性校验，因此库存不足时不会
   产出半成品明细。
2. **不超卖记账**：批次「已承诺量」= 所有未释放行的整笔 `Qty`
   （在持占用 + 已确认拣出 + 已报缺损坏，三者实物都不在可分配池里），
   已释放行仅保留其确认/报缺部分。新分配可用量 = `OnHand − 已承诺量`。
3. **缺货重排**：缺口数量被视为来源批次的实物损坏，重排时整个来源批次
   退出候选（不允许同批次回填），其余批次仍按 FEFO 选择。原行不删除、
   不修改原始 `Qty`，只追加下一版明细，故每次分配变化都可审计追溯。
4. **版本仲裁**：`ReportShortage` 与 `ConfirmPick` 都必须携带
   `Version == CurrentVersion`；旧版本回执即使落在仍有剩余的原行上也被
   拒绝，避免过期回执覆盖新分配。
5. **取消规则**：取消把仍有剩余的行原子地标记 `Released`（幂等标记，
   每份库存只释放一次）；之后任何回执返回 `ErrWaveClosed`，重复取消同样
   被拒绝。若全部行都已无剩余（全确认或确认+报缺），波次自动进入
   completed，不能再取消。
6. **并发**：所有写方法在同一把互斥锁内完成「试算 → 落盘 → 改内存」，
   因此并发创建、确认、报缺、取消的任意交错都只有一个确定的串行结果。

## 持久化与恢复

- 事件类型：`batch_registered`、`wave_created`、`shortage_reported`、
  `shortage_rearrange_failed`（审计）、`pick_confirmed`、
  `wave_completed`、`wave_cancelled`。
- 每条事件一行 JSON，追加写后立即 `fsync`；内存投影与启动回放共用同一
  套 `apply` 逻辑，保证运行态与恢复态一致。
- 若进程在 `pick_confirmed` 与自动补记的 `wave_completed` 之间崩溃，
  回放结束后会依据明细自愈：已无剩余占用的待处理波次恢复为 completed。
- 日志尾部出现写坏的半行时回放返回错误（之前完整落盘的事件不受影响），
  需人工介入后再启动，避免带病恢复。

## 测试

```bash
go test -race -count=1 ./...
```

覆盖要点：FEFO 排序与跨批次拆分、全有或全无且无占用泄漏、外部号幂等与
内容冲突、同号并发重复提交、多波次并发争用不超卖、缺货重排成功/失败、
连续缺货多版本、旧版本回执拒绝、取消只释放未确认部分且只释放一次、
确认与取消 100 次随机交错的不变量、文件日志重启恢复（含幂等索引恢复与
完成态恢复）等共 20+ 个用例。
