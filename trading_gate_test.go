package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"opensqt/exchange"
	"opensqt/safety"
)

func TestTradingGateHealthCanTradeTruthTable(t *testing.T) {
	for mask := 0; mask < 32; mask++ {
		health := tradingGateHealth{
			OrderStreamReady: mask&(1<<0) != 0,
			RiskReady:        mask&(1<<1) != 0,
			RiskTriggered:    mask&(1<<2) != 0,
			ReconcilerReady:  mask&(1<<3) != 0,
			PriceFresh:       mask&(1<<4) != 0,
		}
		want := health.OrderStreamReady && health.RiskReady && !health.RiskTriggered &&
			health.ReconcilerReady && health.PriceFresh
		t.Run(fmt.Sprintf("mask_%05b", mask), func(t *testing.T) {
			if got := health.CanTrade(); got != want {
				t.Fatalf("CanTrade() = %v, want %v, health=%+v", got, want, health)
			}
		})
	}
}

func TestTradingGateHealthReasons(t *testing.T) {
	health := tradingGateHealth{
		OrderStreamState: "DEGRADED",
		RiskTriggered:    true,
	}
	want := []string{"订单流=DEGRADED", "风控未就绪", "风控已触发", "对账不健康", "价格流陈旧"}
	if got := health.Reasons(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Reasons() = %v, want %v", got, want)
	}
}

func TestPriceIsFresh(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name       string
		last       time.Time
		now        time.Time
		staleAfter time.Duration
		want       bool
	}{
		{name: "fresh", last: now.Add(-time.Second), now: now, staleAfter: 5 * time.Second, want: true},
		{name: "exact boundary", last: now.Add(-5 * time.Second), now: now, staleAfter: 5 * time.Second, want: true},
		{name: "stale", last: now.Add(-5*time.Second - time.Nanosecond), now: now, staleAfter: 5 * time.Second},
		{name: "future timestamp", last: now.Add(time.Nanosecond), now: now, staleAfter: 5 * time.Second},
		{name: "zero last", now: now, staleAfter: 5 * time.Second},
		{name: "zero now", last: now, staleAfter: 5 * time.Second},
		{name: "default threshold fresh", last: now.Add(-minimumPriceStaleAfter), now: now, want: true},
		{name: "default threshold stale", last: now.Add(-minimumPriceStaleAfter - time.Nanosecond), now: now},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := priceIsFresh(tt.last, tt.now, tt.staleAfter); got != tt.want {
				t.Fatalf("priceIsFresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfiguredPriceStaleAfter(t *testing.T) {
	tests := []struct {
		intervalMS int
		want       time.Duration
	}{
		{intervalMS: -1, want: minimumPriceStaleAfter},
		{intervalMS: 0, want: minimumPriceStaleAfter},
		{intervalMS: 50, want: minimumPriceStaleAfter},
		{intervalMS: 1_500, want: minimumPriceStaleAfter},
		{intervalMS: 2_000, want: 40 * time.Second},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%dms", tt.intervalMS), func(t *testing.T) {
			if got := configuredPriceStaleAfter(tt.intervalMS); got != tt.want {
				t.Fatalf("configuredPriceStaleAfter(%d) = %s, want %s", tt.intervalMS, got, tt.want)
			}
		})
	}
}

type namedOnlyExchange struct {
	name string
}

func (e *namedOnlyExchange) GetName() string { return e.name }

type healthAwareExchange struct {
	namedOnlyExchange
	ready bool
	state string
}

func (e *healthAwareExchange) IsOrderStreamReady() bool    { return e.ready }
func (e *healthAwareExchange) GetOrderStreamState() string { return e.state }

func TestCurrentOrderStreamHealth(t *testing.T) {
	tests := []struct {
		name      string
		exchange  namedExchange
		wantReady bool
		wantState string
	}{
		{
			name:      "Binance without provider fails closed",
			exchange:  &namedOnlyExchange{name: "binance"},
			wantState: "HEALTH_UNAVAILABLE",
		},
		{
			name:      "non-Binance without provider fails closed",
			exchange:  &namedOnlyExchange{name: "Bitget"},
			wantState: "HEALTH_UNAVAILABLE",
		},
		{
			name: "provider ready",
			exchange: &healthAwareExchange{
				namedOnlyExchange: namedOnlyExchange{name: "Binance"},
				ready:             true,
				state:             "READY",
			},
			wantReady: true,
			wantState: "READY",
		},
		{
			name: "provider degraded",
			exchange: &healthAwareExchange{
				namedOnlyExchange: namedOnlyExchange{name: "Binance"},
				state:             "DEGRADED",
			},
			wantState: "DEGRADED",
		},
		{
			name: "ready boolean cannot override degraded state",
			exchange: &healthAwareExchange{
				namedOnlyExchange: namedOnlyExchange{name: "Binance"},
				ready:             true,
				state:             "  degraded  ",
			},
			wantState: "DEGRADED",
		},
		{
			name: "ready state cannot override false boolean",
			exchange: &healthAwareExchange{
				namedOnlyExchange: namedOnlyExchange{name: "Binance"},
				state:             "ready",
			},
			wantState: "READY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, state := currentOrderStreamHealth(tt.exchange)
			if ready != tt.wantReady || state != tt.wantState {
				t.Fatalf("currentOrderStreamHealth() = (%v, %q), want (%v, %q)", ready, state, tt.wantReady, tt.wantState)
			}
		})
	}
}

type recordingNewOrderGate struct {
	mu          sync.Mutex
	enableCalls int
	stopCalls   int
	healthGuard func() error
}

func (g *recordingNewOrderGate) StopNewOrders() {
	g.mu.Lock()
	g.stopCalls++
	g.mu.Unlock()
}

func (g *recordingNewOrderGate) EnableNewOrders() error {
	g.mu.Lock()
	g.enableCalls++
	g.mu.Unlock()
	return nil
}

func (g *recordingNewOrderGate) SetSubmissionHealthGuard(guard func() error) {
	g.mu.Lock()
	g.healthGuard = guard
	g.mu.Unlock()
}

func (g *recordingNewOrderGate) counts() (enable, stop int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enableCalls, g.stopCalls
}

func (g *recordingNewOrderGate) hasHealthGuard() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.healthGuard != nil
}

func TestNewTradingGateRuntimeInstallsSubmissionHealthGuard(t *testing.T) {
	executor := &recordingNewOrderGate{}
	gate := newSerializedOrderGate(executor)
	_ = newTradingGateRuntime(
		gate,
		nil,
		nil,
		nil,
		&safety.Reconciler{},
		nil,
		time.Second,
		time.Second,
	)
	if !executor.hasHealthGuard() {
		t.Fatal("trading gate runtime did not install the final submission health guard")
	}
}

func TestSubmissionHealthErrorFailsClosedForEveryCondition(t *testing.T) {
	healthy := tradingGateHealth{
		OrderStreamReady: true,
		OrderStreamState: "READY",
		RiskReady:        true,
		ReconcilerReady:  true,
		PriceFresh:       true,
	}
	if err := submissionHealthError(healthy, false); err != nil {
		t.Fatalf("healthy submission guard error = %v", err)
	}

	tests := []struct {
		name           string
		mutate         func(*tradingGateHealth)
		needsReconcile bool
		want           string
	}{
		{name: "order stream", mutate: func(h *tradingGateHealth) { h.OrderStreamReady = false }, want: "订单流"},
		{name: "risk not ready", mutate: func(h *tradingGateHealth) { h.RiskReady = false }, want: "风控未就绪"},
		{name: "risk triggered", mutate: func(h *tradingGateHealth) { h.RiskTriggered = true }, want: "风控已触发"},
		{name: "reconciler", mutate: func(h *tradingGateHealth) { h.ReconcilerReady = false }, want: "对账不健康"},
		{name: "price", mutate: func(h *tradingGateHealth) { h.PriceFresh = false }, want: "价格流陈旧"},
		{name: "recovery reconcile", mutate: func(*tradingGateHealth) {}, needsReconcile: true, want: "等待恢复对账"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			health := healthy
			tt.mutate(&health)
			err := submissionHealthError(health, tt.needsReconcile)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("submissionHealthError() = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestSubmissionHealthGuardSynchronouslyEntersRecoveryState(t *testing.T) {
	executor := &recordingNewOrderGate{}
	gate := newSerializedOrderGate(executor)
	if _, err := gate.Enable(); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	reconciler := &safety.Reconciler{}
	runtime := &tradingGateRuntime{
		gate:       gate,
		reconciler: reconciler,
		wake:       make(chan struct{}, 1),
	}
	reconciler.SetHealthHandler(runtime.handleReconcileHealth)

	health := tradingGateHealth{
		OrderStreamState: "DEGRADED",
		RiskReady:        true,
		ReconcilerReady:  true,
		PriceFresh:       true,
	}
	err := runtime.enforceSubmissionHealth(health, false)
	if err == nil || !strings.Contains(err.Error(), "订单流=DEGRADED") {
		t.Fatalf("enforceSubmissionHealth() error = %v, want degraded order stream", err)
	}
	if gate.Enabled() {
		t.Fatal("serialized gate remained enabled after submission guard failure")
	}
	if !runtime.withdrawRequired.Load() || !runtime.needsReconcile.Load() {
		t.Fatalf("recovery flags = withdraw:%v reconcile:%v, want both true",
			runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
	}
	if reconciler.IsHealthy() {
		t.Fatal("reconciler remained healthy after submission guard invalidation")
	}
	enableCalls, stopCalls := executor.counts()
	if enableCalls != 1 || stopCalls != 1 {
		t.Fatalf("executor calls = enable:%d stop:%d, want enable:1 stop:1", enableCalls, stopCalls)
	}

	// 即使外部健康条件立即恢复，待恢复对账标志仍必须阻止放行。
	recovered := tradingGateHealth{
		OrderStreamReady: true,
		OrderStreamState: "READY",
		RiskReady:        true,
		ReconcilerReady:  true,
		PriceFresh:       true,
	}
	err = runtime.enforceSubmissionHealth(recovered, runtime.needsReconcile.Load())
	if err == nil || !strings.Contains(err.Error(), "等待恢复对账") {
		t.Fatalf("instant recovery guard error = %v, want forced reconciliation", err)
	}
	if gate.Enabled() || !runtime.withdrawRequired.Load() || !runtime.needsReconcile.Load() {
		t.Fatalf("instant recovery changed fail-closed state: enabled=%v withdraw=%v reconcile=%v",
			gate.Enabled(), runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
	}
	_, stopCalls = executor.counts()
	if stopCalls != 1 {
		t.Fatalf("already-disabled gate stopped executor %d times, want 1", stopCalls)
	}
}

func TestSerializedOrderGateCannotReenableAfterShutdown(t *testing.T) {
	executor := &recordingNewOrderGate{}
	gate := newSerializedOrderGate(executor)

	generation, err := gate.Enable()
	if err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	if generation == 0 || !gate.Enabled() {
		t.Fatalf("gate did not become enabled: generation=%d", generation)
	}

	gate.BeginShutdown()
	gate.BeginShutdown()
	if gate.Enabled() {
		t.Fatal("gate remained enabled after shutdown")
	}
	if _, err := gate.Enable(); !errors.Is(err, errTradingGateShuttingDown) {
		t.Fatalf("Enable() after shutdown error = %v, want %v", err, errTradingGateShuttingDown)
	}
	if gate.StillEnabled(generation) {
		t.Fatal("old generation remained enabled after shutdown")
	}

	enableCalls, stopCalls := executor.counts()
	if enableCalls != 1 || stopCalls != 1 {
		t.Fatalf("executor calls = enable:%d stop:%d, want enable:1 stop:1", enableCalls, stopCalls)
	}
}

type cancelConfirmExchangeFake struct {
	mu sync.Mutex

	cancelCalls int
	queryCalls  int

	cancelErrors []error
	queryErrors  []error
	openOrders   [][]*exchange.Order
}

func (e *cancelConfirmExchangeFake) CancelAllOrders(context.Context, string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	index := e.cancelCalls
	e.cancelCalls++
	if index < len(e.cancelErrors) {
		return e.cancelErrors[index]
	}
	return nil
}

func (e *cancelConfirmExchangeFake) GetOpenOrders(context.Context, string) ([]*exchange.Order, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	index := e.queryCalls
	e.queryCalls++
	if index < len(e.queryErrors) && e.queryErrors[index] != nil {
		return nil, e.queryErrors[index]
	}
	if len(e.openOrders) == 0 {
		return nil, nil
	}
	if index >= len(e.openOrders) {
		index = len(e.openOrders) - 1
	}
	return e.openOrders[index], nil
}

func (e *cancelConfirmExchangeFake) counts() (cancel, query int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cancelCalls, e.queryCalls
}

func TestCancelAllOrdersAndConfirmRetriesUntilRemoteEmpty(t *testing.T) {
	cancelErr := errors.New("temporary cancel error")
	ex := &cancelConfirmExchangeFake{
		cancelErrors: []error{cancelErr, nil},
		openOrders: [][]*exchange.Order{
			{{OrderID: 1}},
			nil,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := cancelAllOrdersAndConfirm(ctx, ex, "BTCUSDT"); err != nil {
		t.Fatalf("cancelAllOrdersAndConfirm() error = %v", err)
	}
	cancelCalls, queryCalls := ex.counts()
	if cancelCalls != 2 || queryCalls != 2 {
		t.Fatalf("calls = cancel:%d query:%d, want cancel:2 query:2", cancelCalls, queryCalls)
	}
}

func TestCancelAllOrdersAndConfirmTimeoutIncludesLastErrors(t *testing.T) {
	cancelErr := errors.New("cancel unavailable")
	queryErr := errors.New("query unavailable")
	ex := &cancelConfirmExchangeFake{
		cancelErrors: []error{cancelErr},
		queryErrors:  []error{queryErr},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := cancelAllOrdersAndConfirm(ctx, ex, "BTCUSDT")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
	if !errors.Is(err, cancelErr) || !errors.Is(err, queryErr) {
		t.Fatalf("error = %v, want joined cancel and query errors", err)
	}
}

func TestCancelAllOrdersAndConfirmTimeoutReportsRemainingOrders(t *testing.T) {
	ex := &cancelConfirmExchangeFake{
		openOrders: [][]*exchange.Order{{{OrderID: 7}}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := cancelAllOrdersAndConfirm(ctx, ex, "BTCUSDT")
	if err == nil || !strings.Contains(err.Error(), "剩余挂单=1") {
		t.Fatalf("error = %v, want remaining order count", err)
	}
}

func TestCancelAllOrdersAndConfirmRejectsInvalidDependencies(t *testing.T) {
	if err := cancelAllOrdersAndConfirm(nil, &cancelConfirmExchangeFake{}, "BTCUSDT"); err == nil {
		t.Fatal("nil context did not fail")
	}
	if err := cancelAllOrdersAndConfirm(context.Background(), nil, "BTCUSDT"); err == nil {
		t.Fatal("nil exchange did not fail")
	}
}

type failingTradingPosition struct {
	calls int
	err   error
}

func (p *failingTradingPosition) CancelAllBuyOrders() error {
	p.calls++
	return p.err
}

func (p *failingTradingPosition) AdjustOrders(float64) error { return nil }
func (p *failingTradingPosition) GetSymbol() string          { return "BTCUSDT" }

func TestTradingGateKeepsWithdrawalRequiredWhenRemoteCancelIsUncertain(t *testing.T) {
	cancelErr := errors.New("remote cancel uncertain")
	positionManager := &failingTradingPosition{err: cancelErr}
	runtime := &tradingGateRuntime{
		position:   positionManager,
		reconciler: &safety.Reconciler{},
	}
	runtime.withdrawRequired.Store(true)
	runtime.needsReconcile.Store(true)

	err := runtime.evaluate(context.Background(), false)
	if !errors.Is(err, cancelErr) {
		t.Fatalf("evaluate() error = %v, want cancel error", err)
	}
	if positionManager.calls != 1 {
		t.Fatalf("cancel calls = %d, want 1", positionManager.calls)
	}
	if !runtime.withdrawRequired.Load() {
		t.Fatal("withdrawRequired was cleared before remote confirmation")
	}
	if runtime.reconciler.IsHealthy() {
		t.Fatal("reconciler became healthy after uncertain cancellation")
	}
}
