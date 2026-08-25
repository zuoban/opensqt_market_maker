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
	if err == nil || errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want definite API rejection", err)
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
}
