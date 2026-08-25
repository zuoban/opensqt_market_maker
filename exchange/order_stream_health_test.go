package exchange

import (
	"testing"

	"opensqt/exchange/binance"
	"opensqt/exchange/streamhealth"
)

func TestBinanceWrapperOrderStreamHealthIsNilSafe(t *testing.T) {
	var nilWrapper *binanceWrapper
	if nilWrapper.IsOrderStreamReady() {
		t.Fatal("nil Binance wrapper 不应报告订单流就绪")
	}
	if state := nilWrapper.GetOrderStreamState(); state != string(binance.OrderStreamStateStopped) {
		t.Fatalf("nil Binance wrapper 状态 = %q，期望 STOPPED", state)
	}

	wrapper := &binanceWrapper{}
	var provider OrderStreamHealthProvider = wrapper
	if provider.IsOrderStreamReady() {
		t.Fatal("未配置 adapter 的 Binance wrapper 不应报告订单流就绪")
	}
	if state := provider.GetOrderStreamState(); state != string(binance.OrderStreamStateStopped) {
		t.Fatalf("未配置 adapter 的 Binance wrapper 状态 = %q，期望 STOPPED", state)
	}
}

func TestAllProductionWrappersExposeNilSafeOrderStreamHealth(t *testing.T) {
	tests := []struct {
		name     string
		provider OrderStreamHealthProvider
	}{
		{name: "Bybit", provider: (*bybitWrapper)(nil)},
		{name: "Backpack", provider: (*backpackWrapper)(nil)},
		{name: "Bitget", provider: (*bitgetWrapper)(nil)},
		{name: "Gate", provider: (*gateWrapper)(nil)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.provider.IsOrderStreamReady() {
				t.Fatal("nil wrapper 不应报告订单流就绪")
			}
			if state := tt.provider.GetOrderStreamState(); state != string(streamhealth.StateStopped) {
				t.Fatalf("nil wrapper 状态=%q，期望 STOPPED", state)
			}
		})
	}
}
