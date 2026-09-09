package position

import (
	"fmt"
	"testing"
	"time"

	"opensqt/utils"
)

func TestFilledOrderRecordedOnce(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	oid := utils.GenerateOrderID(100, "BUY", 2)
	slot := spm.getOrCreateSlot(100)
	slot.ClientOID = oid
	slot.OrderID = 123
	slot.OrderSide = "BUY"
	slot.OrderStatus = OrderStatusPlaced
	slot.OrderPrice = 100.25
	slot.SlotStatus = SlotStatusLocked

	filledAt := time.Date(2026, time.August, 19, 10, 20, 30, 0, time.UTC)
	update := OrderUpdate{
		OrderID:       123,
		ClientOrderID: oid,
		Status:        OrderStatusFilled,
		ExecutedQty:   0.02,
		Price:         100.25,
		AvgPrice:      99.75,
		UpdateTime:    filledAt.UnixMilli(),
	}
	spm.OnOrderUpdate(update)
	spm.OnOrderUpdate(update)

	snap := spm.Snapshot()
	if len(snap.FilledOrders) != 1 {
		t.Fatalf("filled orders = %d, want 1", len(snap.FilledOrders))
	}
	got := snap.FilledOrders[0]
	if got.OrderID != 123 || got.ClientOrderID != oid || got.Symbol != "ETHUSDT" || got.Side != "BUY" {
		t.Fatalf("unexpected filled order: %+v", got)
	}
	if got.Price != 99.75 {
		t.Fatalf("filled price = %v, want average price 99.75", got.Price)
	}
	if got.SlotPrice != 100 || got.TargetPrice != 101 || got.EntryPrice != 99.75 {
		t.Fatalf("buy slot metadata = slot:%v target:%v entry:%v", got.SlotPrice, got.TargetPrice, got.EntryPrice)
	}
	if got.Quantity != 0.02 {
		t.Fatalf("filled quantity = %v, want 0.02", got.Quantity)
	}
	if !got.FilledAt.Equal(filledAt) {
		t.Fatalf("filled time = %s, want %s", got.FilledAt, filledAt)
	}
	if total := spm.GetTotalBuyQty(); total != 0.02 {
		t.Fatalf("duplicate fill changed total buy quantity to %v", total)
	}
}

func TestFilledSellOrderAccumulatesRealizedPNL(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	oid := utils.GenerateOrderID(100, "SELL", 2)
	slot := spm.getOrCreateSlot(100)
	slot.PositionStatus = PositionStatusFilled
	slot.PositionQty = 0.02
	slot.PositionCost = 2
	slot.ClientOID = oid
	slot.OrderSide = "SELL"
	slot.OrderStatus = OrderStatusPlaced
	slot.OrderPrice = 101
	slot.SlotStatus = SlotStatusLocked

	spm.OnOrderUpdate(OrderUpdate{
		ClientOrderID:          oid,
		Status:                 OrderStatusPartiallyFilled,
		ExecutedQty:            0.01,
		AvgPrice:               101,
		RealizedPNL:            0.03,
		RealizedPNLIncremental: true,
	})
	spm.OnOrderUpdate(OrderUpdate{
		ClientOrderID:          oid,
		Status:                 OrderStatusFilled,
		ExecutedQty:            0.02,
		AvgPrice:               101.5,
		RealizedPNL:            0.04,
		RealizedPNLIncremental: true,
	})

	snap := spm.Snapshot()
	if len(snap.FilledOrders) != 1 {
		t.Fatalf("filled orders = %d, want 1", len(snap.FilledOrders))
	}
	got := snap.FilledOrders[0]
	if got.Side != "SELL" || got.Price != 101.5 || got.Quantity != 0.02 {
		t.Fatalf("unexpected filled sell order: %+v", got)
	}
	if got.RealizedPNL < 0.07-1e-12 || got.RealizedPNL > 0.07+1e-12 {
		t.Fatalf("realized pnl = %v, want 0.07", got.RealizedPNL)
	}
	if got.SlotPrice != 100 || got.TargetPrice != 101 || got.EntryPrice != 100 {
		t.Fatalf("sell slot metadata = slot:%v target:%v entry:%v", got.SlotPrice, got.TargetPrice, got.EntryPrice)
	}
	if got.GridPNL < 0.03-1e-12 || got.GridPNL > 0.03+1e-12 {
		t.Fatalf("grid pnl = %v, want 0.03", got.GridPNL)
	}
}

func TestSameExecutionPriceKeepsDistinctSlotAndPNLAccounting(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 0.10
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 102.55

	sellOID := utils.GenerateOrderID(102.45, "SELL", 2)
	sellSlot := spm.getOrCreateSlot(102.45)
	sellSlot.PositionStatus = PositionStatusFilled
	sellSlot.PositionQty = 1.95
	sellSlot.PositionCost = 1.95 * 103.5683
	sellSlot.ClientOID = sellOID
	sellSlot.OrderSide = "SELL"
	sellSlot.OrderStatus = OrderStatusPlaced
	sellSlot.OrderPrice = 102.55
	sellSlot.SlotStatus = SlotStatusLocked

	wantPNL := 1.95 * (102.55 - 103.5683)
	spm.OnOrderUpdate(OrderUpdate{
		OrderID:                1,
		ClientOrderID:          sellOID,
		Status:                 OrderStatusFilled,
		ExecutedQty:            1.95,
		AvgPrice:               102.55,
		RealizedPNL:            wantPNL,
		RealizedPNLIncremental: true,
	})

	buyOID := utils.GenerateOrderID(102.55, "BUY", 2)
	buySlot := spm.getOrCreateSlot(102.55)
	buySlot.ClientOID = buyOID
	buySlot.OrderSide = "BUY"
	buySlot.OrderStatus = OrderStatusPlaced
	buySlot.OrderPrice = 102.55
	buySlot.SlotStatus = SlotStatusLocked
	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       2,
		ClientOrderID: buyOID,
		Status:        OrderStatusFilled,
		ExecutedQty:   1.95,
		AvgPrice:      102.55,
	})

	snap := spm.Snapshot()
	if len(snap.FilledOrders) != 2 {
		t.Fatalf("filled orders = %d, want 2", len(snap.FilledOrders))
	}
	buy, sell := snap.FilledOrders[0], snap.FilledOrders[1]
	if buy.Price != 102.55 || sell.Price != 102.55 {
		t.Fatalf("execution prices = buy:%v sell:%v, want both 102.55", buy.Price, sell.Price)
	}
	if buy.SlotPrice != 102.55 || buy.TargetPrice != 102.65 {
		t.Fatalf("buy route = slot:%v target:%v", buy.SlotPrice, buy.TargetPrice)
	}
	if sell.SlotPrice != 102.45 || sell.TargetPrice != 102.55 {
		t.Fatalf("sell route = slot:%v target:%v", sell.SlotPrice, sell.TargetPrice)
	}
	if mathAbs(sell.EntryPrice-103.5683) > 1e-12 || mathAbs(sell.GridPNL-wantPNL) > 1e-12 ||
		mathAbs(sell.RealizedPNL-wantPNL) > 1e-12 {
		t.Fatalf("sell pnl metadata = entry:%v grid:%v exchange:%v", sell.EntryPrice, sell.GridPNL, sell.RealizedPNL)
	}
}

func TestRestoredPositionDistributesExchangeEntryCost(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	spm.initializeSellSlotsFromPosition(0.60, 103.50)

	var quantity, cost float64
	spm.forEachSlot(func(_ float64, slot *InventorySlot) bool {
		slot.mu.RLock()
		quantity += slot.PositionQty
		cost += slot.PositionCost
		slot.mu.RUnlock()
		return true
	})
	if mathAbs(quantity-0.60) > 1e-12 {
		t.Fatalf("restored quantity = %v, want 0.60", quantity)
	}
	if mathAbs(cost-0.60*103.50) > 1e-9 {
		t.Fatalf("restored cost = %v, want %v", cost, 0.60*103.50)
	}
}

func TestFilledOrdersKeepNewestRecords(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	for i := 1; i <= maxRecentFilledOrders+1; i++ {
		spm.recordFilledOrder(OrderUpdate{
			OrderID:       int64(i),
			ClientOrderID: fmt.Sprintf("filled-%d", i),
			ExecutedQty:   0.01,
			AvgPrice:      float64(i),
		}, "BUY", 0, 0, 0, 0, 0)
	}

	snap := spm.Snapshot()
	if len(snap.FilledOrders) != maxRecentFilledOrders {
		t.Fatalf("filled orders = %d, want %d", len(snap.FilledOrders), maxRecentFilledOrders)
	}
	if snap.FilledOrderCount != maxRecentFilledOrders+1 {
		t.Fatalf("filled order count = %d, want %d", snap.FilledOrderCount, maxRecentFilledOrders+1)
	}
	if snap.FilledOrders[0].OrderID != int64(maxRecentFilledOrders+1) {
		t.Fatalf("newest order id = %d", snap.FilledOrders[0].OrderID)
	}
	if snap.FilledOrders[len(snap.FilledOrders)-1].OrderID != 2 {
		t.Fatalf("oldest retained order id = %d", snap.FilledOrders[len(snap.FilledOrders)-1].OrderID)
	}
}

func TestHourlyFillsKeep24HoursAfterListTrim(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	now := time.Now()
	oldHour := now.Add(-5 * time.Hour)
	spm.recordFilledOrder(OrderUpdate{
		OrderID:       1,
		ClientOrderID: "old-buy",
		ExecutedQty:   0.1,
		AvgPrice:      100,
		UpdateTime:    oldHour.UnixMilli(),
	}, "BUY", 0, 0, 0, 0, 0)
	for i := 2; i <= maxRecentFilledOrders+2; i++ {
		spm.recordFilledOrder(OrderUpdate{
			OrderID:       int64(i),
			ClientOrderID: fmt.Sprintf("new-%d", i),
			ExecutedQty:   0.01,
			AvgPrice:      101,
			UpdateTime:    now.UnixMilli(),
		}, "SELL", 0, 0, 0, 0.01, 0.02)
	}

	snap := spm.Snapshot()
	if len(snap.FilledOrders) != maxRecentFilledOrders {
		t.Fatalf("filled orders = %d, want %d", len(snap.FilledOrders), maxRecentFilledOrders)
	}
	for _, order := range snap.FilledOrders {
		if order.OrderID == 1 {
			t.Fatal("list should not keep the 5-hour-old fill after trim")
		}
	}
	if len(snap.FilledHourly) != hourlyFillHours {
		t.Fatalf("hourly buckets = %d, want %d", len(snap.FilledHourly), hourlyFillHours)
	}

	var oldBucket, latestBucket HourlyFillBucket
	oldKey := startOfLocalHour(oldHour).Unix()
	latestKey := startOfLocalHour(now).Unix()
	for _, bucket := range snap.FilledHourly {
		switch bucket.Hour.Unix() {
		case oldKey:
			oldBucket = bucket
		case latestKey:
			latestBucket = bucket
		}
	}
	if oldBucket.Buy != 1 || oldBucket.Sell != 0 {
		t.Fatalf("old hour bucket = %+v, want buy=1", oldBucket)
	}
	if latestBucket.Sell != maxRecentFilledOrders+1 {
		t.Fatalf("latest hour sell = %d, want %d", latestBucket.Sell, maxRecentFilledOrders+1)
	}
	wantGridPNL := float64(maxRecentFilledOrders+1) * 0.01
	if mathAbs(latestBucket.GridPnl-wantGridPNL) > 1e-12 {
		t.Fatalf("latest hour grid pnl = %v, want %v", latestBucket.GridPnl, wantGridPNL)
	}
	if !snap.FilledHourly[0].Hour.Equal(startOfLocalHour(now).Add(-time.Duration(hourlyFillHours-1) * time.Hour)) {
		t.Fatalf("window start = %s", snap.FilledHourly[0].Hour)
	}
	if !snap.FilledHourly[len(snap.FilledHourly)-1].Hour.Equal(startOfLocalHour(now)) {
		t.Fatalf("window end = %s", snap.FilledHourly[len(snap.FilledHourly)-1].Hour)
	}
}
