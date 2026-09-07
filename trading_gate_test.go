package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/monitor"
	"opensqt/order"
	"opensqt/safety"
)

func TestTradingGateHealthCanTradeTruthTable(t *testing.T) {
	for mask := 0; mask < 128; mask++ {
		health := tradingGateHealth{
			OrderStreamReady: mask&(1<<0) != 0,
			RiskReady:        mask&(1<<1) != 0,
			RiskTriggered:    mask&(1<<2) != 0,
			MarginReady:      mask&(1<<3) != 0,
			MarginTriggered:  mask&(1<<4) != 0,
			ReconcilerReady:  mask&(1<<5) != 0,
			PriceFresh:       mask&(1<<6) != 0,
		}
		want := health.OrderStreamReady && health.RiskReady && !health.RiskTriggered &&
			health.MarginReady && !health.MarginTriggered && health.ReconcilerReady && health.PriceFresh
		t.Run(fmt.Sprintf("mask_%07b", mask), func(t *testing.T) {
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
	want := []string{"订单流=DEGRADED", "风控未就绪", "风控已触发", "保证金守卫未就绪", "对账不健康", "价格流陈旧"}
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
		MarginReady:      true,
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
		{name: "margin not ready", mutate: func(h *tradingGateHealth) { h.MarginReady = false }, want: "保证金守卫未就绪"},
		{name: "margin triggered", mutate: func(h *tradingGateHealth) { h.MarginTriggered = true }, want: "保证金限制已触发"},
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
		MarginReady:      true,
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
		MarginReady:      true,
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

func TestCancelAllOrdersAndConfirmStableResetsEmptySequenceAfterQueryError(t *testing.T) {
	ex := &cancelConfirmExchangeFake{
		queryErrors: []error{nil, errors.New("temporary query error")},
		openOrders:  [][]*exchange.Order{nil, nil, nil, nil, nil},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := cancelAllOrdersAndConfirmStable(ctx, ex, "BTCUSDT", 3); err != nil {
		t.Fatalf("cancelAllOrdersAndConfirmStable() error = %v", err)
	}
	cancelCalls, queryCalls := ex.counts()
	if cancelCalls != 5 || queryCalls != 5 {
		t.Fatalf("calls = cancel:%d query:%d, want 5/5 after stability reset", cancelCalls, queryCalls)
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

type marginCancelRuntimeExchange struct {
	exchange.IExchange
	remote *cancelConfirmExchangeFake
}

func (e *marginCancelRuntimeExchange) CancelAllOrders(ctx context.Context, symbol string) error {
	return e.remote.CancelAllOrders(ctx, symbol)
}

func (e *marginCancelRuntimeExchange) GetOpenOrders(ctx context.Context, symbol string) ([]*exchange.Order, error) {
	return e.remote.GetOpenOrders(ctx, symbol)
}

func (e *marginCancelRuntimeExchange) IsOrderStreamReady() bool {
	provider, ok := e.IExchange.(exchange.OrderStreamHealthProvider)
	return ok && provider.IsOrderStreamReady()
}

func (e *marginCancelRuntimeExchange) GetOrderStreamState() string {
	provider, ok := e.IExchange.(exchange.OrderStreamHealthProvider)
	if !ok {
		return "HEALTH_UNAVAILABLE"
	}
	return provider.GetOrderStreamState()
}

func TestMarginLimitEscalatesToFullCancelWhenGateAlreadyClosed(t *testing.T) {
	executor := &recordingNewOrderGate{}
	gate := newSerializedOrderGate(executor)
	runtime := &tradingGateRuntime{
		gate:       gate,
		reconciler: &safety.Reconciler{},
		wake:       make(chan struct{}, 1),
	}
	runtime.withdrawRequired.Store(true)

	runtime.handleMarginLimit(safety.MarginSnapshot{
		Triggered:    true,
		UsagePercent: 61,
		LimitPercent: 60,
	})

	if gate.Enabled() {
		t.Fatal("already-closed gate became enabled")
	}
	if !runtime.cancelAllRequired.Load() || runtime.withdrawRequired.Load() || !runtime.needsReconcile.Load() {
		t.Fatalf("withdrawal flags = all:%v buy:%v reconcile:%v",
			runtime.cancelAllRequired.Load(), runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
	}
	_, stopCalls := executor.counts()
	if stopCalls != 0 {
		t.Fatalf("already-closed gate stopped executor %d times, want 0", stopCalls)
	}
}

func TestMarginLimitStopsEnabledGateImmediately(t *testing.T) {
	executor := &recordingNewOrderGate{}
	gate := newSerializedOrderGate(executor)
	if _, err := gate.Enable(); err != nil {
		t.Fatal(err)
	}
	runtime := &tradingGateRuntime{
		gate:       gate,
		reconciler: &safety.Reconciler{},
		wake:       make(chan struct{}, 1),
	}

	runtime.handleMarginLimit(safety.MarginSnapshot{Triggered: true, UsagePercent: 61, LimitPercent: 60})
	if gate.Enabled() || !runtime.cancelAllRequired.Load() {
		t.Fatalf("margin trigger did not close and escalate: enabled=%v all=%v",
			gate.Enabled(), runtime.cancelAllRequired.Load())
	}
	_, stopCalls := executor.counts()
	if stopCalls != 1 {
		t.Fatalf("stop calls = %d, want 1", stopCalls)
	}
}

func TestMarginLimitClearsStaleFullCancelRequestAfterCompletion(t *testing.T) {
	runtime := &tradingGateRuntime{
		gate:       newSerializedOrderGate(&recordingNewOrderGate{}),
		reconciler: &safety.Reconciler{},
		wake:       make(chan struct{}, 1),
	}
	// 模拟旧 handler 在全撤完成后才落地 CAS 的过期状态。
	runtime.marginCancelDone.Store(true)
	runtime.cancelAllRequired.Store(true)

	runtime.handleMarginLimit(safety.MarginSnapshot{
		Triggered:    true,
		UsagePercent: 61,
		LimitPercent: 60,
	})

	if !runtime.marginCancelDone.Load() || runtime.cancelAllRequired.Load() {
		t.Fatalf("latched cancellation state = done:%v required:%v, want true/false",
			runtime.marginCancelDone.Load(), runtime.cancelAllRequired.Load())
	}
}

func TestProcessMarginFullCancelConfirmsRemoteEmptyAndLatches(t *testing.T) {
	remote := &cancelConfirmExchangeFake{
		openOrders: [][]*exchange.Order{
			{{OrderID: 1}, {OrderID: 2}},
			nil,
		},
	}
	runtime := &tradingGateRuntime{
		exchange:   &marginCancelRuntimeExchange{remote: remote},
		position:   &adjustmentTradingPosition{},
		reconciler: &safety.Reconciler{},
	}
	runtime.cancelAllRequired.Store(true)
	runtime.withdrawRequired.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runtime.processMarginFullCancel(ctx); err != nil {
		t.Fatalf("processMarginFullCancel() error = %v", err)
	}
	if runtime.cancelAllRequired.Load() || runtime.withdrawRequired.Load() || !runtime.marginCancelDone.Load() {
		t.Fatalf("final flags = all:%v buy:%v done:%v",
			runtime.cancelAllRequired.Load(), runtime.withdrawRequired.Load(), runtime.marginCancelDone.Load())
	}
	cancelCalls, queryCalls := remote.counts()
	if cancelCalls != 4 || queryCalls != 4 {
		t.Fatalf("remote calls = cancel:%d query:%d, want 4/4", cancelCalls, queryCalls)
	}

	// 锁存完成后重复健康观察不得重新武装全撤。
	runtime.handleMarginLimit(safety.MarginSnapshot{Triggered: true, UsagePercent: 0, LimitPercent: 60})
	if runtime.cancelAllRequired.Load() {
		t.Fatal("latched margin limit re-armed full cancellation")
	}
}

func TestProcessMarginFullCancelKeepsRequirementOnTimeout(t *testing.T) {
	remote := &cancelConfirmExchangeFake{
		openOrders: [][]*exchange.Order{{{OrderID: 7}}},
	}
	runtime := &tradingGateRuntime{
		exchange:   &marginCancelRuntimeExchange{remote: remote},
		position:   &adjustmentTradingPosition{},
		reconciler: &safety.Reconciler{},
	}
	runtime.cancelAllRequired.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := runtime.processMarginFullCancel(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("processMarginFullCancel() error = %v, want deadline", err)
	}
	if !runtime.cancelAllRequired.Load() || runtime.marginCancelDone.Load() {
		t.Fatalf("failed cancellation flags = all:%v done:%v",
			runtime.cancelAllRequired.Load(), runtime.marginCancelDone.Load())
	}
}

func TestMarginLateOrderAuditCancelsResidualAndExtendsWindow(t *testing.T) {
	remote := &cancelConfirmExchangeFake{
		openOrders: [][]*exchange.Order{
			{{OrderID: 9}},
			nil,
		},
	}
	now := time.Unix(1_800_000_000, 0)
	runtime := &tradingGateRuntime{
		exchange:         &marginCancelRuntimeExchange{remote: remote},
		position:         &adjustmentTradingPosition{},
		marginAuditUntil: now.Add(time.Second),
	}
	runtime.marginCancelDone.Store(true)

	if err := runtime.auditLateMarginOrders(context.Background(), now); err != nil {
		t.Fatalf("auditLateMarginOrders() error = %v", err)
	}
	cancelCalls, queryCalls := remote.counts()
	if cancelCalls != 1 || queryCalls != 2 {
		t.Fatalf("remote calls = cancel:%d query:%d, want 1/2", cancelCalls, queryCalls)
	}
	if want := now.Add(marginAuditWindow); !runtime.marginAuditUntil.Equal(want) {
		t.Fatalf("audit until = %s, want %s", runtime.marginAuditUntil, want)
	}
}

func TestMarginLateOrderAuditQueriesOnlyWhenDueAndSkipsEmptyCancel(t *testing.T) {
	remote := &cancelConfirmExchangeFake{}
	now := time.Unix(1_800_000_000, 0)
	runtime := &tradingGateRuntime{
		exchange:         &marginCancelRuntimeExchange{remote: remote},
		position:         &adjustmentTradingPosition{},
		marginAuditUntil: now.Add(marginAuditWindow),
	}
	runtime.marginCancelDone.Store(true)

	if err := runtime.auditLateMarginOrders(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if err := runtime.auditLateMarginOrders(context.Background(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := runtime.auditLateMarginOrders(context.Background(), now.Add(marginAuditInterval)); err != nil {
		t.Fatal(err)
	}
	cancelCalls, queryCalls := remote.counts()
	if cancelCalls != 0 || queryCalls != 2 {
		t.Fatalf("remote calls = cancel:%d query:%d, want 0/2", cancelCalls, queryCalls)
	}
}

func TestMarginLateOrderAuditStopsAfterWindow(t *testing.T) {
	remote := &cancelConfirmExchangeFake{}
	now := time.Unix(1_800_000_000, 0)
	runtime := &tradingGateRuntime{
		exchange:         &marginCancelRuntimeExchange{remote: remote},
		position:         &adjustmentTradingPosition{},
		marginAuditUntil: now,
	}
	runtime.marginCancelDone.Store(true)

	if err := runtime.auditLateMarginOrders(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	cancelCalls, queryCalls := remote.counts()
	if cancelCalls != 0 || queryCalls != 0 {
		t.Fatalf("expired audit made remote calls = cancel:%d query:%d", cancelCalls, queryCalls)
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

type healthyTradingGateExchange struct {
	exchange.IExchange
}

func (e *healthyTradingGateExchange) GetName() string { return "test" }

func (e *healthyTradingGateExchange) IsOrderStreamReady() bool { return true }

func (e *healthyTradingGateExchange) GetOrderStreamState() string { return "READY" }

func (e *healthyTradingGateExchange) StartPriceStream(
	_ context.Context,
	_ string,
	callback func(float64),
) error {
	callback(100)
	return nil
}

type emptyTradingGateReconcileExchange struct{}

func (e *emptyTradingGateReconcileExchange) GetPositions(context.Context, string) (interface{}, error) {
	return nil, nil
}

func (e *emptyTradingGateReconcileExchange) GetOpenOrders(context.Context, string) (interface{}, error) {
	return nil, nil
}

func (e *emptyTradingGateReconcileExchange) GetBaseAsset() string { return "BTC" }

type emptyTradingGateReconcilePosition struct {
	reconcileCount int64
}

func (p *emptyTradingGateReconcilePosition) IterateSlots(func(float64, safety.SlotInfo) bool) {}
func (p *emptyTradingGateReconcilePosition) GetTotalBuyQty() float64                          { return 0 }
func (p *emptyTradingGateReconcilePosition) GetTotalSellQty() float64                         { return 0 }
func (p *emptyTradingGateReconcilePosition) GetReconcileCount() int64                         { return p.reconcileCount }
func (p *emptyTradingGateReconcilePosition) IncrementReconcileCount()                         { p.reconcileCount++ }
func (p *emptyTradingGateReconcilePosition) UpdateLastReconcileTime(time.Time)                {}
func (p *emptyTradingGateReconcilePosition) GetSymbol() string                                { return "BTCUSDT" }
func (p *emptyTradingGateReconcilePosition) GetPriceInterval() float64                        { return 1 }

type adjustmentTradingPosition struct {
	adjustErr   error
	adjustCalls int
	cancelCalls int
}

func (p *adjustmentTradingPosition) CancelAllBuyOrders() error {
	p.cancelCalls++
	return nil
}

func (p *adjustmentTradingPosition) AdjustOrders(float64) error {
	p.adjustCalls++
	return p.adjustErr
}

func (p *adjustmentTradingPosition) GetSymbol() string { return "BTCUSDT" }

type adjustmentNotifierTradingPosition struct {
	adjustmentTradingPosition
	notifier func(time.Duration)
}

func (p *adjustmentNotifierTradingPosition) SetAdjustmentNotifier(notifier func(time.Duration)) {
	p.notifier = notifier
}

type staticMarginGateMonitor struct {
	mu        sync.RWMutex
	ready     bool
	triggered bool
	snapshot  safety.MarginSnapshot
}

func (m *staticMarginGateMonitor) IsReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
}

func (m *staticMarginGateMonitor) IsTriggered() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.triggered
}

func (m *staticMarginGateMonitor) Snapshot() safety.MarginSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snap := m.snapshot
	snap.Ready = m.ready
	snap.Triggered = m.triggered
	return snap
}

func (m *staticMarginGateMonitor) setUsagePercent(usage float64) {
	m.mu.Lock()
	m.snapshot.UsagePercent = usage
	m.mu.Unlock()
}

type concurrentTradingPosition struct {
	mu          sync.Mutex
	adjustCalls int
	cancelCalls int
}

func (p *concurrentTradingPosition) CancelAllBuyOrders() error {
	p.mu.Lock()
	p.cancelCalls++
	p.mu.Unlock()
	return nil
}

func (p *concurrentTradingPosition) AdjustOrders(float64) error {
	p.mu.Lock()
	p.adjustCalls++
	p.mu.Unlock()
	return nil
}

func (p *concurrentTradingPosition) GetSymbol() string { return "BTCUSDT" }

func (p *concurrentTradingPosition) counts() (adjust, cancel int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.adjustCalls, p.cancelCalls
}

type observedAdjustCall struct {
	number int
	at     time.Time
}

type observedTradingPosition struct {
	mu                sync.Mutex
	adjustCalls       int
	activeAdjusts     int
	maxActive         int
	blockCall         int
	release           <-chan struct{}
	onAdjust          func(int)
	calls             chan observedAdjustCall
	skipUnchangedGrid bool
}

func (p *observedTradingPosition) ShouldSkipUnchangedGrid(float64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.skipUnchangedGrid
}

func newObservedTradingPosition() *observedTradingPosition {
	return &observedTradingPosition{calls: make(chan observedAdjustCall, 32)}
}

func (p *observedTradingPosition) CancelAllBuyOrders() error { return nil }

func (p *observedTradingPosition) AdjustOrders(float64) error {
	p.mu.Lock()
	p.adjustCalls++
	callNumber := p.adjustCalls
	p.activeAdjusts++
	if p.activeAdjusts > p.maxActive {
		p.maxActive = p.activeAdjusts
	}
	block := callNumber == p.blockCall
	release := p.release
	onAdjust := p.onAdjust
	p.mu.Unlock()

	p.calls <- observedAdjustCall{number: callNumber, at: time.Now()}
	if onAdjust != nil {
		onAdjust(callNumber)
	}
	if block && release != nil {
		<-release
	}

	p.mu.Lock()
	p.activeAdjusts--
	p.mu.Unlock()
	return nil
}

func (p *observedTradingPosition) GetSymbol() string { return "BTCUSDT" }

func (p *observedTradingPosition) stats() (calls, maxActive int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.adjustCalls, p.maxActive
}

func waitObservedAdjustCall(t *testing.T, p *observedTradingPosition, want int, timeout time.Duration) observedAdjustCall {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case call := <-p.calls:
			if call.number == want {
				return call
			}
			if call.number > want {
				t.Fatalf("observed AdjustOrders call %d before expected call %d", call.number, want)
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for AdjustOrders call %d", want)
		}
	}
}

func snapshotAdjustRequests(runtime *tradingGateRuntime) (bool, []time.Time) {
	runtime.adjustMu.Lock()
	defer runtime.adjustMu.Unlock()
	return runtime.adjustImmediate, append([]time.Time(nil), runtime.adjustDeadlines...)
}

func newHealthyTradingGateTestRuntime(
	t *testing.T,
	positionManager tradingPositionManager,
) (*tradingGateRuntime, *serializedOrderGate, *recordingNewOrderGate) {
	t.Helper()

	ex := &healthyTradingGateExchange{}
	priceMonitor := monitor.NewPriceMonitor(ex, "BTCUSDT", 1_000)
	if err := priceMonitor.Start(); err != nil {
		t.Fatalf("price monitor Start() error = %v", err)
	}
	t.Cleanup(priceMonitor.Stop)

	cfg := &config.Config{}
	riskMonitor := safety.NewRiskMonitor(cfg, ex)
	if err := riskMonitor.Start(context.Background()); err != nil {
		t.Fatalf("risk monitor Start() error = %v", err)
	}

	reconciler := safety.NewReconciler(
		cfg,
		&emptyTradingGateReconcileExchange{},
		&emptyTradingGateReconcilePosition{},
	)
	reconciler.SetPauseChecker(func() bool { return true })
	if err := reconciler.Reconcile(); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}

	executor := &recordingNewOrderGate{}
	gate := newSerializedOrderGate(executor)
	runtime := newTradingGateRuntime(
		gate,
		ex,
		priceMonitor,
		riskMonitor,
		&staticMarginGateMonitor{ready: true},
		reconciler,
		positionManager,
		minimumPriceStaleAfter,
		time.Minute,
	)
	return runtime, gate, executor
}

func TestTradingGateStartContinuesDormantAfterInitialMarginFullCancel(t *testing.T) {
	positionManager := &concurrentTradingPosition{}
	runtime, gate, executor := newHealthyTradingGateTestRuntime(t, positionManager)
	margin := &staticMarginGateMonitor{
		ready:     true,
		triggered: true,
		snapshot: safety.MarginSnapshot{
			UsagePercent: 61,
			LimitPercent: 60,
		},
	}
	remote := &cancelConfirmExchangeFake{openOrders: [][]*exchange.Order{nil}}
	runtime.margin = margin
	runtime.exchange = &marginCancelRuntimeExchange{
		IExchange: runtime.exchange,
		remote:    remote,
	}

	// 模拟 SetLimitHandler 对首轮已触发读数的同步补发。
	runtime.handleMarginLimit(margin.Snapshot())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runtime.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v, want dormant success", err)
	}
	defer runtime.Stop()

	if gate.Enabled() {
		t.Fatal("gate enabled after initial margin limit")
	}
	if !runtime.marginCancelDone.Load() || runtime.cancelAllRequired.Load() {
		t.Fatalf("margin cancellation flags = done:%v required:%v, want true/false",
			runtime.marginCancelDone.Load(), runtime.cancelAllRequired.Load())
	}
	select {
	case <-runtime.done:
		t.Fatal("runtime exited instead of continuing in dormant state")
	default:
	}
	if enableCalls, stopCalls := executor.counts(); enableCalls != 0 || stopCalls != 0 {
		t.Fatalf("executor calls = enable:%d stop:%d, want 0/0 before Stop", enableCalls, stopCalls)
	}
	if adjustCalls, cancelCalls := positionManager.counts(); adjustCalls != 0 || cancelCalls != 0 {
		t.Fatalf("position calls = adjust:%d cancel-buy:%d, want 0/0", adjustCalls, cancelCalls)
	}
	if cancelCalls, queryCalls := remote.counts(); cancelCalls != marginCancelStableReads || queryCalls != marginCancelStableReads {
		t.Fatalf("remote calls = cancel:%d query:%d, want %d/%d",
			cancelCalls, queryCalls, marginCancelStableReads, marginCancelStableReads)
	}

	// 全撤释放挂单保证金后，账户比例可以回落，但 Triggered 锁存仍必须阻止重新挂单。
	margin.setUsagePercent(10)
	runtime.signal()
	time.Sleep(2 * gateHealthPollInterval)
	if gate.Enabled() {
		t.Fatal("gate reopened after usage fell below the limit")
	}
	if enableCalls, _ := executor.counts(); enableCalls != 0 {
		t.Fatalf("EnableNewOrders calls = %d, want 0", enableCalls)
	}
	if adjustCalls, _ := positionManager.counts(); adjustCalls != 0 {
		t.Fatalf("AdjustOrders calls = %d, want 0", adjustCalls)
	}
}

func TestTradingGateStartFailsWhenInitialMarginFullCancelIsUnconfirmed(t *testing.T) {
	positionManager := &concurrentTradingPosition{}
	runtime, gate, executor := newHealthyTradingGateTestRuntime(t, positionManager)
	margin := &staticMarginGateMonitor{
		ready:     true,
		triggered: true,
		snapshot: safety.MarginSnapshot{
			UsagePercent: 61,
			LimitPercent: 60,
		},
	}
	remote := &cancelConfirmExchangeFake{
		openOrders: [][]*exchange.Order{
			{{OrderID: 7}},
			nil,
		},
	}
	runtime.margin = margin
	runtime.exchange = &marginCancelRuntimeExchange{
		IExchange: runtime.exchange,
		remote:    remote,
	}
	runtime.handleMarginLimit(margin.Snapshot())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := runtime.Start(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start() error = %v, want deadline from unconfirmed full cancellation", err)
	}
	if gate.Enabled() || runtime.marginCancelDone.Load() || !runtime.cancelAllRequired.Load() {
		t.Fatalf("failed start state = enabled:%v done:%v required:%v, want false/false/true",
			gate.Enabled(), runtime.marginCancelDone.Load(), runtime.cancelAllRequired.Load())
	}
	if enableCalls, _ := executor.counts(); enableCalls != 0 {
		t.Fatalf("EnableNewOrders calls = %d, want 0", enableCalls)
	}
	if adjustCalls, _ := positionManager.counts(); adjustCalls != 0 {
		t.Fatalf("AdjustOrders calls = %d, want 0", adjustCalls)
	}
}

func TestTradingGateStartStillFailsForOtherInitialUnhealthyState(t *testing.T) {
	positionManager := &concurrentTradingPosition{}
	runtime, gate, executor := newHealthyTradingGateTestRuntime(t, positionManager)
	runtime.margin = &staticMarginGateMonitor{}
	remote := &cancelConfirmExchangeFake{openOrders: [][]*exchange.Order{nil}}
	runtime.exchange = &marginCancelRuntimeExchange{
		IExchange: runtime.exchange,
		remote:    remote,
	}

	err := runtime.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "保证金守卫未就绪") {
		t.Fatalf("Start() error = %v, want initial margin readiness failure", err)
	}
	if gate.Enabled() {
		t.Fatal("gate enabled for an unrelated initial unhealthy state")
	}
	if enableCalls, stopCalls := executor.counts(); enableCalls != 0 || stopCalls != 1 {
		t.Fatalf("executor calls = enable:%d stop:%d, want 0/1 rollback", enableCalls, stopCalls)
	}
	if adjustCalls, _ := positionManager.counts(); adjustCalls != 0 {
		t.Fatalf("AdjustOrders calls = %d, want 0", adjustCalls)
	}
}

func definiteAdjustmentRejection() error {
	return fmt.Errorf("batch placement failed: %w", errors.Join(
		order.NewOrderRejectedError(order.OrderRejectionPostOnly, errors.New("maker would take")),
		order.NewOrderRejectedError(order.OrderRejectionMargin, errors.New("insufficient margin")),
	))
}

func reduceOnlySellMarginAdjustmentError() error {
	return fmt.Errorf("batch placement failed: %w", errors.Join(
		order.ErrReduceOnlySellMarginRejected,
		order.NewOrderRejectedError(order.OrderRejectionMargin, errors.New("insufficient margin")),
	))
}

func TestTradingGateInitialDefiniteRejectionKeepsGateEnabled(t *testing.T) {
	positionManager := &adjustmentTradingPosition{adjustErr: definiteAdjustmentRejection()}
	runtime, gate, executor := newHealthyTradingGateTestRuntime(t, positionManager)

	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("evaluate() error = %v, want nil for definite rejection", err)
	}
	if !gate.Enabled() {
		t.Fatal("gate was disabled by a definite initial order rejection")
	}
	if runtime.withdrawRequired.Load() || runtime.needsReconcile.Load() {
		t.Fatalf("definite rejection entered recovery: withdraw=%v reconcile=%v",
			runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
	}
	if !runtime.reconciler.IsHealthy() {
		t.Fatal("definite rejection invalidated the reconciler")
	}
	if positionManager.adjustCalls != 1 || positionManager.cancelCalls != 0 {
		t.Fatalf("position calls = adjust:%d cancel:%d, want adjust:1 cancel:0",
			positionManager.adjustCalls, positionManager.cancelCalls)
	}
	enableCalls, stopCalls := executor.counts()
	if enableCalls != 1 || stopCalls != 0 {
		t.Fatalf("executor calls = enable:%d stop:%d, want enable:1 stop:0", enableCalls, stopCalls)
	}
}

func TestTradingGateRealtimeDefiniteRejectionKeepsGateEnabled(t *testing.T) {
	positionManager := &adjustmentTradingPosition{adjustErr: definiteAdjustmentRejection()}
	runtime, gate, executor := newHealthyTradingGateTestRuntime(t, positionManager)
	if _, err := gate.Enable(); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	if err := runtime.evaluate(context.Background(), true); err != nil {
		t.Fatalf("evaluate() error = %v, want nil for definite rejection", err)
	}
	if !gate.Enabled() || runtime.withdrawRequired.Load() || runtime.needsReconcile.Load() {
		t.Fatalf("definite rejection changed gate state: enabled=%v withdraw=%v reconcile=%v",
			gate.Enabled(), runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
	}
	if !runtime.reconciler.IsHealthy() {
		t.Fatal("definite rejection invalidated the reconciler")
	}
	if positionManager.adjustCalls != 1 || positionManager.cancelCalls != 0 {
		t.Fatalf("position calls = adjust:%d cancel:%d, want adjust:1 cancel:0",
			positionManager.adjustCalls, positionManager.cancelCalls)
	}
	enableCalls, stopCalls := executor.counts()
	if enableCalls != 1 || stopCalls != 0 {
		t.Fatalf("executor calls = enable:%d stop:%d, want enable:1 stop:0", enableCalls, stopCalls)
	}
}

func TestTradingGateOrdinaryBuyMarginRejectionKeepsGateEnabled(t *testing.T) {
	marginErr := order.NewOrderRejectedError(
		order.OrderRejectionMargin,
		errors.New("insufficient margin"),
	)
	positionManager := &adjustmentTradingPosition{adjustErr: marginErr}
	runtime, gate, executor := newHealthyTradingGateTestRuntime(t, positionManager)
	if _, err := gate.Enable(); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	if err := runtime.evaluate(context.Background(), true); err != nil {
		t.Fatalf("evaluate() error = %v, want nil for ordinary BUY margin rejection", err)
	}
	if !gate.Enabled() || runtime.withdrawRequired.Load() || runtime.needsReconcile.Load() {
		t.Fatalf("ordinary BUY margin rejection changed gate state: enabled=%v withdraw=%v reconcile=%v",
			gate.Enabled(), runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
	}
	if !runtime.reconciler.IsHealthy() {
		t.Fatal("ordinary BUY margin rejection invalidated the reconciler")
	}
	if positionManager.adjustCalls != 1 || positionManager.cancelCalls != 0 {
		t.Fatalf("position calls = adjust:%d cancel:%d, want adjust:1 cancel:0",
			positionManager.adjustCalls, positionManager.cancelCalls)
	}
	if enableCalls, stopCalls := executor.counts(); enableCalls != 1 || stopCalls != 0 {
		t.Fatalf("executor calls = enable:%d stop:%d, want 1/0", enableCalls, stopCalls)
	}
}

func TestTradingGateReduceOnlySellMarginRejectionRecoversFailClosed(t *testing.T) {
	criticalErr := reduceOnlySellMarginAdjustmentError()
	if order.IsDefiniteOrderRejection(criticalErr) {
		t.Fatalf("critical SELL margin error = %v, want non-definite", criticalErr)
	}
	positionManager := &adjustmentTradingPosition{adjustErr: criticalErr}
	runtime, gate, executor := newHealthyTradingGateTestRuntime(t, positionManager)
	if _, err := gate.Enable(); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}

	err := runtime.evaluate(context.Background(), true)
	if !errors.Is(err, order.ErrReduceOnlySellMarginRejected) {
		t.Fatalf("evaluate() error = %v, want ErrReduceOnlySellMarginRejected", err)
	}
	if gate.Enabled() {
		t.Fatal("gate remained enabled after critical SELL margin rejection")
	}
	if !runtime.withdrawRequired.Load() || !runtime.needsReconcile.Load() {
		t.Fatalf("recovery flags = withdraw:%v reconcile:%v, want both true",
			runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
	}
	if runtime.reconciler.IsHealthy() {
		t.Fatal("critical SELL margin rejection did not invalidate reconciler")
	}
	if positionManager.adjustCalls != 1 || positionManager.cancelCalls != 0 {
		t.Fatalf("first position calls = adjust:%d cancel:%d, want adjust:1 cancel:0",
			positionManager.adjustCalls, positionManager.cancelCalls)
	}
	if enableCalls, stopCalls := executor.counts(); enableCalls != 1 || stopCalls != 1 {
		t.Fatalf("first executor calls = enable:%d stop:%d, want 1/1", enableCalls, stopCalls)
	}

	// 模拟关键拒绝原因已消失。下一次协调应先撤 BUY、强制对账，
	// 然后重新放行并执行恢复后的首次 AdjustOrders。
	positionManager.adjustErr = nil
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("recovery evaluate() error = %v", err)
	}
	if !gate.Enabled() || runtime.withdrawRequired.Load() || runtime.needsReconcile.Load() {
		t.Fatalf("recovered gate state: enabled=%v withdraw=%v reconcile=%v, want true/false/false",
			gate.Enabled(), runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
	}
	if !runtime.reconciler.IsHealthy() {
		t.Fatal("forced reconciliation did not restore reconciler health")
	}
	if positionManager.adjustCalls != 2 || positionManager.cancelCalls != 1 {
		t.Fatalf("recovered position calls = adjust:%d cancel:%d, want adjust:2 cancel:1",
			positionManager.adjustCalls, positionManager.cancelCalls)
	}
	if enableCalls, stopCalls := executor.counts(); enableCalls != 2 || stopCalls != 1 {
		t.Fatalf("recovered executor calls = enable:%d stop:%d, want 2/1", enableCalls, stopCalls)
	}
}

func TestTradingGateRealtimeUnknownAndOrdinaryErrorsEnterRecovery(t *testing.T) {
	unknownErr := fmt.Errorf("gateway response lost: %w", exchange.ErrOrderPlacementUnknown)
	ordinaryErr := errors.New("local state update failed")

	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "placement unknown", err: unknownErr},
		{name: "ordinary error", err: ordinaryErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			positionManager := &adjustmentTradingPosition{adjustErr: tt.err}
			runtime, gate, executor := newHealthyTradingGateTestRuntime(t, positionManager)
			if _, err := gate.Enable(); err != nil {
				t.Fatalf("Enable() error = %v", err)
			}

			err := runtime.evaluate(context.Background(), true)
			if !errors.Is(err, tt.err) {
				t.Fatalf("evaluate() error = %v, want wrapped %v", err, tt.err)
			}
			if gate.Enabled() {
				t.Fatal("gate remained enabled after a non-definite adjustment failure")
			}
			if !runtime.withdrawRequired.Load() || !runtime.needsReconcile.Load() {
				t.Fatalf("recovery flags = withdraw:%v reconcile:%v, want both true",
					runtime.withdrawRequired.Load(), runtime.needsReconcile.Load())
			}
			if runtime.reconciler.IsHealthy() {
				t.Fatal("reconciler remained healthy after a non-definite adjustment failure")
			}
			if positionManager.adjustCalls != 1 || positionManager.cancelCalls != 0 {
				t.Fatalf("position calls = adjust:%d cancel:%d, want adjust:1 cancel:0",
					positionManager.adjustCalls, positionManager.cancelCalls)
			}
			enableCalls, stopCalls := executor.counts()
			if enableCalls != 1 || stopCalls != 1 {
				t.Fatalf("executor calls = enable:%d stop:%d, want enable:1 stop:1", enableCalls, stopCalls)
			}
		})
	}
}

func TestTradingGateInitialAdjustKeepsFutureRequestAndRequestDuringAdjust(t *testing.T) {
	positionManager := newObservedTradingPosition()
	runtime, gate, _ := newHealthyTradingGateTestRuntime(t, positionManager)
	positionManager.onAdjust = func(callNumber int) {
		if callNumber == 1 && !runtime.RequestAdjustOrders(0) {
			t.Error("request made during initial AdjustOrders was rejected")
		}
	}

	if !runtime.RequestAdjustOrders(time.Hour) {
		t.Fatal("pre-existing delayed adjust request was rejected")
	}
	futureDeadline, ok := runtime.nextAdjustDeadline()
	if !ok {
		t.Fatal("pre-existing delayed adjust request was not queued")
	}
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("initial evaluate() error = %v", err)
	}
	if !gate.Enabled() {
		t.Fatal("gate was not enabled by initial evaluation")
	}
	if calls, _ := positionManager.stats(); calls != 1 {
		t.Fatalf("initial AdjustOrders calls = %d, want 1", calls)
	}
	if deadline, ok := runtime.nextAdjustDeadline(); !ok || !deadline.Equal(futureDeadline) {
		t.Fatalf("initial AdjustOrders consumed future deadline: got (%s, %v), want (%s, true)",
			deadline, ok, futureDeadline)
	}

	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("requested evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 2 {
		t.Fatalf("AdjustOrders calls after in-flight request = %d, want 2", calls)
	}
	if deadline, ok := runtime.nextAdjustDeadline(); !ok || !deadline.Equal(futureDeadline) {
		t.Fatalf("immediate AdjustOrders consumed future deadline: got (%s, %v), want (%s, true)",
			deadline, ok, futureDeadline)
	}
	if err := runtime.evaluate(context.Background(), true); err != nil {
		t.Fatalf("price-change evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 3 {
		t.Fatalf("price-change AdjustOrders calls = %d, want 3", calls)
	}
	if deadline, ok := runtime.nextAdjustDeadline(); !ok || !deadline.Equal(futureDeadline) {
		t.Fatalf("price-change AdjustOrders consumed future deadline: got (%s, %v), want (%s, true)",
			deadline, ok, futureDeadline)
	}
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("idle evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 3 {
		t.Fatalf("consumed request ran repeatedly: AdjustOrders calls = %d, want 3", calls)
	}
}

func TestTradingGateSkipsPriceTickWhenGridUnchanged(t *testing.T) {
	positionManager := newObservedTradingPosition()
	runtime, _, _ := newHealthyTradingGateTestRuntime(t, positionManager)
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("initial evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 1 {
		t.Fatalf("initial AdjustOrders calls = %d, want 1", calls)
	}

	positionManager.mu.Lock()
	positionManager.skipUnchangedGrid = true
	positionManager.mu.Unlock()

	if err := runtime.evaluate(context.Background(), true); err != nil {
		t.Fatalf("skipped price-tick evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 1 {
		t.Fatalf("unchanged-grid price tick AdjustOrders calls = %d, want 1", calls)
	}

	if !runtime.RequestAdjustOrders(0) {
		t.Fatal("explicit adjust request was rejected")
	}
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("explicit-request evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 2 {
		t.Fatalf("explicit adjust request AdjustOrders calls = %d, want 2", calls)
	}

	if err := runtime.evaluate(context.Background(), true); err != nil {
		t.Fatalf("second skipped price-tick evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 2 {
		t.Fatalf("price tick after explicit request AdjustOrders calls = %d, want 2", calls)
	}
}

func TestNewTradingGateRuntimeConnectsOptionalAdjustmentNotifier(t *testing.T) {
	positionManager := &adjustmentNotifierTradingPosition{}
	runtime := newTradingGateRuntime(
		newSerializedOrderGate(&recordingNewOrderGate{}),
		nil,
		nil,
		nil,
		nil,
		&safety.Reconciler{},
		positionManager,
		time.Second,
		time.Second,
	)
	if positionManager.notifier == nil {
		t.Fatal("optional adjustment notifier was not installed")
	}
	positionManager.notifier(time.Hour)
	if _, ok := runtime.nextAdjustDeadline(); !ok {
		t.Fatal("installed notifier did not enqueue a delayed adjust request")
	}
}

func TestTradingGateUnhealthyEvaluationRetainsImmediateWithoutZeroTimer(t *testing.T) {
	positionManager := newObservedTradingPosition()
	runtime, gate, _ := newHealthyTradingGateTestRuntime(t, positionManager)
	if _, err := gate.Enable(); err != nil {
		t.Fatalf("Enable() error = %v", err)
	}
	runtime.margin = &staticMarginGateMonitor{}
	if !runtime.RequestAdjustOrders(0) {
		t.Fatal("immediate request was rejected")
	}

	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("unhealthy evaluate() error = %v", err)
	}
	runtime.adjustMu.Lock()
	immediate := runtime.adjustImmediate
	deadlines := len(runtime.adjustDeadlines)
	runtime.adjustMu.Unlock()
	if !immediate || deadlines != 0 {
		t.Fatalf("unhealthy request state = immediate:%v deadlines:%d, want true/0", immediate, deadlines)
	}
	if _, ok := runtime.nextAdjustDeadline(); ok {
		t.Fatal("retained immediate request incorrectly armed a zero-duration timer")
	}
	if calls, _ := positionManager.stats(); calls != 0 {
		t.Fatalf("unhealthy evaluation called AdjustOrders %d times, want 0", calls)
	}
}

func TestTradingGateAdjustRequestsCoalesceAndRunSerially(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBlockedAdjust := func() {
		releaseOnce.Do(func() { close(release) })
	}
	t.Cleanup(releaseBlockedAdjust)
	positionManager := newObservedTradingPosition()
	positionManager.blockCall = 2
	positionManager.release = release
	runtime, _, _ := newHealthyTradingGateTestRuntime(t, positionManager)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := runtime.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		releaseBlockedAdjust()
		runtime.Stop()
	})
	waitObservedAdjustCall(t, positionManager, 1, time.Second)

	if !runtime.RequestAdjustOrders(0) {
		t.Fatal("immediate adjust request was rejected")
	}
	waitObservedAdjustCall(t, positionManager, 2, time.Second)
	for i := 0; i < 64; i++ {
		delay := time.Duration(i%2) * time.Hour
		if !runtime.RequestAdjustOrders(delay) {
			t.Fatalf("in-flight adjust request %d was rejected", i)
		}
	}
	releaseBlockedAdjust()
	waitObservedAdjustCall(t, positionManager, 3, time.Second)

	time.Sleep(50 * time.Millisecond)
	if calls, maxActive := positionManager.stats(); calls != 3 || maxActive != 1 {
		t.Fatalf("AdjustOrders stats = calls:%d max-active:%d, want 3/1", calls, maxActive)
	}
}

func TestTradingGateImmediateAdjustDoesNotConsumeFutureDeadline(t *testing.T) {
	positionManager := newObservedTradingPosition()
	runtime, _, _ := newHealthyTradingGateTestRuntime(t, positionManager)
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("initial evaluate() error = %v", err)
	}

	if !runtime.RequestAdjustOrders(time.Hour) {
		t.Fatal("delayed request was rejected")
	}
	futureDeadline, ok := runtime.nextAdjustDeadline()
	if !ok {
		t.Fatal("delayed request was not queued")
	}
	if !runtime.RequestAdjustOrders(0) {
		t.Fatal("immediate request was rejected")
	}
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("immediate evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 2 {
		t.Fatalf("AdjustOrders calls after immediate request = %d, want 2", calls)
	}
	if deadline, ok := runtime.nextAdjustDeadline(); !ok || !deadline.Equal(futureDeadline) {
		t.Fatalf("immediate adjustment consumed future deadline: got (%s, %v), want (%s, true)",
			deadline, ok, futureDeadline)
	}

	if !runtime.promoteDueAdjustRequests(futureDeadline) {
		t.Fatal("future deadline did not become ready at its deadline")
	}
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("delayed evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 3 {
		t.Fatalf("AdjustOrders calls after delayed request = %d, want 3", calls)
	}
	if _, ok := runtime.nextAdjustDeadline(); ok {
		t.Fatal("consumed delayed request remained queued")
	}
}

func TestTradingGateMultipleDelayedDeadlinesRemainIndependent(t *testing.T) {
	positionManager := newObservedTradingPosition()
	runtime, _, _ := newHealthyTradingGateTestRuntime(t, positionManager)
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("initial evaluate() error = %v", err)
	}

	for _, delay := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour} {
		if !runtime.RequestAdjustOrders(delay) {
			t.Fatalf("delayed request %s was rejected", delay)
		}
	}
	immediate, deadlines := snapshotAdjustRequests(runtime)
	if immediate || len(deadlines) != 3 {
		t.Fatalf("initial delayed state = immediate:%v deadlines:%d, want false/3", immediate, len(deadlines))
	}

	for i, deadline := range deadlines {
		if !runtime.promoteDueAdjustRequests(deadline) {
			t.Fatalf("deadline %d was not promoted at its due time", i+1)
		}
		if err := runtime.evaluate(context.Background(), false); err != nil {
			t.Fatalf("evaluate() for deadline %d error = %v", i+1, err)
		}
		immediate, remaining := snapshotAdjustRequests(runtime)
		wantRemaining := len(deadlines) - i - 1
		if immediate || len(remaining) != wantRemaining {
			t.Fatalf("state after deadline %d = immediate:%v remaining:%d, want false/%d",
				i+1, immediate, len(remaining), wantRemaining)
		}
		if wantRemaining > 0 && !remaining[0].Equal(deadlines[i+1]) {
			t.Fatalf("deadline %d consumed next future deadline: got %s, want %s",
				i+1, remaining[0], deadlines[i+1])
		}
	}
	if calls, maxActive := positionManager.stats(); calls != 4 || maxActive != 1 {
		t.Fatalf("delayed AdjustOrders stats = calls:%d max-active:%d, want 4/1", calls, maxActive)
	}
}

func TestTradingGateRecoveryBeforeCooldownDoesNotConsumeDelayedRetry(t *testing.T) {
	positionManager := newObservedTradingPosition()
	runtime, gate, _ := newHealthyTradingGateTestRuntime(t, positionManager)

	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("initial evaluate() error = %v", err)
	}
	if !gate.Disable() {
		t.Fatal("test gate was not enabled before simulated recovery")
	}

	if !runtime.RequestAdjustOrders(time.Hour) {
		t.Fatal("recovery delayed request was rejected")
	}
	originalDeadline, ok := runtime.nextAdjustDeadline()
	if !ok {
		t.Fatal("recovery delayed request was not queued")
	}
	// 在冷却期前手动执行恢复评估，避免测试依赖调度器是否及时处理 wake。
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("recovery evaluate() error = %v", err)
	}
	if !gate.Enabled() {
		t.Fatal("gate did not recover before cooldown")
	}

	if deadline, ok := runtime.nextAdjustDeadline(); !ok || !deadline.Equal(originalDeadline) {
		t.Fatalf("gate recovery consumed cooldown deadline: got (%s, %v), want (%s, true)",
			deadline, ok, originalDeadline)
	}
	if calls, _ := positionManager.stats(); calls != 2 {
		t.Fatalf("AdjustOrders calls after recovery = %d, want 2", calls)
	}
	if err := runtime.evaluate(context.Background(), false); err != nil {
		t.Fatalf("post-recovery evaluate() error = %v", err)
	}
	if calls, _ := positionManager.stats(); calls != 2 {
		t.Fatalf("future cooldown retried early: AdjustOrders calls = %d, want 2", calls)
	}
}

func TestTradingGateStopCancelsDelayedAdjustAndRejectsNewRequests(t *testing.T) {
	positionManager := newObservedTradingPosition()
	runtime, _, _ := newHealthyTradingGateTestRuntime(t, positionManager)

	if err := runtime.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitObservedAdjustCall(t, positionManager, 1, time.Second)
	if !runtime.RequestAdjustOrders(100 * time.Millisecond) {
		t.Fatal("delayed request before Stop was rejected")
	}
	runtime.Stop()

	if runtime.RequestAdjustOrders(0) {
		t.Fatal("adjust request after Stop was accepted")
	}
	time.Sleep(150 * time.Millisecond)
	if calls, maxActive := positionManager.stats(); calls != 1 || maxActive != 1 {
		t.Fatalf("post-Stop AdjustOrders stats = calls:%d max-active:%d, want 1/1", calls, maxActive)
	}
	if _, ok := runtime.nextAdjustDeadline(); ok {
		t.Fatal("delayed adjust deadline remained armed after Stop")
	}
}
