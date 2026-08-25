package backpack

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"opensqt/exchange/exchangeerr"
)

func newPlacementTestClient(t *testing.T, handler http.Handler) (*Client, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	secret := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	client, err := NewClient("test-key", secret)
	if err != nil {
		server.Close()
		t.Fatalf("NewClient() error = %v", err)
	}
	client.baseURL = server.URL
	client.httpClient = server.Client()
	return client, server.Close
}

func TestPlaceOrderInvalidSuccessIsUnknown(t *testing.T) {
	client, closeServer := newPlacementTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer closeServer()

	adapter := &BackpackAdapter{
		client: client, idMapper: newClientIDMapper(), priceDecimals: 2, quantityDecimals: 3,
	}
	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDC", Side: SideBuy, Type: OrderTypeLimit,
		Price: 100, Quantity: 0.01, ClientOrderID: "grid-order-1",
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementUnknown", err)
	}
}

func TestPlaceOrderExplicitClientErrorIsDefinite(t *testing.T) {
	client, closeServer := newPlacementTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"INVALID_ORDER","message":"rejected"}`))
	}))
	defer closeServer()

	adapter := &BackpackAdapter{client: client, idMapper: newClientIDMapper(), priceDecimals: 2, quantityDecimals: 3}
	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDC", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01,
	})
	if err == nil || errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want definite API rejection", err)
	}
}

func TestPlaceOrderServerErrorIsUnknown(t *testing.T) {
	client, closeServer := newPlacementTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"SERVER_ERROR","message":"timeout"}`))
	}))
	defer closeServer()

	adapter := &BackpackAdapter{client: client, idMapper: newClientIDMapper(), priceDecimals: 2, quantityDecimals: 3}
	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDC", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01,
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementUnknown", err)
	}
}
