package bitget

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

func TestPrivateOrderStreamLifecycleWaitsForSubscriptionAndRecovers(t *testing.T) {
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
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"login","code":"0"}`)); err != nil {
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
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"subscribe","arg":{"instType":"USDT-FUTURES","channel":"orders","instId":"default"}}`)); err != nil {
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

	manager := NewWebSocketManager("key", "secret", "passphrase")
	manager.privateURL = "ws" + strings.TrimPrefix(server.URL, "http")
	manager.reconnectDelay = 10 * time.Millisecond
	manager.handshakeTTL = time.Second
	// This test exercises only the private lifecycle; the price stream has its
	// own manager loop and must not contact the real public endpoint.
	manager.publicHandlerStarted = true
	manager.publicCtx = context.Background()
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		manager.Stop()
		cancelTest()
	}()

	startResult := make(chan error, 1)
	go func() { startResult <- manager.Start(ctx, "BTCUSDT", "USDT-FUTURES", func(interface{}) {}) }()
	waitBitgetSignal(t, firstSubscribed, "首次订阅请求")
	select {
	case err := <-startResult:
		t.Fatalf("订阅确认前 Start 已返回: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if state := manager.orderHealth.State(); state != streamhealth.StateStarting {
		t.Fatalf("等待首次订阅确认时状态=%s，期望 STARTING", state)
	}
	close(allowFirstAck)
	if err := waitBitgetStart(t, startResult); err != nil {
		t.Fatalf("Start 返回错误: %v", err)
	}
	if !manager.orderHealth.Ready() {
		t.Fatalf("订阅确认后状态=%s，期望 READY", manager.orderHealth.State())
	}

	close(dropFirst)
	waitBitgetSignal(t, secondSubscribed, "重连订阅请求")
	if state := manager.orderHealth.State(); state != streamhealth.StateDegraded {
		t.Fatalf("断线且重连尚未确认时状态=%s，期望 DEGRADED", state)
	}
	close(allowSecondAck)
	waitBitgetState(t, manager, streamhealth.StateReady)
}

func TestPrivateSubscriptionRejectsWrongProductType(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if req.URL.Path == "/public" {
			_, _, _ = conn.ReadMessage()
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"login","code":"0"}`)); err != nil {
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"subscribe","arg":{"instType":"USDT-FUTURES","channel":"orders","instId":"default"}}`))
	}))
	defer server.Close()

	manager := NewWebSocketManager("key", "secret", "passphrase")
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	manager.privateURL = wsURL + "/private"
	manager.publicURL = wsURL + "/public"
	manager.handshakeTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := manager.Start(ctx, "BTCUSDC", "USDC-FUTURES", func(interface{}) {})
	if err == nil || !strings.Contains(err.Error(), "产品类型不匹配") {
		t.Fatalf("错误产品类型 ACK 未被拒绝: %v", err)
	}
	manager.Stop()
}

func TestStopThenStartRestartsPrivateAndPublicGenerations(t *testing.T) {
	privateConnected := make(chan struct{}, 4)
	publicSubscribed := make(chan struct{}, 4)
	var privateConnections atomic.Int32
	var publicConnections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if req.URL.Path == "/public" {
			publicConnections.Add(1)
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			publicSubscribed <- struct{}{}
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}

		privateConnections.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"login","code":"0"}`)); err != nil {
			return
		}
		var subscribe struct {
			Args []WSSubscribeArg `json:"args"`
		}
		if err := conn.ReadJSON(&subscribe); err != nil || len(subscribe.Args) != 1 {
			return
		}
		privateConnected <- struct{}{}
		if err := conn.WriteJSON(map[string]any{"event": "subscribe", "arg": subscribe.Args[0]}); err != nil {
			return
		}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	manager := NewWebSocketManager("key", "secret", "passphrase")
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	manager.privateURL = wsURL + "/private"
	manager.publicURL = wsURL + "/public"
	manager.handshakeTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := func() {
		t.Helper()
		if err := manager.Start(ctx, "BTCUSDC", "USDC-FUTURES", func(interface{}) {}); err != nil {
			t.Fatalf("Start 失败: %v", err)
		}
		waitBitgetSignal(t, privateConnected, "私有订阅")
		waitBitgetSignal(t, publicSubscribed, "公共订阅")
	}
	start()
	stopDone := make(chan struct{})
	go func() {
		manager.Stop()
		close(stopDone)
	}()
	waitBitgetSignal(t, stopDone, "首代 Stop")
	start()
	if got := privateConnections.Load(); got != 2 {
		t.Fatalf("私有连接代际=%d，期望 2", got)
	}
	if got := publicConnections.Load(); got != 2 {
		t.Fatalf("公共连接代际=%d，期望 2", got)
	}
	manager.Stop()
}

func TestStartRejectsCanceledOrExitingContexts(t *testing.T) {
	manager := NewWebSocketManager("key", "secret", "passphrase")
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := manager.Start(canceledCtx, "BTCUSDT", "USDT-FUTURES", nil); err == nil || !strings.Contains(err.Error(), "上下文已结束") {
		t.Fatalf("已取消入参上下文未被拒绝: %v", err)
	}

	publicCtx, cancelPublic := context.WithCancel(context.Background())
	cancelPublic()
	manager.mu.Lock()
	manager.publicHandlerStarted = true
	manager.publicCtx = publicCtx
	manager.subscribedSymbol = "BTCUSDT"
	manager.subscribedInstType = "USDT-FUTURES"
	manager.mu.Unlock()
	if err := manager.Start(context.Background(), "BTCUSDT", "USDT-FUTURES", nil); err == nil || !strings.Contains(err.Error(), "公共 WebSocket 正在退出") {
		t.Fatalf("已取消公共代际未被拒绝: %v", err)
	}

	manager.mu.Lock()
	manager.publicHandlerStarted = false
	manager.publicCtx = nil
	privateCtx, cancelPrivate := context.WithCancel(context.Background())
	cancelPrivate()
	manager.privateHandlerStarted = true
	manager.privateCtx = privateCtx
	manager.orderHealth.Set(streamhealth.StateReady, nil)
	manager.mu.Unlock()
	if err := manager.Start(context.Background(), "BTCUSDT", "USDT-FUTURES", func(interface{}) {}); err == nil || !strings.Contains(err.Error(), "私有 WebSocket 正在退出") {
		t.Fatalf("已取消私有代际未被拒绝: %v", err)
	}
}

func waitBitgetSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待%s超时", name)
	}
}

func waitBitgetStart(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("等待 Start 返回超时")
		return nil
	}
}

func waitBitgetState(t *testing.T, manager *WebSocketManager, want streamhealth.State) {
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
