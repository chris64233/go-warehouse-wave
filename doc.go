// Package gowarehousewave 实现仓库拣货波次的库存分配与缺货重排。
//
// 核心能力：
//   - 批次登记（RegisterBatch）与 FEFO（有效期早 → 入库早）批次分配；
//   - 波次创建（CreateWave）整体成立或整体失败，外部波次号幂等；
//   - 缺货报告（ReportShortage）保留历史、版本化重排缺口；
//   - 拣货确认（ConfirmPick）按当前分配版本校验回执；
//   - 波次取消（CancelWave）只释放未确认占用，每份库存至多释放一次；
//   - 分配明细查询（GetWave/GetBatch/ListBatches）；
//   - OpenService 提供 JSONL 追加日志持久化与重启重放，NewService 为纯内存模式。
package gowarehousewave
