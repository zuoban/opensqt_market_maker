package position

import (
	"testing"

	"opensqt/utils"
)

func TestApplySellRealizedPNLFromExchange(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	slot := spm.getOrCreateSlot(100)
	slot.PositionStatus = PositionStatusFilled
	slot.PositionQty = 0.02
	slot.SlotStatus = SlotStatusLocked

	oid := utils.GenerateOrderID(100, "SELL", 2)
	slot.ClientOID = oid
	slot.OrderSide = "SELL"
	slot.OrderStatus = OrderStatusPlaced

	spm.OnOrderUpdate(OrderUpdate{
		ClientOrderID:          oid,
		Status:                 "PARTIALLY_FILLED",
		Side:                   "SELL",
		ExecutedQty:            0.01,
		AvgPrice:               101,
		RealizedPNL:            0.05,
		RealizedPNLIncremental: true,
	})
	if got := spm.GetRealizedPNL(); got != 0.05 {
		t.Fatalf("after first incremental fill: %v", got)
	}

	spm.OnOrderUpdate(OrderUpdate{
		ClientOrderID:          oid,
		Status:                 "FILLED",
		Side:                   "SELL",
		ExecutedQty:            0.02,
		AvgPrice:               101,
		RealizedPNL:            0.07,
		RealizedPNLIncremental: true,
	})
	if got := spm.GetRealizedPNL(); got < 0.12-1e-12 || got > 0.12+1e-12 {
		t.Fatalf("after incremental fills: %v", got)
	}
}

func TestApplySellRealizedPNLCumulative(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	slot := spm.getOrCreateSlot(100)
	slot.PositionStatus = PositionStatusFilled
	slot.PositionQty = 0.02
	oid := utils.GenerateOrderID(100, "SELL", 2)
	slot.ClientOID = oid
	slot.OrderSide = "SELL"

	spm.OnOrderUpdate(OrderUpdate{
		ClientOrderID: oid,
		Status:        "PARTIALLY_FILLED",
		Side:          "SELL",
		ExecutedQty:   0.01,
		AvgPrice:      101,
		RealizedPNL:   0.05,
	})
	spm.OnOrderUpdate(OrderUpdate{
		ClientOrderID: oid,
		Status:        "FILLED",
		Side:          "SELL",
		ExecutedQty:   0.02,
		AvgPrice:      101,
		RealizedPNL:   0.12,
	})
	if got := spm.GetRealizedPNL(); got != 0.12 {
		t.Fatalf("cumulative should not double count: %v", got)
	}
}

func TestApplySellRealizedPNLFallbackSpread(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	slot := spm.getOrCreateSlot(100)
	slot.PositionStatus = PositionStatusFilled
	slot.PositionQty = 0.01
	oid := utils.GenerateOrderID(100, "SELL", 2)
	slot.ClientOID = oid
	slot.OrderSide = "SELL"

	spm.OnOrderUpdate(OrderUpdate{
		ClientOrderID: oid,
		Status:        "FILLED",
		Side:          "SELL",
		ExecutedQty:   0.01,
		AvgPrice:      101.5,
	})
	want := 0.01 * 1.5
	if got := spm.GetRealizedPNL(); got < want-1e-12 || got > want+1e-12 {
		t.Fatalf("fallback spread = %v, want %v", got, want)
	}
}
