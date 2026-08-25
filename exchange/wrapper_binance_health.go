package exchange

import "opensqt/exchange/binance"

var _ OrderStreamHealthProvider = (*binanceWrapper)(nil)

// IsOrderStreamReady 转发 Binance 用户订单流的就绪状态。
func (w *binanceWrapper) IsOrderStreamReady() bool {
	return w != nil && w.adapter != nil && w.adapter.IsOrderStreamReady()
}

// GetOrderStreamState 转发 Binance 用户订单流的生命周期状态。
func (w *binanceWrapper) GetOrderStreamState() string {
	if w == nil || w.adapter == nil {
		return string(binance.OrderStreamStateStopped)
	}
	return w.adapter.GetOrderStreamState()
}
