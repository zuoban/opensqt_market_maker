package position

import (
	"context"
	"math"
	"testing"

	"opensqt/exchange"
)

func TestSnapshotIncludesMinimumQuantityPosition(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	spm.lastMarketPrice.Store(100.0)
	slot := spm.getOrCreateSlot(120)
	slot.PositionStatus = PositionStatusFilled
	slot.PositionQty = 0.001
	snap := spm.Snapshot()
	if snap.PositionQty != 0.001 || snap.FilledSlotCount != 1 || len(snap.Slots) != 1 {
		t.Fatalf("0.001 position omitted: quantity=%v filledSlots=%d visibleSlots=%d", snap.PositionQty, snap.FilledSlotCount, len(snap.Slots))
	}
}

func TestSnapshotIncludesPartialBuyInventory(t *testing.T) {
	spm, _, oid := setupOrderSlot(t, "BUY", 0)
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: OrderStatusPartiallyFilled, Quantity: 0.1, ExecutedQty: 0.04, Price: 100, AvgPrice: 100})
	snap := spm.Snapshot()
	if snap.PositionQty != 0.04 {
		t.Fatalf("partial buy inventory omitted: snapshot quantity=%v, want 0.04", snap.PositionQty)
	}
}

func TestTerminalReplayAfterFormerRetentionLimit(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	terminal := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "CANCELED", Quantity: 0.1, ExecutedQty: 0.04, AvgPrice: 100, UpdateTime: 100}
	spm.OnOrderUpdate(terminal)
	for i := 0; i < formerTerminalHistoryLimit; i++ {
		spm.storeTerminalOrderProgress(OrderUpdate{OrderID: int64(1000 + i)}, terminalOrderProgress{})
	}
	spm.OnOrderUpdate(terminal)
	if slot.PositionQty != 0.04 || spm.GetTotalBuyQty() != 0.04 {
		t.Fatalf("replayed terminal duplicated inventory: position=%v totalBuy=%v, want 0.04 each", slot.PositionQty, spm.GetTotalBuyQty())
	}
}

func TestIncrementalZeroPNLPreservesExchangeValue(t *testing.T) {
	spm, _, oid := setupOrderSlot(t, "SELL", 0.02)
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: OrderStatusFilled, Quantity: 0.02, ExecutedQty: 0.02, AvgPrice: 101, RealizedPNL: 0, RealizedPNLIncremental: true})
	if got := spm.GetRealizedPNL(); got != 0 {
		t.Fatalf("explicit exchange zero replaced with synthetic profit: %v, want 0", got)
	}
}

func TestRestoreSmallQuotedOrderPreservesPosition(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.Symbol = "BTCUSDT"
	cfg.Trading.OrderQuantity = 30
	cfg.Trading.MinOrderValue = 20
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100000
	if got := spm.gridBuyQuantity(100000); got != 0.001 {
		t.Fatalf("fixture: normal buy must be valid 0.001, got %v", got)
	}
	if err := spm.initializeSellSlotsFromPosition(0.001, 100000); err != nil {
		t.Fatal(err)
	}
	var quantity float64
	spm.forEachSlot(func(_ float64, slot *InventorySlot) bool { quantity += slot.PositionQty; return true })
	if quantity != 0.001 {
		t.Fatalf("startup lost exchange position: restored=%v, want 0.001", quantity)
	}
}

// This was the production eviction threshold; crossing it must never reset accounting.
const formerTerminalHistoryLimit = 4096

func TestTerminalZeroIncrementalPNLDoesNotEstimateProfit(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "SELL", 0.1)
	first := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "CANCELED", ExecutedQty: 0.04, AvgPrice: 101, RealizedPNL: 0.05, RealizedPNLIncremental: true, UpdateTime: 100}
	spm.OnOrderUpdate(first)
	corrected := first
	corrected.ExecutedQty = 0.06
	corrected.RealizedPNL = 0
	corrected.UpdateTime = 200
	spm.OnOrderUpdate(corrected)
	spm.OnOrderUpdate(corrected)
	assertClose(t, "position", slot.PositionQty, 0.04)
	assertClose(t, "pnl", spm.GetRealizedPNL(), 0.05)
	assertClose(t, "sell quantity", spm.GetTotalSellQty(), 0.06)
}

func TestTerminalCorrectionAfterFormerLimitPreservesNewBinding(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	first := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "CANCELED", ExecutedQty: 0.04, AvgPrice: 100, UpdateTime: 100}
	spm.OnOrderUpdate(first)
	for i := 0; i < formerTerminalHistoryLimit; i++ {
		spm.storeTerminalOrderProgress(OrderUpdate{OrderID: int64(1000 + i)}, terminalOrderProgress{})
	}
	newOID := spm.generateClientOrderID(100, "SELL")
	slot.ClientOID, slot.OrderID, slot.OrderSide, slot.OrderStatus, slot.SlotStatus = newOID, 456, "SELL", OrderStatusPlaced, SlotStatusLocked
	next := first
	next.ExecutedQty = 0.05
	next.UpdateTime = 200
	spm.OnOrderUpdate(next)
	spm.OnOrderUpdate(next)
	assertClose(t, "position", slot.PositionQty, 0.05)
	assertClose(t, "total buy", spm.GetTotalBuyQty(), 0.05)
	if slot.ClientOID != newOID || slot.OrderID != 456 || slot.OrderSide != "SELL" || slot.OrderStatus != OrderStatusPlaced || slot.SlotStatus != SlotStatusLocked {
		t.Fatal("late correction replaced new order binding")
	}
}

func TestRestoreRejectsInvalidScaleBeforeCreatingSlots(t *testing.T) {
	for _, tt := range []struct {
		name                              string
		position, anchor, interval, entry float64
	}{
		{"nan quantity", math.NaN(), 100, 1, 100},
		{"infinite quantity", math.Inf(1), 100, 1, 100},
		{"negative quantity", -1, 100, 1, 100},
		{"zero anchor", 1, 0, 1, 100},
		{"nan anchor", 1, math.NaN(), 1, 100},
		{"too many slots", 1000000, 100, 1, 100},
		{"invalid interval", 1, 100, math.Inf(1), 100},
		{"cost overflow", 2, 100, 1, math.MaxFloat64},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Trading.PriceInterval = tt.interval
			spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)
			spm.anchorPrice = tt.anchor
			if err := spm.initializeSellSlotsFromPosition(tt.position, tt.entry); err == nil {
				t.Fatal("invalid restore succeeded")
			}
			if len(spm.slotIndex.snapshot()) != 0 {
				t.Fatal("invalid restore published inventory")
			}
		})
	}
}

type oversizedRestoreExchange struct{ stubEx }

func (oversizedRestoreExchange) GetPositions(_ context.Context, symbol string) (interface{}, error) {
	return []*exchange.Position{{Symbol: symbol, Size: 1000000, EntryPrice: 100}}, nil
}

func TestInitializePropagatesRestoreFailure(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, oversizedRestoreExchange{}, 2, 3)
	if err := spm.Initialize(100, "100"); err == nil {
		t.Fatal("initialization ignored restore failure")
	}
	if spm.isInitialized.Load() {
		t.Fatal("failed restore marked initialized")
	}
}
