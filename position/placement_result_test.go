package position

import "testing"

type authoritativePlacementExecutor struct {
	status      string
	executedQty func(float64) float64
	request     *OrderRequest
	orderID     int64
}

func (e *authoritativePlacementExecutor) PlaceOrder(*OrderRequest) (*Order, error) {
	return nil, nil
}

func (e *authoritativePlacementExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	if len(requests) == 0 {
		return nil, false, nil
	}
	e.request = requests[0]
	release, ok := e.request.AcquireSubmissionLease()
	if !ok {
		return nil, false, nil
	}
	release()
	if e.orderID == 0 {
		e.orderID = 801
	}
	executedQty := 0.0
	if e.executedQty != nil {
		executedQty = e.executedQty(e.request.Quantity)
	}
	return []*Order{{
		OrderID:       e.orderID,
		ClientOrderID: e.request.ClientOrderID,
		Symbol:        e.request.Symbol,
		Side:          e.request.Side,
		Type:          "LIMIT",
		Price:         e.request.Price,
		Quantity:      e.request.Quantity,
		ExecutedQty:   executedQty,
		AvgPrice:      e.request.Price + 0.01,
		Status:        e.status,
		UpdateTime:    1_700_000_000_000,
	}}, false, nil
}

func (e *authoritativePlacementExecutor) BatchCancelOrders([]int64) error { return nil }

func TestAdjustOrdersConvergesAuthoritativePlacementStatusWithoutWebSocket(t *testing.T) {
	tests := []struct {
		status          string
		executedQty     func(float64) float64
		wantOrderStatus string
		wantSlotStatus  string
		wantBoundOrder  bool
	}{
		{
			status:          OrderStatusPartiallyFilled,
			executedQty:     func(quantity float64) float64 { return quantity / 2 },
			wantOrderStatus: OrderStatusPartiallyFilled,
			wantSlotStatus:  SlotStatusLocked,
			wantBoundOrder:  true,
		},
		{
			status:          "CANCELED",
			executedQty:     func(quantity float64) float64 { return quantity / 2 },
			wantOrderStatus: OrderStatusCanceled,
			wantSlotStatus:  SlotStatusFree,
		},
		{
			status:          OrderStatusFilled,
			executedQty:     func(quantity float64) float64 { return quantity },
			wantOrderStatus: OrderStatusNotPlaced,
			wantSlotStatus:  SlotStatusFree,
		},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			cfg := testConfig()
			cfg.Trading.BuyWindowSize = 2
			cfg.Trading.SellWindowSize = 0
			executor := &authoritativePlacementExecutor{
				status:      tt.status,
				executedQty: tt.executedQty,
			}
			spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
			spm.anchorPrice = 100

			if err := spm.AdjustOrders(100); err != nil {
				t.Fatalf("AdjustOrders() error = %v", err)
			}
			if executor.request == nil {
				t.Fatal("executor did not receive request")
			}
			price, side, valid := spm.parseClientOrderID(executor.request.ClientOrderID)
			if !valid || side != "BUY" {
				t.Fatalf("request identity = price:%v side:%s valid:%v", price, side, valid)
			}
			slot := spm.getOrCreateSlot(price)
			wantExecuted := tt.executedQty(executor.request.Quantity)
			assertClose(t, "position quantity", slot.PositionQty, wantExecuted)
			if slot.PositionStatus != PositionStatusFilled && tt.status != OrderStatusPartiallyFilled {
				t.Fatalf("PositionStatus = %s, want FILLED", slot.PositionStatus)
			}
			if slot.OrderStatus != tt.wantOrderStatus || slot.SlotStatus != tt.wantSlotStatus {
				t.Fatalf("slot state = order:%s slot:%s, want order:%s slot:%s",
					slot.OrderStatus, slot.SlotStatus, tt.wantOrderStatus, tt.wantSlotStatus)
			}
			if tt.wantBoundOrder {
				if slot.OrderID != executor.orderID || slot.OrderFilledQty != wantExecuted {
					t.Fatalf("partial result was not retained: %+v", slot)
				}
			} else if slot.OrderID != 0 || slot.ClientOID != "" {
				t.Fatalf("terminal result did not clear order identity: %+v", slot)
			}
		})
	}
}
