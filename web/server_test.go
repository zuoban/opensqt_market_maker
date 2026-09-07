package web

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"opensqt/config"
	"opensqt/logger"
	"opensqt/position"
)

func testDashCfg(listen, token string) *config.Config {
	cfg := &config.Config{}
	cfg.Trading.Symbol = "ETHUSDT"
	cfg.Trading.OrderQuantity = 30
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 2
	cfg.Exchanges.Binance = config.BinanceConfig{APIKey: "k", SecretKey: "s", FeeRate: 0.0002}
	enabled := true
	cfg.Dashboard.Enabled = &enabled
	cfg.Dashboard.Listen = listen
	cfg.Dashboard.Token = token
	cfg.Dashboard.PushIntervalMS = 200
	cfg.Dashboard.AccountRefreshSec = 10
	if err := cfg.Validate(); err != nil {
		panic(err)
	}
	return cfg
}

func startServer(t *testing.T, token string) *Server {
	t.Helper()
	s := New(Options{Cfg: testDashCfg("127.0.0.1:0", token), Version: "test-ver"})
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Addr() != "" {
			return s
		}
		select {
		case err := <-errCh:
			t.Fatalf("start: %v", err)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("server did not start")
	return s
}

func TestNewWiresPositionCacheToDashboardPushInterval(t *testing.T) {
	cfg := testDashCfg("127.0.0.1:0", "")
	cfg.Dashboard.PushIntervalMS = 275
	manager := &position.SuperPositionManager{}
	s := New(Options{Cfg: cfg, Position: manager})

	if s.position == nil {
		t.Fatal("position cache was not created")
	}
	if s.position.interval != 275*time.Millisecond {
		t.Fatalf("position refresh interval = %v, want 275ms", s.position.interval)
	}
	if s.assembler.position != s.position || s.assembler.pos != manager {
		t.Fatal("assembler was not wired to the position cache with manager fallback")
	}
}

func TestShutdownWhileListenIsPendingPreventsLateServerStart(t *testing.T) {
	s := New(Options{Cfg: testDashCfg("127.0.0.1:0", "")})
	listenStarted := make(chan struct{})
	releaseListen := make(chan struct{})
	s.listenFunc = func(network, address string) (net.Listener, error) {
		close(listenStarted)
		<-releaseListen
		return net.Listen(network, address)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	select {
	case <-listenStarted:
	case <-time.After(time.Second):
		t.Fatal("dashboard listen did not start")
	}

	s.Shutdown(100 * time.Millisecond)
	close(releaseListen)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("start after shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("dashboard Start did not stop after pending listen was released")
	}
	if addr := s.Addr(); addr != "" {
		t.Fatalf("dashboard published a late listener after shutdown: %s", addr)
	}
}

func TestHealthAndSnapshotAndAuth(t *testing.T) {
	s := startServer(t, "s3cret")
	defer s.Shutdown(time.Second)
	base := "http://" + s.Addr()

	resp, err := http.Get(base + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	var health map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if health["version"] != "test-ver" {
		t.Fatalf("health = %v", health)
	}
	if health["startedAt"] == nil {
		t.Fatalf("health missing startedAt: %v", health)
	}

	resp, err = http.Get(base + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, base+"/api/snapshot?token=s3cret", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("snapshot status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !json.Valid(body) {
		t.Fatalf("invalid json: %s", body)
	}

	resp, err = http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(page) < 100 {
		t.Fatalf("index status=%d len=%d", resp.StatusCode, len(page))
	}

	wsURL := "ws://" + s.Addr() + "/ws?token=s3cret"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var env wsEnvelope
	if err := conn.ReadJSON(&env); err != nil {
		t.Fatalf("ws read: %v", err)
	}
	if env.Type != "snapshot" || env.Data == nil || env.Data.Version != "test-ver" {
		t.Fatalf("ws env = %+v", env)
	}
}

func TestWebSocketClientLifecycle(t *testing.T) {
	s := startServer(t, "")
	defer s.Shutdown(time.Second)

	conn, _, err := websocket.DefaultDialer.Dial("ws://"+s.Addr()+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var env wsEnvelope
	if err := conn.ReadJSON(&env); err != nil {
		t.Fatalf("ws initial snapshot: %v", err)
	}
	waitForDashboardClientCount(t, s.hub, 1)

	if err := conn.Close(); err != nil {
		t.Fatalf("ws close: %v", err)
	}
	waitForDashboardClientCount(t, s.hub, 0)
}

func TestShutdownClosesWebSocketClients(t *testing.T) {
	s := startServer(t, "")
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+s.Addr()+"/ws", nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var env wsEnvelope
	if err := conn.ReadJSON(&env); err != nil {
		t.Fatalf("ws initial snapshot: %v", err)
	}
	waitForDashboardClientCount(t, s.hub, 1)

	s.Shutdown(time.Second)
	waitForDashboardClientCount(t, s.hub, 0)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("websocket remained open after server shutdown")
	}
}

func waitForDashboardClientCount(t *testing.T, h *hub, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.clientCount(); got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("dashboard client count = %d, want %d", h.clientCount(), want)
}

func TestIsNonLocalListen(t *testing.T) {
	if !isNonLocalListen("0.0.0.0:8787") {
		t.Fatal("0.0.0.0 should be non-local")
	}
	if isNonLocalListen("127.0.0.1:8787") {
		t.Fatal("127.0.0.1 should be local")
	}
}

func TestDashboardOriginAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8787/ws", nil)
	if !dashboardOriginAllowed(req) {
		t.Fatal("clients without an Origin header should be allowed")
	}

	req.Header.Set("Origin", "http://127.0.0.1:8787")
	if !dashboardOriginAllowed(req) {
		t.Fatal("same-origin websocket should be allowed")
	}

	req.Header.Set("Origin", "https://attacker.example")
	if dashboardOriginAllowed(req) {
		t.Fatal("cross-origin websocket should be rejected")
	}
}

func TestDemoServer(t *testing.T) {
	if os.Getenv("WEB_DEMO") == "" {
		t.Skip("set WEB_DEMO=1 to hold a local dashboard")
	}
	s := New(Options{Cfg: testDashCfg("127.0.0.1:18787", ""), Version: "demo"})
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start() }()
	t.Log("dashboard http://127.0.0.1:18787")
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Minute):
		s.Shutdown(time.Second)
	}
}

func TestRecentLogsRoundTrip(t *testing.T) {
	logger.Info("dashboard-test-log-%d", 42)
	found := false
	for _, e := range logger.RecentLogs(50) {
		if e.Level == "INFO" && e.Message != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected recent logs")
	}
}
