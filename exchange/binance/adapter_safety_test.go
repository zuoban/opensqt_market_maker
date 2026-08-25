package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/exchange/exchangeerr"

	"github.com/adshao/go-binance/v2/futures"
)

func testAdapter(t *testing.T, serverURL, marginAsset string) *BinanceAdapter {
	t.Helper()
	symbol := validContractSymbol()
	symbol.MarginAsset = marginAsset
	spec, err := contractSpecFromSymbol(symbol)
	if err != nil {
		t.Fatal(err)
	}
	client := futures.NewClient("key", "secret")
	client.BaseURL = serverURL
	return &BinanceAdapter{
		client:                   client,
		symbol:                   spec.Symbol,
		contractSpec:             spec,
		cancelAllConfirmAttempts: 3,
		cancelAllConfirmDelay:    time.Millisecond,
		orderConfirmationTimeout: 100 * time.Millisecond,
		unknownPlacementAttempts: 2,
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("Encode() error = %v", err)
	}
}

func orderFixture(orderID int64, clientOrderID string) map[string]any {
	return map[string]any{
		"symbol":        "TESTUSDT",
		"orderId":       orderID,
		"clientOrderId": clientOrderID,
		"price":         "100.1",
		"origQty":       "0.050",
		"executedQty":   "0",
		"avgPrice":      "0",
		"side":          "BUY",
		"type":          "LIMIT",
		"status":        "NEW",
		"time":          int64(1234),
		"updateTime":    int64(1235),
	}
}

func validPositionRisk(amount string) *futures.PositionRisk {
	return &futures.PositionRisk{
		Symbol:           "TESTUSDT",
		PositionAmt:      amount,
		EntryPrice:       "100",
		UnRealizedProfit: "1.25",
		MarkPrice:        "101",
		IsolatedMargin:   "0",
		Leverage:         "5",
		MarginType:       "cross",
	}
}

func TestNewBinanceAdapterRunsBoundedStartupValidation(t *testing.T) {
	var timeCalls, exchangeInfoCalls, accountCalls, modeCalls, positionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/time":
			timeCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"serverTime": time.Now().UnixMilli()})
		case "/fapi/v1/exchangeInfo":
			exchangeInfoCalls.Add(1)
			writeJSON(t, w, http.StatusOK, &futures.ExchangeInfo{Symbols: []futures.Symbol{validContractSymbol()}})
		case "/fapi/v2/account":
			accountCalls.Add(1)
			writeJSON(t, w, http.StatusOK, &futures.Account{CanTrade: true})
		case "/fapi/v1/positionSide/dual":
			modeCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{"dualSidePosition": false})
		case "/fapi/v2/positionRisk":
			positionCalls.Add(1)
			writeJSON(t, w, http.StatusOK, []*futures.PositionRisk{validPositionRisk("0")})
		default:
			t.Errorf("unexpected startup request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	oldBaseURL := futures.BaseApiMainUrl
	futures.BaseApiMainUrl = server.URL
	defer func() { futures.BaseApiMainUrl = oldBaseURL }()

	adapter, err := NewBinanceAdapter(map[string]string{"api_key": "key", "secret_key": "secret"}, "TESTUSDT")
	if err != nil {
		t.Fatalf("NewBinanceAdapter() error = %v", err)
	}
	if adapter.contractSpec == nil || adapter.contractSpec.Symbol != "TESTUSDT" {
		t.Fatalf("adapter contract spec = %+v", adapter.contractSpec)
	}
	if _, ok := adapter.client.HTTPClient.Transport.(*statusAwareTransport); !ok {
		t.Fatalf("adapter REST transport = %T, want *statusAwareTransport", adapter.client.HTTPClient.Transport)
	}
	if adapter.wsManager == nil || adapter.wsManager.client == nil {
		t.Fatal("order stream REST client was not initialized")
	}
	if _, ok := adapter.wsManager.client.HTTPClient.Transport.(*statusAwareTransport); !ok {
		t.Fatalf("order stream REST transport = %T, want *statusAwareTransport", adapter.wsManager.client.HTTPClient.Transport)
	}
	if timeCalls.Load() != 1 || exchangeInfoCalls.Load() != 1 || accountCalls.Load() != 1 || modeCalls.Load() != 1 || positionCalls.Load() != 1 {
		t.Fatalf("startup calls = time:%d info:%d account:%d mode:%d position:%d",
			timeCalls.Load(), exchangeInfoCalls.Load(), accountCalls.Load(), modeCalls.Load(), positionCalls.Load())
	}
}

func TestNewBinanceAdapterFailsWhenServerTimeSyncFails(t *testing.T) {
	var otherCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/time" {
			otherCalls.Add(1)
		}
		writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": -1000, "msg": "time unavailable"})
	}))
	defer server.Close()

	oldBaseURL := futures.BaseApiMainUrl
	futures.BaseApiMainUrl = server.URL
	defer func() { futures.BaseApiMainUrl = oldBaseURL }()

	_, err := NewBinanceAdapter(map[string]string{"api_key": "key", "secret_key": "secret"}, "TESTUSDT")
	if err == nil || !strings.Contains(err.Error(), "同步 Binance 服务器时间失败") {
		t.Fatalf("NewBinanceAdapter() error = %v", err)
	}
	if otherCalls.Load() != 0 {
		t.Fatalf("startup continued after time sync failure: %d calls", otherCalls.Load())
	}
}

func TestCancelAllOrdersUsesNativeEndpointAndConfirmsEmpty(t *testing.T) {
	var cancelCalls, queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == "/fapi/v1/allOpenOrders":
			cancelCalls.Add(1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("ReadAll() error = %v", err)
			}
			form, err := url.ParseQuery(string(body))
			if err != nil {
				t.Errorf("ParseQuery() error = %v", err)
			}
			if got := form.Get("symbol"); got != "TESTUSDT" {
				t.Errorf("cancel symbol = %q", got)
			}
			writeJSON(t, w, http.StatusOK, map[string]any{"code": 200, "msg": "done"})
		case r.Method == http.MethodGet && r.URL.Path == "/fapi/v1/openOrders":
			call := queryCalls.Add(1)
			if got := r.URL.Query().Get("symbol"); got != "TESTUSDT" {
				t.Errorf("query symbol = %q", got)
			}
			if call == 1 {
				writeJSON(t, w, http.StatusOK, []any{orderFixture(11, "pending")})
				return
			}
			writeJSON(t, w, http.StatusOK, []any{})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	if err := adapter.CancelAllOrders(context.Background(), "TESTUSDT"); err != nil {
		t.Fatalf("CancelAllOrders() error = %v", err)
	}
	if cancelCalls.Load() != 1 || queryCalls.Load() != 2 {
		t.Fatalf("calls = cancel %d query %d", cancelCalls.Load(), queryCalls.Load())
	}
}

func TestCancelAllOrdersReportsResidualOrders(t *testing.T) {
	var queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/allOpenOrders":
			writeJSON(t, w, http.StatusOK, map[string]any{"code": 200})
		case "/fapi/v1/openOrders":
			queryCalls.Add(1)
			writeJSON(t, w, http.StatusOK, []any{orderFixture(21, "still-open")})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	adapter.cancelAllConfirmAttempts = 2
	err := adapter.CancelAllOrders(context.Background(), "TESTUSDT")
	if err == nil || !strings.Contains(err.Error(), "仍残留 1 个") || !strings.Contains(err.Error(), "21") {
		t.Fatalf("CancelAllOrders() error = %v", err)
	}
	if queryCalls.Load() != 2 {
		t.Fatalf("query calls = %d, want 2", queryCalls.Load())
	}
}

func TestGetOpenOrdersRejectsNullEntriesWithoutPanicking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, []any{nil})
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	_, err := adapter.GetOpenOrders(context.Background(), "TESTUSDT")
	if err == nil || !strings.Contains(err.Error(), "第 1 条") || !strings.Contains(err.Error(), "订单为空") {
		t.Fatalf("GetOpenOrders() error = %v", err)
	}
}

func TestOrderFromBinanceFailsClosedOnInvalidIdentityAndAmounts(t *testing.T) {
	valid := func() *futures.Order {
		return &futures.Order{
			OrderID:          1,
			ClientOrderID:    "client-1",
			Symbol:           "TESTUSDT",
			Side:             futures.SideTypeBuy,
			Type:             futures.OrderTypeLimit,
			Status:           futures.OrderStatusTypeNew,
			Price:            "100",
			OrigQuantity:     "1",
			ExecutedQuantity: "0",
			AvgPrice:         "0",
		}
	}
	tests := []struct {
		name   string
		mutate func(*futures.Order)
		want   string
	}{
		{name: "zero order id", mutate: func(order *futures.Order) { order.OrderID = 0 }, want: "orderId=0"},
		{name: "missing symbol", mutate: func(order *futures.Order) { order.Symbol = "" }, want: "缺少 symbol"},
		{name: "missing client id", mutate: func(order *futures.Order) { order.ClientOrderID = "" }, want: "缺少 clientOrderId"},
		{name: "missing status", mutate: func(order *futures.Order) { order.Status = "" }, want: "缺少 status"},
		{name: "invalid side", mutate: func(order *futures.Order) { order.Side = "HOLD" }, want: "side="},
		{name: "missing type", mutate: func(order *futures.Order) { order.Type = "" }, want: "缺少 type"},
		{name: "negative price", mutate: func(order *futures.Order) { order.Price = "-1" }, want: "不能为负"},
		{name: "over executed", mutate: func(order *futures.Order) { order.ExecutedQuantity = "2" }, want: "大于 origQty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order := valid()
			tt.mutate(order)
			_, err := orderFromBinance(order)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("orderFromBinance() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCancelAllOrdersPropagatesCancelAndQueryErrors(t *testing.T) {
	tests := []struct {
		name       string
		cancelFail bool
		want       string
	}{
		{name: "cancel error", cancelFail: true, want: "一键全撤"},
		{name: "query error", want: "确认失败"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/fapi/v1/allOpenOrders" {
					if tt.cancelFail {
						writeJSON(t, w, http.StatusBadRequest, map[string]any{"code": -1100, "msg": "cancel failed"})
						return
					}
					writeJSON(t, w, http.StatusOK, map[string]any{"code": 200})
					return
				}
				writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": -1000, "msg": "query failed"})
			}))
			defer server.Close()

			adapter := testAdapter(t, server.URL, "USDT")
			err := adapter.CancelAllOrders(context.Background(), "TESTUSDT")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CancelAllOrders() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBatchCancelOrdersReturnsBatchAndFallbackErrors(t *testing.T) {
	var fallbackCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v1/batchOrders":
			writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": -1000, "msg": "batch failed"})
		case "/fapi/v1/order":
			fallbackCalls.Add(1)
			writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": -1000, "msg": "single failed"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	err := adapter.BatchCancelOrders(context.Background(), "TESTUSDT", []int64{1, 2})
	if err == nil || !strings.Contains(err.Error(), "批量撤单失败") || !strings.Contains(err.Error(), "订单 1") || !strings.Contains(err.Error(), "订单 2") {
		t.Fatalf("BatchCancelOrders() error = %v", err)
	}
	if fallbackCalls.Load() != 2 {
		t.Fatalf("fallback calls = %d, want 2", fallbackCalls.Load())
	}
}

func TestBatchCancelOrdersRetryDelayRespectsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var fallbackCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fapi/v1/batchOrders" {
			writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": -1000, "msg": "batch failed"})
			return
		}
		if fallbackCalls.Add(1) == 1 {
			cancel()
		}
		writeJSON(t, w, http.StatusOK, orderFixture(1, "cancelled"))
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	started := time.Now()
	err := adapter.BatchCancelOrders(ctx, "TESTUSDT", []int64{1, 2})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("BatchCancelOrders() error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("BatchCancelOrders() did not stop promptly after cancellation")
	}
	if fallbackCalls.Load() != 1 {
		t.Fatalf("fallback calls = %d, want 1", fallbackCalls.Load())
	}
}

func TestValidateAccountRejectsUnsafeAccountStates(t *testing.T) {
	tests := []struct {
		name         string
		canTrade     bool
		dualSide     bool
		positionFail bool
		positionAmt  string
		want         string
	}{
		{name: "trading disabled", canTrade: false, want: "canTrade=false"},
		{name: "hedge mode", canTrade: true, dualSide: true, want: "双向持仓模式"},
		{name: "position query failure", canTrade: true, positionFail: true, want: "查询 Binance TESTUSDT 持仓失败"},
		{name: "negative position", canTrade: true, positionAmt: "-0.25", want: "存在空头持仓"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/fapi/v2/account":
					writeJSON(t, w, http.StatusOK, &futures.Account{CanTrade: tt.canTrade})
				case "/fapi/v1/positionSide/dual":
					writeJSON(t, w, http.StatusOK, map[string]any{"dualSidePosition": tt.dualSide})
				case "/fapi/v2/positionRisk":
					if tt.positionFail {
						writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": -1000, "msg": "position unavailable"})
						return
					}
					writeJSON(t, w, http.StatusOK, []*futures.PositionRisk{validPositionRisk(tt.positionAmt)})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			adapter := testAdapter(t, server.URL, "USDT")
			err := adapter.ValidateAccount(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ValidateAccount() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateAccountFailsClosedOnMissingPositionRecord(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v2/account":
			writeJSON(t, w, http.StatusOK, &futures.Account{CanTrade: true})
		case "/fapi/v1/positionSide/dual":
			writeJSON(t, w, http.StatusOK, map[string]any{"dualSidePosition": false})
		case "/fapi/v2/positionRisk":
			writeJSON(t, w, http.StatusOK, []*futures.PositionRisk{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	err := adapter.ValidateAccount(context.Background())
	if err == nil || !strings.Contains(err.Error(), "未返回配置交易对") {
		t.Fatalf("ValidateAccount() error = %v", err)
	}
}

func TestValidateAccountAcceptsOneWayNonNegativePosition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fapi/v2/account":
			writeJSON(t, w, http.StatusOK, &futures.Account{CanTrade: true})
		case "/fapi/v1/positionSide/dual":
			writeJSON(t, w, http.StatusOK, map[string]any{"dualSidePosition": false})
		case "/fapi/v2/positionRisk":
			writeJSON(t, w, http.StatusOK, []*futures.PositionRisk{validPositionRisk("0")})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	if err := adapter.ValidateAccount(context.Background()); err != nil {
		t.Fatalf("ValidateAccount() error = %v", err)
	}
}

func TestValidateAccountRejectsMissingOrInvalidPositionModeField(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "missing", body: `{}`},
		{name: "null", body: `{"dualSidePosition":null}`},
		{name: "wrong type", body: `{"dualSidePosition":"false"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var positionCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/fapi/v2/account":
					writeJSON(t, w, http.StatusOK, &futures.Account{CanTrade: true})
				case "/fapi/v1/positionSide/dual":
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tt.body)
				case "/fapi/v2/positionRisk":
					positionCalls.Add(1)
					writeJSON(t, w, http.StatusOK, []*futures.PositionRisk{validPositionRisk("0")})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			adapter := testAdapter(t, server.URL, "USDT")
			err := adapter.ValidateAccount(context.Background())
			if err == nil || !strings.Contains(err.Error(), "查询 Binance 持仓模式失败") {
				t.Fatalf("ValidateAccount() error = %v", err)
			}
			if positionCalls.Load() != 0 {
				t.Fatalf("position query continued after invalid mode: %d", positionCalls.Load())
			}
		})
	}
}

func TestGetPositionsRejectsMalformedNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		position := validPositionRisk("not-a-number")
		writeJSON(t, w, http.StatusOK, []*futures.PositionRisk{position})
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	_, err := adapter.GetPositions(context.Background(), "TESTUSDT")
	if err == nil || !strings.Contains(err.Error(), "positionAmt") {
		t.Fatalf("GetPositions() error = %v", err)
	}
}

func accountBalanceFixture() *futures.Account {
	return &futures.Account{
		CanTrade: true,
		Assets: []*futures.AccountAsset{
			{Asset: "USDT", WalletBalance: "100", MarginBalance: "110", AvailableBalance: "80"},
			{Asset: "USDC", WalletBalance: "7", MarginBalance: "8", AvailableBalance: "6"},
			{Asset: "BUSD", WalletBalance: "1000", MarginBalance: "1000", AvailableBalance: "1000"},
		},
	}
}

func TestGetAccountUsesOnlyContractMarginAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v2/account" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, http.StatusOK, accountBalanceFixture())
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDC")
	account, err := adapter.GetAccount(context.Background())
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if account.TotalWalletBalance != 7 || account.TotalMarginBalance != 8 || account.AvailableBalance != 6 {
		t.Fatalf("GetAccount() = %+v", account)
	}
}

func TestGetAccountFailsWhenMarginAssetMissingOrMalformed(t *testing.T) {
	tests := []struct {
		name    string
		assets  []*futures.AccountAsset
		wantErr string
	}{
		{
			name:    "missing",
			assets:  []*futures.AccountAsset{{Asset: "USDT", WalletBalance: "1", MarginBalance: "1", AvailableBalance: "1"}},
			wantErr: "缺少资产 USDC",
		},
		{
			name: "malformed",
			assets: []*futures.AccountAsset{
				{Asset: "USDC", WalletBalance: "7", MarginBalance: "bad", AvailableBalance: "6"},
			},
			wantErr: "marginBalance",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(t, w, http.StatusOK, &futures.Account{Assets: tt.assets})
			}))
			defer server.Close()
			adapter := testAdapter(t, server.URL, "USDC")

			_, err := adapter.GetAccount(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("GetAccount() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestGetBalanceHonorsRequestedAssetAndFailsClosed(t *testing.T) {
	fixture := accountBalanceFixture()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, http.StatusOK, fixture)
	}))
	defer server.Close()
	adapter := testAdapter(t, server.URL, "USDC")

	balance, err := adapter.GetBalance(context.Background(), "USDT")
	if err != nil || balance != 80 {
		t.Fatalf("GetBalance(USDT) = %g, %v", balance, err)
	}
	if _, err := adapter.GetBalance(context.Background(), "BTC"); err == nil || !strings.Contains(err.Error(), "缺少资产 BTC") {
		t.Fatalf("GetBalance(BTC) error = %v", err)
	}

	fixture.Assets[0].AvailableBalance = "broken"
	if _, err := adapter.GetBalance(context.Background(), "USDT"); err == nil || !strings.Contains(err.Error(), "availableBalance") {
		t.Fatalf("GetBalance(malformed) error = %v", err)
	}
}

func TestGetHistoricalKlinesMarksOnlyExpiredCandlesClosed(t *testing.T) {
	now := time.Now().UnixMilli()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/klines" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, http.StatusOK, [][]any{
			{now - 60_000, "100", "102", "99", "101", "10", now - 1, "1000", 5, "4", "400"},
			{now, "101", "103", "100", "102", "11", now + 60_000, "1100", 6, "5", "500"},
		})
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	candles, err := adapter.GetHistoricalKlines(context.Background(), "TESTUSDT", "1m", 2)
	if err != nil {
		t.Fatalf("GetHistoricalKlines() error = %v", err)
	}
	if len(candles) != 2 || !candles[0].IsClosed || candles[1].IsClosed {
		t.Fatalf("candles = %+v", candles)
	}
}

func placementRequest() *OrderRequest {
	return &OrderRequest{
		Symbol:        "TESTUSDT",
		Side:          SideBuy,
		Type:          OrderTypeLimit,
		Price:         100.19,
		Quantity:      0.0509,
		PostOnly:      true,
		ClientOrderID: "grid-order-1",
	}
}

func TestPlaceOrder503ConfirmsByExactClientOrderID(t *testing.T) {
	const brokerID = "x-zdfVM8vYgrid-order-1"
	var postCalls, queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fapi/v1/order" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			postCalls.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm() error = %v", err)
			}
			if got := r.FormValue("newClientOrderId"); got != brokerID {
				t.Errorf("newClientOrderId = %q", got)
			}
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "Service Unavailable"})
			return
		}
		queryCalls.Add(1)
		if got := r.URL.Query().Get("origClientOrderId"); got != brokerID {
			t.Errorf("origClientOrderId = %q", got)
		}
		writeJSON(t, w, http.StatusOK, orderFixture(42, brokerID))
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	order, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err != nil {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if order.OrderID != 42 || order.ClientOrderID != brokerID || postCalls.Load() != 1 || queryCalls.Load() != 1 {
		t.Fatalf("order=%+v calls=post:%d query:%d", order, postCalls.Load(), queryCalls.Load())
	}
}

func TestPlaceOrderUnknownConfirmationPreservesAuthoritativeState(t *testing.T) {
	const brokerID = "x-zdfVM8vYgrid-order-1"
	tests := []struct {
		status      string
		executedQty string
		avgPrice    string
	}{
		{status: "PARTIALLY_FILLED", executedQty: "0.020", avgPrice: "100.15"},
		{status: "FILLED", executedQty: "0.050", avgPrice: "100.20"},
		{status: "CANCELED", executedQty: "0.030", avgPrice: "100.18"},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writeJSON(t, w, http.StatusServiceUnavailable,
						map[string]any{"code": -1000, "msg": "Service Unavailable"})
					return
				}
				fixture := orderFixture(142, brokerID)
				fixture["status"] = tt.status
				fixture["executedQty"] = tt.executedQty
				fixture["avgPrice"] = tt.avgPrice
				writeJSON(t, w, http.StatusOK, fixture)
			}))
			defer server.Close()

			adapter := testAdapter(t, server.URL, "USDT")
			got, err := adapter.PlaceOrder(context.Background(), placementRequest())
			if err != nil {
				t.Fatalf("PlaceOrder() error = %v", err)
			}
			wantExecuted, _ := strconv.ParseFloat(tt.executedQty, 64)
			wantAvgPrice, _ := strconv.ParseFloat(tt.avgPrice, 64)
			if got.Status != OrderStatus(tt.status) || got.ExecutedQty != wantExecuted ||
				got.AvgPrice != wantAvgPrice || got.Type != OrderTypeLimit ||
				got.UpdateTime != 1235 || got.CreatedAt.UnixMilli() != 1234 {
				t.Fatalf("confirmed order lost authoritative fields: %+v", got)
			}
		})
	}
}

func TestPlaceOrderInvalid2xxResponseConfirmsByClientOrderID(t *testing.T) {
	const brokerID = "x-zdfVM8vYgrid-order-1"
	var postCalls, queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{})
			return
		}
		queryCalls.Add(1)
		writeJSON(t, w, http.StatusOK, orderFixture(46, brokerID))
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	order, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err != nil || order.OrderID != 46 {
		t.Fatalf("PlaceOrder() = %+v, %v", order, err)
	}
	if postCalls.Load() != 1 || queryCalls.Load() != 1 {
		t.Fatalf("calls=post:%d query:%d", postCalls.Load(), queryCalls.Load())
	}
}

func TestPlaceOrderMalformed2xxResponseConfirmsByClientOrderID(t *testing.T) {
	const brokerID = "x-zdfVM8vYgrid-order-1"
	var queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"incomplete"`)
			return
		}
		queryCalls.Add(1)
		writeJSON(t, w, http.StatusOK, orderFixture(47, brokerID))
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	order, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err != nil || order.OrderID != 47 {
		t.Fatalf("PlaceOrder() = %+v, %v", order, err)
	}
	if queryCalls.Load() != 1 {
		t.Fatalf("query calls = %d, want 1", queryCalls.Load())
	}
}

func TestPlaceOrderInvalid2xxAndNotFoundNeverRetries(t *testing.T) {
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalls.Add(1)
			writeJSON(t, w, http.StatusOK, map[string]any{})
			return
		}
		writeJSON(t, w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	_, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err == nil || !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if postCalls.Load() != 1 {
		t.Fatalf("post calls = %d, want 1", postCalls.Load())
	}
}

func TestPlaceOrderInvalid2xxWithoutClientIDIsUnknown(t *testing.T) {
	var queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			queryCalls.Add(1)
		}
		_, _ = io.WriteString(w, `null`)
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	req := placementRequest()
	req.ClientOrderID = ""
	_, err := adapter.PlaceOrder(context.Background(), req)
	if err == nil || !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if queryCalls.Load() != 0 {
		t.Fatalf("query calls = %d, want 0 without client ID", queryCalls.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type placementTimeoutError struct{}

func (placementTimeoutError) Error() string   { return "simulated create timeout" }
func (placementTimeoutError) Timeout() bool   { return true }
func (placementTimeoutError) Temporary() bool { return true }

func TestPlaceOrderTransportTimeoutConfirmsExistingOrder(t *testing.T) {
	const brokerID = "x-zdfVM8vYgrid-order-1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, orderFixture(43, brokerID))
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	base := http.DefaultTransport
	adapter.client.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			return nil, placementTimeoutError{}
		}
		return base.RoundTrip(r)
	})}

	order, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err != nil || order.OrderID != 43 {
		t.Fatalf("PlaceOrder() = %+v, %v", order, err)
	}
}

func TestUnknownPlacementClassifiesClosedConnections(t *testing.T) {
	for _, err := range []error{net.ErrClosed, io.ErrClosedPipe, fmt.Errorf("wrapped: %w", net.ErrClosed)} {
		if !isUnknownPlacementResult(err) {
			t.Errorf("isUnknownPlacementResult(%v) = false", err)
		}
	}
}

func TestPlaceOrderRetriesOnlyAfterExplicitNotFound(t *testing.T) {
	const brokerID = "x-zdfVM8vYgrid-order-1"
	var postCalls, queryCalls atomic.Int32
	var postedIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			queryCalls.Add(1)
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm() error = %v", err)
		}
		postedIDs = append(postedIDs, r.FormValue("newClientOrderId"))
		if postCalls.Add(1) == 1 {
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "Service Unavailable"})
			return
		}
		writeJSON(t, w, http.StatusOK, map[string]any{
			"symbol": "TESTUSDT", "orderId": 44, "clientOrderId": brokerID, "status": "NEW", "updateTime": 12,
		})
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	order, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err != nil || order.OrderID != 44 {
		t.Fatalf("PlaceOrder() = %+v, %v", order, err)
	}
	if postCalls.Load() != 2 || queryCalls.Load() != 1 || len(postedIDs) != 2 || postedIDs[0] != brokerID || postedIDs[1] != brokerID {
		t.Fatalf("calls=post:%d query:%d ids=%v", postCalls.Load(), queryCalls.Load(), postedIDs)
	}
}

func TestPlaceOrderDuplicateOnSameIDRetryQueriesExistingOrder(t *testing.T) {
	const brokerID = "x-zdfVM8vYgrid-order-1"
	var postCalls, queryCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if postCalls.Add(1) == 1 {
				writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "Service Unavailable"})
				return
			}
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"code": -4111, "msg": "Duplicate client order id"})
			return
		}
		if queryCalls.Add(1) == 1 {
			writeJSON(t, w, http.StatusBadRequest, map[string]any{"code": -2013, "msg": "Order does not exist."})
			return
		}
		if got := r.URL.Query().Get("origClientOrderId"); got != brokerID {
			t.Errorf("origClientOrderId = %q", got)
		}
		writeJSON(t, w, http.StatusOK, orderFixture(45, brokerID))
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	order, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err != nil || order.OrderID != 45 {
		t.Fatalf("PlaceOrder() = %+v, %v", order, err)
	}
	if postCalls.Load() != 2 || queryCalls.Load() != 2 {
		t.Fatalf("calls=post:%d query:%d", postCalls.Load(), queryCalls.Load())
	}
}

func TestPlaceOrderInconclusiveConfirmationNeverDuplicates(t *testing.T) {
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalls.Add(1)
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "Service Unavailable"})
			return
		}
		writeJSON(t, w, http.StatusInternalServerError, map[string]any{"code": -1000, "msg": "confirmation unavailable"})
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	_, err := adapter.PlaceOrder(context.Background(), placementRequest())
	if err == nil || !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v, want ErrOrderPlacementUnknown", err)
	}
	if postCalls.Load() != 1 {
		t.Fatalf("post calls = %d, want 1", postCalls.Load())
	}
}

func TestPlaceOrderConfirmationTimeoutIsBounded(t *testing.T) {
	var postCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postCalls.Add(1)
			writeJSON(t, w, http.StatusServiceUnavailable, map[string]any{"code": -1000, "msg": "Service Unavailable"})
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	adapter := testAdapter(t, server.URL, "USDT")
	adapter.orderConfirmationTimeout = 30 * time.Millisecond
	started := time.Now()
	_, err := adapter.PlaceOrder(context.Background(), placementRequest())
	elapsed := time.Since(started)
	if err == nil || !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatalf("PlaceOrder() error = %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("confirmation elapsed = %s, want bounded", elapsed)
	}
	if postCalls.Load() != 1 {
		t.Fatalf("post calls = %d, want 1", postCalls.Load())
	}
}

func TestUnknownPlacementErrorIncludesUsefulContext(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", exchangeerr.ErrOrderPlacementUnknown)
	if !errors.Is(err, exchangeerr.ErrOrderPlacementUnknown) {
		t.Fatal("sentinel must survive wrapping")
	}
}
