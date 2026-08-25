package gate

import "opensqt/exchange/streamhealth"

func (g *GateAdapter) IsOrderStreamReady() bool {
	return g != nil && g.wsManager != nil && g.wsManager.orderHealth.Ready()
}

func (g *GateAdapter) GetOrderStreamState() string {
	if g == nil || g.wsManager == nil {
		return string(streamhealth.StateStopped)
	}
	return string(g.wsManager.orderHealth.State())
}
