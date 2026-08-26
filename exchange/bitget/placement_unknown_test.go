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
	if !exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementRejected", err)
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
	if exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("PlaceOrder() error = %v, server error must not be definite rejection", err)
	}
}

func TestPlaceOrderLocalRequestConstructionErrorIsDefinite(t *testing.T) {
	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = "://invalid"
	adapter := &BitgetAdapter{
		client: client, symbol: "BTCUSDT", productType: "usdt-futures",
		volumePlace: 3, pricePlace: 2, posMode: "one_way_mode",
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
		err := classifyBitgetPlacementError(&APIError{StatusCode: status, Code: "50000"})
		if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
			t.Fatalf("status %d classified as %v, want UNKNOWN only", status, err)
		}
	}

	err := classifyBitgetPlacementError(errors.New("请求失败: connection reset"))
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("transport error classified as %v, want UNKNOWN only", err)
	}

	err = classifyBitgetPlacementError(&APIError{
		StatusCode: http.StatusOK,
		Code:       "40786",
		Message:    "Duplicate clientOid",
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("duplicate clientOid classified as %v, want UNKNOWN only", err)
	}
	err = classifyBitgetPlacementError(errors.New("clientOid has already been used"))
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("duplicate clientOid message classified as %v, want UNKNOWN only", err)
	}
	err = classifyBitgetPlacementError(&APIError{
		StatusCode: http.StatusOK,
		Code:       "50000",
		Message:    "server timeout",
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("ambiguous business error classified as %v, want UNKNOWN only", err)
	}
}
