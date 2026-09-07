package order

import (
	"fmt"
)

// OrderRejectionKind 描述交易所明确拒绝受理订单的原因。
type OrderRejectionKind string

const (
	OrderRejectionPostOnly         OrderRejectionKind = "post_only"
	OrderRejectionMakerMoved       OrderRejectionKind = "maker_quote_moved"
	OrderRejectionMarketDataStale  OrderRejectionKind = "market_data_stale"
	OrderRejectionRateLimit        OrderRejectionKind = "rate_limit"
	OrderRejectionMargin           OrderRejectionKind = "margin"
	OrderRejectionPositionMode     OrderRejectionKind = "position_mode"
	OrderRejectionTimestamp        OrderRejectionKind = "timestamp"
	OrderRejectionExchangeRejected OrderRejectionKind = "exchange_rejected"
)

// OrderRejectedError 表示交易所已明确返回“订单未受理”。
//
// 该错误与 ErrOrderPlacementUnknown 相反：调用方可以确信本次请求没有生成
// 远端订单，因此无需仅为确认该订单是否存在而触发全局恢复或对账。
type OrderRejectedError struct {
	Kind OrderRejectionKind
	Err  error
}

func (e *OrderRejectedError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err == nil {
		return fmt.Sprintf("订单明确未受理（%s）", e.Kind)
	}
	return fmt.Sprintf("订单明确未受理（%s）: %v", e.Kind, e.Err)
}

func (e *OrderRejectedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// NewOrderRejectedError 创建一个“订单明确未受理”错误，并保留底层错误以便诊断。
func NewOrderRejectedError(kind OrderRejectionKind, err error) error {
	return &OrderRejectedError{Kind: kind, Err: err}
}

// IsDefiniteOrderRejection 判断整个错误是否只由“订单明确未受理”分支组成。
// 它会穿透 fmt.Errorf 的 %w 包装，并逐支检查 errors.Join；只要任一分支是
// UNKNOWN、门禁/上下文错误或普通错误，就返回 false。
func IsDefiniteOrderRejection(err error) bool {
	if err == nil {
		return false
	}
	return allErrorBranchesDefiniteRejections(err)
}

func allErrorBranchesDefiniteRejections(err error) bool {
	if err == nil {
		return false
	}

	// OrderRejectedError 是语义边界：它包装的底层 SDK 错误用于保留诊断信息，
	// 不应再被当成一个额外的未知分支。
	if _, ok := err.(*OrderRejectedError); ok {
		return true
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !allErrorBranchesDefiniteRejections(child) {
				return false
			}
		}
		return true
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return allErrorBranchesDefiniteRejections(wrapped.Unwrap())
	}

	return false
}

// 保证编译期校验标准 errors 包装仍可识别该类型。
var _ error = (*OrderRejectedError)(nil)
