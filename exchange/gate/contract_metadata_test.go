package gate

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGateContractMetadataPreservesNonPowerOfTenPriceTick(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/futures/usdt/contracts/TEST_USDT" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"name":"TEST_USDT",
			"quanto_multiplier":"0.001",
			"order_price_round":"0.25",
			"order_size_min":1,
			"order_size_round":"1"
		}`))
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &GateAdapter{
		client: client, symbol: "TESTUSDT", gateSymbol: "TEST_USDT", settle: "usdt",
	}

	if err := adapter.fetchContractInfo(context.Background()); err != nil {
		t.Fatalf("fetchContractInfo() error = %v", err)
	}
	if got := adapter.GetPriceTickSize(); math.Abs(got-0.25) > 1e-12 {
		t.Fatalf("GetPriceTickSize() = %.12f, want 0.25", got)
	}
	if got := adapter.GetPriceDecimals(); got != 2 {
		t.Fatalf("GetPriceDecimals() = %d, want 2 for order_price_round=0.25", got)
	}
}

func TestCalculateDecimalPlacesUsesCompleteStepPrecision(t *testing.T) {
	tests := []struct {
		step float64
		want int
	}{
		{step: 0.25, want: 2},
		{step: 0.05, want: 2},
		{step: 0.0025, want: 4},
		{step: 0.1, want: 1},
		{step: 0.000001, want: 6},
		{step: 1, want: 0},
		{step: 2.5, want: 1},
	}

	for _, tt := range tests {
		if got := calculateDecimalPlaces(tt.step); got != tt.want {
			t.Errorf("calculateDecimalPlaces(%g) = %d, want %d", tt.step, got, tt.want)
		}
	}
}

func TestPriceTickSizeDoesNotGuessFromPriceDecimals(t *testing.T) {
	adapter := &GateAdapter{pricePlace: 2}
	if got := adapter.GetPriceTickSize(); got != 0 {
		t.Fatalf("GetPriceTickSize() = %v, want 0 without validated order_price_round", got)
	}
}
