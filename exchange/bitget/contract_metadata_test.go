package bitget

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestContractMetadataCombinesPriceEndStepAndPricePlace(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v2/mix/market/contracts" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"code":"00000",
			"msg":"success",
			"data":[{
				"symbol":"TESTUSDT",
				"volumePlace":"3",
				"pricePlace":"2",
				"priceEndStep":"25",
				"minTradeNum":"0.001",
				"minTradeUSDT":"5",
				"baseCoin":"TEST",
				"quoteCoin":"USDT",
				"supportMarginCoins":["USDT"]
			}]
		}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret", "test-passphrase")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &BitgetAdapter{client: client, symbol: "TESTUSDT"}

	if err := adapter.fetchContractInfo(context.Background()); err != nil {
		t.Fatalf("fetchContractInfo() error = %v", err)
	}
	if got := adapter.GetPriceTickSize(); math.Abs(got-0.25) > 1e-12 {
		t.Fatalf("GetPriceTickSize() = %.12f, want 0.25", got)
	}
	if got := adapter.GetPriceDecimals(); got != 2 {
		t.Fatalf("GetPriceDecimals() = %d, want 2", got)
	}
}

func TestPriceTickSizeDoesNotGuessFromPricePlace(t *testing.T) {
	adapter := &BitgetAdapter{pricePlace: 2}
	if got := adapter.GetPriceTickSize(); got != 0 {
		t.Fatalf("GetPriceTickSize() = %v, want 0 without validated priceEndStep", got)
	}
}
