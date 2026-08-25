package backpack

import "opensqt/exchange/streamhealth"

func (b *BackpackAdapter) IsOrderStreamReady() bool {
	return b != nil && b.wsManager != nil && b.wsManager.orderHealth.Ready()
}

func (b *BackpackAdapter) GetOrderStreamState() string {
	if b == nil || b.wsManager == nil {
		return string(streamhealth.StateStopped)
	}
	return string(b.wsManager.orderHealth.State())
}
