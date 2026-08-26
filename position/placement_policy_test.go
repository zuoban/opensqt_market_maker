package position

import (
	"errors"
	"testing"
	"time"
)

type marginThenAcceptExecutor struct {
	batches     [][]*OrderRequest
	nextOrderID int64
	cancelCalls int
}

func (e *marginThenAcceptExecutor) PlaceOrder(*OrderRequest) (*Order, error) {
	return nil, nil
}

func (e *marginThenAcceptExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	e.batches = append(e.batches, append([]*OrderRequest(nil), requests...))
	if len(e.batches) == 1 {
		for _, req := range requests {
			release, ok := req.AcquireSubmissionLease()
			if ok {
				release()
			}
		}
		return nil, true, nil
	}

	placed := make([]*Order, 0, len(requests))
	for _, req := range requests {
		release, ok := req.AcquireSubmissionLease()
		if !ok {
			continue
		}
		release()
		e.nextOrderID++
		placed = append(placed, &Order{
			OrderID:       e.nextOrderID,
			ClientOrderID: req.ClientOrderID,
			Symbol:        req.Symbol,
			Side:          req.Side,
			Type:          "LIMIT",
			Price:         req.Price,
			Quantity:      req.Quantity,
			Status:        "NEW",
		})
	}
	return placed, false, nil
}

func (e *marginThenAcceptExecutor) BatchCancelOrders([]int64) error {
	e.cancelCalls++
	return nil
}

func TestMarginLockKeepsExistingBuysAndStillPlacesReduceOnlySells(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 3
	cfg.Trading.SellWindowSize = 0
	cfg.Trading.MarginLockDurationSec = 30

	executor := &marginThenAcceptExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	existingBuy := spm.getOrCreateSlot(97)
	existingBuy.OrderID = 77
	existingBuy.ClientOID = spm.generateClientOrderID(97, "BUY")
	existingBuy.OrderSide = "BUY"
	existingBuy.OrderStatus = OrderStatusPlaced
	existingBuy.OrderPrice = 97
	existingBuy.OrderQuantity = 0.3
	existingBuy.SlotStatus = SlotStatusLocked

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("first AdjustOrders() error = %v", err)
	}
	if !spm.insufficientMargin {
		t.Fatal("margin error did not activate the buy placement lock")
	}
	if executor.cancelCalls != 0 {
		t.Fatalf("margin error canceled existing buys: cancel calls = %d", executor.cancelCalls)
	}
	if existingBuy.OrderID != 77 || existingBuy.OrderStatus != OrderStatusPlaced ||
		existingBuy.SlotStatus != SlotStatusLocked {
		t.Fatalf("existing buy was mutated after margin error: %+v", existingBuy)
	}

	// 换一个行情网格，确保锁定期内本来有新 BUY 候选；同时增加一个
	// 已持仓槽位。第二批应只有 ReduceOnly SELL。
	cfg.Trading.SellWindowSize = 2
	filled := spm.getOrCreateSlot(100)
	filled.PositionStatus = PositionStatusFilled
	filled.PositionQty = 0.1

	if err := spm.AdjustOrders(102); err != nil {
		t.Fatalf("second AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 2 {
		t.Fatalf("placement batches = %d, want 2", len(executor.batches))
	}
	if len(executor.batches[1]) == 0 {
		t.Fatal("margin lock suppressed the reduce-only sell batch")
	}
	for _, req := range executor.batches[1] {
		if req.Side != "SELL" || !req.ReduceOnly || !req.PostOnly {
			t.Fatalf("order submitted during margin lock = %+v, want PostOnly ReduceOnly SELL", req)
		}
	}
	if existingBuy.OrderID != 77 || existingBuy.OrderStatus != OrderStatusPlaced ||
		existingBuy.SlotStatus != SlotStatusLocked {
		t.Fatalf("existing buy changed during margin lock: %+v", existingBuy)
	}
}

func TestAdjustOrdersRepricesCrossedSellAndSubmitsReduceOnlyFirst(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1

	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	filled := spm.getOrCreateSlot(100)
	filled.PositionStatus = PositionStatusFilled
	filled.PositionQty = 0.1

	if err := spm.AdjustOrders(110); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.orders) < 2 {
		t.Fatalf("submitted orders = %d, want both SELL and BUY", len(executor.orders))
	}
	first := executor.orders[0]
	if first.Side != "SELL" || !first.ReduceOnly || !first.PostOnly {
		t.Fatalf("first submitted order = %+v, want PostOnly ReduceOnly SELL", first)
	}
	if first.Price <= 110 {
		t.Fatalf("crossed sell price = %.2f, want above current price 110", first.Price)
	}
	if first.Price < 101 {
		t.Fatalf("repriced sell %.2f fell below grid target 101", first.Price)
	}
	if executor.orders[1].Side != "BUY" {
		t.Fatalf("second submitted side = %s, want BUY after reduce-only sells", executor.orders[1].Side)
	}
}

type failThenSucceedExecutor struct {
	fail        bool
	calls       int
	nextOrderID int64
	wantErr     error
}

func (e *failThenSucceedExecutor) PlaceOrder(*OrderRequest) (*Order, error) {
	return nil, nil
}

func (e *failThenSucceedExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	e.calls++
	placed := make([]*Order, 0, len(requests))
	for _, req := range requests {
		release, ok := req.AcquireSubmissionLease()
		if !ok {
			continue
		}
		release()
		if e.fail {
			continue
		}
		e.nextOrderID++
		placed = append(placed, &Order{
			OrderID:       e.nextOrderID,
			ClientOrderID: req.ClientOrderID,
			Symbol:        req.Symbol,
			Side:          req.Side,
			Price:         req.Price,
			Quantity:      req.Quantity,
			Status:        "NEW",
		})
	}
	if e.fail {
		return nil, false, e.wantErr
	}
	return placed, false, nil
}

func (e *failThenSucceedExecutor) BatchCancelOrders([]int64) error { return nil }

func TestDefinitePlacementFailureUsesPerSlotRetryCooldown(t *testing.T) {
	wantErr := errors.New("definite reject")
	executor := &failThenSucceedExecutor{fail: true, wantErr: wantErr}
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 0
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	err := spm.AdjustOrders(100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("first AdjustOrders() error = %v, want definite rejection", err)
	}
	slot := spm.getOrCreateSlot(99)
	if slot.SlotStatus != SlotStatusFree || slot.ClientOID != "" ||
		!slot.placementRetryNotBefore.After(time.Now()) {
		t.Fatalf("failed reservation did not enter retry cooldown: %+v", slot)
	}

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("second AdjustOrders() error = %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("immediate price tick retried failed slot: executor calls = %d", executor.calls)
	}

	// 无需在测试中真实等待一秒：模拟冷却已到期，下一个 reservation
	// 应能正常创建，成功后不应残留失败冷却。
	slot.mu.Lock()
	slot.placementRetryNotBefore = time.Now().Add(-time.Millisecond)
	slot.mu.Unlock()
	executor.fail = false

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("third AdjustOrders() error = %v", err)
	}
	if executor.calls != 2 {
		t.Fatalf("expired cooldown did not retry: executor calls = %d", executor.calls)
	}
	if slot.OrderID == 0 || slot.SlotStatus != SlotStatusLocked ||
		!slot.placementRetryNotBefore.IsZero() {
		t.Fatalf("successful retry retained stale cooldown or was not bound: %+v", slot)
	}
}

func TestOrderThresholdPrioritizesValidReduceOnlySellOverBuy(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.OrderCleanupThreshold = 1
	cfg.Trading.MinOrderValue = 6

	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	buySlot := spm.getOrCreateSlot(99)
	filledSlot := spm.getOrCreateSlot(100)
	filledSlot.PositionStatus = PositionStatusFilled
	filledSlot.PositionQty = 0.1

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.orders) != 1 {
		t.Fatalf("submitted orders = %d, want exactly one SELL", len(executor.orders))
	}
	order := executor.orders[0]
	if order.Side != "SELL" || !order.PostOnly || !order.ReduceOnly {
		t.Fatalf("submitted order = %+v, want PostOnly ReduceOnly SELL", order)
	}
	if buySlot.SlotStatus != SlotStatusFree || buySlot.OrderID != 0 || buySlot.ClientOID != "" {
		t.Fatalf("BUY slot consumed despite SELL priority: %+v", buySlot)
	}
}

func TestInvalidSellCandidateLeavesThresholdCapacityForBuy(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.OrderCleanupThreshold = 1
	cfg.Trading.MinOrderValue = 20

	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	filledSlot := spm.getOrCreateSlot(100)
	filledSlot.PositionStatus = PositionStatusFilled
	filledSlot.PositionQty = 0.1 // 卖单名义价值约 10.1，低于 20。

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.orders) != 1 {
		t.Fatalf("submitted orders = %d, want exactly one BUY", len(executor.orders))
	}
	order := executor.orders[0]
	if order.Side != "BUY" || !order.PostOnly || order.ReduceOnly {
		t.Fatalf("submitted order = %+v, want PostOnly BUY", order)
	}
}
