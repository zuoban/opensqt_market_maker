package gate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"opensqt/exchange/exchangeerr"
)

func TestPlaceOrderInvalidSuccessIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &GateAdapter{
		client: client, symbol: "BTCUSDT", gateSymbol: "BTC_USDT", settle: "usdt",
		quantoMultiplier: 0.001, pricePlace: 2,
	}
	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit,
		Price: 100, Quantity: 0.01, ClientOrderID: "grid-order-1",
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementUnknown", err)
	}
}

func TestPlaceOrderExplicitClientErrorIsDefinite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"label":"INVALID_ARGUMENT","message":"rejected"}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &GateAdapter{client: client, symbol: "BTCUSDT", gateSymbol: "BTC_USDT", settle: "usdt", quantoMultiplier: 0.001, pricePlace: 2}
	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01,
	})
	if !exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementRejected", err)
	}
}

func TestPlaceOrderServerErrorIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"label":"SERVER_ERROR","message":"timeout"}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &GateAdapter{client: client, symbol: "BTCUSDT", gateSymbol: "BTC_USDT", settle: "usdt", quantoMultiplier: 0.001, pricePlace: 2}
	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01,
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementUnknown", err)
	}
	if exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("PlaceOrder() error = %v, server error must not be definite rejection", err)
	}
}

func TestPlaceOrderLocalRequestConstructionErrorIsDefinite(t *testing.T) {
	client := NewClient("test-key", "test-secret")
	client.baseURL = "://invalid"
	adapter := &GateAdapter{
		client: client, symbol: "BTCUSDT", gateSymbol: "BTC_USDT", settle: "usdt",
		quantoMultiplier: 0.001, pricePlace: 2,
	}

	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01,
	})
	if !exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("PlaceOrder() error = %v, want local ErrOrderPlacementRejected", err)
	}
}

func TestClassifyPlacementErrorNeverRejectsAmbiguousStatusOrTransportFailure(t *testing.T) {
	for _, status := range []int{408, 409, 500, 502, 503, 504} {
		err := classifyGatePlacementError(&APIError{
			StatusCode: status,
			Label:      "INVALID_KEY",
			Message:    "upstream unavailable",
		})
		if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
			t.Fatalf("status %d classified as %v, want UNKNOWN only", status, err)
		}
	}

	err := classifyGatePlacementError(errors.New("请求失败: connection reset"))
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("transport error classified as %v, want UNKNOWN only", err)
	}

	err = classifyGatePlacementError(&APIError{
		StatusCode: http.StatusBadRequest,
		Label:      "DUPLICATE_REQUEST",
		Message:    "client order id already exists",
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("duplicate request classified as %v, want UNKNOWN only", err)
	}
	err = classifyGatePlacementError(errors.New("client order id already exists"))
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("duplicate client order id message classified as %v, want UNKNOWN only", err)
	}
	err = classifyGatePlacementError(&APIError{
		StatusCode: http.StatusBadRequest,
		Label:      "REPEATED_CREATION",
		Message:    "order already exists",
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("repeated creation classified as %v, want UNKNOWN only", err)
	}
}
