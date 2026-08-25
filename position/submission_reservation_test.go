package position

import (
	"errors"
	"fmt"
	"testing"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/utils"
)

type leaseAwareExecutor struct {
	beforeAcquire func(*OrderRequest)
	submitted     []submittedOrder
	nextOrderID   int64
}

type submittedOrder struct {
	clientOrderID string
	side          string
	quantity      float64
}

func (e *leaseAwareExecutor) PlaceOrder(req *OrderRequest) (*Order, error) {
	return nil, nil
}

func (e *leaseAwareExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	placed := make([]*Order, 0, len(requests))
	for _, req := range requests {
		if e.beforeAcquire != nil {
			e.beforeAcquire(req)
		}
		release, ok := req.AcquireSubmissionLease()
		if !ok {
			continue
		}
		e.submitted = append(e.submitted, submittedOrder{
			clientOrderID: req.ClientOrderID,
			side:          req.Side,
			quantity:      req.Quantity,
		})
		e.nextOrderID++
		orderID := e.nextOrderID
		release()
		placed = append(placed, &Order{
			OrderID:       orderID,
			ClientOrderID: req.ClientOrderID,
			Symbol:        req.Symbol,
			Side:          req.Side,
			Price:         req.Price,
			Quantity:      req.Quantity,
			Status:        "NEW",
		})
	}
	return placed, false, nil
}

func (e *leaseAwareExecutor) BatchCancelOrders([]int64) error { return nil }

type unknownLeaseExecutor struct {
	request *OrderRequest
}

func (e *unknownLeaseExecutor) PlaceOrder(*OrderRequest) (*Order, error) { return nil, nil }

func (e *unknownLeaseExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	if len(requests) == 0 {
		return nil, false, nil
	}
	e.request = requests[0]
	release, ok := e.request.AcquireSubmissionLease()
	if !ok {
		return nil, false, nil
	}
	// 模拟请求已经进入交易所边界后响应丢失。lease 只负责线性化本次调用，
	// UNKNOWN reservation 必须留给后续订单流/对账确认。
	release()
	e.request.MarkSubmissionUncertain()
	return nil, false, fmt.Errorf("response lost: %w", exchange.ErrOrderPlacementUnknown)
}

func (e *unknownLeaseExecutor) BatchCancelOrders([]int64) error { return nil }

type acceptedThenCorrectionExecutor struct {
	afterAcceptance func(*OrderRequest)
	request         *OrderRequest
	orderID         int64
	canceled        []int64
}

func (e *acceptedThenCorrectionExecutor) PlaceOrder(*OrderRequest) (*Order, error) { return nil, nil }

func (e *acceptedThenCorrectionExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	if len(requests) == 0 {
		return nil, false, nil
	}
	e.request = requests[0]
	release, ok := e.request.AcquireSubmissionLease()
	if !ok {
		return nil, false, nil
	}
	if e.orderID == 0 {
		e.orderID = 701
	}
	// 远端已接受订单后 REST 调用返回，lease 立即释放。此时注入
	// 旧订单迟到终态，精确重现“成功回包尚未由 position 处理”的窗口。
	release()
	if e.afterAcceptance != nil {
		e.afterAcceptance(e.request)
	}
	return []*Order{{
		OrderID:       e.orderID,
		ClientOrderID: e.request.ClientOrderID,
		Symbol:        e.request.Symbol,
		Side:          e.request.Side,
		Type:          "LIMIT",
		Price:         e.request.Price,
		Quantity:      e.request.Quantity,
		Status:        "NEW",
	}}, false, nil
}

func (e *acceptedThenCorrectionExecutor) BatchCancelOrders(orderIDs []int64) error {
	e.canceled = append(e.canceled, orderIDs...)
	return nil
}

func bindTerminalOrder(slot *InventorySlot, orderID int64, clientOrderID, side string, quantity float64) {
	slot.OrderID = orderID
	slot.ClientOID = clientOrderID
	slot.OrderSide = side
	slot.OrderStatus = OrderStatusPlaced
	slot.OrderPrice = slot.Price
	slot.OrderQuantity = quantity
	slot.SlotStatus = SlotStatusLocked
}

func TestBuyReservationIsInvalidatedByTerminalFillBeforeSubmission(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 0

	executor := &leaseAwareExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	slot := spm.getOrCreateSlot(99)
	oldOID := utils.GenerateOrderID(99, "BUY", 2)
	bindTerminalOrder(slot, 41, oldOID, "BUY", 0.03)
	spm.OnOrderUpdate(OrderUpdate{
		OrderID: 41, ClientOrderID: oldOID, Status: "CANCELED", UpdateTime: 100,
	})

	executor.beforeAcquire = func(req *OrderRequest) {
		if req.Side != "BUY" {
			return
		}
		spm.OnOrderUpdate(OrderUpdate{
			OrderID: 41, ClientOrderID: oldOID, Status: "CANCELED",
			ExecutedQty: 0.03, AvgPrice: 99, UpdateTime: 200,
		})
	}

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.submitted) != 0 {
		t.Fatalf("stale BUY reached submission boundary: %+v", executor.submitted)
	}
	assertClose(t, "position", slot.PositionQty, 0.03)
	assertClose(t, "total buy", spm.GetTotalBuyQty(), 0.03)
	if slot.PositionStatus != PositionStatusFilled || slot.SlotStatus != SlotStatusFree ||
		slot.ClientOID != "" || slot.OrderID != 0 {
		t.Fatalf("invalidated BUY reservation was not cleared safely: %+v", slot)
	}
}

func TestSellReservationNeverSubmitsQuantityStaleAfterTerminalCorrection(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 0
	cfg.Trading.SellWindowSize = 1

	executor := &leaseAwareExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	slot := spm.getOrCreateSlot(100)
	slot.PositionStatus = PositionStatusFilled
	slot.PositionQty = 0.10
	oldOID := utils.GenerateOrderID(100, "SELL", 2)
	bindTerminalOrder(slot, 52, oldOID, "SELL", 0.10)
	spm.OnOrderUpdate(OrderUpdate{
		OrderID: 52, ClientOrderID: oldOID, Status: "CANCELED", UpdateTime: 100,
	})

	executor.beforeAcquire = func(req *OrderRequest) {
		if req.Side != "SELL" {
			return
		}
		spm.OnOrderUpdate(OrderUpdate{
			OrderID: 52, ClientOrderID: oldOID, Status: "CANCELED",
			ExecutedQty: 0.03, AvgPrice: 101, UpdateTime: 200,
		})
	}

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("first AdjustOrders() error = %v", err)
	}
	if len(executor.submitted) != 0 {
		t.Fatalf("SELL with stale quantity reached submission boundary: %+v", executor.submitted)
	}
	assertClose(t, "corrected position", slot.PositionQty, 0.07)
	if slot.SlotStatus != SlotStatusFree || slot.ClientOID != "" {
		t.Fatalf("invalidated SELL reservation was not cleared: %+v", slot)
	}

	// 下一轮必须基于修正后的持仓创建全新的 reservation，且只提交最新数量。
	executor.beforeAcquire = nil
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("second AdjustOrders() error = %v", err)
	}
	if len(executor.submitted) != 1 {
		t.Fatalf("submitted orders = %d, want 1", len(executor.submitted))
	}
	if executor.submitted[0].side != "SELL" {
		t.Fatalf("submitted side = %s, want SELL", executor.submitted[0].side)
	}
	assertClose(t, "submitted latest quantity", executor.submitted[0].quantity, 0.07)
}

func TestUnknownSubmissionKeepsExactReservationPending(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 0
	executor := &unknownLeaseExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	err := spm.AdjustOrders(100)
	if !errors.Is(err, exchange.ErrOrderPlacementUnknown) {
		t.Fatalf("AdjustOrders() error = %v, want ErrOrderPlacementUnknown", err)
	}
	if executor.request == nil {
		t.Fatal("executor did not receive request")
	}
	price, side, valid := spm.parseClientOrderID(executor.request.ClientOrderID)
	if !valid || side != "BUY" {
		t.Fatalf("request identity = price:%v side:%s valid:%v", price, side, valid)
	}
	slot := spm.getOrCreateSlot(price)
	if slot.SlotStatus != SlotStatusPending || slot.ClientOID != executor.request.ClientOrderID ||
		slot.OrderSide != "BUY" || !sameOrderQuantity(slot.OrderQuantity, executor.request.Quantity) {
		t.Fatalf("UNKNOWN reservation was released or mutated: %+v", slot)
	}
}

func TestAcceptedOrderInvalidatedAfterRESTReturnIsCanceledAndFailsClosed(t *testing.T) {
	tests := []struct {
		name            string
		side            string
		oldExecutedQty  float64
		wantPositionQty float64
		configure       func(*config.Config)
		prepareSlot     func(*InventorySlot)
	}{
		{
			name:            "BUY gains inventory from old terminal correction",
			side:            "BUY",
			oldExecutedQty:  0.03,
			wantPositionQty: 0.03,
			configure: func(cfg *config.Config) {
				cfg.Trading.BuyWindowSize = 2
				cfg.Trading.SellWindowSize = 0
			},
			prepareSlot: func(*InventorySlot) {},
		},
		{
			name:            "SELL quantity shrinks from old terminal correction",
			side:            "SELL",
			oldExecutedQty:  0.03,
			wantPositionQty: 0.07,
			configure: func(cfg *config.Config) {
				cfg.Trading.BuyWindowSize = 0
				cfg.Trading.SellWindowSize = 1
			},
			prepareSlot: func(slot *InventorySlot) {
				slot.PositionStatus = PositionStatusFilled
				slot.PositionQty = 0.10
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			tt.configure(cfg)
			executor := &acceptedThenCorrectionExecutor{}
			spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
			spm.anchorPrice = 100

			slotPrice := 99.0
			if tt.side == "SELL" {
				slotPrice = 100
			}
			slot := spm.getOrCreateSlot(slotPrice)
			tt.prepareSlot(slot)
			oldOID := utils.GenerateOrderID(slotPrice, tt.side, 2)
			bindTerminalOrder(slot, 600, oldOID, tt.side, 0.10)
			spm.OnOrderUpdate(OrderUpdate{
				OrderID: 600, ClientOrderID: oldOID, Status: "CANCELED", UpdateTime: 100,
			})

			executor.afterAcceptance = func(*OrderRequest) {
				spm.OnOrderUpdate(OrderUpdate{
					OrderID: 600, ClientOrderID: oldOID, Status: "CANCELED",
					ExecutedQty: tt.oldExecutedQty, AvgPrice: slotPrice, UpdateTime: 200,
				})
			}

			err := spm.AdjustOrders(100)
			if !errors.Is(err, ErrAcceptedOrderPreconditionInvalid) {
				t.Fatalf("AdjustOrders() error = %v, want ErrAcceptedOrderPreconditionInvalid", err)
			}
			if executor.request == nil {
				t.Fatal("executor did not receive the new order")
			}
			if len(executor.canceled) != 1 || executor.canceled[0] != executor.orderID {
				t.Fatalf("canceled orders = %v, want [%d]", executor.canceled, executor.orderID)
			}
			assertClose(t, "corrected position", slot.PositionQty, tt.wantPositionQty)
			if slot.OrderID != executor.orderID ||
				spm.canonicalClientOrderID(slot.ClientOID) != spm.canonicalClientOrderID(executor.request.ClientOrderID) ||
				slot.OrderStatus != OrderStatusCancelRequested || slot.SlotStatus != SlotStatusLocked {
				t.Fatalf("accepted stale order was exposed as healthy: %+v", slot)
			}
		})
	}
}
