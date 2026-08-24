package position

import "testing"

type recordingExecutor struct {
	orders []*OrderRequest
}

func (e *recordingExecutor) PlaceOrder(req *OrderRequest) (*Order, error) { return nil, nil }

func (e *recordingExecutor) BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool) {
	e.orders = append(e.orders, orders...)
	return nil, false
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
