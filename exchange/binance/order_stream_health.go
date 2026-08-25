package binance

type orderStreamHealthProvider interface {
	IsOrderStreamReady() bool
	GetOrderStreamState() string
}

var _ orderStreamHealthProvider = (*BinanceAdapter)(nil)

// IsOrderStreamReady 返回 Binance 用户订单流是否已经完成握手且健康。
func (b *BinanceAdapter) IsOrderStreamReady() bool {
	return b != nil && b.wsManager != nil && b.wsManager.Ready()
}

// GetOrderStreamState 返回 Binance 用户订单流当前生命周期状态。
func (b *BinanceAdapter) GetOrderStreamState() string {
	if b == nil || b.wsManager == nil {
		return string(OrderStreamStateStopped)
	}
	return string(b.wsManager.Health().State)
}
