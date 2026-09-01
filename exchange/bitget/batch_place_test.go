package bitget

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"opensqt/exchange/exchangeerr"
)

func TestPlaceOrderBatchMapsSuccessAndFailureLists(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/mix/order/batch-place-order" {
			t.Errorf("path = %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("request JSON: %v", err)
			return
		}
		list, _ := payload["orderList"].([]any)
		if len(list) != 2 {
			t.Errorf("orderList = %d, want 2", len(list))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"00000","msg":"success","data":{"successList":[{"orderId":"11","clientOid":"buy-1"}],"failureList":[{"clientOid":"buy-2","errorCode":"40794","errorMsg":"Post Only order will be rejected"}]}}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BitgetAdapter{
		client: client, symbol: "BTCUSDT", productType: "usdt-futures",
		volumePlace: 3, pricePlace: 2, posMode: "one_way_mode",
	}
	items, err := adapter.PlaceOrderBatch(context.Background(), []*OrderRequest{
		{Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01, PostOnly: true, ClientOrderID: "buy-1"},
		{Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 99, Quantity: 0.01, PostOnly: true, ClientOrderID: "buy-2"},
	})
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls.Load())
	}
	if len(items) != 2 || items[0].Order == nil || items[0].Order.OrderID != 11 {
		t.Fatalf("success item = %+v", items[0])
	}
	if !exchangeerr.IsOrderPlacementRejected(items[1].Err) {
		t.Fatalf("failure item error = %v, want rejected", items[1].Err)
	}
}

func TestPlaceOrderBatchServerErrorIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"50000","msg":"server timeout","data":{}}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BitgetAdapter{
		client: client, symbol: "BTCUSDT", productType: "usdt-futures",
		volumePlace: 3, pricePlace: 2, posMode: "one_way_mode",
	}
	items, err := adapter.PlaceOrderBatch(context.Background(), []*OrderRequest{
		{Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01, ClientOrderID: "buy-1"},
	})
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if len(items) != 1 || !errors.Is(items[0].Err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("items = %+v, want UNKNOWN", items)
	}
}

func TestPlaceOrderBatchMissingResultIsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"00000","msg":"success","data":{"successList":[],"failureList":[]}}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BitgetAdapter{
		client: client, symbol: "BTCUSDT", productType: "usdt-futures",
		volumePlace: 3, pricePlace: 2, posMode: "one_way_mode",
	}
	items, err := adapter.PlaceOrderBatch(context.Background(), []*OrderRequest{
		{Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 100, Quantity: 0.01, ClientOrderID: "buy-1"},
	})
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if len(items) != 1 || !errors.Is(items[0].Err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("items = %+v, want UNKNOWN", items)
	}
}
