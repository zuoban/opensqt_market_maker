package exchange

import "opensqt/exchange/exchangeerr"

// ErrOrderPlacementUnknown 表示交易所可能已经接受订单，但客户端无法确认结果。
// 它由具体交易所适配器返回，订单执行器必须把它视为不可盲目重试错误。
var ErrOrderPlacementUnknown = exchangeerr.ErrOrderPlacementUnknown

// ErrOrderPlacementRejected 表示订单请求明确未被交易所受理，可安全释放本次 reservation。
var ErrOrderPlacementRejected = exchangeerr.ErrOrderPlacementRejected

// IsOrderPlacementRejected 判断下单错误是否明确未受理；UNKNOWN 始终优先。
func IsOrderPlacementRejected(err error) bool {
	return exchangeerr.IsOrderPlacementRejected(err)
}
