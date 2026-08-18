package web

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"opensqt/exchange"
)

type mockAccount struct {
	fail bool
	acc  *exchange.Account
}

func (m *mockAccount) GetAccount(ctx context.Context) (*exchange.Account, error) {
	if m.fail {
		return nil, errors.New("boom")
	}
	return m.acc, nil
}

func (m *mockAccount) GetQuoteAsset() string { return "USDT" }

func TestAccountCacheKeepsStaleOnError(t *testing.T) {
	src := &mockAccount{acc: &exchange.Account{
		AvailableBalance:   100,
		TotalWalletBalance: 120,
		TotalMarginBalance: 110,
		AccountLeverage:    5,
		Positions: []*exchange.Position{{
			Symbol:        "ETHUSDT",
			Size:          0.2,
			EntryPrice:    3000,
			UnrealizedPNL: 1.5,
		}},
	}}
	c := newAccountCache(src, "ETHUSDT", time.Second)
	c.refresh(context.Background())
	v := c.View()
	if v.Available != 100 || v.PositionSize != 0.2 || v.Stale || !v.InitialMarginReady {
		t.Fatalf("first view = %+v", v)
	}
	if v.InitialMargin != 110 || v.MarginChange != 0 || v.MarginChangePct != 0 {
		t.Fatalf("initial margin view = %+v", v)
	}

	src.acc.TotalMarginBalance = 125
	c.refresh(context.Background())
	v = c.View()
	if v.InitialMargin != 110 || v.MarginChange != 15 {
		t.Fatalf("changed margin view = %+v", v)
	}
	if math.Abs(v.MarginChangePct-13.6363636364) > 1e-9 {
		t.Fatalf("margin change pct = %f", v.MarginChangePct)
	}

	src.fail = true
	c.refresh(context.Background())
	v2 := c.View()
	if !v2.Stale || v2.Available != 100 || v2.MarginChange != 15 || v2.Error == "" {
		t.Fatalf("stale view = %+v", v2)
	}
}
