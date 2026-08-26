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
	if !exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementRejected", err)
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
	if exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("PlaceOrder() error = %v, server error must not be definite rejection", err)
	}
}

func TestPlaceOrderLocalRequestConstructionErrorIsDefinite(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	client, err := NewClient("test-key", secret)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	client.baseURL = "://invalid"
	adapter := &BackpackAdapter{
		client: client, idMapper: newClientIDMapper(), priceDecimals: 2, quantityDecimals: 3,
	}

	_, err = adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDC", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01,
	})
	if !exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("PlaceOrder() error = %v, want local ErrOrderPlacementRejected", err)
	}
}

func TestClassifyPlacementErrorNeverRejectsAmbiguousStatusOrTransportFailure(t *testing.T) {
	for _, status := range []int{408, 409, 500, 502, 503, 504} {
		err := classifyBackpackPlacementError(&APIError{StatusCode: status, Code: "SERVER_ERROR"})
		if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
			t.Fatalf("status %d classified as %v, want UNKNOWN only", status, err)
		}
	}

	err := classifyBackpackPlacementError(errors.New("Backpack 请求失败: connection reset"))
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("transport error classified as %v, want UNKNOWN only", err)
	}

	err = classifyBackpackPlacementError(&APIError{
		StatusCode: http.StatusBadRequest,
		Code:       "DUPLICATE_CLIENT_ID",
		Message:    "clientId already used",
	})
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("duplicate clientId classified as %v, want UNKNOWN only", err)
	}
	err = classifyBackpackPlacementError(errors.New("clientId is not unique"))
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("duplicate clientId message classified as %v, want UNKNOWN only", err)
	}
	for _, code := range []string{"TIMEOUT", "SERVER_ERROR"} {
		err = classifyBackpackPlacementError(&APIError{
			StatusCode: http.StatusBadRequest,
			Code:       code,
		})
		if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
			t.Fatalf("ambiguous code %s classified as %v, want UNKNOWN only", code, err)
		}
	}
}
