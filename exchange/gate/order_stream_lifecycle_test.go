package gate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/exchange/streamhealth"

	"github.com/gorilla/websocket"
)

func TestOrderStreamLifecycleWaitsForPrivateSubscriptionAndRecovers(t *testing.T) {
	testCtx, cancelTest := context.WithCancel(context.Background())
	firstSubscribed := make(chan struct{}, 1)
	secondSubscribed := make(chan struct{}, 1)
	allowFirstAck := make(chan struct{})
	allowSecondAck := make(chan struct{})
	dropFirst := make(chan struct{})
	ordersPayloads := make(chan []any, 4)
	var connections atomic.Int32

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		n := connections.Add(1)
		for i := 0; i < 3; i++ {
			var request struct {
				Channel string `json:"channel"`
				Payload []any  `json:"payload"`
			}
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			if request.Channel == "futures.orders" {
				ordersPayloads <- request.Payload
			}
		}
		if n == 1 {
			firstSubscribed <- struct{}{}
			select {
			case <-allowFirstAck:
			case <-testCtx.Done():
				return
			}
		} else {
			secondSubscribed <- struct{}{}
			select {
			case <-allowSecondAck:
			case <-testCtx.Done():
				return
			}
		}
		ack := []byte(`{"channel":"futures.orders","event":"subscribe","result":{"status":"success"}}`)
		if err := conn.WriteMessage(websocket.TextMessage, ack); err != nil {
			return
		}
		if n == 1 {
			select {
			case <-dropFirst:
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "test disconnect"), time.Now().Add(time.Second))
			case <-testCtx.Done():
			}
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	manager := NewWebSocketManager("key", "secret", "usdt")
	manager.SetUserID(12345)
	manager.wsURL = "ws" + strings.TrimPrefix(server.URL, "http")
	manager.reconnectDelay = 10 * time.Millisecond
	manager.handshakeTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = manager.Stop()
		cancelTest()
	}()

	startResult := make(chan error, 1)
	go func() { startResult <- manager.Start(ctx, "BTCUSDT") }()
	waitGateSignal(t, firstSubscribed, "首次订阅请求")
	select {
	case payload := <-ordersPayloads:
		if len(payload) != 2 || payload[0] != float64(12345) || payload[1] != "BTC_USDT" {
			t.Fatalf("Gate orders payload=%v，期望 [12345 BTC_USDT]", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Gate orders payload 超时")
	}
	select {
	case err := <-startResult:
		t.Fatalf("订阅确认前 Start 已返回: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if state := manager.orderHealth.State(); state != streamhealth.StateStarting {
		t.Fatalf("等待首次订阅确认时状态=%s，期望 STARTING", state)
	}
	close(allowFirstAck)
	if err := waitGateStart(t, startResult); err != nil {
		t.Fatalf("Start 返回错误: %v", err)
	}
	if !manager.orderHealth.Ready() {
		t.Fatalf("订阅确认后状态=%s，期望 READY", manager.orderHealth.State())
	}

	close(dropFirst)
	waitGateSignal(t, secondSubscribed, "重连订阅请求")
	if state := manager.orderHealth.State(); state != streamhealth.StateDegraded {
		t.Fatalf("断线且重连尚未确认时状态=%s，期望 DEGRADED", state)
	}
	close(allowSecondAck)
	waitGateState(t, manager, streamhealth.StateReady)
}

func TestStopThenStartUsesIndependentGeneration(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections.Add(1)
		for i := 0; i < 3; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"channel":"futures.orders","event":"subscribe","result":{"status":"success"}}`)); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	manager := NewWebSocketManager("key", "secret", "usdt")
	manager.SetUserID(9876)
	manager.wsURL = "ws" + strings.TrimPrefix(server.URL, "http")
	manager.handshakeTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var firstCallbacks atomic.Int32
	if err := manager.Start(ctx, "BTCUSDT", func(interface{}) {
		firstCallbacks.Add(1)
	}); err != nil {
		t.Fatalf("首次 Start 失败: %v", err)
	}
	manager.mu.RLock()
	firstGeneration := manager.generation
	manager.mu.RUnlock()
	stopResult := make(chan error, 1)
	go func() { stopResult <- manager.Stop() }()
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("首次 Stop 失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("首次 Stop 挂起")
	}
	var secondCallbacks atomic.Int32
	if err := manager.Start(ctx, "BTCUSDT", func(interface{}) {
		secondCallbacks.Add(1)
	}); err != nil {
		t.Fatalf("第二次 Start 失败: %v", err)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("连接代际=%d，期望 2", got)
	}
	orderMessage := []byte(`{"channel":"futures.orders","event":"update","result":[{"id":42,"contract":"BTC_USDT","status":"open","size":1,"left":1,"price":"100000","fill_price":"0","text":"t-test","finish_time":1700000000,"pnl":"0"}]}`)
	if err := manager.handleMessage(orderMessage, firstGeneration); err != nil {
		t.Fatalf("注入旧代消息失败: %v", err)
	}
	if firstCallbacks.Load() != 0 || secondCallbacks.Load() != 0 {
		t.Fatalf("旧代消息串线: first=%d second=%d", firstCallbacks.Load(), secondCallbacks.Load())
	}
	manager.mu.RLock()
	secondGeneration := manager.generation
	manager.mu.RUnlock()
	if err := manager.handleMessage(orderMessage, secondGeneration); err != nil {
		t.Fatalf("注入当前代消息失败: %v", err)
	}
	if secondCallbacks.Load() != 1 {
		t.Fatalf("当前代回调次数=%d，期望 1", secondCallbacks.Load())
	}
	if err := manager.Stop(); err != nil {
		t.Fatalf("第二次 Stop 失败: %v", err)
	}
}

func TestAdapterStartFailsWhileManagerIsExiting(t *testing.T) {
	manager := NewWebSocketManager("key", "secret", "usdt")
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.mu.Lock()
	manager.ctx = runCtx
	manager.conn = &websocket.Conn{}
	manager.stopping = true
	manager.generation = 1
	manager.orderHealth.Set(streamhealth.StateStopping, nil)
	manager.mu.Unlock()

	adapter := &GateAdapter{symbol: "BTCUSDT", wsManager: manager}
	if err := adapter.StartOrderStream(context.Background(), func(interface{}) {}); err == nil || !strings.Contains(err.Error(), "正在退出") {
		t.Fatalf("STOPPING 期间 StartOrderStream 未拒绝: %v", err)
	}
	if err := adapter.StartPriceStream(context.Background(), func(string, float64) {}); err == nil || !strings.Contains(err.Error(), "正在退出") {
		t.Fatalf("STOPPING 期间 StartPriceStream 未拒绝: %v", err)
	}
}

func TestSilentConnectionDegradesAndReconnects(t *testing.T) {
	testCtx, cancelTest := context.WithCancel(context.Background())
	secondSubscribed := make(chan struct{}, 1)
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		n := connections.Add(1)
		for i := 0; i < 3; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
		if n > 1 {
			secondSubscribed <- struct{}{}
			<-testCtx.Done()
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"channel":"futures.orders","event":"subscribe","result":{"status":"success"}}`)); err != nil {
			return
		}
		// 持续读取 ping 但故意不回复 futures.pong，模拟静默半开连接。
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	manager := NewWebSocketManager("key", "secret", "usdt")
	manager.SetUserID(12345)
	manager.wsURL = "ws" + strings.TrimPrefix(server.URL, "http")
	manager.reconnectDelay = 5 * time.Millisecond
	manager.handshakeTTL = time.Second
	manager.pingInterval = 10 * time.Millisecond
	manager.pongWait = 80 * time.Millisecond
	manager.writeTTL = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		_ = manager.Stop()
		cancelTest()
	}()

	if err := manager.Start(ctx, "BTCUSDT"); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	waitGateSignal(t, secondSubscribed, "静默连接后的重连订阅")
	if state := manager.orderHealth.State(); state != streamhealth.StateDegraded {
		t.Fatalf("重连尚未确认时状态=%s，期望 DEGRADED", state)
	}
}

func waitGateSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待%s超时", name)
	}
}

func waitGateStart(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Start 返回超时")
		return nil
	}
}

func waitGateState(t *testing.T, manager *WebSocketManager, want streamhealth.State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if manager.orderHealth.State() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("订单流状态=%s，期望 %s", manager.orderHealth.State(), want)
}
