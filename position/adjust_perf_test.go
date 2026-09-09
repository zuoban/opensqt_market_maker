package position

import (
	"errors"
	"testing"
	"time"

	"opensqt/safety"
)

func slotExists(spm *SuperPositionManager, price float64) bool {
	found := false
	spm.IterateSlots(func(p float64, _ safety.SlotInfo) bool {
		if p == price {
			found = true
			return false
		}
		return true
	})
	return found
}

func TestShouldSkipUnchangedGridAfterAdjust(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	if spm.ShouldSkipUnchangedGrid(100) {
		t.Fatal("ShouldSkipUnchangedGrid() = true before any AdjustOrders")
	}
	if spm.ShouldSkipUnchangedGrid(0) {
		t.Fatal("ShouldSkipUnchangedGrid(0) = true")
	}

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if !spm.ShouldSkipUnchangedGrid(100.05) {
		t.Fatal("same-grid tick inside the buy safety pad should be skippable")
	}
	if spm.ShouldSkipUnchangedGrid(100.4) {
		t.Fatal("same-grid tick that crosses the buy safety pad should not be skipped")
	}
	if spm.ShouldSkipUnchangedGrid(100.6) {
		t.Fatal("price that moves the nearest grid should not be skipped")
	}
}

func TestShouldSkipUnchangedGridDoesNotSkipSellWindowBoundaryCrossing(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 1
	cfg.Trading.SellWindowSize = 1
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	filled := spm.getOrCreateSlot(101)
	filled.PositionStatus = PositionStatusFilled
	filled.PositionQty = 0.1
	filled.SlotStatus = SlotStatusFree

	// Both prices map to grid 100 and keep the same buy/maker bounds. The sell
	// window max nevertheless moves from 100.95 to 101.05, making slot 101
	// newly eligible and requiring another adjustment.
	spm.storeAdjustFingerprint(100, 99.95)
	if spm.ShouldSkipUnchangedGrid(100.05) {
		t.Fatal("sell-window boundary crossing should not be skipped")
	}
}

func TestRecycleIdleSlotsOutsideBuyWindow(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	idle := spm.getOrCreateSlot(90)
	if idle.SlotStatus != SlotStatusFree {
		t.Fatalf("idle slot status = %s", idle.SlotStatus)
	}
	spm.getOrCreateSlot(95)

	filled := spm.getOrCreateSlot(80)
	filled.PositionStatus = PositionStatusFilled
	filled.PositionQty = 0.1

	pending := spm.getOrCreateSlot(70)
	pending.SlotStatus = SlotStatusPending
	pending.ClientOID = "pending-oid"
	pending.OrderSide = "BUY"

	cooling := spm.getOrCreateSlot(85)
	cooling.placementRetryNotBefore = time.Now().Add(time.Hour)

	canceled := spm.getOrCreateSlot(91)
	canceled.OrderStatus = OrderStatusCanceled
	canceled.OrderSide = "BUY"
	canceled.SlotStatus = SlotStatusFree

	canceledFilled := spm.getOrCreateSlot(81)
	canceledFilled.OrderStatus = OrderStatusCanceled
	canceledFilled.OrderSide = "BUY"
	canceledFilled.PositionStatus = PositionStatusFilled
	canceledFilled.PositionQty = 0.1
	canceledFilled.SlotStatus = SlotStatusFree

	spm.getOrCreateSlot(99)

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}

	if slotExists(spm, 90) || slotExists(spm, 95) || slotExists(spm, 91) {
		t.Fatal("empty free slots outside the buy window were not recycled")
	}
	if !slotExists(spm, 80) {
		t.Fatal("filled slot was recycled")
	}
	if !slotExists(spm, 81) {
		t.Fatal("canceled slot with position was recycled")
	}
	if !slotExists(spm, 70) {
		t.Fatal("pending slot was recycled")
	}
	if !slotExists(spm, 85) {
		t.Fatal("slot in placement cooldown was recycled")
	}
	if !slotExists(spm, 99) || !slotExists(spm, 100) {
		t.Fatal("buy-window slots were recycled")
	}
}

func TestAdjustOrdersRetainsBuyInOneGridLowerHysteresisBand(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	retained := spm.getOrCreateSlot(98) // 目标窗口 [100, 99] 下方第一格
	retained.OrderSide = "BUY"
	retained.OrderStatus = OrderStatusConfirmed
	retained.OrderID = 8
	retained.ClientOID = "hysteresis-buy"
	retained.SlotStatus = SlotStatusLocked

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.cancelIDs) != 0 {
		t.Fatalf("cancelIDs = %v, want none inside lower one-grid hysteresis band", executor.cancelIDs)
	}
	if retained.OrderStatus != OrderStatusConfirmed || retained.SlotStatus != SlotStatusLocked {
		t.Fatalf("hysteresis buy was mutated: status=%s slot=%s", retained.OrderStatus, retained.SlotStatus)
	}
}

func TestAdjustOrdersCancelsBuysBeyondWindowHysteresis(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	stale := spm.getOrCreateSlot(97)
	stale.OrderSide = "BUY"
	stale.OrderStatus = OrderStatusConfirmed
	stale.OrderID = 7
	stale.ClientOID = "stale-97"
	stale.SlotStatus = SlotStatusLocked

	inWindow := spm.getOrCreateSlot(99)
	inWindow.OrderSide = "BUY"
	inWindow.OrderStatus = OrderStatusPlaced
	inWindow.OrderID = 9
	inWindow.ClientOID = "live-99"
	inWindow.SlotStatus = SlotStatusLocked

	partial := spm.getOrCreateSlot(96)
	partial.OrderSide = "BUY"
	partial.OrderStatus = OrderStatusPartiallyFilled
	partial.OrderID = 6
	partial.ClientOID = "partial-96"
	partial.SlotStatus = SlotStatusLocked

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.cancelIDs) != 1 || executor.cancelIDs[0] != 7 {
		t.Fatalf("cancelIDs = %v, want [7]", executor.cancelIDs)
	}
	if stale.OrderStatus != OrderStatusCancelRequested {
		t.Fatalf("stale buy status = %s, want CANCEL_REQUESTED", stale.OrderStatus)
	}
	if inWindow.OrderStatus != OrderStatusPlaced {
		t.Fatalf("in-window buy was cancelled: %s", inWindow.OrderStatus)
	}
	if partial.OrderStatus != OrderStatusPartiallyFilled {
		t.Fatalf("partial buy was cancelled: %s", partial.OrderStatus)
	}
}

func TestAdjustOrdersFreesQuotaWhenCancellingOutOfWindowBuys(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.OrderCleanupThreshold = 1
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	stale := spm.getOrCreateSlot(97)
	stale.OrderSide = "BUY"
	stale.OrderStatus = OrderStatusPlaced
	stale.OrderID = 7
	stale.ClientOID = "stale-97"
	stale.SlotStatus = SlotStatusLocked

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.cancelIDs) != 1 || executor.cancelIDs[0] != 7 {
		t.Fatalf("cancelIDs = %v, want [7]", executor.cancelIDs)
	}
	if len(executor.orders) == 0 {
		t.Fatal("cancelled out-of-window buy did not free quota for a new window order")
	}
}

func TestCommitOutOfWindowBuyCancelsFollowsPlacedToConfirmed(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	slot := spm.getOrCreateSlot(97)
	slot.OrderSide = "BUY"
	slot.OrderStatus = OrderStatusConfirmed
	slot.OrderID = 7
	slot.ClientOID = "stale-97"
	slot.SlotStatus = SlotStatusLocked

	ids, leftover, _ := spm.commitOutOfWindowBuyCancels([]outOfWindowBuy{{
		price:     97,
		orderID:   7,
		clientOID: "stale-97",
	}})
	if leftover {
		t.Fatal("PLACED→CONFIRMED promotion reported leftover live buy")
	}
	if len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("cancel IDs = %v, want [7]", ids)
	}
	if slot.OrderStatus != OrderStatusCancelRequested {
		t.Fatalf("status = %s, want CANCEL_REQUESTED", slot.OrderStatus)
	}
}

func TestAdjustOrdersCancelsBuyPromotedFromPlacedToConfirmed(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	stale := spm.getOrCreateSlot(97)
	stale.OrderSide = "BUY"
	stale.OrderStatus = OrderStatusPlaced
	stale.OrderID = 7
	stale.ClientOID = "stale-97"
	stale.SlotStatus = SlotStatusLocked

	spm.beforeCommitOutOfWindowBuys = func() {
		stale.mu.Lock()
		stale.OrderStatus = OrderStatusConfirmed
		stale.mu.Unlock()
	}

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.cancelIDs) != 1 || executor.cancelIDs[0] != 7 {
		t.Fatalf("cancelIDs = %v, want [7]", executor.cancelIDs)
	}
	if stale.OrderStatus != OrderStatusCancelRequested {
		t.Fatalf("status = %s, want CANCEL_REQUESTED", stale.OrderStatus)
	}
	if !spm.ShouldSkipUnchangedGrid(100) {
		t.Fatal("successful out-of-window cancel should still set skip fingerprint")
	}
}

func TestAdjustOrdersDoesNotSkipWhenOutOfWindowLiveBuyRemains(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("seed AdjustOrders() error = %v", err)
	}
	if !spm.ShouldSkipUnchangedGrid(100) {
		t.Fatal("seed adjust did not set skip fingerprint")
	}

	stale := spm.getOrCreateSlot(97)
	stale.OrderSide = "BUY"
	stale.OrderStatus = OrderStatusPlaced
	stale.OrderID = 7
	stale.ClientOID = "stale-97"
	stale.SlotStatus = SlotStatusLocked

	spm.beforeCommitOutOfWindowBuys = func() {
		stale.mu.Lock()
		stale.OrderID = 8
		stale.ClientOID = "stale-97-new"
		stale.OrderStatus = OrderStatusConfirmed
		stale.mu.Unlock()
	}

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.cancelIDs) != 0 {
		t.Fatalf("cancelIDs = %v, want none for identity mismatch", executor.cancelIDs)
	}
	if stale.OrderStatus != OrderStatusConfirmed || stale.OrderID != 8 {
		t.Fatalf("live buy overwritten: id=%d status=%s", stale.OrderID, stale.OrderStatus)
	}
	if spm.ShouldSkipUnchangedGrid(100) {
		t.Fatal("uncommitted out-of-window live buy must not freeze skip fingerprint")
	}
}

func TestAdjustOrdersDoesNotPlaceWhenOutOfWindowCancelFails(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.OrderCleanupThreshold = 1
	wantErr := errors.New("cancel failed")
	executor := &recordingExecutor{cancelErr: wantErr}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	stale := spm.getOrCreateSlot(97)
	stale.OrderSide = "BUY"
	stale.OrderStatus = OrderStatusConfirmed
	stale.OrderID = 7
	stale.ClientOID = "stale-97"
	stale.SlotStatus = SlotStatusLocked

	err := spm.AdjustOrders(100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("AdjustOrders() error = %v, want cancel failure", err)
	}
	if len(executor.cancelIDs) != 1 || executor.cancelIDs[0] != 7 {
		t.Fatalf("cancelIDs = %v, want [7]", executor.cancelIDs)
	}
	if len(executor.orders) != 0 {
		t.Fatalf("placed %d orders after cancel REST failed", len(executor.orders))
	}
	if stale.OrderStatus != OrderStatusConfirmed {
		t.Fatalf("status = %s, want CONFIRMED so the buy can be cancelled again", stale.OrderStatus)
	}
	if stale.SlotStatus != SlotStatusLocked || stale.OrderID != 7 {
		t.Fatalf("stale buy identity was dropped: id=%d status=%s", stale.OrderID, stale.SlotStatus)
	}
	if spm.ShouldSkipUnchangedGrid(100) {
		t.Fatal("failed out-of-window cancel must not freeze skip fingerprint")
	}
}
