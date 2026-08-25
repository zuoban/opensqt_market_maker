package binance

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adshao/go-binance/v2/futures"
)

func validContractSymbol() futures.Symbol {
	return futures.Symbol{
		Symbol:            "TESTUSDT",
		Status:            string(futures.SymbolStatusTypeTrading),
		ContractType:      futures.ContractTypePerpetual,
		BaseAsset:         "TEST",
		QuoteAsset:        "USDT",
		MarginAsset:       "USDT",
		PricePrecision:    4,
		QuantityPrecision: 3,
		Filters: []map[string]interface{}{
			{
				"filterType": "PRICE_FILTER",
				"minPrice":   "0.10",
				"maxPrice":   "1000000",
				"tickSize":   "0.10",
			},
			{
				"filterType": "LOT_SIZE",
				"minQty":     "0.001",
				"maxQty":     "1000",
				"stepSize":   "0.001",
			},
			{
				"filterType": "MIN_NOTIONAL",
				"notional":   "5",
			},
		},
	}
}

func TestContractSpecFromSymbol(t *testing.T) {
	spec, err := contractSpecFromSymbol(validContractSymbol())
	if err != nil {
		t.Fatalf("contractSpecFromSymbol() error = %v", err)
	}
	if spec.Symbol != "TESTUSDT" || spec.MarginAsset != "USDT" {
		t.Fatalf("unexpected identity: %+v", spec)
	}
	if spec.TickSize.String() != "0.1" || spec.StepSize.String() != "0.001" {
		t.Fatalf("unexpected filters: tick=%s step=%s", spec.TickSize, spec.StepSize)
	}
	if spec.MinQuantity.String() != "0.001" || spec.MaxQuantity.String() != "1000" || spec.MinNotional.String() != "5" {
		t.Fatalf("unexpected limits: minQty=%s maxQty=%s minNotional=%s",
			spec.MinQuantity, spec.MaxQuantity, spec.MinNotional)
	}
}

func TestContractSpecRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*futures.Symbol)
		wantErr string
	}{
		{
			name: "not trading",
			mutate: func(symbol *futures.Symbol) {
				symbol.Status = "SETTLING"
			},
			wantErr: "状态不是 TRADING",
		},
		{
			name: "not perpetual",
			mutate: func(symbol *futures.Symbol) {
				symbol.ContractType = futures.ContractTypeCurrentQuarter
			},
			wantErr: "不是 PERPETUAL",
		},
		{
			name: "missing margin asset",
			mutate: func(symbol *futures.Symbol) {
				symbol.MarginAsset = ""
			},
			wantErr: "资产信息不完整",
		},
		{
			name: "missing price filter",
			mutate: func(symbol *futures.Symbol) {
				symbol.Filters = symbol.Filters[1:]
			},
			wantErr: "缺少 PRICE_FILTER",
		},
		{
			name: "zero tick size",
			mutate: func(symbol *futures.Symbol) {
				symbol.Filters[0]["tickSize"] = "0"
			},
			wantErr: "tickSize 必须>0",
		},
		{
			name: "missing lot size",
			mutate: func(symbol *futures.Symbol) {
				symbol.Filters = append(symbol.Filters[:1], symbol.Filters[2:]...)
			},
			wantErr: "缺少 LOT_SIZE",
		},
		{
			name: "invalid quantity range",
			mutate: func(symbol *futures.Symbol) {
				symbol.Filters[1]["minQty"] = "1001"
			},
			wantErr: "minQty 大于 maxQty",
		},
		{
			name: "missing min notional",
			mutate: func(symbol *futures.Symbol) {
				symbol.Filters = symbol.Filters[:2]
			},
			wantErr: "缺少 MIN_NOTIONAL",
		},
		{
			name: "zero min notional",
			mutate: func(symbol *futures.Symbol) {
				symbol.Filters[2]["notional"] = "0"
			},
			wantErr: "notional 必须>0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symbol := validContractSymbol()
			tt.mutate(&symbol)
			_, err := contractSpecFromSymbol(symbol)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestFetchExchangeInfoInstallsValidatedSpec(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/exchangeInfo" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(futures.ExchangeInfo{Symbols: []futures.Symbol{validContractSymbol()}}); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	}))
	defer server.Close()

	client := futures.NewClient("key", "secret")
	client.BaseURL = server.URL
	adapter := &BinanceAdapter{client: client, symbol: "TESTUSDT"}
	if err := adapter.fetchExchangeInfo(context.Background()); err != nil {
		t.Fatalf("fetchExchangeInfo() error = %v", err)
	}
	if adapter.contractSpec == nil || adapter.contractSpec.MarginAsset != "USDT" {
		t.Fatalf("contract spec not installed: %+v", adapter.contractSpec)
	}
	if adapter.priceDecimals != 4 || adapter.quantityDecimals != 3 || adapter.baseAsset != "TEST" || adapter.quoteAsset != "USDT" {
		t.Fatalf("adapter metadata = price %d quantity %d base %s quote %s",
			adapter.priceDecimals, adapter.quantityDecimals, adapter.baseAsset, adapter.quoteAsset)
	}
}

func TestFetchExchangeInfoKeepsAdapterUninitializedOnInvalidSpec(t *testing.T) {
	invalid := validContractSymbol()
	invalid.Status = "SETTLING"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(futures.ExchangeInfo{Symbols: []futures.Symbol{invalid}})
	}))
	defer server.Close()

	client := futures.NewClient("key", "secret")
	client.BaseURL = server.URL
	adapter := &BinanceAdapter{client: client, symbol: "TESTUSDT"}
	err := adapter.fetchExchangeInfo(context.Background())
	if err == nil || !strings.Contains(err.Error(), "状态不是 TRADING") {
		t.Fatalf("fetchExchangeInfo() error = %v", err)
	}
	if adapter.contractSpec != nil {
		t.Fatalf("invalid spec was installed: %+v", adapter.contractSpec)
	}
}

func TestNormalizeOrderUsesDirectionalTickAndStep(t *testing.T) {
	spec, err := contractSpecFromSymbol(validContractSymbol())
	if err != nil {
		t.Fatal(err)
	}
	noisyPrice := 0.1
	noisyPrice += 0.2

	tests := []struct {
		name         string
		req          OrderRequest
		wantPrice    string
		wantQuantity string
	}{
		{
			name: "buy floors price and quantity",
			req: OrderRequest{
				Symbol: "TESTUSDT", Side: SideBuy, Price: 100.19, Quantity: 0.0509,
			},
			wantPrice: "100.1", wantQuantity: "0.050",
		},
		{
			name: "sell ceils price and floors quantity",
			req: OrderRequest{
				Symbol: "TESTUSDT", Side: SideSell, Price: 100.11, Quantity: 0.0509,
			},
			wantPrice: "100.2", wantQuantity: "0.050",
		},
		{
			name: "float noise snaps to exact tick",
			req: OrderRequest{
				Symbol: "testusdt", Side: SideSell, Price: noisyPrice, Quantity: 20,
			},
			wantPrice: "0.3", wantQuantity: "20.000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := spec.normalizeOrder(&tt.req)
			if err != nil {
				t.Fatalf("normalizeOrder() error = %v", err)
			}
			if got.PriceText != tt.wantPrice || got.QuantityText != tt.wantQuantity {
				t.Fatalf("normalized = price %s quantity %s, want price %s quantity %s",
					got.PriceText, got.QuantityText, tt.wantPrice, tt.wantQuantity)
			}
		})
	}
}

func TestNormalizeOrderSupportsNonPowerOfTenTickAndIntegerStep(t *testing.T) {
	symbol := validContractSymbol()
	symbol.Filters[0]["tickSize"] = "0.25"
	symbol.Filters[1]["minQty"] = "1"
	symbol.Filters[1]["stepSize"] = "1"
	spec, err := contractSpecFromSymbol(symbol)
	if err != nil {
		t.Fatal(err)
	}

	buy, err := spec.normalizeOrder(&OrderRequest{Symbol: "TESTUSDT", Side: SideBuy, Price: 100.37, Quantity: 200.9})
	if err != nil {
		t.Fatal(err)
	}
	if buy.PriceText != "100.25" || buy.QuantityText != "200" {
		t.Fatalf("buy = price %s quantity %s", buy.PriceText, buy.QuantityText)
	}

	sell, err := spec.normalizeOrder(&OrderRequest{Symbol: "TESTUSDT", Side: SideSell, Price: 100.26, Quantity: 200.9})
	if err != nil {
		t.Fatal(err)
	}
	if sell.PriceText != "100.50" || sell.QuantityText != "200" {
		t.Fatalf("sell = price %s quantity %s", sell.PriceText, sell.QuantityText)
	}
}

func TestNormalizeOrderRejectsFilterViolations(t *testing.T) {
	spec, err := contractSpecFromSymbol(validContractSymbol())
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		req     OrderRequest
		wantErr string
	}{
		{
			name:    "quantity becomes zero",
			req:     OrderRequest{Symbol: "TESTUSDT", Side: SideBuy, Price: 100, Quantity: 0.0009},
			wantErr: "量化后数值为 0",
		},
		{
			name:    "quantity exceeds max",
			req:     OrderRequest{Symbol: "TESTUSDT", Side: SideBuy, Price: 100, Quantity: 1000.1},
			wantErr: "大于 maxQty",
		},
		{
			name:    "notional below minimum",
			req:     OrderRequest{Symbol: "TESTUSDT", Side: SideBuy, Price: 100, Quantity: 0.049},
			wantErr: "小于 minNotional",
		},
		{
			name:    "symbol mismatch",
			req:     OrderRequest{Symbol: "OTHERUSDT", Side: SideBuy, Price: 100, Quantity: 1},
			wantErr: "不匹配",
		},
		{
			name:    "invalid side",
			req:     OrderRequest{Symbol: "TESTUSDT", Side: Side("HOLD"), Price: 100, Quantity: 1},
			wantErr: "不支持的订单方向",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := spec.normalizeOrder(&tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeOrderRejectsPriceBounds(t *testing.T) {
	symbol := validContractSymbol()
	symbol.Filters[0]["minPrice"] = "1"
	symbol.Filters[0]["maxPrice"] = "10"
	symbol.Filters[2]["notional"] = "0.001"
	spec, err := contractSpecFromSymbol(symbol)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		req     OrderRequest
		wantErr string
	}{
		{
			name:    "below min price",
			req:     OrderRequest{Symbol: "TESTUSDT", Side: SideBuy, Price: 0.99, Quantity: 1},
			wantErr: "小于 minPrice",
		},
		{
			name:    "above max price",
			req:     OrderRequest{Symbol: "TESTUSDT", Side: SideSell, Price: 10.01, Quantity: 1},
			wantErr: "大于 maxPrice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := spec.normalizeOrder(&tt.req)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestPlaceOrderSendsAndReturnsNormalizedValues(t *testing.T) {
	var receivedPrice, receivedQuantity, receivedSymbol, receivedTIF, receivedReduceOnly string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/order" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		receivedPrice = r.Form.Get("price")
		receivedQuantity = r.Form.Get("quantity")
		receivedSymbol = r.Form.Get("symbol")
		receivedTIF = r.Form.Get("timeInForce")
		receivedReduceOnly = r.Form.Get("reduceOnly")

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"symbol":"TESTUSDT","orderId":42,"clientOrderId":"server-client","status":"NEW","updateTime":1234}`)
	}))
	defer server.Close()

	spec, err := contractSpecFromSymbol(validContractSymbol())
	if err != nil {
		t.Fatal(err)
	}
	client := futures.NewClient("key", "secret")
	client.BaseURL = server.URL
	adapter := &BinanceAdapter{client: client, symbol: spec.Symbol, contractSpec: spec}

	order, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol:     "TESTUSDT",
		Side:       SideSell,
		Type:       OrderTypeLimit,
		Price:      100.11,
		Quantity:   0.0509,
		PostOnly:   true,
		ReduceOnly: true,
	})
	if err != nil {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if receivedPrice != "100.2" || receivedQuantity != "0.050" || receivedSymbol != "TESTUSDT" {
		t.Fatalf("request = symbol %s price %s quantity %s", receivedSymbol, receivedPrice, receivedQuantity)
	}
	if receivedTIF != "GTX" || receivedReduceOnly != "true" {
		t.Fatalf("request = timeInForce %s reduceOnly %s", receivedTIF, receivedReduceOnly)
	}
	if order.Price != 100.2 || order.Quantity != 0.05 || order.Symbol != "TESTUSDT" {
		t.Fatalf("returned order = %+v", order)
	}
}
