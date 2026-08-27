package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"opensqt/exchange"
)

func TestShouldCancelOrdersOnShutdown(t *testing.T) {
	tests := []struct {
		name                 string
		cancelOnExit         bool
		marginLimitTriggered bool
		want                 bool
	}{
		{name: "ordinary exit keeps orders when disabled"},
		{name: "ordinary exit cancels when enabled", cancelOnExit: true, want: true},
		{name: "margin limit overrides disabled exit cancellation", marginLimitTriggered: true, want: true},
		{name: "both reasons cancel", cancelOnExit: true, marginLimitTriggered: true, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldCancelOrdersOnShutdown(tc.cancelOnExit, tc.marginLimitTriggered); got != tc.want {
				t.Fatalf("shouldCancelOrdersOnShutdown(%v, %v) = %v, want %v",
					tc.cancelOnExit, tc.marginLimitTriggered, got, tc.want)
			}
		})
	}
}

type shutdownMarginMonitorFake struct {
	calls           []string
	triggered       bool
	triggerWhenStop bool
}

func (m *shutdownMarginMonitorFake) Stop() {
	m.calls = append(m.calls, "stop")
	if m.triggerWhenStop {
		m.triggered = true
	}
}

func (m *shutdownMarginMonitorFake) IsTriggered() bool {
	m.calls = append(m.calls, "read")
	return m.triggered
}

func TestStopMarginMonitorAndReadTriggeredFreezesBeforeRead(t *testing.T) {
	monitor := &shutdownMarginMonitorFake{triggerWhenStop: true}
	if !stopMarginMonitorAndReadTriggered(monitor) {
		t.Fatal("trigger completed during Stop was missed")
	}
	wantCalls := []string{"stop", "read"}
	if got := fmt.Sprint(monitor.calls); got != fmt.Sprint(wantCalls) {
		t.Fatalf("calls = %v, want %v", monitor.calls, wantCalls)
	}
}

func TestConsumePendingShutdownSignal(t *testing.T) {
	signals := make(chan os.Signal, 1)
	if consumePendingShutdownSignal(signals) {
		t.Fatal("empty signal channel reported a pending shutdown")
	}
	signals <- syscall.SIGTERM
	if !consumePendingShutdownSignal(signals) {
		t.Fatal("queued SIGTERM was not consumed before gate startup")
	}
}

type fakeShutdownAuditClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *fakeShutdownAuditClock) Now() time.Time { return c.now }

func (c *fakeShutdownAuditClock) Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay < 0 {
		return fmt.Errorf("negative wait: %s", delay)
	}
	c.waits = append(c.waits, delay)
	c.now = c.now.Add(delay)
	return nil
}

type shutdownScopedCancelFake struct {
	remote *cancelConfirmExchangeFake

	mu      sync.Mutex
	symbols []string
}

func (e *shutdownScopedCancelFake) CancelAllOrders(ctx context.Context, symbol string) error {
	e.recordSymbol(symbol)
	return e.remote.CancelAllOrders(ctx, symbol)
}

func (e *shutdownScopedCancelFake) GetOpenOrders(ctx context.Context, symbol string) ([]*exchange.Order, error) {
	e.recordSymbol(symbol)
	return e.remote.GetOpenOrders(ctx, symbol)
}

func (e *shutdownScopedCancelFake) recordSymbol(symbol string) {
	e.mu.Lock()
	e.symbols = append(e.symbols, symbol)
	e.mu.Unlock()
}

func (e *shutdownScopedCancelFake) assertOnlySymbol(t *testing.T, want string) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, symbol := range e.symbols {
		if symbol != want {
			t.Fatalf("symbol call %d = %q, want %q", i, symbol, want)
		}
	}
}

func TestCleanupMarginOrdersOnShutdownUsesFixedWindowAndQueryFirst(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	clock := &fakeShutdownAuditClock{now: start}
	remote := &cancelConfirmExchangeFake{openOrders: [][]*exchange.Order{nil}}
	ex := &shutdownScopedCancelFake{remote: remote}
	deadline := start.Add(marginAuditWindow)

	if err := cleanupMarginOrdersOnShutdownUntil(
		context.Background(), ex, "BTCUSDT", clock, deadline,
	); err != nil {
		t.Fatalf("cleanupMarginOrdersOnShutdownUntil() error = %v", err)
	}
	if !clock.now.Equal(deadline) {
		t.Fatalf("clock stopped at %s, want absolute deadline %s", clock.now, deadline)
	}
	cancelCalls, queryCalls := remote.counts()
	if cancelCalls != marginCancelStableReads || queryCalls != marginCancelStableReads+7 {
		t.Fatalf("remote calls = cancel:%d query:%d, want %d/%d",
			cancelCalls, queryCalls, marginCancelStableReads, marginCancelStableReads+7)
	}
	ex.assertOnlySymbol(t, "BTCUSDT")
}

func TestCleanupMarginOrdersOnShutdownCancelsLateOrderWithoutExtendingDeadline(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	clock := &fakeShutdownAuditClock{now: start}
	remote := &cancelConfirmExchangeFake{
		openOrders: [][]*exchange.Order{
			nil,
			nil,
			nil,
			nil,
			{{OrderID: 9}},
			nil,
		},
	}
	ex := &shutdownScopedCancelFake{remote: remote}
	deadline := start.Add(marginAuditWindow)

	if err := cleanupMarginOrdersOnShutdownUntil(
		context.Background(), ex, "BTCUSDT", clock, deadline,
	); err != nil {
		t.Fatalf("cleanupMarginOrdersOnShutdownUntil() error = %v", err)
	}
	if !clock.now.Equal(deadline) {
		t.Fatalf("late order extended deadline: stopped at %s, want %s", clock.now, deadline)
	}
	cancelCalls, queryCalls := remote.counts()
	if cancelCalls != marginCancelStableReads+1 || queryCalls != marginCancelStableReads+8 {
		t.Fatalf("remote calls = cancel:%d query:%d, want %d/%d",
			cancelCalls, queryCalls, marginCancelStableReads+1, marginCancelStableReads+8)
	}
	ex.assertOnlySymbol(t, "BTCUSDT")
}

func TestCleanupMarginOrdersOnShutdownReportsUnrecoveredFinalAuditError(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	clock := &fakeShutdownAuditClock{now: start}
	queryErr := errors.New("query unavailable")
	queryErrors := make([]error, marginCancelStableReads+7)
	queryErrors[len(queryErrors)-1] = queryErr
	remote := &cancelConfirmExchangeFake{
		queryErrors: queryErrors,
		openOrders:  [][]*exchange.Order{nil},
	}
	ex := &shutdownScopedCancelFake{remote: remote}

	err := cleanupMarginOrdersOnShutdownUntil(
		context.Background(), ex, "BTCUSDT", clock, start.Add(marginAuditWindow),
	)
	if !errors.Is(err, queryErr) {
		t.Fatalf("cleanup error = %v, want final query error", err)
	}
}
