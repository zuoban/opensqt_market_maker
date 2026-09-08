package binance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

func TestParseCombinedMarketUpdate(t *testing.T) {
	receivedAt := time.Unix(1_700_000_100, 0)

	trade, recognized, err := parseCombinedMarketUpdate([]byte(`{
		"stream":"ethusdt@trade",
		"data":{"e":"trade","E":1700000000123,"s":"ETHUSDT","p":"2010.25"}
	}`), "ETHUSDT", 7, 3, receivedAt)
	if err != nil || !recognized {
		t.Fatalf("trade parse = recognized:%v err:%v", recognized, err)
	}
	if trade.Symbol != "ETHUSDT" || trade.LastPrice != 2010.25 || trade.StreamEpoch != 3 ||
		trade.QuoteVersion != 0 || !trade.ReceivedAt.Equal(receivedAt) ||
		!trade.EventTime.Equal(time.UnixMilli(1_700_000_000_123)) {
		t.Fatalf("trade update = %+v", trade)
	}

	book, recognized, err := parseCombinedMarketUpdate([]byte(`{
		"stream":"ethusdt@bookTicker",
		"data":{"e":"bookTicker","E":1700000000456,"s":"ETHUSDT","b":"2010.20","a":"2010.30"}
	}`), "ethusdt", 7, 3, receivedAt)
	if err != nil || !recognized {
		t.Fatalf("book parse = recognized:%v err:%v", recognized, err)
	}
	if book.BestBid != 2010.20 || book.BestAsk != 2010.30 || book.QuoteVersion != 8 ||
		book.StreamEpoch != 3 || !book.EventTime.Equal(time.UnixMilli(1_700_000_000_456)) {
		t.Fatalf("book update = %+v", book)
	}
}

func TestParseCombinedMarketUpdateRejectsInvalidBookAndSymbol(t *testing.T) {
	tests := []struct {
		name    string
		message string
		symbol  string
	}{
		{
			name: "crossed book",
			message: `{"stream":"ethusdt@bookTicker","data":` +
				`{"e":"bookTicker","s":"ETHUSDT","b":"2010.30","a":"2010.20"}}`,
			symbol: "ETHUSDT",
		},
		{
			name: "non finite bid",
			message: `{"stream":"ethusdt@bookTicker","data":` +
				`{"e":"bookTicker","s":"ETHUSDT","b":"NaN","a":"2010.20"}}`,
			symbol: "ETHUSDT",
		},
		{
			name: "wrong symbol",
			message: `{"stream":"btcusdt@bookTicker","data":` +
				`{"e":"bookTicker","s":"BTCUSDT","b":"100","a":"101"}}`,
			symbol: "ETHUSDT",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, recognized, err := parseCombinedMarketUpdate(
				[]byte(tt.message), tt.symbol, 1, 1, time.Now(),
			); err == nil || recognized {
				t.Fatalf("recognized=%v err=%v, want rejected message", recognized, err)
			}
		})
	}
}

func BenchmarkParseCombinedMarketUpdate(b *testing.B) {
	message := []byte(`{"stream":"ethusdt@bookTicker","data":{"e":"bookTicker","E":1700000000456,"s":"ETHUSDT","b":"2010.20","a":"2010.30"}}`)
	receivedAt := time.Unix(1_700_000_100, 0)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, recognized, err := parseCombinedMarketUpdate(
			message, "ETHUSDT", uint64(i), 3, receivedAt,
		); err != nil || !recognized {
			b.Fatalf("recognized=%v err=%v", recognized, err)
		}
	}
}

type fakeUserStreamConnection struct {
	done      chan struct{}
	stop      chan struct{}
	closeOnce sync.Once
}

func newFakeUserStreamConnection() *fakeUserStreamConnection {
	c := &fakeUserStreamConnection{
		done: make(chan struct{}),
		stop: make(chan struct{}),
	}
	go func() {
		select {
		case <-c.stop:
			c.disconnect()
		case <-c.done:
		}
	}()
	return c
}

func (c *fakeUserStreamConnection) disconnect() {
	c.closeOnce.Do(func() { close(c.done) })
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("等待条件满足超时")
}

func configureSuccessfulUserStream(w *WebSocketManager) <-chan *fakeUserStreamConnection {
	connections := make(chan *fakeUserStreamConnection, 8)
	w.startUserStreamFn = func(context.Context) (string, error) {
		return "test-listen-key", nil
	}
	w.serveUserStreamFn = func(string, futures.WsUserDataHandler, futures.ErrHandler) (chan struct{}, chan struct{}, error) {
		connection := newFakeUserStreamConnection()
		connections <- connection
		return connection.done, connection.stop, nil
	}
	w.keepAliveFn = func(context.Context, string) error { return nil }
	w.keepAliveInterval = time.Hour
	w.closeTimeout = 100 * time.Millisecond
	return connections
}

func TestOrderStreamStartWaitsForHandshake(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	w.startUserStreamFn = func(context.Context) (string, error) {
		return "test-listen-key", nil
	}
	handshakeStarted := make(chan struct{})
	releaseHandshake := make(chan struct{})
	connection := newFakeUserStreamConnection()
	w.serveUserStreamFn = func(string, futures.WsUserDataHandler, futures.ErrHandler) (chan struct{}, chan struct{}, error) {
		close(handshakeStarted)
		<-releaseHandshake
		return connection.done, connection.stop, nil
	}
	w.keepAliveInterval = time.Hour
	w.closeTimeout = 100 * time.Millisecond

	startResult := make(chan error, 1)
	go func() {
		startResult <- w.Start(context.Background(), func(OrderUpdate) {})
	}()

	select {
	case <-handshakeStarted:
	case <-time.After(time.Second):
		t.Fatal("WebSocket 握手没有开始")
	}
	select {
	case err := <-startResult:
		t.Fatalf("握手完成前 Start 提前返回: %v", err)
	default:
	}

	close(releaseHandshake)
	select {
	case err := <-startResult:
		if err != nil {
			t.Fatalf("Start 返回错误: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("握手完成后 Start 未返回")
	}
	if !w.Ready() {
		t.Fatalf("订单流应为 READY，实际健康状态: %+v", w.Health())
	}

	w.Stop()
	w.Stop()
	if health := w.Health(); health.State != OrderStreamStateStopped || health.Ready {
		t.Fatalf("幂等停止后的状态错误: %+v", health)
	}
}

func TestOrderStreamCanceledDuringHandshakeRollsBack(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	w.startUserStreamFn = func(context.Context) (string, error) {
		return "test-listen-key", nil
	}
	handshakeStarted := make(chan struct{})
	releaseHandshake := make(chan struct{})
	connection := newFakeUserStreamConnection()
	w.serveUserStreamFn = func(string, futures.WsUserDataHandler, futures.ErrHandler) (chan struct{}, chan struct{}, error) {
		close(handshakeStarted)
		<-releaseHandshake
		return connection.done, connection.stop, nil
	}
	w.closeTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	startResult := make(chan error, 1)
	go func() {
		startResult <- w.Start(ctx, func(OrderUpdate) {})
	}()
	select {
	case <-handshakeStarted:
	case <-time.After(time.Second):
		t.Fatal("WebSocket 握手没有开始")
	}
	cancel()
	close(releaseHandshake)

	select {
	case err := <-startResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start 错误 = %v，期望 context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("取消上下文后 Start 未返回")
	}
	if health := w.Health(); health.State != OrderStreamStateStopped || health.Ready {
		t.Fatalf("握手取消后未完整回滚: %+v", health)
	}
}

func TestOrderStreamInitialFailureRollsBack(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*WebSocketManager)
		wantError string
	}{
		{
			name: "listen key",
			configure: func(w *WebSocketManager) {
				w.startUserStreamFn = func(context.Context) (string, error) {
					return "", errors.New("listen key unavailable")
				}
			},
			wantError: "获取listenKey失败",
		},
		{
			name: "websocket handshake",
			configure: func(w *WebSocketManager) {
				w.startUserStreamFn = func(context.Context) (string, error) {
					return "test-listen-key", nil
				}
				w.serveUserStreamFn = func(string, futures.WsUserDataHandler, futures.ErrHandler) (chan struct{}, chan struct{}, error) {
					return nil, nil, errors.New("dial failed")
				}
			},
			wantError: "WebSocket订单流握手失败",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := NewWebSocketManager("api", "secret")
			tt.configure(w)
			err := w.Start(context.Background(), func(OrderUpdate) {})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Start 错误 = %v，期望包含 %q", err, tt.wantError)
			}
			if health := w.Health(); health.State != OrderStreamStateStopped || health.Ready {
				t.Fatalf("首连失败未完整回滚: %+v", health)
			}

			connections := configureSuccessfulUserStream(w)
			if err := w.Start(context.Background(), func(OrderUpdate) {}); err != nil {
				t.Fatalf("首连失败后无法重试 Start: %v", err)
			}
			<-connections
			w.Stop()
		})
	}
}

func TestListenKeyExpiredRefreshesAndReconnects(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	w.reconnectDelay = 50 * time.Millisecond
	w.keepAliveInterval = time.Hour
	w.closeTimeout = 100 * time.Millisecond

	var keyCalls atomic.Int32
	var keysMu sync.Mutex
	var keys []string
	w.startUserStreamFn = func(context.Context) (string, error) {
		key := "key-" + time.Now().Format("150405.000000000")
		keyCalls.Add(1)
		keysMu.Lock()
		keys = append(keys, key)
		keysMu.Unlock()
		return key, nil
	}
	connections := make(chan *fakeUserStreamConnection, 4)
	w.serveUserStreamFn = func(string, futures.WsUserDataHandler, futures.ErrHandler) (chan struct{}, chan struct{}, error) {
		connection := newFakeUserStreamConnection()
		connections <- connection
		return connection.done, connection.stop, nil
	}
	w.keepAliveFn = func(context.Context, string) error { return nil }

	if err := w.Start(context.Background(), func(OrderUpdate) {}); err != nil {
		t.Fatalf("Start 返回错误: %v", err)
	}
	firstConnection := <-connections
	w.handleUserDataEvent(&futures.WsUserDataEvent{Event: futures.UserDataEventTypeListenKeyExpired})
	if health := w.Health(); health.State != OrderStreamStateDegraded || health.Ready {
		t.Fatalf("listenKey 过期后未立即降级: %+v", health)
	}

	waitUntil(t, time.Second, func() bool {
		return keyCalls.Load() >= 2 && w.Ready()
	})
	select {
	case <-firstConnection.done:
	default:
		t.Fatal("listenKey 过期后旧连接未关闭")
	}
	keysMu.Lock()
	if len(keys) < 2 {
		t.Fatalf("listenKey 获取次数不足: %v", keys)
	}
	keysMu.Unlock()

	<-connections
	w.Stop()
}

func TestKeepAliveFailureRefreshesAndReconnects(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	w.reconnectDelay = 50 * time.Millisecond
	w.keepAliveInterval = 5 * time.Millisecond
	w.closeTimeout = 100 * time.Millisecond

	var keyCalls atomic.Int32
	w.startUserStreamFn = func(context.Context) (string, error) {
		keyCalls.Add(1)
		return "key", nil
	}
	connections := make(chan *fakeUserStreamConnection, 4)
	w.serveUserStreamFn = func(string, futures.WsUserDataHandler, futures.ErrHandler) (chan struct{}, chan struct{}, error) {
		connection := newFakeUserStreamConnection()
		connections <- connection
		return connection.done, connection.stop, nil
	}
	var keepAliveCalls atomic.Int32
	w.keepAliveFn = func(context.Context, string) error {
		if keepAliveCalls.Add(1) == 1 {
			return errors.New("keepalive rejected")
		}
		return nil
	}

	if err := w.Start(context.Background(), func(OrderUpdate) {}); err != nil {
		t.Fatalf("Start 返回错误: %v", err)
	}
	<-connections
	waitUntil(t, time.Second, func() bool {
		return w.Health().State == OrderStreamStateDegraded
	})
	waitUntil(t, time.Second, func() bool {
		return keyCalls.Load() >= 2 && w.Ready()
	})
	<-connections
	w.Stop()
}

func TestStopCancelsReconnectWait(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	w.reconnectDelay = 5 * time.Second
	w.closeTimeout = 100 * time.Millisecond
	connections := configureSuccessfulUserStream(w)
	w.reconnectDelay = 5 * time.Second

	if err := w.Start(context.Background(), func(OrderUpdate) {}); err != nil {
		t.Fatalf("Start 返回错误: %v", err)
	}
	connection := <-connections
	connection.disconnect()
	waitUntil(t, time.Second, func() bool {
		return w.Health().State == OrderStreamStateDegraded
	})

	startedAt := time.Now()
	w.Stop()
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("Stop 未及时打断重连等待，耗时 %v", elapsed)
	}
	w.Stop()
	if health := w.Health(); health.State != OrderStreamStateStopped || health.Ready {
		t.Fatalf("停止后的状态错误: %+v", health)
	}
}

func TestOrderStreamIgnoresPreviousGenerationHandlers(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	w.startUserStreamFn = func(context.Context) (string, error) {
		return "test-listen-key", nil
	}
	type capturedStream struct {
		connection *fakeUserStreamConnection
		handler    futures.WsUserDataHandler
		errHandler futures.ErrHandler
	}
	streams := make(chan capturedStream, 2)
	w.serveUserStreamFn = func(_ string, handler futures.WsUserDataHandler, errHandler futures.ErrHandler) (chan struct{}, chan struct{}, error) {
		connection := newFakeUserStreamConnection()
		streams <- capturedStream{connection: connection, handler: handler, errHandler: errHandler}
		return connection.done, connection.stop, nil
	}
	w.keepAliveFn = func(context.Context, string) error { return nil }
	w.keepAliveInterval = time.Hour
	w.closeTimeout = 100 * time.Millisecond

	var firstCalls atomic.Int32
	if err := w.Start(context.Background(), func(OrderUpdate) {
		firstCalls.Add(1)
	}); err != nil {
		t.Fatalf("第一代 Start() error = %v", err)
	}
	first := <-streams
	w.Stop()

	var secondCalls atomic.Int32
	if err := w.Start(context.Background(), func(OrderUpdate) {
		secondCalls.Add(1)
	}); err != nil {
		t.Fatalf("第二代 Start() error = %v", err)
	}
	second := <-streams
	if health := w.Health(); health.State != OrderStreamStateReady || !health.Ready {
		t.Fatalf("第二代订单流未就绪: %+v", health)
	}

	oldEvent := &futures.WsUserDataEvent{
		Event: futures.UserDataEventTypeOrderTradeUpdate,
		WsUserDataOrderTradeUpdate: futures.WsUserDataOrderTradeUpdate{
			OrderTradeUpdate: validOrderTradeUpdate(),
		},
	}
	first.handler(oldEvent)
	first.errHandler(errors.New("第一代连接迟到错误"))

	if firstCalls.Load() != 0 || secondCalls.Load() != 0 {
		t.Fatalf("第一代迟到事件串线: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
	if health := w.Health(); health.State != OrderStreamStateReady || !health.Ready || health.LastError != "" {
		t.Fatalf("第一代迟到错误污染第二代健康状态: %+v", health)
	}

	second.handler(oldEvent)
	if secondCalls.Load() != 1 {
		t.Fatalf("第二代事件回调次数 = %d，期望 1", secondCalls.Load())
	}
	w.Stop()
}

func TestWaitPriceReconnectIsContextCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if waitPriceReconnect(ctx, time.Minute) {
		t.Fatal("waitPriceReconnect() = true after cancellation")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled reconnect wait took %s", elapsed)
	}
}

func validOrderTradeUpdate() futures.WsOrderTradeUpdate {
	return futures.WsOrderTradeUpdate{
		Symbol:               "BTCUSDT",
		ClientOrderID:        "x-zdfVM8vY100000_B_1700000000001",
		Side:                 futures.SideTypeBuy,
		Type:                 futures.OrderTypeLimit,
		TimeInForce:          futures.TimeInForceTypeGTX,
		OriginalQty:          "0.010",
		OriginalPrice:        "100000.00",
		AveragePrice:         "100000.00",
		ExecutionType:        futures.OrderExecutionTypeTrade,
		Status:               futures.OrderStatusTypeFilled,
		ID:                   42,
		LastFilledQty:        "0.010",
		AccumulatedFilledQty: "0.010",
		LastFilledPrice:      "100000.00",
		CommissionAsset:      "USDT",
		Commission:           "0.20",
		TradeTime:            1_700_000_000_000,
		TradeID:              99,
		PositionSide:         futures.PositionSideTypeBoth,
		RealizedPnL:          "-0.05",
	}
}

func TestParseOrderTradeUpdateStrictly(t *testing.T) {
	update, err := parseOrderTradeUpdate(validOrderTradeUpdate())
	if err != nil {
		t.Fatalf("parseOrderTradeUpdate() error = %v", err)
	}
	if update.OrderID != 42 || update.Symbol != "BTCUSDT" || update.Status != OrderStatusFilled {
		t.Fatalf("订单核心字段解析错误: %+v", update)
	}
	if update.Quantity != 0.01 || update.ExecutedQty != 0.01 || update.AvgPrice != 100000 {
		t.Fatalf("订单数字字段解析错误: %+v", update)
	}
	if update.RealizedPNL != -0.05 || !update.RealizedPNLIncremental {
		t.Fatalf("订单盈亏字段解析错误: %+v", update)
	}
}

func TestParseOrderTradeUpdateRejectsMalformedCoreFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*futures.WsOrderTradeUpdate)
	}{
		{name: "missing order id", mutate: func(order *futures.WsOrderTradeUpdate) { order.ID = 0 }},
		{name: "missing symbol", mutate: func(order *futures.WsOrderTradeUpdate) { order.Symbol = "" }},
		{name: "missing client id", mutate: func(order *futures.WsOrderTradeUpdate) { order.ClientOrderID = "" }},
		{name: "invalid side", mutate: func(order *futures.WsOrderTradeUpdate) { order.Side = "" }},
		{name: "side conflicts with client id", mutate: func(order *futures.WsOrderTradeUpdate) { order.Side = futures.SideTypeSell; order.IsReduceOnly = true }},
		{name: "non limit order", mutate: func(order *futures.WsOrderTradeUpdate) { order.Type = futures.OrderTypeMarket }},
		{name: "unknown execution type", mutate: func(order *futures.WsOrderTradeUpdate) { order.ExecutionType = futures.OrderExecutionType("UNKNOWN") }},
		{name: "not post only", mutate: func(order *futures.WsOrderTradeUpdate) { order.TimeInForce = futures.TimeInForceTypeGTC }},
		{name: "buy reduce only", mutate: func(order *futures.WsOrderTradeUpdate) { order.IsReduceOnly = true }},
		{name: "hedge position side", mutate: func(order *futures.WsOrderTradeUpdate) { order.PositionSide = futures.PositionSideTypeLong }},
		{name: "missing trade time", mutate: func(order *futures.WsOrderTradeUpdate) { order.TradeTime = 0 }},
		{name: "unknown status", mutate: func(order *futures.WsOrderTradeUpdate) { order.Status = futures.OrderStatusType("UNKNOWN") }},
		{name: "malformed quantity", mutate: func(order *futures.WsOrderTradeUpdate) { order.OriginalQty = "broken" }},
		{name: "non finite quantity", mutate: func(order *futures.WsOrderTradeUpdate) { order.OriginalQty = "NaN" }},
		{name: "malformed executed quantity", mutate: func(order *futures.WsOrderTradeUpdate) { order.AccumulatedFilledQty = "broken" }},
		{name: "overfilled quantity", mutate: func(order *futures.WsOrderTradeUpdate) { order.AccumulatedFilledQty = "0.011" }},
		{name: "filled with zero quantity", mutate: func(order *futures.WsOrderTradeUpdate) { order.AccumulatedFilledQty = "0" }},
		{name: "filled below original quantity", mutate: func(order *futures.WsOrderTradeUpdate) {
			order.AccumulatedFilledQty = "0.009"
			order.LastFilledQty = "0.009"
		}},
		{name: "last fill exceeds accumulated", mutate: func(order *futures.WsOrderTradeUpdate) { order.LastFilledQty = "0.011" }},
		{name: "missing average fill price", mutate: func(order *futures.WsOrderTradeUpdate) { order.AveragePrice = "0" }},
		{name: "malformed last fill price", mutate: func(order *futures.WsOrderTradeUpdate) { order.LastFilledPrice = "broken" }},
		{name: "non finite pnl", mutate: func(order *futures.WsOrderTradeUpdate) { order.RealizedPnL = "+Inf" }},
		{name: "malformed commission", mutate: func(order *futures.WsOrderTradeUpdate) { order.Commission = "broken" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order := validOrderTradeUpdate()
			tt.mutate(&order)
			if update, err := parseOrderTradeUpdate(order); err == nil {
				t.Fatalf("parseOrderTradeUpdate() = %+v, want error", update)
			}
		})
	}
}

func TestParseOrderTradeUpdateNormalizesExpiredInMatch(t *testing.T) {
	order := validOrderTradeUpdate()
	order.Status = futures.OrderStatusType("EXPIRED_IN_MATCH")
	order.ExecutionType = futures.OrderExecutionTypeExpired
	order.AccumulatedFilledQty = "0"
	order.LastFilledQty = "0"
	order.AveragePrice = "0"
	order.LastFilledPrice = "0"
	order.RealizedPnL = "0"

	update, err := parseOrderTradeUpdate(order)
	if err != nil {
		t.Fatalf("parseOrderTradeUpdate() error = %v", err)
	}
	if update.Status != OrderStatusExpired {
		t.Fatalf("status = %q, want %q", update.Status, OrderStatusExpired)
	}
}

func TestMalformedOrderUpdateDegradesAndReconnectsStream(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	connections := configureSuccessfulUserStream(w)
	w.reconnectDelay = 10 * time.Millisecond

	var callbackCalls atomic.Int32
	if err := w.Start(context.Background(), func(OrderUpdate) {
		callbackCalls.Add(1)
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	<-connections

	order := validOrderTradeUpdate()
	order.AccumulatedFilledQty = "broken"
	w.handleUserDataEvent(&futures.WsUserDataEvent{
		Event: futures.UserDataEventTypeOrderTradeUpdate,
		WsUserDataOrderTradeUpdate: futures.WsUserDataOrderTradeUpdate{
			OrderTradeUpdate: order,
		},
	})

	if callbackCalls.Load() != 0 {
		t.Fatalf("畸形订单推送触发了 %d 次业务回调", callbackCalls.Load())
	}
	if health := w.Health(); health.State != OrderStreamStateDegraded || health.Ready {
		t.Fatalf("畸形订单推送后未立即降级: %+v", health)
	}

	select {
	case <-connections:
	case <-time.After(time.Second):
		t.Fatal("畸形订单推送后订单流未重连")
	}
	waitUntil(t, time.Second, w.Ready)
	w.Stop()
}

func TestStrategyOrderForWrongSymbolDegradesStream(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	w.SetExpectedSymbol("ETHUSDT")
	connections := configureSuccessfulUserStream(w)

	var callbackCalls atomic.Int32
	if err := w.Start(context.Background(), func(OrderUpdate) {
		callbackCalls.Add(1)
	}); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	<-connections

	order := validOrderTradeUpdate()
	w.handleUserDataEvent(&futures.WsUserDataEvent{
		Event: futures.UserDataEventTypeOrderTradeUpdate,
		WsUserDataOrderTradeUpdate: futures.WsUserDataOrderTradeUpdate{
			OrderTradeUpdate: order,
		},
	})

	if callbackCalls.Load() != 0 {
		t.Fatalf("错误交易对订单触发了 %d 次业务回调", callbackCalls.Load())
	}
	if health := w.Health(); health.State != OrderStreamStateDegraded || health.Ready {
		t.Fatalf("错误交易对订单后未立即降级: %+v", health)
	}
	w.Stop()
}

func TestNonStrategyOrderUpdateIsIgnored(t *testing.T) {
	w := NewWebSocketManager("api", "secret")
	var callbackCalls atomic.Int32
	w.mu.Lock()
	w.callbacks = []OrderUpdateCallback{func(OrderUpdate) { callbackCalls.Add(1) }}
	w.mu.Unlock()

	order := validOrderTradeUpdate()
	order.ClientOrderID = "manual-order"
	w.handleUserDataEvent(&futures.WsUserDataEvent{
		Event: futures.UserDataEventTypeOrderTradeUpdate,
		WsUserDataOrderTradeUpdate: futures.WsUserDataOrderTradeUpdate{
			OrderTradeUpdate: order,
		},
	})
	if callbackCalls.Load() != 0 {
		t.Fatalf("人工订单触发了 %d 次策略回调", callbackCalls.Load())
	}
}
