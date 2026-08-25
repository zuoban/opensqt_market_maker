package bybit

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

func TestOrderStreamLifecycleWaitsForHandshakeAndRecovers(t *testing.T) {
	testCtx, cancelTest := context.WithCancel(context.Background())
	firstSubscribed := make(chan struct{}, 1)
	secondSubscribed := make(chan struct{}, 1)
	allowFirstAck := make(chan struct{})
	allowSecondAck := make(chan struct{})
	dropFirst := make(chan struct{})
	var connections atomic.Int32

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		n := connections.Add(1)

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"op": "auth", "success": true}); err != nil {
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
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
		if err := conn.WriteJSON(map[string]any{"op": "subscribe", "success": true}); err != nil {
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

	manager := NewWebSocketManager(NewClient("key", "secret"), "BTCUSDT", "BTCUSDT", newOrderIDMapper())
	manager.orderURL = "ws" + strings.TrimPrefix(server.URL, "http")
	manager.reconnectGap = 10 * time.Millisecond
	manager.handshakeTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		manager.Stop()
		cancelTest()
	}()

	startResult := make(chan error, 1)
	go func() { startResult <- manager.Start(ctx, func(OrderUpdate) {}) }()
	waitBybitSignal(t, firstSubscribed, "首次订阅请求")
	select {
	case err := <-startResult:
		t.Fatalf("订阅确认前 Start 已返回: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if state := manager.orderHealth.State(); state != streamhealth.StateStarting {
		t.Fatalf("等待首次订阅确认时状态=%s，期望 STARTING", state)
	}
	close(allowFirstAck)
	if err := waitBybitStart(t, startResult); err != nil {
		t.Fatalf("Start 返回错误: %v", err)
	}
	if !manager.orderHealth.Ready() {
		t.Fatalf("握手完成后状态=%s，期望 READY", manager.orderHealth.State())
	}

	close(dropFirst)
	waitBybitSignal(t, secondSubscribed, "重连订阅请求")
	if state := manager.orderHealth.State(); state != streamhealth.StateDegraded {
		t.Fatalf("断线且重连尚未确认时状态=%s，期望 DEGRADED", state)
	}
	close(allowSecondAck)
	waitBybitState(t, manager, streamhealth.StateReady)
}

func TestOrderStreamRestartRejectsStaleGenerationHealthAndMessages(t *testing.T) {
	testCtx, cancelTest := context.WithCancel(context.Background())
	defer cancelTest()
	secondSubscribed := make(chan struct{}, 1)
	allowSecondAck := make(chan struct{})
	var connections atomic.Int32

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		n := connections.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"op": "auth", "success": true}); err != nil {
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if n == 2 {
			secondSubscribed <- struct{}{}
			select {
			case <-allowSecondAck:
			case <-testCtx.Done():
				return
			}
		}
		if err := conn.WriteJSON(map[string]any{"op": "subscribe", "success": true}); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	manager := NewWebSocketManager(NewClient("key", "secret"), "BTCUSDT", "BTCUSDT", newOrderIDMapper())
	manager.orderURL = "ws" + strings.TrimPrefix(server.URL, "http")
	manager.handshakeTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := manager.Start(ctx, func(OrderUpdate) {}); err != nil {
		t.Fatalf("首次 Start 失败: %v", err)
	}
	manager.mu.RLock()
	firstGeneration := manager.orderGeneration
	manager.mu.RUnlock()
	manager.Stop()

	var secondCallbacks atomic.Int32
	startResult := make(chan error, 1)
	go func() {
		startResult <- manager.Start(ctx, func(OrderUpdate) { secondCallbacks.Add(1) })
	}()
	waitBybitSignal(t, secondSubscribed, "第二代订阅请求")
	if manager.setOrderHealth(firstGeneration, streamhealth.StateReady, nil) {
		t.Fatal("旧代际不应能更新当前健康状态")
	}
	if state := manager.orderHealth.State(); state != streamhealth.StateStarting {
		t.Fatalf("旧代际写入后状态=%s，期望 STARTING", state)
	}
	staleOrder := []byte(`{"topic":"order","data":[{"category":"linear","orderId":"1","symbol":"BTCUSDT","side":"Buy","orderType":"Limit","orderStatus":"New","price":"100","qty":"1"}]}`)
	manager.handleOrderMessage(staleOrder, firstGeneration)
	if got := secondCallbacks.Load(); got != 0 {
		t.Fatalf("旧代际消息触发了新回调: %d", got)
	}
	close(allowSecondAck)
	if err := waitBybitStart(t, startResult); err != nil {
		t.Fatalf("第二次 Start 失败: %v", err)
	}
	manager.Stop()
}

func waitBybitSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待%s超时", name)
	}
}

func waitBybitStart(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Start 返回超时")
		return nil
	}
}

func waitBybitState(t *testing.T, manager *WebSocketManager, want streamhealth.State) {
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
