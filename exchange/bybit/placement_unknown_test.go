package bybit

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
		_, _ = w.Write([]byte(`{"retCode":0,"retMsg":"OK","result":{},"time":1}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BybitAdapter{
		client:           client,
		orderIDs:         newOrderIDMapper(),
		priceDecimals:    2,
		quantityDecimals: 3,
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
		_, _ = w.Write([]byte(`{"retCode":10001,"retMsg":"bad request","result":{},"time":1}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BybitAdapter{client: client, orderIDs: newOrderIDMapper(), priceDecimals: 2, quantityDecimals: 3}

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
		_, _ = w.Write([]byte(`{"retCode":10000,"retMsg":"server timeout","result":{},"time":1}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BybitAdapter{client: client, orderIDs: newOrderIDMapper(), priceDecimals: 2, quantityDecimals: 3}
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
	adapter := &BybitAdapter{
		client: client, orderIDs: newOrderIDMapper(), priceDecimals: 2, quantityDecimals: 3,
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
		err := classifyBybitPlacementError(&APIError{StatusCode: status, RetCode: 10000})
		if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
			t.Fatalf("status %d classified as %v, want UNKNOWN only", status, err)
		}
	}

	err := classifyBybitPlacementError(errors.New("Bybit 请求失败: connection reset"))
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("transport error classified as %v, want UNKNOWN only", err)
	}

	for _, code := range []int{10014, 110072} {
		err = classifyBybitPlacementError(&APIError{
			StatusCode: http.StatusOK,
			RetCode:    code,
			Message:    "duplicate orderLinkId",
		})
		if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
			t.Fatalf("duplicate code %d classified as %v, want UNKNOWN only", code, err)
		}
	}
	err = classifyBybitPlacementError(errors.New("orderLinkId is not unique"))
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
		t.Fatalf("duplicate orderLinkId message classified as %v, want UNKNOWN only", err)
	}
	for _, code := range []int{10000, 10016, 170007, 170146} {
		err = classifyBybitPlacementError(&APIError{
			StatusCode: http.StatusOK,
			RetCode:    code,
			Message:    "server timeout",
		})
		if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) || exchangeerr.IsOrderPlacementRejected(err) {
			t.Fatalf("ambiguous code %d classified as %v, want UNKNOWN only", code, err)
		}
	}
}
