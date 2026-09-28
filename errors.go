package gowarehousewave

import "errors"

// 业务错误。调用方使用 errors.Is 判断。
var (
	// ErrInvalidArgument 表示入参不合法（空标识、非正数量等）。
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrNotFound 表示批次或波次不存在。
	ErrNotFound = errors.New("not found")
	// ErrConflict 表示外部波次号已存在但订单集合与原提交不一致。
	ErrConflict = errors.New("wave content conflicts with existing submission")
	// ErrVersionConflict 表示回执基于过期分配版本，不能作用于当前分配。
	ErrVersionConflict = errors.New("allocation version conflict")
	// ErrInsufficientStock 表示按 FEFO 顺序无法完整满足需求。
	ErrInsufficientStock = errors.New("insufficient stock")
	// ErrWaveClosed 表示波次已完成或已取消，拒绝该操作。
	ErrWaveClosed = errors.New("wave already closed")
	// ErrEntryClosed 表示该分配行已全部确认/报缺/释放，无剩余占用。
	ErrEntryClosed = errors.New("allocation entry has no remaining hold")
)
