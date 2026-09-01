package bitget

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestBatchCancelOrdersReturnsSingleCancelFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"40017","msg":"cancel rejected","data":{}}`))
	}))
	defer server.Close()

	adapter := newBitgetCancelTestAdapter(server)
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
				switch r.URL.Path {
				case "/api/v2/mix/order/batch-cancel-orders":
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"code":"50000","msg":"temporarily unavailable","data":{}}`))
				case "/api/v2/mix/order/cancel-order":
					fallbackCalls.Add(1)
					var body struct {
						OrderID string `json:"orderId"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode cancel request: %v", err)
					}
					if body.OrderID == tt.failID {
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"code":"40017","msg":"cancel rejected","data":{}}`))
						return
					}
					_, _ = w.Write([]byte(`{"code":"00000","msg":"success","data":{}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			adapter := newBitgetCancelTestAdapter(server)
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
		name    string
		data    string
		wantErr bool
	}{
		{
			name: "all orders covered",
			data: `{"successList":[{"orderId":"11"},{"orderId":"12"}],"failureList":[]}`,
		},
		{
			name: "not found is already canceled",
			data: `{"successList":[{"orderId":"11"}],"failureList":[{"orderId":"12","errorCode":"40029","errorMsg":"order does not exist"}]}`,
		},
		{
			name:    "failed result item",
			data:    `{"successList":[{"orderId":"11"}],"failureList":[{"orderId":"12","errorCode":"40017","errorMsg":"cancel rejected"}]}`,
			wantErr: true,
		},
		{
			name:    "missing result item",
			data:    `{"successList":[{"orderId":"11"}],"failureList":[]}`,
			wantErr: true,
		},
		{
			name:    "duplicate result item",
			data:    `{"successList":[{"orderId":"11"},{"orderId":"11"},{"orderId":"12"}],"failureList":[]}`,
			wantErr: true,
		},
		{
			name:    "unrequested result item",
			data:    `{"successList":[{"orderId":"11"},{"orderId":"13"}],"failureList":[]}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/mix/order/batch-cancel-orders" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"code":"00000","msg":"success","data":%s}`, tt.data)
			}))
			defer server.Close()

			adapter := newBitgetCancelTestAdapter(server)
			err := adapter.BatchCancelOrders(context.Background(), "BTCUSDT", []int64{11, 12})
			if (err != nil) != tt.wantErr {
				t.Fatalf("BatchCancelOrders() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func newBitgetCancelTestAdapter(server *httptest.Server) *BitgetAdapter {
	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	return &BitgetAdapter{
		client:      client,
		symbol:      "BTCUSDT",
		productType: "usdt-futures",
		marginCoin:  "USDT",
	}
}
