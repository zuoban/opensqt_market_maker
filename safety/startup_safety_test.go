package safety

import (
	"context"
	"testing"

	"opensqt/exchange"
)

type startupSafetyExchange struct {
	exchange.IExchange
	size     float64
	leverage int
}

func (s startupSafetyExchange) GetName() string       { return "Binance" }
func (s startupSafetyExchange) GetQuoteAsset() string { return "USDT" }
func (s startupSafetyExchange) GetAccount(context.Context) (*exchange.Account, error) {
	return &exchange.Account{AvailableBalance: 10000}, nil
}
func (s startupSafetyExchange) GetPositions(context.Context, string) ([]*exchange.Position, error) {
	return []*exchange.Position{{Symbol: "BTCUSDT", Size: s.size, Leverage: s.leverage}}, nil
}
func TestStartupSafetyAppliesToExistingPositions(t *testing.T) {
	for _, test := range []struct {
		name     string
		leverage int
		fee      float64
	}{{"excess_leverage", 20, 0}, {"unprofitable_grid", 10, 0.001}} {
		t.Run(test.name, func(t *testing.T) {
			ex := startupSafetyExchange{leverage: test.leverage}
			if err := CheckAccountSafety(ex, "BTCUSDT", 10000, 30, 1, test.fee, 100, 2); err == nil {
				t.Fatal("fixture: flat account must be rejected")
			}
			ex.size = 0.01
			if err := CheckAccountSafety(ex, "BTCUSDT", 10000, 30, 1, test.fee, 100, 2); err == nil {
				t.Fatal("existing position bypassed startup guard and permits fresh buy orders")
			}
		})
	}
}

func TestStartupSafetyAcceptsHealthyExistingPosition(t *testing.T) {
	ex := startupSafetyExchange{size: 0.01, leverage: 10}
	if err := CheckAccountSafety(ex, "BTCUSDT", 10000, 30, 10, 0.0002, 100, 2); err != nil {
		t.Fatalf("healthy existing position was rejected: %v", err)
	}
}
