package position

import (
	"errors"
	"testing"
)

type recordingExecutor struct {
	orders   []*OrderRequest
	batchErr error
}

func (e *recordingExecutor) PlaceOrder(req *OrderRequest) (*Order, error) { return nil, nil }

func (e *recordingExecutor) BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool, error) {
	e.orders = append(e.orders, orders...)
	return nil, false, e.batchErr
}

func TestAdjustOrdersPropagatesBatchPlacementError(t *testing.T) {
	wantErr := errors.New("placement state uncertain")
	executor := &recordingExecutor{batchErr: wantErr}
	spm := NewSuperPositionManager(testConfig(), executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	err := spm.AdjustOrders(100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("AdjustOrders() error = %v, want wrapped batch error", err)
	}
	if len(executor.orders) == 0 {
		t.Fatal("AdjustOrders() did not attempt an order")
	}
	for _, req := range executor.orders {
		price, _, valid := spm.parseClientOrderID(req.ClientOrderID)
		if !valid {
			t.Fatalf("invalid client order ID: %s", req.ClientOrderID)
		}
		slot := spm.getOrCreateSlot(price)
		if slot.SlotStatus == SlotStatusPending {
			t.Fatalf("failed order left slot %.2f pending", price)
		}
	}
}

func (e *recordingExecutor) BatchCancelOrders(orderIDs []int64) error { return nil }

func TestAdjustOrdersKeepsBuyAndSellPostOnlyAfterRepeatedCancellations(t *testing.T) {
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(testConfig(), executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	buySlot := spm.getOrCreateSlot(99)
	buySlot.PostOnlyFailCount = 99

	sellSlot := spm.getOrCreateSlot(100)
	sellSlot.PositionStatus = PositionStatusFilled
	sellSlot.PositionQty = 0.1
	sellSlot.PostOnlyFailCount = 99

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}

	var sawBuy, sawSell bool
	for _, req := range executor.orders {
		if !req.PostOnly {
			t.Fatalf("%s order at %.2f is not PostOnly", req.Side, req.Price)
		}
		switch req.Side {
		case "BUY":
			sawBuy = true
		case "SELL":
			sawSell = true
		}
	}
	if !sawBuy || !sawSell {
		t.Fatalf("captured sides: BUY=%v SELL=%v, want both", sawBuy, sawSell)
	}
}
