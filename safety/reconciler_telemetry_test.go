package safety

import (
	"context"
	"errors"
	"testing"
	"time"

	"opensqt/telemetry"
)

func TestReconcileTelemetryIncludesRemoteWaitAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		wantErr := error(nil)
		if failed {
			wantErr = errors.New("remote read failed")
		}
		ex := parallelReadExchange{
			positions: func(context.Context) (interface{}, error) {
				time.Sleep(10 * time.Millisecond)
				return []*reconcileTestPosition{}, wantErr
			},
			orders: func(context.Context) (interface{}, error) { return []*reconcileTestOrder{}, nil },
		}
		r := NewReconciler(nil, ex, &reconcileTestPM{})
		performance := telemetry.New(time.Now())
		r.SetTelemetry(performance)
		if err := r.Reconcile(); !errors.Is(err, wantErr) {
			t.Fatal(err)
		}
		performance.Refresh()
		s := performance.Snapshot().Latencies[telemetry.ReconcileTotal]
		if s.Count != 1 || s.LastMS < 10 || (s.Errors == 1) != failed || r.IsHealthy() == failed {
			t.Fatalf("failed=%v reconcile sample=%+v", failed, s)
		}
		r.SetTelemetry(nil)
		_ = r.Reconcile() // 禁用观测不能影响对账行为。
		performance.Refresh()
		if performance.Snapshot().Latencies[telemetry.ReconcileTotal].Count != 1 {
			t.Fatal("disabled recorder received a sample")
		}
	}
}

func TestReconcileTelemetryIncludesPendingResolutionFailure(t *testing.T) {
	want := errors.New("pending unresolved")
	pm := &resolvingReconcileTestPM{reconcileTestPM: &reconcileTestPM{}, remaining: 1, err: want}
	r := NewReconciler(nil, nil, pm)
	performance := telemetry.New(time.Now())
	r.SetTelemetry(performance)
	if err := r.Reconcile(); !errors.Is(err, want) {
		t.Fatal(err)
	}
	performance.Refresh()
	s := performance.Snapshot().Latencies[telemetry.ReconcileTotal]
	if s.Count != 1 || s.Errors != 1 || s.LastMS <= 0 {
		t.Fatalf("pending resolution failure missing: %+v", s)
	}
}
