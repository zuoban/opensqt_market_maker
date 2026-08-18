package position

import (
	"context"
	"testing"
	"time"

	"opensqt/config"
)

type stubExecutor struct{}

func (stubExecutor) PlaceOrder(req *OrderRequest) (*Order, error) { return nil, nil }
func (stubExecutor) BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool) {
	return nil, false
}
func (stubExecutor) BatchCancelOrders(orderIDs []int64) error { return nil }

type stubEx struct{}

func (stubEx) GetName() string { return "TestEx" }
func (stubEx) GetPositions(ctx context.Context, symbol string) (interface{}, error) {
	return nil, nil
}
func (stubEx) GetOpenOrders(ctx context.Context, symbol string) (interface{}, error) {
	return nil, nil
}
func (stubEx) GetOrder(ctx context.Context, symbol string, orderID int64) (interface{}, error) {
	return nil, nil
}
func (stubEx) GetBaseAsset() string                                     { return "ETH" }
func (stubEx) CancelAllOrders(ctx context.Context, symbol string) error { return nil }

func testConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Trading.Symbol = "ETHUSDT"
	cfg.Trading.PriceInterval = 1
	cfg.Trading.OrderQuantity = 30
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 2
	return cfg
}

func TestSnapshotCountsAndProfit(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	spm.lastMarketPrice.Store(100.0)
	spm.isInitialized.Store(true)
	spm.totalBuyQty.Store(2.0)
	spm.totalSellQty.Store(1.5)
	spm.realizedPNL.Store(0.42)
	spm.lastReconcileTime.Store(time.Unix(1700000000, 0))

	s1 := spm.getOrCreateSlot(101)
	s1.PositionStatus = PositionStatusFilled
	s1.PositionQty = 0.01
	s1.OrderSide = "SELL"
	s1.OrderStatus = OrderStatusPlaced
	s1.OrderID = 11
	s1.SlotStatus = SlotStatusLocked

	s2 := spm.getOrCreateSlot(99)
	s2.OrderSide = "BUY"
	s2.OrderStatus = OrderStatusConfirmed
	s2.OrderID = 22
	s2.ClientOID = "buy-99"
	s2.SlotStatus = SlotStatusLocked

	spm.getOrCreateSlot(98)

	snap := spm.Snapshot()
	if !snap.Initialized {
		t.Fatal("expected initialized")
	}
	if snap.BaseAsset != "ETH" {
		t.Fatalf("base asset: %s", snap.BaseAsset)
	}
	if snap.FilledSlotCount != 1 {
		t.Fatalf("filled slots = %d", snap.FilledSlotCount)
	}
	if snap.ActiveBuyOrders != 1 || snap.ActiveSellOrders != 1 {
		t.Fatalf("active orders buy=%d sell=%d", snap.ActiveBuyOrders, snap.ActiveSellOrders)
	}
	if snap.EstimatedProfit != 1.5 {
		t.Fatalf("estimated profit = %v", snap.EstimatedProfit)
	}
	if snap.RealizedPNL != 0.42 {
		t.Fatalf("realized pnl = %v", snap.RealizedPNL)
	}
	if len(snap.Slots) != 3 {
		t.Fatalf("slots = %d", len(snap.Slots))
	}
	if snap.Slots[0].Price < snap.Slots[1].Price {
		t.Fatal("slots should be price desc")
	}
	byPrice := map[float64]SlotSnapshot{}
	for _, sl := range snap.Slots {
		byPrice[sl.Price] = sl
	}
	if !byPrice[101].InSellWindow {
		t.Fatal("101 should be in sell window")
	}
	if !byPrice[99].InBuyWindow {
		t.Fatal("99 should be in buy window")
	}
	if byPrice[98].InBuyWindow || byPrice[98].InSellWindow {
		t.Fatal("98 should be outside both windows")
	}
	foundOID := false
	for _, sl := range snap.Slots {
		if sl.ClientOID == "buy-99" {
			foundOID = true
		}
	}
	if !foundOID {
		t.Fatal("missing client oid")
	}
}

func TestMarginLockRemaining(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	if rem := spm.MarginLockRemaining(); rem != 0 {
		t.Fatalf("unlocked remaining = %s", rem)
	}
	spm.mu.Lock()
	spm.insufficientMargin = true
	spm.marginLockTime = time.Now()
	spm.marginLockDuration = 10 * time.Second
	spm.mu.Unlock()
	rem := spm.MarginLockRemaining()
	if rem <= 0 || rem > 10*time.Second {
		t.Fatalf("remaining = %s", rem)
	}
}
