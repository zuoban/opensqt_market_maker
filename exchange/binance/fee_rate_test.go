package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adshao/go-binance/v2/futures"
)

func TestParseMakerFeeRate(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    float64
		wantErr bool
	}{
		{name: "standard", raw: "0.0002", want: 0.0002},
		{name: "zero", raw: "0", want: 0},
		{name: "empty", raw: "", wantErr: true},
		{name: "negative", raw: "-0.0001", wantErr: true},
		{name: "too large", raw: "1", wantErr: true},
		{name: "nan", raw: "NaN", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMakerFeeRate(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseMakerFeeRate(%q) error = %v", tt.raw, err)
			}
			if err == nil && got != tt.want {
				t.Fatalf("parseMakerFeeRate(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestGetMakerFeeRate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/commissionRate" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("symbol"); got != "TESTUSDT" {
			t.Errorf("symbol = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"symbol":              "TESTUSDT",
			"makerCommissionRate": "0.00017",
			"takerCommissionRate": "0.0004",
		})
	}))
	defer server.Close()

	client := futures.NewClient("key", "secret")
	client.BaseURL = server.URL
	adapter := &BinanceAdapter{client: client, symbol: "TESTUSDT"}
	got, err := adapter.GetMakerFeeRate(context.Background(), "testusdt")
	if err != nil {
		t.Fatalf("GetMakerFeeRate() error = %v", err)
	}
	if got != 0.00017 {
		t.Fatalf("GetMakerFeeRate() = %v, want 0.00017", got)
	}
}

func TestGetMakerFeeRateRejectsMismatchedSymbol(t *testing.T) {
	adapter := &BinanceAdapter{
		client:       futures.NewClient("key", "secret"),
		contractSpec: &ContractSpec{Symbol: "TESTUSDT"},
	}
	_, err := adapter.GetMakerFeeRate(context.Background(), "OTHERUSDT")
	if err == nil || !strings.Contains(err.Error(), "不匹配") {
		t.Fatalf("GetMakerFeeRate() error = %v", err)
	}
}

func TestGetMakerFeeRateRejectsMismatchedResponseSymbol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"symbol":              "OTHERUSDT",
			"makerCommissionRate": "0.0002",
		})
	}))
	defer server.Close()

	client := futures.NewClient("key", "secret")
	client.BaseURL = server.URL
	adapter := &BinanceAdapter{client: client, symbol: "TESTUSDT"}
	_, err := adapter.GetMakerFeeRate(context.Background(), "TESTUSDT")
	if err == nil || !strings.Contains(err.Error(), "响应交易对不匹配") {
		t.Fatalf("GetMakerFeeRate() error = %v", err)
	}
}
