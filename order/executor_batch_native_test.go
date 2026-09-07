package order

import (
	"context"
	"errors"
	"testing"
	"time"

	"opensqt/exchange"
	"opensqt/exchange/exchangeerr"
)

type batchRecordingExchange struct {
	recordingExchange
	batchSize  int
	batchCalls int
	placeBatch func(context.Context, []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error)
}

func (e *batchRecordingExchange) PlaceOrderBatchSize() int {
	if e.batchSize <= 0 {
		return 2
	}
	return e.batchSize
}

func (e *batchRecordingExchange) PlaceOrderBatch(ctx context.Context, orders []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
	e.batchCalls++
	for _, req := range orders {
		recorded := *req
		e.requests = append(e.requests, &recorded)
	}
	if e.placeBatch != nil {
		return e.placeBatch(ctx, orders)
	}
	items := make([]exchange.PlaceOrderBatchItem, len(orders))
	for i, req := range orders {
		items[i].Order = &exchange.Order{
			OrderID:       int64(i + 1),
			ClientOrderID: req.ClientOrderID,
			Status:        exchange.OrderStatusNew,
			Price:         req.Price,
			Quantity:      req.Quantity,
			Side:          req.Side,
			Symbol:        req.Symbol,
		}
	}
	return items, nil
}

func TestNativeBatchPlaceOrdersUsesOneHTTPCallAndForcesPostOnly(t *testing.T) {
	ex := &batchRecordingExchange{batchSize: 5}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	waiter := &countingWaiter{}
	executor.rateLimiter = waiter

	placed, margin, err := executor.BatchPlaceOrders([]*OrderRequest{
		{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1, ClientOrderID: "a", PostOnly: false},
		{Symbol: "ETHUSDT", Side: "BUY", Price: 99, Quantity: 0.1, ClientOrderID: "b", PostOnly: false},
	})
	if err != nil || margin {
		t.Fatalf("BatchPlaceOrders() error = %v margin=%v", err, margin)
	}
	if ex.batchCalls != 1 {
		t.Fatalf("native batch calls = %d, want 1", ex.batchCalls)
	}
	if len(placed) != 2 || waiter.count != 2 {
		t.Fatalf("placed=%d waits=%d, want 2/2", len(placed), waiter.count)
	}
	for i, req := range ex.requests {
		if !req.PostOnly {
			t.Fatalf("request %d was not forced PostOnly", i)
		}
	}
}

func TestNativeBatchFiltersUnsafeMakerItemBeforeExchange(t *testing.T) {
	ex := &batchRecordingExchange{batchSize: 5}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.rateLimiter = &countingWaiter{}
	providerCalls := 0
	executor.SetMakerGuard(func() exchange.MarketSnapshot {
		providerCalls++
		return exchange.MarketSnapshot{
			Symbol: "ETHUSDT", BestBid: 100, BestAsk: 100.10, Ready: true,
			QuoteReceivedAt: time.Now(), QuoteVersion: 5, StreamEpoch: 1,
		}
	}, 0.01, 2, time.Second)

	var unsafeKind OrderRejectionKind
	placed, margin, err := executor.BatchPlaceOrders([]*OrderRequest{
		{Symbol: "ETHUSDT", Side: "BUY", Price: 99, Quantity: 0.1, ClientOrderID: "safe"},
		{
			Symbol: "ETHUSDT", Side: "BUY", Price: 100.09, Quantity: 0.1, ClientOrderID: "unsafe",
			OnDefiniteRejection: func(kind OrderRejectionKind) { unsafeKind = kind },
		},
	})
	if err == nil || margin {
		t.Fatalf("BatchPlaceOrders() error=%v margin=%v, want local rejection only", err, margin)
	}
	if len(placed) != 1 || placed[0].ClientOrderID != "safe" {
		t.Fatalf("placed = %+v, want safe item only", placed)
	}
	if unsafeKind != OrderRejectionMakerMoved {
		t.Fatalf("unsafe rejection kind = %q", unsafeKind)
	}
	if ex.batchCalls != 1 || len(ex.requests) != 1 || ex.requests[0].ClientOrderID != "safe" {
		t.Fatalf("batchCalls=%d requests=%+v", ex.batchCalls, ex.requests)
	}
	if providerCalls != 1 {
		t.Fatalf("market snapshot provider calls = %d, want one atomic snapshot per batch", providerCalls)
	}
}

func TestNativeBatchSubmitsNearTouchOrderSeparately(t *testing.T) {
	ex := &batchRecordingExchange{batchSize: 5}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.rateLimiter = &countingWaiter{}

	placed, margin, err := executor.BatchPlaceOrders([]*OrderRequest{
		{Symbol: "ETHUSDT", Side: "BUY", Price: 90, Quantity: 0.1, ClientOrderID: "deep-1"},
		{Symbol: "ETHUSDT", Side: "BUY", Price: 99, Quantity: 0.1, ClientOrderID: "near", NearTouch: true},
		{Symbol: "ETHUSDT", Side: "BUY", Price: 89, Quantity: 0.1, ClientOrderID: "deep-2"},
	})
	if err != nil || margin || len(placed) != 3 {
		t.Fatalf("placed=%d margin=%v err=%v", len(placed), margin, err)
	}
	if ex.batchCalls != 3 {
		t.Fatalf("native batch calls = %d, want deep/near/deep as 3 calls", ex.batchCalls)
	}
}

func TestBatchPlaceOrdersRejectsNilRequestWithoutPanic(t *testing.T) {
	ex := &batchRecordingExchange{batchSize: 5}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.rateLimiter = &countingWaiter{}

	placed, margin, err := executor.BatchPlaceOrders([]*OrderRequest{nil})
	if err == nil || margin || len(placed) != 0 || ex.batchCalls != 0 {
		t.Fatalf("placed=%d margin=%v err=%v batchCalls=%d", len(placed), margin, err, ex.batchCalls)
	}
}

func TestNativeBatchPlaceOrdersStopsLaterChunksOnUnknown(t *testing.T) {
	ex := &batchRecordingExchange{
		batchSize: 1,
		placeBatch: func(_ context.Context, orders []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
			return []exchange.PlaceOrderBatchItem{{
				Err: errors.Join(exchange.ErrOrderPlacementUnknown, errors.New("lost")),
			}}, nil
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.rateLimiter = &countingWaiter{}

	unknown := false
	placed, _, err := executor.BatchPlaceOrders([]*OrderRequest{
		{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1, ClientOrderID: "a", OnSubmissionUnknown: func() { unknown = true }},
		{Symbol: "ETHUSDT", Side: "BUY", Price: 99, Quantity: 0.1, ClientOrderID: "b"},
	})
	if !errors.Is(err, exchange.ErrOrderPlacementUnknown) {
		t.Fatalf("error = %v, want UNKNOWN", err)
	}
	if !unknown {
		t.Fatal("UNKNOWN callback was not invoked")
	}
	if len(placed) != 0 || ex.batchCalls != 1 || len(ex.requests) != 1 {
		t.Fatalf("placed=%d batchCalls=%d requests=%d, want 0/1/1", len(placed), ex.batchCalls, len(ex.requests))
	}
}

func TestNativeBatchPlaceOrdersHonorsPlacementMarkerBeforeContextCause(t *testing.T) {
	tests := []struct {
		name         string
		placementErr error
		wantUnknown  bool
	}{
		{
			name:         "rejected canceled",
			placementErr: exchangeerr.WrapOrderPlacementRejected(context.Canceled),
		},
		{
			name:         "rejected deadline exceeded",
			placementErr: exchangeerr.WrapOrderPlacementRejected(context.DeadlineExceeded),
		},
		{
			name:         "unknown canceled",
			placementErr: exchangeerr.WrapOrderPlacementUnknown(context.Canceled),
			wantUnknown:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := &batchRecordingExchange{
				batchSize: 5,
				placeBatch: func(_ context.Context, _ []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
					return []exchange.PlaceOrderBatchItem{{Err: tt.placementErr}}, nil
				},
			}
			executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
			executor.rateLimiter = &countingWaiter{}

			unknown := false
			placed, margin, err := executor.BatchPlaceOrders([]*OrderRequest{{
				Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1,
				ClientOrderID: "a", OnSubmissionUnknown: func() { unknown = true },
			}})
			if err == nil {
				t.Fatal("BatchPlaceOrders() error = nil, want placement error")
			}
			if got := errors.Is(err, exchange.ErrOrderPlacementUnknown); got != tt.wantUnknown {
				t.Fatalf("UNKNOWN classification = %v, want %v: %v", got, tt.wantUnknown, err)
			}
			if unknown != tt.wantUnknown {
				t.Fatalf("UNKNOWN callback = %v, want %v", unknown, tt.wantUnknown)
			}
			if got := IsDefiniteOrderRejection(err); got == tt.wantUnknown {
				t.Fatalf("definite rejection = %v, want %v: %v", got, !tt.wantUnknown, err)
			}
			if !tt.wantUnknown {
				var rejected *OrderRejectedError
				if !errors.As(err, &rejected) || rejected.Kind != OrderRejectionExchangeRejected {
					t.Fatalf("rejection = %#v, want kind %q", rejected, OrderRejectionExchangeRejected)
				}
			}
			if len(placed) != 0 || margin || ex.batchCalls != 1 {
				t.Fatalf("placed=%d margin=%v batchCalls=%d, want 0/false/1", len(placed), margin, ex.batchCalls)
			}
		})
	}
}

func TestNativeBatchPlaceOrdersKeepsSellsAfterBuyMarginReject(t *testing.T) {
	ex := &batchRecordingExchange{
		batchSize: 5,
		placeBatch: func(_ context.Context, orders []*exchange.OrderRequest) ([]exchange.PlaceOrderBatchItem, error) {
			items := make([]exchange.PlaceOrderBatchItem, len(orders))
			for i, req := range orders {
				if req.Side == exchange.SideBuy {
					items[i].Err = NewOrderRejectedError(OrderRejectionMargin, errors.New("insufficient margin"))
					continue
				}
				items[i].Order = &exchange.Order{
					OrderID: 9, ClientOrderID: req.ClientOrderID, Side: req.Side,
					Status: exchange.OrderStatusNew, Price: req.Price, Quantity: req.Quantity,
				}
			}
			return items, nil
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)
	executor.rateLimiter = &countingWaiter{}

	placed, margin, err := executor.BatchPlaceOrders([]*OrderRequest{
		{Symbol: "ETHUSDT", Side: "SELL", Price: 101, Quantity: 0.1, ReduceOnly: true, ClientOrderID: "s1"},
		{Symbol: "ETHUSDT", Side: "BUY", Price: 100, Quantity: 0.1, ClientOrderID: "b1"},
		{Symbol: "ETHUSDT", Side: "BUY", Price: 99, Quantity: 0.1, ClientOrderID: "b2"},
	})
	if err != nil {
		t.Fatalf("BatchPlaceOrders() error = %v", err)
	}
	if !margin || len(placed) != 1 || placed[0].Side != "SELL" {
		t.Fatalf("placed=%+v margin=%v", placed, margin)
	}
	if ex.batchCalls != 1 {
		t.Fatalf("native batch calls = %d, want 1", ex.batchCalls)
	}
}
