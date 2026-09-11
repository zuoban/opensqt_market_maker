package notification

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/config"
	"opensqt/position"
)

var testTelegramConfig = config.TelegramConfig{Enabled: true, BotToken: "123:test-token", ChatID: "-100123456"}

func testFill() position.FilledOrderRecord {
	return position.FilledOrderRecord{OrderID: 42, Symbol: "ETHUSDC", Side: "SELL", Price: 2500.25,
		Quantity: 0.01, RealizedPNL: -0.025, FilledAt: time.Date(2026, 9, 12, 10, 20, 30, 0, time.UTC)}
}

func TestTelegramSendPayload(t *testing.T) {
	received := make(chan map[string]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/bot123:test-token/sendMessage" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("incorrect Telegram request method, path or content type")
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		received <- payload
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":123}}`)
	}))
	defer server.Close()
	n := newTelegram(context.Background(), testTelegramConfig, "USDC", server.URL, server.Client())
	defer n.Close()
	n.NotifyFill(testFill())
	select {
	case payload := <-received:
		if payload["chat_id"] != testTelegramConfig.ChatID || len(payload) != 2 {
			t.Fatalf("unexpected payload: %+v", payload)
		}
		if !strings.HasPrefix(payload["text"], "卖出（SELL）｜ 均价 2500.25 USDC\n\n") {
			t.Errorf("notification title missing direction or average price: %s", payload["text"])
		}
		if strings.Contains(payload["text"], "\n方向：") || strings.Contains(payload["text"], "\n成交均价：") || strings.Contains(payload["text"], "交易所") {
			t.Error("notification body contains redundant fields")
		}
		for _, want := range []string{"ETHUSDC", "0.01", "25.00250000 USDC", "订单 ID：42", "2026-09-12", "-0.02500000 USDC（未扣手续费）"} {
			if !strings.Contains(payload["text"], want) {
				t.Errorf("notification missing %q: %s", want, payload["text"])
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("notification was not sent")
	}
	buy := testFill()
	buy.Side = "BUY"
	text := formatFill(buy, "USDC")
	if !strings.HasPrefix(text, "买入（BUY）｜ 均价 2500.25 USDC\n\n") || strings.Contains(text, "盈亏") || strings.Contains(text, "交易所") {
		t.Fatalf("unexpected buy notification: %s", text)
	}
}

func TestTelegramResponseHandlingAndRedaction(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		retry  time.Duration
		ok     bool
	}{
		{name: "success", status: 200, body: `{"ok":true}`, ok: true},
		{name: "rejected", status: 200, body: `{"ok":false,"error_code":400,"description":"123:test-token -100123456"}`},
		{name: "unauthorized", status: 401, body: `{"ok":false,"description":"123:test-token"}`},
		{name: "server error", status: 500, body: "123:test-token"},
		{name: "invalid json", status: 200, body: "123:test-token"},
		{name: "oversized", status: 200, body: strings.Repeat("x", telegramResponseLimit+1)},
		{name: "rate limited", status: 429, body: `{"ok":false,"parameters":{"retry_after":2}}`, retry: 2 * time.Second},
		{name: "rate limited invalid body", status: 429, body: "", retry: time.Second},
		{name: "excessive retry wait", status: 429, body: `{"ok":false,"parameters":{"retry_after":3600}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			n := newTelegram(context.Background(), testTelegramConfig, "USDC", server.URL, server.Client())
			defer n.Close()
			retry, err := n.send(testFill())
			if (err == nil) != tt.ok || retry != tt.retry {
				t.Fatalf("send() = %s, %v", retry, err)
			}
			if err != nil && (strings.Contains(err.Error(), testTelegramConfig.BotToken) || strings.Contains(err.Error(), testTelegramConfig.ChatID)) {
				t.Fatal("error contains Telegram credentials")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramNetworkErrorsAreRedactedAndNotRetried(t *testing.T) {
	var calls atomic.Int64
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("transport leaked " + r.URL.String())
	})}
	n := newTelegram(context.Background(), testTelegramConfig, "USDC", "https://api.telegram.org", client)
	defer n.Close()
	retry, err := n.send(testFill())
	if err == nil || strings.Contains(err.Error(), testTelegramConfig.BotToken) || retry != 0 || calls.Load() != 1 {
		t.Fatalf("unsafe network error handling: retry=%v err=%v calls=%d", retry, err, calls.Load())
	}
}

func TestTelegramQueueDoesNotBlockAndCloseCancelsRequest(t *testing.T) {
	started := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	n := newTelegram(context.Background(), testTelegramConfig, "USDC", "https://api.telegram.org", client)
	defer n.Close()
	n.NotifyFill(testFill())
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	queued := make(chan struct{})
	go func() {
		for i := 0; i < telegramQueueSize+10; i++ {
			n.NotifyFill(testFill())
		}
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("full queue blocked caller")
	}
	if len(n.queue) != telegramQueueSize || n.dropped.Load() != 10 {
		t.Fatalf("queue=%d dropped=%d", len(n.queue), n.dropped.Load())
	}
	closed := make(chan struct{})
	go func() { n.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel in-flight request")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Go(func() { n.NotifyFill(testFill()); n.Close() })
	}
	wg.Wait()
}

func TestTelegramRetriesOnlyExplicitRateLimit(t *testing.T) {
	var calls atomic.Int64
	delivered := make(chan time.Time, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ok":false,"parameters":{"retry_after":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
		delivered <- time.Now()
	}))
	defer server.Close()
	n := newTelegram(context.Background(), testTelegramConfig, "USDC", server.URL, server.Client())
	defer n.Close()
	started := time.Now()
	n.NotifyFill(testFill())
	select {
	case at := <-delivered:
		if at.Sub(started) < time.Second || calls.Load() != 2 {
			t.Fatalf("retry did not honor Telegram rate limit: calls=%d", calls.Load())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("rate limited notification was not retried")
	}
}

func TestTelegramDisabled(t *testing.T) {
	n := NewTelegram(context.Background(), config.TelegramConfig{}, "USDT")
	if n != nil {
		t.Fatal("disabled notifier should not start")
	}
	n.NotifyFill(testFill())
	n.Close()
}
