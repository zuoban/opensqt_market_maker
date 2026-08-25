package bybit

import "opensqt/exchange/streamhealth"

func (b *BybitAdapter) IsOrderStreamReady() bool {
	return b != nil && b.wsManager != nil && b.wsManager.orderHealth.Ready()
}

func (b *BybitAdapter) GetOrderStreamState() string {
	if b == nil || b.wsManager == nil {
		return string(streamhealth.StateStopped)
	}
	return string(b.wsManager.orderHealth.State())
}
