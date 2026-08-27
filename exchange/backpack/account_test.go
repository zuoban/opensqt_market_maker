package backpack

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestGetAccountRejectsInvalidCriticalBalances(t *testing.T) {
	tests := []struct {
		name      string
		equity    string
		available string
		wantField string
	}{
		{name: "malformed available", equity: "100", available: "broken", wantField: "netEquityAvailable"},
		{name: "non-finite available", equity: "100", available: "NaN", wantField: "netEquityAvailable"},
		{name: "missing equity", available: "75", wantField: "netEquity"},
		{name: "non-finite equity", equity: "+Inf", available: "75", wantField: "netEquity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, closeServer := newPlacementTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/account":
					_, _ = w.Write([]byte(`{"leverageLimit":"5"}`))
				case "/api/v1/capital/collateral":
					_, _ = fmt.Fprintf(w, `{
						"netEquity":%q,
						"netEquityAvailable":%q,
						"netEquityLocked":"0"
					}`, tt.equity, tt.available)
				default:
					http.Error(w, "unexpected path", http.StatusNotFound)
				}
			}))
			defer closeServer()

			adapter := &BackpackAdapter{client: client, symbol: "BTCUSDC"}
			_, err := adapter.GetAccount(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantField) {
				t.Fatalf("GetAccount() error = %v, want field %s", err, tt.wantField)
			}
		})
	}
}

func TestGetAccountUsesNetEquityAsTotalMarginBalance(t *testing.T) {
	client, closeServer := newPlacementTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/account":
			_, _ = w.Write([]byte(`{"leverageLimit":"5"}`))
		case "/api/v1/capital/collateral":
			_, _ = w.Write([]byte(`{
				"netEquity":"125.5",
				"netEquityAvailable":"80.25",
				"netEquityLocked":"17.75"
			}`))
		case "/api/v1/position":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer closeServer()

	adapter := &BackpackAdapter{client: client, symbol: "BTCUSDC"}
	account, err := adapter.GetAccount(context.Background())
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.TotalWalletBalance != 125.5 {
		t.Fatalf("TotalWalletBalance = %v, want 125.5", account.TotalWalletBalance)
	}
	if account.TotalMarginBalance != 125.5 {
		t.Fatalf("TotalMarginBalance = %v, want netEquity 125.5", account.TotalMarginBalance)
	}
	if account.AvailableBalance != 80.25 {
		t.Fatalf("AvailableBalance = %v, want 80.25", account.AvailableBalance)
	}
	if account.TotalMarginBalance == 17.75 {
		t.Fatal("TotalMarginBalance must not use netEquityLocked")
	}
}

func TestConcurrentGetAccountAndGetPositions(t *testing.T) {
	client, closeServer := newPlacementTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/account":
			_, _ = w.Write([]byte(`{"leverageLimit":"5"}`))
		case "/api/v1/capital/collateral":
			_, _ = w.Write([]byte(`{
				"netEquity":"125.5",
				"netEquityAvailable":"80.25",
				"netEquityLocked":"45.25"
			}`))
		case "/api/v1/position":
			_, _ = w.Write([]byte(`[{
				"symbol":"BTC_USDC_PERP",
				"netQuantity":"0.01",
				"entryPrice":"100",
				"markPrice":"101",
				"pnlUnrealized":"0.01"
			}]`))
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer closeServer()

	adapter := &BackpackAdapter{client: client, symbol: "BTCUSDC"}
	if _, err := adapter.GetAccount(context.Background()); err != nil {
		t.Fatalf("initial GetAccount() error = %v", err)
	}

	const workers = 8
	const iterations = 20
	start := make(chan struct{})
	errs := make(chan error, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(2)
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
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				positions, err := adapter.GetPositions(context.Background(), "BTCUSDC")
				if err != nil {
					errs <- fmt.Errorf("GetPositions() error = %w", err)
					return
				}
				if len(positions) != 1 || positions[0].Leverage != 5 {
					errs <- fmt.Errorf("GetPositions() = %+v, want one position at 5x", positions)
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
