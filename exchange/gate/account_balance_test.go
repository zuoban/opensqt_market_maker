package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetAccountRejectsInvalidBalanceFields(t *testing.T) {
	tests := []struct {
		name    string
		account string
		wantErr []string
	}{
		{
			name:    "total missing",
			account: `{"unrealised_pnl":"1","available":"2"}`,
			wantErr: []string{"total", "缺失"},
		},
		{
			name:    "unrealised pnl malformed",
			account: `{"total":"10","unrealised_pnl":"broken","available":"2"}`,
			wantErr: []string{"unrealised_pnl", "不是有效数字"},
		},
		{
			name:    "available malformed",
			account: `{"total":"10","unrealised_pnl":"1","available":"broken"}`,
			wantErr: []string{"available", "不是有效数字"},
		},
		{
			name:    "total non-finite",
			account: `{"total":"NaN","unrealised_pnl":"1","available":"2"}`,
			wantErr: []string{"total", "不是有限数字"},
		},
		{
			name:    "unrealised pnl non-finite",
			account: `{"total":"10","unrealised_pnl":"-Inf","available":"2"}`,
			wantErr: []string{"unrealised_pnl", "不是有限数字"},
		},
		{
			name:    "available non-finite",
			account: `{"total":"10","unrealised_pnl":"1","available":"+Inf"}`,
			wantErr: []string{"available", "不是有限数字"},
		},
		{
			name:    "total margin overflow",
			account: `{"total":"1.7976931348623157e308","unrealised_pnl":"1.7976931348623157e308","available":"2"}`,
			wantErr: []string{"totalMarginBalance", "不是有限数字"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.account))
			}))
			defer server.Close()

			client := NewClient("test-key", "test-secret")
			client.baseURL = server.URL
			client.httpClient = server.Client()
			adapter := &GateAdapter{
				client: client, wsManager: NewWebSocketManager("test-key", "test-secret", "usdt"),
				symbol: "BTCUSDT", gateSymbol: "BTC_USDT", settle: "usdt",
			}

			_, err := adapter.GetAccount(context.Background())
			if err == nil {
				t.Fatal("GetAccount() error = nil, want invalid balance error")
			}
			for _, fragment := range tt.wantErr {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("GetAccount() error = %q, want it to contain %q", err, fragment)
				}
			}
		})
	}
}
