package backpack

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/exchange/streamhealth"

	"github.com/gorilla/websocket"
)

func TestOrderStreamLifecycleWaitsForWebSocketHandshakeAndRecovers(t *testing.T) {
	testCtx, cancelTest := context.WithCancel(context.Background())
	firstSubscribed := make(chan struct{}, 1)
	secondDialing := make(chan struct{}, 1)
	secondSubscribed := make(chan struct{}, 1)
	allowSecondUpgrade := make(chan struct{})
	dropFirst := make(chan struct{})
	var connections atomic.Int32

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		n := connections.Add(1)
		if n == 2 {
			secondDialing <- struct{}{}
			select {
			case <-allowSecondUpgrade:
			case <-testCtx.Done():
				return
			}
		}
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if n == 1 {
			firstSubscribed <- struct{}{}
			select {
			case <-dropFirst:
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "test disconnect"), time.Now().Add(time.Second))
			case <-testCtx.Done():
			}
			return
		}
		secondSubscribed <- struct{}{}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	secret := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	client, err := NewClient("key", secret)
	if err != nil {
		t.Fatalf("创建测试客户端失败: %v", err)
	}
	manager := NewWebSocketManager(client, "BTC_USDC_PERP", "BTCUSDC", newClientIDMapper(), 2)
	manager.orderURL = "ws" + strings.TrimPrefix(server.URL, "http")
	manager.reconnectGap = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		manager.Stop()
		cancelTest()
	}()

	if err := manager.Start(ctx, func(OrderUpdate) {}); err != nil {
		t.Fatalf("Start 返回错误: %v", err)
	}
	waitBackpackSignal(t, firstSubscribed, "首次签名订阅帧")
	if !manager.orderHealth.Ready() {
		t.Fatalf("WebSocket 握手并写入订阅后状态=%s，期望 READY", manager.orderHealth.State())
	}

	close(dropFirst)
	waitBackpackSignal(t, secondDialing, "重连握手")
	if state := manager.orderHealth.State(); state != streamhealth.StateDegraded {
		t.Fatalf("断线且重连握手尚未完成时状态=%s，期望 DEGRADED", state)
	}
	close(allowSecondUpgrade)
	waitBackpackSignal(t, secondSubscribed, "重连签名订阅帧")
	waitBackpackState(t, manager, streamhealth.StateReady)
}

func TestBackpackOrderStreamErrorFrameIsDetected(t *testing.T) {
	err := backpackOrderStreamMessageError([]byte(`{"error":{"code":"INVALID_SIGNATURE","message":"bad signature"}}`))
	if err == nil {
		t.Fatal("私有流错误帧未被识别")
	}
	if err := backpackOrderStreamMessageError([]byte(`{"stream":"account.orderUpdate.BTC_USDC_PERP","data":{}}`)); err != nil {
		t.Fatalf("正常订单帧被误判: %v", err)
	}
}

func TestOrderStreamRestartRejectsStaleGenerationHealthAndMessages(t *testing.T) {
	var connections atomic.Int32
	subscribed := make(chan struct{}, 2)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		conn, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		subscribed <- struct{}{}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	secret := base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize))
	client, err := NewClient("key", secret)
	if err != nil {
		t.Fatalf("创建测试客户端失败: %v", err)
	}
	manager := NewWebSocketManager(client, "BTC_USDC_PERP", "BTCUSDC", newClientIDMapper(), 2)
	manager.orderURL = "ws" + strings.TrimPrefix(server.URL, "http")
	manager.handshakeTTL = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := manager.Start(ctx, func(OrderUpdate) {}); err != nil {
		t.Fatalf("首次 Start 失败: %v", err)
	}
	waitBackpackSignal(t, subscribed, "首代订阅帧")
	manager.mu.RLock()
	firstGeneration := manager.orderGeneration
	manager.mu.RUnlock()
	manager.Stop()

	var secondCallbacks atomic.Int32
	if err := manager.Start(ctx, func(OrderUpdate) { secondCallbacks.Add(1) }); err != nil {
		t.Fatalf("第二次 Start 失败: %v", err)
	}
	waitBackpackSignal(t, subscribed, "第二代订阅帧")
	if manager.setOrderHealth(firstGeneration, streamhealth.StateDegraded, fmt.Errorf("stale")) {
		t.Fatal("旧代际不应能更新当前健康状态")
	}
	if state := manager.orderHealth.State(); state != streamhealth.StateReady {
		t.Fatalf("旧代际写入后状态=%s，期望 READY", state)
	}
	staleOrder := []byte(`{"stream":"account.orderUpdate.BTC_USDC_PERP","data":{"i":"1","S":"Bid","o":"Limit","X":"New","p":"100","q":"1"}}`)
	manager.handleOrderMessage(staleOrder, firstGeneration)
	if got := secondCallbacks.Load(); got != 0 {
		t.Fatalf("旧代际消息触发了新回调: %d", got)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("连接代际数=%d，期望 2", got)
	}
	manager.Stop()
}

func waitBackpackSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("等待%s超时", name)
	}
}

func waitBackpackState(t *testing.T, manager *WebSocketManager, want streamhealth.State) {
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
