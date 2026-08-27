package bitget

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
		data    string
		wantErr []string
	}{
		{
			name:    "account equity missing",
			data:    `{"available":"2","marginMode":"crossed"}`,
			wantErr: []string{"accountEquity", "缺失"},
		},
		{
			name:    "account equity malformed",
			data:    `{"accountEquity":"broken","available":"2","marginMode":"crossed"}`,
			wantErr: []string{"accountEquity", "不是有效数字"},
		},
		{
			name:    "available malformed",
			data:    `{"accountEquity":"10","available":"broken","marginMode":"crossed"}`,
			wantErr: []string{"available", "不是有效数字"},
		},
		{
			name:    "account equity non-finite",
			data:    `{"accountEquity":"NaN","available":"2","marginMode":"crossed"}`,
			wantErr: []string{"accountEquity", "不是有限数字"},
		},
		{
			name:    "available non-finite",
			data:    `{"accountEquity":"10","available":"+Inf","marginMode":"crossed"}`,
			wantErr: []string{"available", "不是有限数字"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":"00000","msg":"success","data":` + tt.data + `}`))
			}))
			defer server.Close()

			client := NewClient("test-key", "test-secret", "test-passphrase")
			client.baseURL = server.URL
			client.httpClient = server.Client()
			adapter := &BitgetAdapter{
				client: client, symbol: "BTCUSDT", productType: "usdt-futures", marginCoin: "USDT",
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
