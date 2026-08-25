package gate

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGateRESTQuantitiesUseBaseAssetUnits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/futures/usdt/orders":
			var request struct {
				Size int64 `json:"size"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode place request: %v", err)
			}
			if request.Size != 10 {
				t.Errorf("place request size = %d contracts, want 10", request.Size)
			}
			_, _ = w.Write([]byte(`{"id":101,"contract":"BTC_USDT","size":10,"fill_size":4,"price":"100.00","fill_price":"99.50","status":"open"}`))

		case r.Method == http.MethodGet && r.URL.Path == "/futures/usdt/orders/202":
			_, _ = w.Write([]byte(`{"id":202,"contract":"BTC_USDT","size":-8,"fill_size":-2,"price":"101.00","fill_price":"101.50","status":"finished"}`))

		case r.Method == http.MethodGet && r.URL.Path == "/futures/usdt/orders":
			if got := r.URL.Query().Get("contract"); got != "BTC_USDT" {
				t.Errorf("open-order contract = %q, want BTC_USDT", got)
			}
			_, _ = w.Write([]byte(`[{"id":303,"contract":"BTC_USDT","size":5,"fill_size":1,"price":"98.00","fill_price":"98.50","status":"open"}]`))

		case r.Method == http.MethodGet && r.URL.Path == "/futures/usdt/positions/BTC_USDT":
			_, _ = w.Write([]byte(`{"contract":"BTC_USDT","size":12,"leverage":"0","cross_leverage_limit":"5","entry_price":"97.00","mark_price":"99.00","unrealised_pnl":"1.25"}`))

		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient("test-key", "test-secret")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	adapter := &GateAdapter{
		client: client, symbol: "BTCUSDT", gateSymbol: "BTC_USDT", settle: "usdt",
		quantoMultiplier: 0.001, pricePlace: 2,
	}

	placed, err := adapter.PlaceOrder(context.Background(), &OrderRequest{
		Symbol: "BTCUSDT", Side: SideBuy, Type: OrderTypeLimit,
		Price: 100, Quantity: 0.01,
	})
	if err != nil {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	assertGateQuantity(t, "placed quantity", placed.Quantity, 0.01)
	assertGateQuantity(t, "placed executed quantity", placed.ExecutedQty, 0.004)

	queried, err := adapter.GetOrder(context.Background(), "BTCUSDT", 202)
	if err != nil {
		t.Fatalf("GetOrder() error = %v", err)
	}
	if queried.Side != SideSell {
		t.Fatalf("queried side = %s, want SELL", queried.Side)
	}
	assertGateQuantity(t, "queried quantity", queried.Quantity, 0.008)
	assertGateQuantity(t, "queried executed quantity", queried.ExecutedQty, 0.002)

	openOrders, err := adapter.GetOpenOrders(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetOpenOrders() error = %v", err)
	}
	if len(openOrders) != 1 {
		t.Fatalf("open order count = %d, want 1", len(openOrders))
	}
	assertGateQuantity(t, "open quantity", openOrders[0].Quantity, 0.005)
	assertGateQuantity(t, "open executed quantity", openOrders[0].ExecutedQty, 0.001)

	positions, err := adapter.GetPositions(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetPositions() error = %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("position count = %d, want 1", len(positions))
	}
	assertGateQuantity(t, "position size", positions[0].Size, 0.012)
}

func assertGateQuantity(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("%s = %.12f, want %.12f", name, got, want)
	}
}
