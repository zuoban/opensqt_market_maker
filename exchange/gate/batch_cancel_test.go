package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBatchCancelOrdersReturnsSingleCancelFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"label":"INVALID_ARGUMENT","message":"cancel rejected"}`))
	}))
	defer server.Close()

	adapter := newGateCancelTestAdapter(server)
	if err := adapter.BatchCancelOrders(context.Background(), "BTCUSDT", []int64{11}); err == nil {
		t.Fatal("BatchCancelOrders() error = nil, want single cancel failure")
	}
}

func TestBatchCancelOrdersFallsBackAfterBatchRequestFailure(t *testing.T) {
	tests := []struct {
		name      string
		failID    string
		wantErr   bool
		wantCalls int32
	}{
		{name: "all individual cancels succeed", wantCalls: 2},
		{name: "one individual cancel fails", failID: "12", wantErr: true, wantCalls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fallbackCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/futures/usdt/batch_cancel_orders":
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"label":"SERVER_ERROR","message":"temporarily unavailable"}`))
				case r.Method == http.MethodDelete:
					fallbackCalls.Add(1)
					orderID := r.URL.Path[len("/futures/usdt/orders/"):]
					if orderID == tt.failID {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"label":"INVALID_ARGUMENT","message":"cancel rejected"}`))
						return
					}
					_, _ = w.Write([]byte(`{}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			adapter := newGateCancelTestAdapter(server)
			err := adapter.BatchCancelOrders(context.Background(), "BTCUSDT", []int64{11, 12})
			if (err != nil) != tt.wantErr {
				t.Fatalf("BatchCancelOrders() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got := fallbackCalls.Load(); got != tt.wantCalls {
				t.Fatalf("individual cancel calls = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestBatchCancelOrdersValidatesEveryBatchResult(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantErr  bool
	}{
		{
			name:     "all orders covered",
			response: `[{"id":"11","succeeded":true},{"id":"12","succeeded":true}]`,
		},
		{
			name:     "not found is already canceled",
			response: `[{"id":"11","succeeded":true},{"id":"12","succeeded":false,"label":"ORDER_NOT_FOUND","message":"order not found"}]`,
		},
		{
			name:     "failed result item",
			response: `[{"id":"11","succeeded":true},{"id":"12","succeeded":false,"message":"order is still open"}]`,
			wantErr:  true,
		},
		{
			name:     "missing result item",
			response: `[{"id":"11","succeeded":true}]`,
			wantErr:  true,
		},
		{
			name:     "duplicate result item",
			response: `[{"id":"11","succeeded":true},{"id":"11","succeeded":true},{"id":"12","succeeded":true}]`,
			wantErr:  true,
		},
		{
			name:     "unrequested result item",
			response: `[{"id":"11","succeeded":true},{"id":"13","succeeded":true}]`,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/futures/usdt/batch_cancel_orders" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.response))
			}))
			defer server.Close()

			adapter := newGateCancelTestAdapter(server)
			err := adapter.BatchCancelOrders(context.Background(), "BTCUSDT", []int64{11, 12})
			if (err != nil) != tt.wantErr {
				t.Fatalf("BatchCancelOrders() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func newGateCancelTestAdapter(server *httptest.Server) *GateAdapter {
	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	return &GateAdapter{
		client: client,
		symbol: "BTCUSDT",
		settle: "usdt",
	}
}
