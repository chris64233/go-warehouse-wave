// Package gowarehousewave 提供仓库拣货波次的库存分配与缺货重排能力：
// FEFO 批次分配、波次级全有或全无、外部波次号幂等、版本化缺货重排、
// 并发安全的拣货确认/取消仲裁，以及基于 append-only 事件日志的持久化。
//
// 入口类型为 Service，典型流程见 README。
package gowarehousewave
