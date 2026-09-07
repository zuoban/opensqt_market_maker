package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/exchange/exchangeerr"
	"opensqt/utils"
)

func TestPlaceOrderBatchPostsNativeBatchAndMapsMixedResults(t *testing.T) {
	var batchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != batchCreateOrderEndpoint || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		batchCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
			return
		}
		raw := r.Form.Get("batchOrders")
		var payload []map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Errorf("batchOrders JSON = %q: %v", raw, err)
			return
		}
		if len(payload) != 2 {
			t.Errorf("batch size = %d, want 2; payload=%#v", len(payload), payload)
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"code": -1102, "msg": "unexpected batch size"})
			return
		}
		if payload[0]["timeInForce"] != "GTX" || payload[1]["timeInForce"] != "GTX" {
			t.Errorf("timeInForce = %#v", payload)
		}
		writeJSON(t, w, http.StatusOK, []any{
			orderFixture(11, utils.AddBinanceBrokerPrefix("buy-1")),
			map[string]any{"code": -5022, "msg": "Post Only order will be rejected."},
		})
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	items, err := adapter.PlaceOrderBatch(context.Background(), []*OrderRequest{
		{Symbol: "TESTUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 100.1, Quantity: 0.1, PostOnly: true, ClientOrderID: "buy-1"},
		{Symbol: "TESTUSDT", Side: SideBuy, Type: OrderTypeLimit, Price: 99.9, Quantity: 0.1, PostOnly: true, ClientOrderID: "buy-2"},
	})
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if batchCalls.Load() != 1 {
		t.Fatalf("batch HTTP calls = %d, want 1", batchCalls.Load())
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}
	if items[0].Err != nil || items[0].Order == nil || items[0].Order.OrderID != 11 {
		t.Fatalf("success item = %+v", items[0])
	}
	if items[0].Order.Price != 100.1 || items[0].Order.Quantity != 0.1 {
		t.Fatalf("normalized order = %+v", items[0].Order)
	}
	if !exchangeerr.IsOrderPlacementRejected(items[1].Err) {
		t.Fatalf("PostOnly item error = %v, want rejected", items[1].Err)
	}
}

func TestPlaceOrderBatchAmbiguousHTTPConfirmsByClientOrderID(t *testing.T) {
	const clientOID = "grid-order-1"
	brokerID := utils.AddBinanceBrokerPrefix(clientOID)
	var postCalls, queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == batchCreateOrderEndpoint:
			postCalls.Add(1)
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "unknown"})
		case r.Method == http.MethodGet && r.URL.Path == createOrderEndpoint:
			queryCalls.Add(1)
			if got := r.URL.Query().Get("origClientOrderId"); got != brokerID {
				t.Errorf("origClientOrderId = %q, want %q", got, brokerID)
			}
			writeJSON(t, w, http.StatusOK, orderFixture(42, brokerID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	items, err := adapter.PlaceOrderBatch(context.Background(), []*OrderRequest{placementRequest()})
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if postCalls.Load() != 1 || queryCalls.Load() != 1 {
		t.Fatalf("calls = post:%d query:%d, want 1/1", postCalls.Load(), queryCalls.Load())
	}
	if len(items) != 1 || items[0].Err != nil || items[0].Order == nil || items[0].Order.OrderID != 42 {
		t.Fatalf("items = %+v", items)
	}
}

func TestPlaceOrderBatchConcurrentConfirmationKeepsRequestResultAlignment(t *testing.T) {
	orders := make([]*OrderRequest, maxPlaceOrderBatchSize)
	wantOrderIDs := make(map[string]int64, maxPlaceOrderBatchSize)
	for i := range orders {
		req := placementRequest()
		req.ClientOrderID = fmt.Sprintf("grid-order-%d", i+1)
		orders[i] = req
		brokerID := utils.AddBinanceBrokerPrefix(req.ClientOrderID)
		wantOrderIDs[brokerID] = int64(101 + i)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == batchCreateOrderEndpoint:
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "unknown"})
		case r.Method == http.MethodGet && r.URL.Path == createOrderEndpoint:
			clientOID := r.URL.Query().Get("origClientOrderId")
			orderID, ok := wantOrderIDs[clientOID]
			if !ok {
				t.Errorf("unexpected client order ID %q", clientOID)
				http.NotFound(w, r)
				return
			}
			// 后面的请求先完成，确保结果不能依赖 goroutine 完成顺序。
			time.Sleep(time.Duration(105-orderID) * 5 * time.Millisecond)
			writeJSON(t, w, http.StatusOK, orderFixture(orderID, clientOID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	items, err := adapter.PlaceOrderBatch(context.Background(), orders)
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if len(items) != len(orders) {
		t.Fatalf("items = %d, want %d", len(items), len(orders))
	}
	for i, item := range items {
		brokerID := utils.AddBinanceBrokerPrefix(orders[i].ClientOrderID)
		if item.Err != nil || item.Order == nil {
			t.Fatalf("items[%d] = %+v, want confirmed order", i, item)
		}
		if item.Order.ClientOrderID != brokerID || item.Order.OrderID != wantOrderIDs[brokerID] {
			t.Fatalf("items[%d] = %+v, want clientOID=%q orderID=%d",
				i, item.Order, brokerID, wantOrderIDs[brokerID])
		}
	}
}

func TestPlaceOrderBatchLocalValidationDoesNotSend(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	req := placementRequest()
	req.Quantity = 0
	items, err := adapter.PlaceOrderBatch(context.Background(), []*OrderRequest{req})
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP calls = %d, want 0", requests.Load())
	}
	if len(items) != 1 || !exchangeerr.IsOrderPlacementRejected(items[0].Err) {
		t.Fatalf("items = %+v", items)
	}
}

func TestPlaceOrderBatchUnknownStopsLaterChunks(t *testing.T) {
	var batchCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != batchCreateOrderEndpoint {
			http.NotFound(w, r)
			return
		}
		batchCalls.Add(1)
		writeJSON(t, w, http.StatusOK, []any{
			map[string]any{"code": -1000, "msg": "unknown error, please check your request"},
		})
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	orders := make([]*OrderRequest, maxPlaceOrderBatchSize+1)
	for i := range orders {
		req := placementRequest()
		req.ClientOrderID = fmt.Sprintf("grid-order-%d", i+1)
		orders[i] = req
	}
	items, err := adapter.PlaceOrderBatch(context.Background(), orders)
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if batchCalls.Load() != 1 {
		t.Fatalf("batch HTTP calls = %d, want 1", batchCalls.Load())
	}
	if !errors.Is(items[0].Err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("first item error = %v, want UNKNOWN", items[0].Err)
	}
	last := items[len(items)-1]
	if last.Err == nil || errors.Is(last.Err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("unsent item should be rejected, got %v", last.Err)
	}
}

func TestPlaceOrderBatchUnknownConfirmationUsesOneConcurrentBoundedWindow(t *testing.T) {
	const confirmationTimeout = 200 * time.Millisecond
	var queryCalls, activeQueries, peakQueries atomic.Int32
	releaseHandlers := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == batchCreateOrderEndpoint:
			// 让原下单上下文先超时，确认查询仍应使用独立的只读窗口。
			<-releaseHandlers
		case r.Method == http.MethodGet && r.URL.Path == createOrderEndpoint:
			queryCalls.Add(1)
			active := activeQueries.Add(1)
			defer activeQueries.Add(-1)
			for {
				peak := peakQueries.Load()
				if active <= peak || peakQueries.CompareAndSwap(peak, active) {
					break
				}
			}
			<-releaseHandlers
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		close(releaseHandlers)
		server.Close()
	})

	adapter := testAdapter(t, server.URL, "USDT")
	adapter.orderConfirmationTimeout = confirmationTimeout
	orders := make([]*OrderRequest, maxPlaceOrderBatchSize)
	for i := range orders {
		req := placementRequest()
		req.ClientOrderID = fmt.Sprintf("grid-order-%d", i+1)
		orders[i] = req
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	items, err := adapter.PlaceOrderBatch(ctx, orders)
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if queryCalls.Load() != maxPlaceOrderBatchSize {
		t.Fatalf("confirmation queries = %d, want %d", queryCalls.Load(), maxPlaceOrderBatchSize)
	}
	if peakQueries.Load() != maxPlaceOrderBatchSize {
		t.Fatalf("peak concurrent confirmation queries = %d, want %d", peakQueries.Load(), maxPlaceOrderBatchSize)
	}
	if elapsed >= 3*confirmationTimeout {
		t.Fatalf("batch confirmation took %v, want one bounded window below %v", elapsed, 3*confirmationTimeout)
	}
	for i, item := range items {
		if !errors.Is(item.Err, exchangeerr.ErrOrderPlacementUnknown) {
			t.Fatalf("items[%d].Err = %v, want UNKNOWN", i, item.Err)
		}
	}
}

func TestPlaceOrderBatchItemUnknownConfirmationsUseOneConcurrentBoundedWindow(t *testing.T) {
	const confirmationTimeout = 200 * time.Millisecond
	var queryCalls, activeQueries, peakQueries atomic.Int32
	releaseHandlers := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == batchCreateOrderEndpoint:
			response := make([]any, maxPlaceOrderBatchSize)
			for i := range response {
				response[i] = map[string]any{"code": -1000, "msg": "unknown error, please check your request"}
			}
			writeJSON(t, w, http.StatusOK, response)
		case r.Method == http.MethodGet && r.URL.Path == createOrderEndpoint:
			queryCalls.Add(1)
			active := activeQueries.Add(1)
			defer activeQueries.Add(-1)
			for {
				peak := peakQueries.Load()
				if active <= peak || peakQueries.CompareAndSwap(peak, active) {
					break
				}
			}
			<-releaseHandlers
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		close(releaseHandlers)
		server.Close()
	})

	adapter := testAdapter(t, server.URL, "USDT")
	adapter.orderConfirmationTimeout = confirmationTimeout
	orders := make([]*OrderRequest, maxPlaceOrderBatchSize)
	for i := range orders {
		req := placementRequest()
		req.ClientOrderID = fmt.Sprintf("grid-order-%d", i+1)
		orders[i] = req
	}

	startedAt := time.Now()
	items, err := adapter.PlaceOrderBatch(context.Background(), orders)
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if queryCalls.Load() != maxPlaceOrderBatchSize || peakQueries.Load() != maxPlaceOrderBatchSize {
		t.Fatalf("confirmation queries = %d, peak concurrent = %d, want %d/%d",
			queryCalls.Load(), peakQueries.Load(), maxPlaceOrderBatchSize, maxPlaceOrderBatchSize)
	}
	if elapsed >= 3*confirmationTimeout {
		t.Fatalf("item confirmation took %v, want one bounded window below %v", elapsed, 3*confirmationTimeout)
	}
	for i, item := range items {
		if !errors.Is(item.Err, exchangeerr.ErrOrderPlacementUnknown) {
			t.Fatalf("items[%d].Err = %v, want UNKNOWN", i, item.Err)
		}
	}
}

func TestPlaceOrderBatchDoesNotRetryConfirmedMissingOrdersAfterParentCancellation(t *testing.T) {
	var queryCalls, singlePlaceCalls atomic.Int32
	releaseBatchHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == batchCreateOrderEndpoint:
			<-releaseBatchHandler
		case r.Method == http.MethodGet && r.URL.Path == createOrderEndpoint:
			queryCalls.Add(1)
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
		case r.Method == http.MethodPost && r.URL.Path == createOrderEndpoint:
			singlePlaceCalls.Add(1)
			writeJSON(t, w, http.StatusOK, orderFixture(999, r.URL.Query().Get("newClientOrderId")))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(func() {
		close(releaseBatchHandler)
		server.Close()
	})

	adapter := testAdapter(t, server.URL, "USDT")
	orders := make([]*OrderRequest, maxPlaceOrderBatchSize)
	for i := range orders {
		req := placementRequest()
		req.ClientOrderID = fmt.Sprintf("grid-order-%d", i+1)
		orders[i] = req
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	items, err := adapter.PlaceOrderBatch(ctx, orders)
	if err != nil {
		t.Fatalf("PlaceOrderBatch() error = %v", err)
	}
	if queryCalls.Load() != maxPlaceOrderBatchSize {
		t.Fatalf("confirmation queries = %d, want %d", queryCalls.Load(), maxPlaceOrderBatchSize)
	}
	if singlePlaceCalls.Load() != 0 {
		t.Fatalf("single-order retries = %d, want 0 after parent cancellation", singlePlaceCalls.Load())
	}
	for i, item := range items {
		if !errors.Is(item.Err, exchangeerr.ErrOrderPlacementUnknown) ||
			exchangeerr.IsOrderPlacementRejected(item.Err) {
			t.Fatalf("items[%d].Err = %v, want UNKNOWN without retry", i, item.Err)
		}
	}
}
