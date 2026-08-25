package binance

import "testing"

func TestBinanceAdapterOrderStreamHealth(t *testing.T) {
	var nilAdapter *BinanceAdapter
	if nilAdapter.IsOrderStreamReady() {
		t.Fatal("nil adapter 不应报告订单流就绪")
	}
	if state := nilAdapter.GetOrderStreamState(); state != string(OrderStreamStateStopped) {
		t.Fatalf("nil adapter 状态 = %q，期望 STOPPED", state)
	}

	adapter := &BinanceAdapter{}
	if adapter.IsOrderStreamReady() {
		t.Fatal("未配置 wsManager 的 adapter 不应报告订单流就绪")
	}
	if state := adapter.GetOrderStreamState(); state != string(OrderStreamStateStopped) {
		t.Fatalf("未配置 wsManager 的状态 = %q，期望 STOPPED", state)
	}

	manager := NewWebSocketManager("api", "secret")
	adapter.wsManager = manager
	manager.mu.Lock()
	manager.isRunning = true
	manager.state = OrderStreamStateReady
	manager.mu.Unlock()
	if !adapter.IsOrderStreamReady() {
		t.Fatal("wsManager READY 时 adapter 应报告订单流就绪")
	}
	if state := adapter.GetOrderStreamState(); state != string(OrderStreamStateReady) {
		t.Fatalf("adapter 状态 = %q，期望 READY", state)
	}

	manager.mu.Lock()
	manager.state = OrderStreamStateDegraded
	manager.lastError = "disconnected"
	manager.mu.Unlock()
	if adapter.IsOrderStreamReady() {
		t.Fatal("wsManager DEGRADED 时 adapter 不应报告订单流就绪")
	}
	if state := adapter.GetOrderStreamState(); state != string(OrderStreamStateDegraded) {
		t.Fatalf("adapter 状态 = %q，期望 DEGRADED", state)
	}
}
