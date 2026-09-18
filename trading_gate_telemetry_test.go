package main

import (
	"context"
	"testing"
	"time"

	"opensqt/telemetry"
)

func TestAdjustQueueTelemetryCoalescesFromEarliestReadyTime(t *testing.T) {
	r := &tradingGateRuntime{wake: make(chan struct{}, 1)}
	performance := telemetry.New(time.Now())
	r.performance.Store(performance)
	r.RequestAdjustOrders(0)
	first := r.adjustReadySince
	r.RequestAdjustOrders(0)
	r.RequestAdjustOrders(time.Hour)
	if r.adjustReadySince != first || !r.takeReadyAdjustRequest(first.Add(25*time.Millisecond)) {
		t.Fatal("coalescing lost earliest ready request")
	}
	if r.takeReadyAdjustRequest(first.Add(30 * time.Millisecond)) {
		t.Fatal("one request consumed twice")
	}
	performance.Refresh()
	s := performance.Snapshot().Latencies[telemetry.AdjustRequestWait]
	if s.Count != 1 || s.LastMS != 25 {
		t.Fatalf("coalesced queue latency=%+v", s)
	}
	deadline, ok := r.nextAdjustDeadline()
	if !ok || !r.takeReadyAdjustRequest(deadline.Add(7*time.Millisecond)) {
		t.Fatal("future retry was lost")
	}
	performance.Refresh()
	s = performance.Snapshot().Latencies[telemetry.AdjustRequestWait]
	if s.Count != 2 || s.LastMS != 7 {
		t.Fatalf("intentional cooldown included in queue latency=%+v", s)
	}
	r.RequestAdjustOrders(0)
	r.stopAdjustRequests()
	if !r.adjustReadySince.IsZero() || r.takeReadyAdjustRequest(time.Now()) || r.RequestAdjustOrders(0) {
		t.Fatal("stop left a pending telemetry timestamp/request")
	}
}

func TestAdjustQueueTelemetryUsesExpiredDeadlineBeforeImmediateRequest(t *testing.T) {
	r := &tradingGateRuntime{wake: make(chan struct{}, 1)}
	performance := telemetry.New(time.Now())
	r.performance.Store(performance)
	r.RequestAdjustOrders(time.Hour)
	deadline, _ := r.nextAdjustDeadline()
	// 模拟 deadline 已到期、协调器尚忙时又收到新 immediate。
	r.adjustImmediate = true
	r.adjustReadySince = deadline.Add(20 * time.Millisecond)
	if !r.takeReadyAdjustRequest(deadline.Add(30 * time.Millisecond)) {
		t.Fatal("expired request not consumed")
	}
	performance.Refresh()
	if got := performance.Snapshot().Latencies[telemetry.AdjustRequestWait].LastMS; got != 30 {
		t.Fatalf("earliest deadline wait=%v want 30ms", got)
	}
}

func TestBatchContinuationRechecksHealthAndPreservesQueueObservation(t *testing.T) {
	p := newObservedTradingPosition()
	r, gate, _ := newHealthyTradingGateTestRuntime(t, p)
	performance := telemetry.New(time.Now())
	r.performance.Store(performance)
	healthyMargin := r.margin
	p.onAdjust = func(n int) {
		if n == 1 {
			r.RequestAdjustOrders(0) // 一批完成时发出的 continuation。
			r.margin = &staticMarginGateMonitor{}
		}
	}
	if err := r.evaluate(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := r.evaluate(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	performance.Refresh()
	if calls, maxActive := p.stats(); calls != 1 || maxActive != 1 || gate.Enabled() {
		t.Fatal("continuation bypassed health check or ran concurrently")
	}
	if performance.Snapshot().Latencies[telemetry.AdjustRequestWait].Count != 0 || r.adjustReadySince.IsZero() {
		t.Fatal("unhealthy gate consumed request observation")
	}
	r.margin = healthyMargin
	if err := r.evaluate(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	performance.Refresh()
	if calls, maxActive := p.stats(); calls != 2 || maxActive != 1 || !gate.Enabled() {
		t.Fatal("healthy recovery did not resume serial continuation")
	}
	if performance.Snapshot().Latencies[telemetry.AdjustRequestWait].Count != 1 {
		t.Fatal("recovery did not sample retained request exactly once")
	}
	r.RequestAdjustOrders(0)
	gate.BeginShutdown()
	_ = r.evaluate(context.Background(), false)
	if calls, _ := p.stats(); calls != 2 {
		t.Fatal("shutdown allowed a continuation")
	}
}
