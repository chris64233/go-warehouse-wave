package gowarehousewave

import "errors"

// 领域错误。调用方可用 errors.Is 判定具体原因。
var (
	// ErrBatchExists 批次号已登记。
	ErrBatchExists = errors.New("batch already exists")
	// ErrBatchNotFound 批次不存在。
	ErrBatchNotFound = errors.New("batch not found")
	// ErrWaveNotFound 波次不存在。
	ErrWaveNotFound = errors.New("wave not found")
	// ErrWaveConflict 同号波次重复提交但订单集合不同。
	ErrWaveConflict = errors.New("wave idempotency conflict: order set differs")
	// ErrInsufficientStock 可用库存不足以完整满足波次或缺口重排。
	ErrInsufficientStock = errors.New("insufficient stock")
	// ErrInvalidQty 数量非法（为零、为负或超过可操作量）。
	ErrInvalidQty = errors.New("invalid quantity")
	// ErrInvalidRequest 请求参数非法。
	ErrInvalidRequest = errors.New("invalid request")
	// ErrEntryNotFound 分配明细不存在。
	ErrEntryNotFound = errors.New("allocation entry not found")
	// ErrStaleVersion 回执基于旧分配版本，不能覆盖当前分配。
	ErrStaleVersion = errors.New("stale allocation version")
	// ErrWaveNotActive 波次已完成或已取消，不能再操作。
	ErrWaveNotActive = errors.New("wave not active")
	// ErrEntryNotActive 该明细已被新版本取代或已结清。
	ErrEntryNotActive = errors.New("allocation entry not active")
	// ErrSKUMismatch 回执/上报中的 SKU 与明细不符。
	ErrSKUMismatch = errors.New("sku mismatch")
)
