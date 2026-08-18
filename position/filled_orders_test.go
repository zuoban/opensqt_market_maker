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
}

func TestFilledOrdersKeepNewestRecords(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	for i := 1; i <= maxFilledOrderRecords+1; i++ {
		spm.recordFilledOrder(OrderUpdate{
			OrderID:       int64(i),
			ClientOrderID: fmt.Sprintf("filled-%d", i),
			ExecutedQty:   0.01,
			AvgPrice:      float64(i),
		}, "BUY", 0, 0, 0)
	}

	snap := spm.Snapshot()
	if len(snap.FilledOrders) != maxFilledOrderRecords {
		t.Fatalf("filled orders = %d, want %d", len(snap.FilledOrders), maxFilledOrderRecords)
	}
	if snap.FilledOrderCount != maxFilledOrderRecords+1 {
		t.Fatalf("filled order count = %d, want %d", snap.FilledOrderCount, maxFilledOrderRecords+1)
	}
	if snap.FilledOrders[0].OrderID != int64(maxFilledOrderRecords+1) {
		t.Fatalf("newest order id = %d", snap.FilledOrders[0].OrderID)
	}
	if snap.FilledOrders[len(snap.FilledOrders)-1].OrderID != 2 {
		t.Fatalf("oldest retained order id = %d", snap.FilledOrders[len(snap.FilledOrders)-1].OrderID)
	}
}
