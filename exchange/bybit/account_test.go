package bybit

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestGetAccountRejectsInvalidCriticalBalances(t *testing.T) {
	tests := []struct {
		name      string
		wallet    string
		margin    string
		available string
		wantField string
	}{
		{name: "malformed available", wallet: "100", margin: "100", available: "broken", wantField: "totalAvailableBalance"},
		{name: "non-finite available", wallet: "100", margin: "100", available: "NaN", wantField: "totalAvailableBalance"},
		{name: "missing wallet", margin: "100", available: "75", wantField: "totalWalletBalance"},
		{name: "non-finite margin", wallet: "100", margin: "+Inf", available: "75", wantField: "totalMarginBalance"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/v5/account/wallet-balance" {
					http.Error(w, "unexpected path", http.StatusNotFound)
					return
				}
				_, _ = fmt.Fprintf(w, `{
					"retCode":0,"retMsg":"OK",
					"result":{"list":[{
						"totalWalletBalance":%q,
						"totalMarginBalance":%q,
						"totalAvailableBalance":%q
					}]},"time":1}`, tt.wallet, tt.margin, tt.available)
			}))
			defer server.Close()

			client := NewClient("test-key", "test-secret")
			client.baseURL = server.URL
			client.httpClient = server.Client()
			adapter := &BybitAdapter{client: client, symbol: "BTCUSDT"}
			_, err := adapter.GetAccount(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantField) {
				t.Fatalf("GetAccount() error = %v, want field %s", err, tt.wantField)
			}
		})
	}
}

func TestConcurrentGetAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v5/account/wallet-balance":
			_, _ = w.Write([]byte(`{
				"retCode":0,
				"retMsg":"OK",
				"result":{"list":[{
					"totalWalletBalance":"100",
					"totalMarginBalance":"100",
					"totalAvailableBalance":"75"
				}]},
				"time":1
			}`))
		case "/v5/position/list":
			_, _ = w.Write([]byte(`{
				"retCode":0,
				"retMsg":"OK",
				"result":{"list":[{
					"symbol":"BTCUSDT",
					"side":"Buy",
					"size":"0.01",
					"avgPrice":"100",
					"markPrice":"101",
					"leverage":"5",
					"tradeMode":0,
					"positionIM":"0.2",
					"unrealisedPnl":"0.01"
				}]},
				"time":1
			}`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BybitAdapter{client: client, symbol: "BTCUSDT"}

	const workers = 12
	const iterations = 20
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				account, err := adapter.GetAccount(context.Background())
				if err != nil {
					errs <- fmt.Errorf("GetAccount() error = %w", err)
					return
				}
				if account.AccountLeverage != 5 {
					errs <- fmt.Errorf("AccountLeverage = %d, want 5", account.AccountLeverage)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
