package bitget

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
		_, _ = w.Write([]byte(`{"code":"00000","msg":"success","data":{}}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BitgetAdapter{
		client: client, symbol: "BTCUSDT", productType: "usdt-futures",
		volumePlace: 3, pricePlace: 2, posMode: "one_way_mode",
	}
	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit,
		Price: 100, Quantity: 0.01, ClientOrderID: "grid-order-1",
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementUnknown", err)
	}
}

func TestPlaceOrderExplicitAPIRejectionIsDefinite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"40017","msg":"rejected","data":{}}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BitgetAdapter{client: client, symbol: "BTCUSDT", productType: "usdt-futures", volumePlace: 3, pricePlace: 2}
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
		_, _ = w.Write([]byte(`{"code":"50000","msg":"server timeout","data":{}}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BitgetAdapter{client: client, symbol: "BTCUSDT", productType: "usdt-futures", volumePlace: 3, pricePlace: 2}
	_, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01,
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementUnknown", err)
	}
}
