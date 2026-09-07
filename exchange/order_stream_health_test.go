package exchange

import (
	"testing"

	"opensqt/exchange/binance"
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
