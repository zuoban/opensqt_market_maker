package position

import (
	"math"
	"testing"

	"opensqt/utils"
)

func setupOrderSlot(t *testing.T, side string, positionQty float64) (*SuperPositionManager, *InventorySlot, string) {
	t.Helper()
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	oid := utils.GenerateOrderID(100, side, 2)
	slot := spm.getOrCreateSlot(100)
	slot.OrderID = 123
	slot.ClientOID = oid
	slot.OrderSide = side
	slot.OrderStatus = OrderStatusPlaced
	slot.OrderPrice = 101
	slot.SlotStatus = SlotStatusLocked
	slot.PositionQty = positionQty
	if positionQty > 0 {
		slot.PositionStatus = PositionStatusFilled
	}
	return spm, slot, oid
}

func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("%s = %.12f, want %.12f", name, got, want)
	}
}

type terminalBeforeReturnExecutor struct {
	spm         *SuperPositionManager
	executedQty float64
	requests    []*OrderRequest
}

func (e *terminalBeforeReturnExecutor) PlaceOrder(req *OrderRequest) (*Order, error) {
	return nil, nil
}

func (e *terminalBeforeReturnExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	e.requests = append(e.requests, requests...)
	placed := make([]*Order, 0, len(requests))
	for i, req := range requests {
		orderID := int64(1000 + i)
		e.spm.OnOrderUpdate(OrderUpdate{
			OrderID:       orderID,
			ClientOrderID: req.ClientOrderID,
			Status:        "CANCELED",
			ExecutedQty:   e.executedQty,
			AvgPrice:      req.Price,
			UpdateTime:    100,
		})
		placed = append(placed, &Order{
			OrderID:       orderID,
			ClientOrderID: req.ClientOrderID,
			Side:          req.Side,
			Price:         req.Price,
			Quantity:      req.Quantity,
		})
	}
	return placed, false, nil
}

func (e *terminalBeforeReturnExecutor) BatchCancelOrders(orderIDs []int64) error {
	return nil
}

func TestAdjustOrdersDoesNotReviveTerminalBeforeRESTReturn(t *testing.T) {
	for _, side := range []string{"BUY", "SELL"} {
		for _, executedQty := range []float64{0, 0.03} {
			t.Run(side+" execution", func(t *testing.T) {
				cfg := testConfig()
				if side == "BUY" {
					// 向下窗口包含当前格，至少取 2 才会产生一个低于市价的买单。
					cfg.Trading.BuyWindowSize = 2
					cfg.Trading.SellWindowSize = 0
				} else {
					cfg.Trading.BuyWindowSize = 0
					cfg.Trading.SellWindowSize = 1
				}

				executor := &terminalBeforeReturnExecutor{executedQty: executedQty}
				spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
				executor.spm = spm
				spm.anchorPrice = 100
				if side == "SELL" {
					slot := spm.getOrCreateSlot(100)
					slot.PositionStatus = PositionStatusFilled
					slot.PositionQty = 0.1
				}

				if err := spm.AdjustOrders(100); err != nil {
					t.Fatalf("AdjustOrders() error = %v", err)
				}
				if len(executor.requests) != 1 {
					t.Fatalf("placed requests = %d, want 1", len(executor.requests))
				}
				price, gotSide, valid := spm.parseClientOrderID(executor.requests[0].ClientOrderID)
				if !valid || gotSide != side {
					t.Fatalf("parsed request = price:%v side:%s valid:%v", price, gotSide, valid)
				}
				slot := spm.getOrCreateSlot(price)
				if slot.OrderID != 0 || slot.ClientOID != "" || slot.OrderStatus != OrderStatusCanceled || slot.SlotStatus != SlotStatusFree {
					t.Fatalf("terminal order was revived by REST return: %+v", slot)
				}
				if side == "BUY" {
					assertClose(t, "position", slot.PositionQty, executedQty)
					assertClose(t, "total buy", spm.GetTotalBuyQty(), executedQty)
				} else {
					assertClose(t, "position", slot.PositionQty, 0.1-executedQty)
					assertClose(t, "total sell", spm.GetTotalSellQty(), executedQty)
				}
			})
		}
	}
}

func TestTerminalStatesApplyDirectCumulativeExecution(t *testing.T) {
	statuses := []string{"CANCELED", "EXPIRED", "REJECTED"}
	for _, status := range statuses {
		t.Run(status+" buy", func(t *testing.T) {
			spm, slot, oid := setupOrderSlot(t, "BUY", 0)
			spm.OnOrderUpdate(OrderUpdate{
				OrderID: 123, ClientOrderID: oid, Status: status,
				ExecutedQty: 0.03, AvgPrice: 99,
			})

			assertClose(t, "position", slot.PositionQty, 0.03)
			assertClose(t, "total buy", spm.GetTotalBuyQty(), 0.03)
			if slot.PositionStatus != PositionStatusFilled || slot.SlotStatus != SlotStatusFree {
				t.Fatalf("slot state = position:%s slot:%s", slot.PositionStatus, slot.SlotStatus)
			}
			if slot.OrderID != 0 || slot.ClientOID != "" || slot.OrderFilledQty != 0 || slot.OrderStatus != OrderStatusCanceled {
				t.Fatalf("terminal order was not cleared: %+v", slot)
			}
		})

		t.Run(status+" sell", func(t *testing.T) {
			spm, slot, oid := setupOrderSlot(t, "SELL", 0.05)
			spm.OnOrderUpdate(OrderUpdate{
				OrderID: 123, ClientOrderID: oid, Status: status,
				ExecutedQty: 0.03, AvgPrice: 101,
			})

			assertClose(t, "position", slot.PositionQty, 0.02)
			assertClose(t, "total sell", spm.GetTotalSellQty(), 0.03)
			assertClose(t, "fallback pnl", spm.GetRealizedPNL(), 0.03)
			if slot.PositionStatus != PositionStatusFilled || slot.SlotStatus != SlotStatusFree {
				t.Fatalf("slot state = position:%s slot:%s", slot.PositionStatus, slot.SlotStatus)
			}
		})
	}
}

func TestTerminalSellAppliesOnlyNewIncrementAndDeduplicatesReplays(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "SELL", 0.10)
	partial := OrderUpdate{
		OrderID: 123, ClientOrderID: oid, Status: OrderStatusPartiallyFilled,
		ExecutedQty: 0.02, AvgPrice: 101,
		RealizedPNL: 0.03, RealizedPNLIncremental: true,
	}
	spm.OnOrderUpdate(partial)

	terminal := OrderUpdate{
		OrderID: 123, ClientOrderID: oid, Status: "EXPIRED",
		ExecutedQty: 0.05, AvgPrice: 102,
		RealizedPNL: 0.07, RealizedPNLIncremental: true,
	}
	spm.OnOrderUpdate(terminal)
	spm.OnOrderUpdate(terminal)

	stale := partial
	stale.ExecutedQty = 0.03
	stale.RealizedPNL = 0.01
	spm.OnOrderUpdate(stale)

	assertClose(t, "position after replay", slot.PositionQty, 0.05)
	assertClose(t, "total sell after replay", spm.GetTotalSellQty(), 0.05)
	assertClose(t, "pnl after replay", spm.GetRealizedPNL(), 0.10)
	if slot.OrderID != 0 || slot.ClientOID != "" || slot.PostOnlyFailCount != 1 {
		t.Fatalf("replay rebound or recounted terminal order: %+v", slot)
	}

	// 同一终态后续带来更高的权威累计值时，只补记差额。
	corrected := terminal
	corrected.ExecutedQty = 0.06
	corrected.RealizedPNL = 0.02
	spm.OnOrderUpdate(corrected)
	assertClose(t, "corrected position", slot.PositionQty, 0.04)
	assertClose(t, "corrected total sell", spm.GetTotalSellQty(), 0.06)
	assertClose(t, "corrected pnl", spm.GetRealizedPNL(), 0.12)
	if slot.PostOnlyFailCount != 1 {
		t.Fatalf("terminal correction incremented diagnostic counter: %d", slot.PostOnlyFailCount)
	}
}

func TestTerminalSellCumulativePNLIsMonotonicAndCorrectable(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "SELL", 0.08)
	spm.OnOrderUpdate(OrderUpdate{
		OrderID: 123, ClientOrderID: oid, Status: OrderStatusPartiallyFilled,
		ExecutedQty: 0.02, AvgPrice: 101, RealizedPNL: 0.04,
	})
	terminal := OrderUpdate{
		OrderID: 123, ClientOrderID: oid, Status: "REJECTED",
		ExecutedQty: 0.05, AvgPrice: 102, RealizedPNL: 0.11, UpdateTime: 100,
	}
	spm.OnOrderUpdate(terminal)
	spm.OnOrderUpdate(terminal)

	assertClose(t, "position", slot.PositionQty, 0.03)
	assertClose(t, "total sell", spm.GetTotalSellQty(), 0.05)
	assertClose(t, "cumulative pnl", spm.GetRealizedPNL(), 0.11)

	// 数量不变但交易所修正累计 PNL 时，只应用 PNL 差额。
	corrected := terminal
	corrected.RealizedPNL = 0.13
	corrected.UpdateTime = 200
	spm.OnOrderUpdate(corrected)
	assertClose(t, "corrected cumulative pnl", spm.GetRealizedPNL(), 0.13)
	assertClose(t, "unchanged total sell", spm.GetTotalSellQty(), 0.05)

	stale := terminal
	stale.UpdateTime = 150
	spm.OnOrderUpdate(stale)
	assertClose(t, "stale cumulative pnl", spm.GetRealizedPNL(), 0.13)
}

func TestTerminalSellFallbackIsReplacedByCumulativePNL(t *testing.T) {
	spm, _, oid := setupOrderSlot(t, "SELL", 0.10)
	spm.OnOrderUpdate(OrderUpdate{
		OrderID: 123, ClientOrderID: oid, Status: OrderStatusPartiallyFilled,
		ExecutedQty: 0.05, AvgPrice: 102, UpdateTime: 100,
	})
	assertClose(t, "fallback pnl", spm.GetRealizedPNL(), 0.10)

	terminal := OrderUpdate{
		OrderID: 123, ClientOrderID: oid, Status: "CANCELED",
		ExecutedQty: 0.05, AvgPrice: 102, RealizedPNL: 0.11, UpdateTime: 200,
	}
	spm.OnOrderUpdate(terminal)
	assertClose(t, "authoritative pnl", spm.GetRealizedPNL(), 0.11)

	corrected := terminal
	corrected.RealizedPNL = 0.13
	corrected.UpdateTime = 300
	spm.OnOrderUpdate(corrected)
	assertClose(t, "newer correction", spm.GetRealizedPNL(), 0.13)

	stale := terminal
	stale.UpdateTime = 250
	spm.OnOrderUpdate(stale)
	assertClose(t, "stale correction", spm.GetRealizedPNL(), 0.13)
}

func TestTerminalCorrectionDoesNotMutateNewOrderBinding(t *testing.T) {
	for _, slotStatus := range []string{SlotStatusPending, SlotStatusLocked} {
		t.Run(slotStatus, func(t *testing.T) {
			spm, slot, oldOID := setupOrderSlot(t, "SELL", 0.10)
			terminal := OrderUpdate{
				OrderID: 123, ClientOrderID: oldOID, Status: "CANCELED",
				ExecutedQty: 0.05, AvgPrice: 102, RealizedPNL: 0.11, UpdateTime: 100,
			}
			spm.OnOrderUpdate(terminal)

			newOID := ""
			newOrderID := int64(0)
			newOrderStatus := slot.OrderStatus
			if slotStatus == SlotStatusLocked {
				newOID = utils.GenerateOrderID(100, "SELL", 2)
				newOrderID = 456
				newOrderStatus = OrderStatusPlaced
			}
			slot.ClientOID = newOID
			slot.OrderID = newOrderID
			slot.OrderStatus = newOrderStatus
			slot.OrderSide = "SELL"
			slot.OrderFilledQty = 0.01
			slot.SlotStatus = slotStatus
			slot.orderAccumulatedPNL = 0.7

			corrected := terminal
			corrected.ExecutedQty = 0.06
			corrected.RealizedPNL = 0.13
			corrected.UpdateTime = 200
			spm.OnOrderUpdate(corrected)

			assertClose(t, "position", slot.PositionQty, 0.04)
			assertClose(t, "total sell", spm.GetTotalSellQty(), 0.06)
			assertClose(t, "pnl", spm.GetRealizedPNL(), 0.13)
			if slot.ClientOID != newOID || slot.OrderID != newOrderID || slot.OrderStatus != newOrderStatus ||
				slot.SlotStatus != slotStatus || slot.OrderFilledQty != 0.01 || slot.orderAccumulatedPNL != 0.7 {
				t.Fatalf("old terminal correction mutated new order: %+v", slot)
			}
		})
	}
}

func TestTerminalOrderProgressRetainsCompleteHistory(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	const count = formerTerminalHistoryLimit + 1
	for i := 1; i <= count; i++ {
		spm.storeTerminalOrderProgress(OrderUpdate{OrderID: int64(i)}, terminalOrderProgress{ExecutedQty: 1})
	}
	if len(spm.terminalOrders) != count {
		t.Fatalf("terminal ledger size = %d, want %d", len(spm.terminalOrders), count)
	}
	if got, exists := spm.getTerminalOrderProgress(OrderUpdate{OrderID: 1}); !exists || got.ExecutedQty != 1 {
		t.Fatal("oldest terminal progress was lost")
	}
}

func TestTerminalLowerCumulativeStillFinalizesWithoutRegression(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	spm.OnOrderUpdate(OrderUpdate{
		OrderID: 123, ClientOrderID: oid, Status: OrderStatusPartiallyFilled,
		ExecutedQty: 0.04, AvgPrice: 99,
	})
	spm.OnOrderUpdate(OrderUpdate{
		OrderID: 123, ClientOrderID: oid, Status: "CANCELED",
		ExecutedQty: 0.03, AvgPrice: 99,
	})

	assertClose(t, "position", slot.PositionQty, 0.04)
	assertClose(t, "total buy", spm.GetTotalBuyQty(), 0.04)
	if slot.OrderID != 0 || slot.ClientOID != "" || slot.SlotStatus != SlotStatusFree || slot.PositionStatus != PositionStatusFilled {
		t.Fatalf("stale terminal did not finalize safely: %+v", slot)
	}
}
