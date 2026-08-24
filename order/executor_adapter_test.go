package order

import (
	"context"
	"errors"
	"testing"

	"opensqt/exchange"
)

type recordingExchange struct {
	exchange.IExchange
	requests   []*exchange.OrderRequest
	placeOrder func(*exchange.OrderRequest) (*exchange.Order, error)
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

func TestPlaceOrderNeverDowngradesAfterPostOnlyRejections(t *testing.T) {
	ex := &recordingExchange{
		placeOrder: func(*exchange.OrderRequest) (*exchange.Order, error) {
			return nil, errors.New("code=-5022 Post Only order would immediately match")
		},
	}
	executor := NewExchangeOrderExecutor(ex, "ETHUSDT", 0, 0)

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
	for i, req := range ex.requests {
		if !req.PostOnly {
			t.Fatalf("request %d downgraded to a non-PostOnly order", i+1)
		}
	}
}
