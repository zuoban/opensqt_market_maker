package exchange

import "opensqt/exchange/exchangeerr"

// ErrOrderPlacementUnknown 表示交易所可能已经接受订单，但客户端无法确认结果。
// 它由具体交易所适配器返回，订单执行器必须把它视为不可盲目重试错误。
var ErrOrderPlacementUnknown = exchangeerr.ErrOrderPlacementUnknown
