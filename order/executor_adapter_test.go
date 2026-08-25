package order

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"opensqt/exchange"

	"github.com/adshao/go-binance/v2/common"
)

type recordingExchange struct {
	exchange.IExchange
	requests   []*exchange.OrderRequest
	placeOrder func(*exchange.OrderRequest) (*exchange.Order, error)
}

type countingWaiter struct {
	count int
}

func (w *countingWaiter) Wait(ctx context.Context) error {
	w.count++
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type hookWaiter struct {
	count  int
	before func()
}

func (w *hookWaiter) Wait(ctx context.Context) error {
	w.count++
	if w.before != nil {
		w.before()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

type blockingPlaceExchange struct {
	exchange.IExchange
	started chan struct{}
}

func (e *blockingPlaceExchange) GetName() string { return "blocking-place" }

func (e *blockingPlaceExchange) PlaceOrder(ctx context.Context, _ *exchange.OrderRequest) (*exchange.Order, error) {
	close(e.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

type blockingCancelExchange struct {
	exchange.IExchange
	started chan struct{}
}

func (e *blockingCancelExchange) GetName() string { return "blocking-cancel" }

func (e *blockingCancelExchange) CancelOrder(ctx context.Context, _ string, _ int64) error {
	close(e.started)
	<-ctx.Done()
	return ctx.Err()
}

type cancelErrorExchange struct {
	exchange.IExchange
	batchErr    error
	singleErr   map[int64]error
	cancelCalls []int64
}

func (e *cancelErrorExchange) GetName() string { return "cancel-errors" }

func (e *cancelErrorExchange) BatchCancelOrders(context.Context, string, []int64) error {
	return e.batchErr
}

func (e *cancelErrorExchange) CancelOrder(_ context.Context, _ string, orderID int64) error {
	e.cancelCalls = append(e.cancelCalls, orderID)
	return e.singleErr[orderID]
}

func (e *recordingExchange) GetName() string { return "test" }

func (e *recordingExchange) PlaceOrder(_ context.Context, req *exchange.OrderRequest) (*exchange.Order, error) {
	recorded := *req
	e.requests = append(e.requests, &recorded)
	return e.placeOrder(req)
}

func TestPlaceOrderForcesPostOnlyAtExecutionBoundary(t *testing.T) {
	ex := &recordingExchange{
		placeOrder: func(req *exchange.OrderRequest) (*exchange.Order, error) {
			return &exchange.Order{
				OrderID:       1,
				ClientOrderID: req.ClientOrderID,
				Status:        exchange.OrderStatusNew,
			}, nil
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)

	_, err := executor.PlaceOrder(&OrderRequest{
		Symbol:        "ETHUSDT",
		Side:          "BUY",
		Price:         100,
		Quantity:      0.1,
		PriceDecimals: 2,
		PostOnly:      false,
		ClientOrderID: "strict-maker",
	})
	if err != nil {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if len(ex.requests) != 1 {
		t.Fatalf("request count = %d, want 1", len(ex.requests))
	}
	if !ex.requests[0].PostOnly {
		t.Fatal("execution boundary submitted a non-PostOnly order")
	}
}

func TestPlaceOrderPreservesAuthoritativeExecutionFields(t *testing.T) {
	createdAt := time.Unix(1_700_000_000, 0)
	ex := &recordingExchange{
		placeOrder: func(req *exchange.OrderRequest) (*exchange.Order, error) {
			return &exchange.Order{
				OrderID:       99,
				ClientOrderID: req.ClientOrderID,
				Symbol:        req.Symbol,
				Side:          req.Side,
				Type:          exchange.OrderTypeLimit,
				Price:         req.Price,
				Quantity:      req.Quantity,
				ExecutedQty:   0.04,
				AvgPrice:      100.25,
				Status:        exchange.OrderStatusPartiallyFilled,
				CreatedAt:     createdAt,
				UpdateTime:    1_700_000_000_123,
			}, nil
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)

	got, err := executor.PlaceOrder(&OrderRequest{
		Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1, ClientOrderID: "confirmed-partial",
	})
	if err != nil {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if got.Type != "LIMIT" || got.Status != "PARTIALLY_FILLED" ||
		got.ExecutedQty != 0.04 || got.AvgPrice != 100.25 ||
		!got.CreatedAt.Equal(createdAt) || got.UpdateTime != 1_700_000_000_123 {
		t.Fatalf("authoritative execution fields were lost: %+v", got)
	}
}

func TestBatchPlaceOrdersHealthGuardClosesTOCTOUAtSubmissionBoundary(t *testing.T) {
	healthy := true
	exchangeCalls := 0
	ex := &recordingExchange{
		placeOrder: func(*exchange.OrderRequest) (*exchange.Order, error) {
			exchangeCalls++
			return nil, errors.New("exchange call must not happen")
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	waiter := &hookWaiter{before: func() {
		// 模拟上层门禁已读到健康快照，但在限流结束后、
		// 真正进入交易所边界前订单流降级。
		healthy = false
	}}
	executor.rateLimiter = waiter

	leaseHeld := false
	leaseAcquires := 0
	leaseReleases := 0
	guardCalls := 0
	executor.SetSubmissionHealthGuard(func() error {
		guardCalls++
		if !leaseHeld {
			t.Fatal("health guard ran before acquiring submission lease")
		}
		if !healthy {
			return errors.New("order stream DEGRADED")
		}
		return nil
	})
	newRequest := func(id string) *OrderRequest {
		return &OrderRequest{
			Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1, ClientOrderID: id,
			AcquireSubmissionLease: func() (func(), bool) {
				leaseAcquires++
				leaseHeld = true
				return func() {
					leaseHeld = false
					leaseReleases++
				}, true
			},
		}
	}

	placed, _, err := executor.BatchPlaceOrders([]*OrderRequest{
		newRequest("guarded-1"),
		newRequest("guarded-2"),
	})
	if !errors.Is(err, ErrTradingHealthGuardRejected) {
		t.Fatalf("BatchPlaceOrders() error = %v, want ErrTradingHealthGuardRejected", err)
	}
	if len(placed) != 0 || exchangeCalls != 0 {
		t.Fatalf("placed=%d exchangeCalls=%d, want no exchange submission", len(placed), exchangeCalls)
	}
	if waiter.count != 1 || guardCalls != 1 || leaseAcquires != 1 || leaseReleases != 1 || leaseHeld {
		t.Fatalf("wait=%d guard=%d lease=%d/%d held=%v, want one fully released guarded attempt",
			waiter.count, guardCalls, leaseAcquires, leaseReleases, leaseHeld)
	}
	if _, err := executor.PlaceOrder(newRequest("guarded-after-stop")); !errors.Is(err, ErrNewOrdersStopped) {
		t.Fatalf("PlaceOrder() after guard rejection error = %v, want ErrNewOrdersStopped", err)
	}
}

func TestPlaceOrderNeverDowngradesAfterPostOnlyRejections(t *testing.T) {
	ex := &recordingExchange{
		placeOrder: func(*exchange.OrderRequest) (*exchange.Order, error) {
			return nil, errors.New("code=-5022 Post Only order would immediately match")
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	waiter := &countingWaiter{}
	executor.rateLimiter = waiter

	_, err := executor.PlaceOrder(&OrderRequest{
		Symbol:        "ETHUSDT",
		Side:          "SELL",
		Price:         101,
		Quantity:      0.1,
		PriceDecimals: 2,
		PostOnly:      true,
		ClientOrderID: "retry-maker",
	})
	if err == nil {
		t.Fatal("PlaceOrder() error = nil, want retry exhaustion")
	}
	if len(ex.requests) != 6 {
		t.Fatalf("request count = %d, want 6", len(ex.requests))
	}
	if waiter.count != 6 {
		t.Fatalf("rate limiter wait count = %d, want 6", waiter.count)
	}
	for i, req := range ex.requests {
		if !req.PostOnly {
			t.Fatalf("request %d downgraded to a non-PostOnly order", i+1)
		}
	}
}

func TestPlaceOrderReacquiresSubmissionLeaseForEveryAttempt(t *testing.T) {
	attempts := 0
	leaseHeld := false
	leaseAcquires := 0
	leaseReleases := 0
	ex := &recordingExchange{
		placeOrder: func(req *exchange.OrderRequest) (*exchange.Order, error) {
			if !leaseHeld {
				t.Fatal("exchange PlaceOrder ran without submission lease")
			}
			attempts++
			if attempts == 1 {
				return nil, errors.New("code=-5022 Post Only order would immediately match")
			}
			return &exchange.Order{
				OrderID:       7,
				ClientOrderID: req.ClientOrderID,
				Status:        exchange.OrderStatusNew,
			}, nil
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)

	_, err := executor.PlaceOrder(&OrderRequest{
		Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1, ClientOrderID: "leased",
		AcquireSubmissionLease: func() (func(), bool) {
			if leaseHeld {
				t.Fatal("submission lease was retained during retry wait")
			}
			leaseAcquires++
			leaseHeld = true
			return func() {
				leaseHeld = false
				leaseReleases++
			}, true
		},
	})
	if err != nil {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if attempts != 2 || leaseAcquires != 2 || leaseReleases != 2 || leaseHeld {
		t.Fatalf("lease lifecycle = attempts:%d acquire:%d release:%d held:%v",
			attempts, leaseAcquires, leaseReleases, leaseHeld)
	}
}

func TestPlaceOrderSkipsExchangeWhenSubmissionReservationIsStale(t *testing.T) {
	ex := &recordingExchange{
		placeOrder: func(*exchange.OrderRequest) (*exchange.Order, error) {
			t.Fatal("stale reservation reached exchange")
			return nil, nil
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)

	_, err := executor.PlaceOrder(&OrderRequest{
		Symbol: "ETHUSDT", Side: "SELL", Price: 101, Quantity: 0.1,
		AcquireSubmissionLease: func() (func(), bool) {
			return nil, false
		},
	})
	if !errors.Is(err, ErrOrderSubmissionStale) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderSubmissionStale", err)
	}
	if len(ex.requests) != 0 {
		t.Fatalf("exchange request count = %d, want 0", len(ex.requests))
	}
}

func TestNewOrderGateFailsFastAndCanBeEnabledAgain(t *testing.T) {
	ex := &recordingExchange{
		placeOrder: func(req *exchange.OrderRequest) (*exchange.Order, error) {
			return &exchange.Order{
				OrderID:       9,
				ClientOrderID: req.ClientOrderID,
				Status:        exchange.OrderStatusNew,
			}, nil
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.StopNewOrders()

	_, err := executor.PlaceOrder(&OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1})
	if !errors.Is(err, ErrNewOrdersStopped) {
		t.Fatalf("PlaceOrder() error = %v, want ErrNewOrdersStopped", err)
	}
	if len(ex.requests) != 0 {
		t.Fatalf("request count while gate closed = %d, want 0", len(ex.requests))
	}

	if err := executor.EnableNewOrders(); err != nil {
		t.Fatalf("EnableNewOrders() error = %v", err)
	}
	if _, err := executor.PlaceOrder(&OrderRequest{
		Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1, ClientOrderID: "enabled-again",
	}); err != nil {
		t.Fatalf("PlaceOrder() after EnableNewOrders error = %v", err)
	}
	if len(ex.requests) != 1 {
		t.Fatalf("request count after re-enable = %d, want 1", len(ex.requests))
	}
}

func TestStopNewOrdersCancelsInFlightPlaceOrder(t *testing.T) {
	ex := &blockingPlaceExchange{started: make(chan struct{})}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	result := make(chan error, 1)

	go func() {
		_, err := executor.PlaceOrder(&OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1})
		result <- err
	}()

	select {
	case <-ex.started:
	case <-time.After(time.Second):
		t.Fatal("PlaceOrder did not reach exchange")
	}
	executor.StopNewOrders()

	select {
	case err := <-result:
		if !errors.Is(err, ErrNewOrdersStopped) {
			t.Fatalf("PlaceOrder() error = %v, want ErrNewOrdersStopped", err)
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PlaceOrder() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PlaceOrder was not canceled by StopNewOrders")
	}
}

func TestStopNewOrdersStillAllowsCancelOrder(t *testing.T) {
	ex := &cancelErrorExchange{singleErr: map[int64]error{}}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.StopNewOrders()

	if err := executor.CancelOrder(7); err != nil {
		t.Fatalf("CancelOrder() while new-order gate closed error = %v", err)
	}
	if len(ex.cancelCalls) != 1 || ex.cancelCalls[0] != 7 {
		t.Fatalf("single cancel calls = %v, want [7]", ex.cancelCalls)
	}
}

func TestShutdownCancelsInFlightCancelOrder(t *testing.T) {
	ex := &blockingCancelExchange{started: make(chan struct{})}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	result := make(chan error, 1)

	go func() {
		result <- executor.CancelOrder(42)
	}()

	select {
	case <-ex.started:
	case <-time.After(time.Second):
		t.Fatal("CancelOrder did not reach exchange")
	}
	executor.Shutdown()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("CancelOrder() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CancelOrder was not canceled by Shutdown")
	}
	if err := executor.EnableNewOrders(); !errors.Is(err, ErrOrderExecutorStopped) {
		t.Fatalf("EnableNewOrders() after Shutdown error = %v, want ErrOrderExecutorStopped", err)
	}
}

func TestPlaceOrderRequestTimeoutIsNotRetried(t *testing.T) {
	ex := &blockingPlaceExchange{started: make(chan struct{})}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.requestTimeout = 20 * time.Millisecond
	waiter := &countingWaiter{}
	executor.rateLimiter = waiter

	unknown := false
	_, err := executor.PlaceOrder(&OrderRequest{
		Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1,
		OnSubmissionUnknown: func() { unknown = true },
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PlaceOrder() error = %v, want context.DeadlineExceeded", err)
	}
	if !errors.Is(err, exchange.ErrOrderPlacementUnknown) || !unknown {
		t.Fatalf("timed-out in-flight placement was not marked UNKNOWN: err=%v marked=%v", err, unknown)
	}
	if waiter.count != 1 {
		t.Fatalf("rate limiter wait count = %d, want 1", waiter.count)
	}
}

func TestPlaceOrderUnknownResultIsNotRetried(t *testing.T) {
	ex := &recordingExchange{
		placeOrder: func(*exchange.OrderRequest) (*exchange.Order, error) {
			return nil, fmt.Errorf("网关响应丢失: %w", exchange.ErrOrderPlacementUnknown)
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	waiter := &countingWaiter{}
	executor.rateLimiter = waiter

	unknown := false
	_, err := executor.PlaceOrder(&OrderRequest{
		Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1,
		OnSubmissionUnknown: func() { unknown = true },
	})
	if !errors.Is(err, exchange.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementUnknown", err)
	}
	if len(ex.requests) != 1 || waiter.count != 1 {
		t.Fatalf("unknown result was retried: requests=%d waits=%d", len(ex.requests), waiter.count)
	}
	if !unknown {
		t.Fatal("unknown result callback was not invoked")
	}
}

func TestBatchPlaceOrdersPropagatesUnknownResultAndStopsBatch(t *testing.T) {
	ex := &recordingExchange{
		placeOrder: func(*exchange.OrderRequest) (*exchange.Order, error) {
			return nil, fmt.Errorf("响应无法确认: %w", exchange.ErrOrderPlacementUnknown)
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.rateLimiter = &countingWaiter{}

	placed, marginErr, err := executor.BatchPlaceOrders([]*OrderRequest{
		{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1},
		{Symbol: "ETHUSDT", Side: "BUY", Price: 99, Quantity: 0.1},
	})
	if !errors.Is(err, exchange.ErrOrderPlacementUnknown) {
		t.Fatalf("BatchPlaceOrders() error = %v, want ErrOrderPlacementUnknown", err)
	}
	if marginErr || len(placed) != 0 || len(ex.requests) != 1 {
		t.Fatalf("BatchPlaceOrders() = placed:%d margin:%v requests:%d", len(placed), marginErr, len(ex.requests))
	}
}

func TestPlaceOrderReturnsExchangeNormalizedValues(t *testing.T) {
	createdAt := time.Unix(123, 0)
	ex := &recordingExchange{
		placeOrder: func(*exchange.OrderRequest) (*exchange.Order, error) {
			return &exchange.Order{
				OrderID:       88,
				ClientOrderID: "normalized-client-id",
				Symbol:        "ETHUSDT",
				Side:          exchange.SideBuy,
				Price:         99.9,
				Quantity:      0.099,
				Status:        exchange.OrderStatusNew,
				CreatedAt:     createdAt,
			}, nil
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)

	got, err := executor.PlaceOrder(&OrderRequest{
		Symbol: "ETHUSDT", Side: "BUY", Price: 99.99, Quantity: 0.0999, ClientOrderID: "requested-client-id",
	})
	if err != nil {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if got.Price != 99.9 || got.Quantity != 0.099 || got.ClientOrderID != "normalized-client-id" || !got.CreatedAt.Equal(createdAt) {
		t.Fatalf("PlaceOrder() = %+v, want exchange-normalized values", got)
	}
}

func TestPlaceOrderClassifiesTypedBinanceErrors(t *testing.T) {
	tests := []struct {
		name         string
		code         int64
		wantAttempts int
	}{
		{name: "post only", code: -5022, wantAttempts: 6},
		{name: "rate limit", code: -1003, wantAttempts: 6},
		{name: "margin", code: -2019, wantAttempts: 1},
		{name: "position mode", code: -4061, wantAttempts: 1},
		{name: "timestamp", code: -1021, wantAttempts: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := &recordingExchange{
				placeOrder: func(*exchange.OrderRequest) (*exchange.Order, error) {
					return nil, &common.APIError{Code: tt.code, Message: tt.name}
				},
			}
			executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
			waiter := &countingWaiter{}
			executor.rateLimiter = waiter

			_, err := executor.PlaceOrder(&OrderRequest{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1})
			if err == nil {
				t.Fatal("PlaceOrder() error = nil, want error")
			}
			if len(ex.requests) != tt.wantAttempts {
				t.Fatalf("request count = %d, want %d", len(ex.requests), tt.wantAttempts)
			}
			if waiter.count != tt.wantAttempts {
				t.Fatalf("rate limiter wait count = %d, want %d", waiter.count, tt.wantAttempts)
			}
		})
	}
}

func TestBatchCancelOrdersJoinsBatchAndSingleErrors(t *testing.T) {
	batchErr := errors.New("batch failed")
	singleErr := errors.New("single failed")
	ex := &cancelErrorExchange{
		batchErr:  batchErr,
		singleErr: map[int64]error{1: singleErr},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)

	err := executor.BatchCancelOrders([]int64{1, 2})
	if !errors.Is(err, batchErr) {
		t.Fatalf("BatchCancelOrders() error = %v, want batch error", err)
	}
	if !errors.Is(err, singleErr) {
		t.Fatalf("BatchCancelOrders() error = %v, want single error", err)
	}
	if len(ex.cancelCalls) != 2 || ex.cancelCalls[0] != 1 || ex.cancelCalls[1] != 2 {
		t.Fatalf("single cancel calls = %v, want [1 2]", ex.cancelCalls)
	}
}
