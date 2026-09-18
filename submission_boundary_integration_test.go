package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/order"
	"opensqt/position"
	"opensqt/telemetry"
	"opensqt/utils"
)

type confirmationBoundaryExchange struct {
	exchange.IExchange
	call func(context.Context, *exchange.OrderRequest) (*exchange.Order, error)
}

func (*confirmationBoundaryExchange) GetName() string          { return "confirmation-test" }
func (*confirmationBoundaryExchange) PlaceOrderBatchSize() int { return 5 }
func (e *confirmationBoundaryExchange) PlaceOrderBatch(ctx context.Context, reqs []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
	items := make([]exchange.PlaceOrderBatchItem, len(reqs))
	for i, req := range reqs {
		items[i].Order, items[i].Err = e.call(ctx, req)
	}
	return items, nil
}

func confirmationTestManager(t *testing.T, ex *confirmationBoundaryExchange) *position.SuperPositionManager {
	t.Helper()
	executor := order.NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	t.Cleanup(executor.Shutdown)
	cfg := &config.Config{}
	cfg.Trading.Symbol, cfg.Trading.PriceInterval, cfg.Trading.OrderQuantity = "ETHUSDT", 1, 30
	cfg.Trading.BuyWindowSize, cfg.Trading.OrderCleanupThreshold = 2, 100
	return position.NewSuperPositionManager(cfg, &exchangeExecutorAdapter{executor: executor}, nil, 2, 3)
}

func runConfirmationCallback(t *testing.T, callback func()) bool {
	t.Helper()
	done := make(chan struct{})
	go func() { callback(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		t.Error("same-slot callback blocked behind read-only confirmation")
		return false
	}
}

func TestConfirmationBoundaryProcessesFillBeforeRESTReturns(t *testing.T) {
	ex := &confirmationBoundaryExchange{}
	spm := confirmationTestManager(t, ex)
	spm.SetTelemetry(telemetry.New(time.Now()))
	ex.call = func(ctx context.Context, req *exchange.OrderRequest) (*exchange.Order, error) {
		release, err := req.BeginSubmission(ctx)
		if err != nil {
			return nil, err
		}
		// 创建响应已返回，接下来模拟缓慢的只读确认。
		release()
		if !runConfirmationCallback(t, func() {
			spm.OnOrderUpdate(position.OrderUpdate{OrderID: 101, ClientOrderID: req.ClientOrderID, Side: "BUY", Status: "FILLED",
				Quantity: req.Quantity, ExecutedQty: req.Quantity, AvgPrice: req.Price, UpdateTime: 1})
		}) {
			return nil, exchange.ErrOrderPlacementUnknown
		}
		if spm.TelemetryStateCounts().PendingOppositeSells != 1 {
			t.Error("fill not visible while confirmation still in progress")
		}
		// 查询早于 WS 全成交读取的 NEW 回包不能重新锁死该槽位。
		return &exchange.Order{OrderID: 101, ClientOrderID: req.ClientOrderID, Status: exchange.OrderStatusNew,
			Symbol: req.Symbol, Side: req.Side, Price: req.Price, Quantity: req.Quantity}, nil
	}
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatal(err)
	}
	snapshot := spm.Snapshot()
	if snapshot.TotalBuyQty <= 0 {
		t.Fatal("fill accounting missing")
	}
	for _, slot := range snapshot.Slots {
		if slot.Price == 99 && (slot.SlotStatus != position.SlotStatusFree || slot.OrderID != 0) {
			t.Fatalf("late REST revived completed order: %+v", slot)
		}
	}
}

func TestConfirmationRetryAfterLateCorrectionPreservesUnknownReservation(t *testing.T) {
	ex := &confirmationBoundaryExchange{}
	spm := confirmationTestManager(t, ex)
	// 无 telemetry 也必须保留已提交标记，它参与 UNKNOWN reservation 保护。
	oldID := utils.GenerateOrderID(99, "BUY", 2)
	spm.OnOrderUpdate(position.OrderUpdate{OrderID: 7, ClientOrderID: oldID, Status: "CANCELED", Side: "BUY", UpdateTime: 1})
	var submittedID string
	posts := 0
	ex.call = func(ctx context.Context, req *exchange.OrderRequest) (*exchange.Order, error) {
		release, err := req.BeginSubmission(ctx)
		if err != nil {
			return nil, err
		}
		submittedID = req.ClientOrderID
		posts++
		release()
		if !runConfirmationCallback(t, func() {
			spm.OnOrderUpdate(position.OrderUpdate{OrderID: 7, ClientOrderID: oldID, Status: "CANCELED", Side: "BUY",
				Quantity: .03, ExecutedQty: .03, AvgPrice: 99, UpdateTime: 2})
		}) {
			return nil, exchange.ErrOrderPlacementUnknown
		}
		// 模拟按同 ID 查询 -2013 后尝试重提；迟到修正已使 BUY 前提失效。
		retryRelease, retryErr := req.BeginSubmission(ctx)
		if retryErr == nil {
			posts++
			retryRelease()
			t.Error("stale BUY was retried")
		}
		return nil, retryErr
	}
	if err := spm.AdjustOrders(100); !errors.Is(err, exchange.ErrOrderPlacementUnknown) {
		t.Fatalf("unsafe retry err=%v, want UNKNOWN", err)
	}
	if posts != 1 {
		t.Fatalf("created %d times", posts)
	}
	snapshot := spm.Snapshot()
	found := false
	for _, slot := range snapshot.Slots {
		if slot.Price != 99 {
			continue
		}
		found = true
		if slot.SlotStatus != position.SlotStatusPending || slot.ClientOID != submittedID || slot.PositionQty != .03 {
			t.Fatalf("UNKNOWN identity or corrected inventory lost: %+v", slot)
		}
	}
	if !found {
		t.Fatal("UNKNOWN slot was recycled")
	}
}
