package position

import (
	"context"
	"testing"
	"time"

	"opensqt/config"
)

type stubExecutor struct{}

func (stubExecutor) PlaceOrder(req *OrderRequest) (*Order, error) { return nil, nil }
func (stubExecutor) BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool, error) {
	return nil, false, nil
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

func TestSnapshotPreservesDisplayPrecisionAndDescendingPrices(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	spm.lastMarketPrice.Store(100.0)
	prices := []float64{1.005, 100.125, 0.01, 99.995}
	for i, price := range prices {
		slot := spm.getOrCreateSlot(price)
		slot.OrderID = int64(i + 1) // Keep every slot visible, including zero quantity.
		slot.PositionQty = float64(i) * 0.0015
	}
	snap := spm.Snapshot()
	if len(snap.Slots) != len(prices) {
		t.Fatalf("snapshot slots = %d, want %d", len(snap.Slots), len(prices))
	}
	for i, slot := range snap.Slots {
		if slot.PriceText != formatPrice(slot.Price, 2) || slot.PositionQtyText != formatPrice(slot.PositionQty, 3) {
			t.Fatalf("snapshot changed numeric display precision: %+v", slot)
		}
		if i > 0 && slot.Price >= snap.Slots[i-1].Price {
			t.Fatal("snapshot slots are not in descending price order")
		}
	}
}

func TestSnapshotCountsAndProfit(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	spm.lastMarketPrice.Store(100.0)
	spm.isInitialized.Store(true)
	spm.totalBuyQty.Store(2.0)
	spm.totalSellQty.Store(1.5)
	spm.realizedPNL.Store(0.42)
	spm.makerAttempts.Store(10)
	spm.makerAccepted.Store(9)
	spm.makerGuardSkips.Store(1)
	spm.postOnlyRejects.Store(2)
	spm.catchUpOrders.Store(3)
	spm.catchUpAbandoned.Store(1)
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
	if snap.AnchorPrice != 100 {
		t.Fatalf("anchor price = %v, want startup snapshot 100", snap.AnchorPrice)
	}
	if snap.BaseAsset != "ETH" {
		t.Fatalf("base asset: %s", snap.BaseAsset)
	}
	if snap.FilledSlotCount != 1 {
		t.Fatalf("filled slots = %d", snap.FilledSlotCount)
	}
	if snap.PositionQty != 0.01 {
		t.Fatalf("position qty = %v", snap.PositionQty)
	}
	if snap.PositionValue < 0.99 || snap.PositionValue > 1.01 {
		t.Fatalf("position value = %v, want ~1 (0.01 * 100)", snap.PositionValue)
	}
	if snap.ActiveBuyOrders != 1 || snap.ActiveSellOrders != 1 {
		t.Fatalf("active orders buy=%d sell=%d", snap.ActiveBuyOrders, snap.ActiveSellOrders)
	}
	if snap.MakerExecution.Attempts != 10 || snap.MakerExecution.Accepted != 9 ||
		snap.MakerExecution.GuardSkips != 1 || snap.MakerExecution.PostOnlyRejects != 2 ||
		snap.MakerExecution.CatchUpOrders != 3 || snap.MakerExecution.CatchUpAbandoned != 1 {
		t.Fatalf("maker execution = %+v", snap.MakerExecution)
	}
	if snap.EstimatedProfit != 1.5 {
		t.Fatalf("estimated profit = %v", snap.EstimatedProfit)
	}
	if snap.RealizedPNL != 0.42 {
		t.Fatalf("realized pnl = %v", snap.RealizedPNL)
	}
	if len(snap.Slots) != 2 {
		t.Fatalf("slots = %d, want window/occupied only", len(snap.Slots))
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
	if _, ok := byPrice[98]; ok {
		t.Fatal("idle slot outside the window should be omitted from the dashboard snapshot")
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

func TestSnapshotExposesSlotWaitStatesAndOrderCapacity(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.OrderCleanupThreshold = 5
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	spm.lastMarketPrice.Store(100.0)

	pending := spm.getOrCreateSlot(99)
	pending.ClientOID = "9900_B_1700000000001"
	pending.OrderSide = "BUY"
	pending.OrderStatus = OrderStatusNotPlaced
	pending.OrderQuantity = 0.3
	pending.OrderCreatedAt = time.Now()
	pending.SlotStatus = SlotStatusPending

	canceling := spm.getOrCreateSlot(98)
	canceling.OrderID = 12
	canceling.ClientOID = "9800_B_1700000000002"
	canceling.OrderSide = "BUY"
	canceling.OrderStatus = OrderStatusCancelRequested
	canceling.SlotStatus = SlotStatusLocked

	cooling := spm.getOrCreateSlot(101)
	cooling.placementRetryNotBefore = time.Now().Add(time.Second)

	inconsistent := spm.getOrCreateSlot(102)
	inconsistent.OrderID = 13
	inconsistent.ClientOID = "10200_S_1700000000003"
	inconsistent.OrderSide = "SELL"
	inconsistent.OrderStatus = OrderStatusCanceled
	inconsistent.SlotStatus = SlotStatusLocked

	snap := spm.Snapshot()
	if snap.PendingOrderCount != 1 || snap.CancelingOrderCount != 1 ||
		snap.CoolingSlotCount != 1 || snap.InconsistentSlotCount != 1 {
		t.Fatalf("wait counts = pending:%d cancel:%d cooling:%d inconsistent:%d",
			snap.PendingOrderCount, snap.CancelingOrderCount, snap.CoolingSlotCount, snap.InconsistentSlotCount)
	}
	if snap.OrderCapacityUsed != 3 || snap.OrderCapacityLimit != 5 || snap.OrderCapacityRemaining != 2 {
		t.Fatalf("capacity = %d/%d remaining %d", snap.OrderCapacityUsed, snap.OrderCapacityLimit, snap.OrderCapacityRemaining)
	}
	states := make(map[float64]string)
	retryRemaining := 0.0
	for _, slot := range snap.Slots {
		states[slot.Price] = slot.WaitState
		if slot.Price == 101 {
			retryRemaining = slot.RetryRemainingS
		}
	}
	if states[99] != SlotWaitStatePendingConfirmation || states[98] != SlotWaitStateCancelConfirmation ||
		states[101] != SlotWaitStateRetryCooldown || states[102] != SlotWaitStateLockedInconsistent {
		t.Fatalf("slot wait states = %#v", states)
	}
	if retryRemaining <= 0 || retryRemaining > 1 {
		t.Fatalf("retry remaining = %v", retryRemaining)
	}
}

func TestSnapshotKeepsOccupiedSlotsOutsideWindow(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	spm.lastMarketPrice.Store(100.0)

	filled := spm.getOrCreateSlot(80)
	filled.PositionStatus = PositionStatusFilled
	filled.PositionQty = 0.2
	pending := spm.getOrCreateSlot(70)
	pending.SlotStatus = SlotStatusPending
	pending.ClientOID = "pending-70"
	pending.OrderSide = "BUY"

	snap := spm.Snapshot()
	byPrice := map[float64]SlotSnapshot{}
	for _, sl := range snap.Slots {
		byPrice[sl.Price] = sl
	}
	if snap.FilledSlotCount != 1 || snap.PositionQty != 0.2 {
		t.Fatalf("occupied totals = filled:%d qty:%v", snap.FilledSlotCount, snap.PositionQty)
	}
	if _, ok := byPrice[80]; !ok {
		t.Fatal("filled slot outside the window was omitted")
	}
	if _, ok := byPrice[70]; !ok {
		t.Fatal("pending slot outside the window was omitted")
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
