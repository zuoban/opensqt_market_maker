package telemetry

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRecentWindowAndPercentiles(t *testing.T) {
	r := New(time.Now())
	for i := 1; i <= 100; i++ {
		r.Observe(Planning, time.Duration(i)*time.Millisecond, i%10 == 0)
	}
	r.Refresh()
	s := r.Snapshot().Latencies[Planning]
	if s.Count != 100 || s.Errors != 10 || s.Samples != 100 || s.LastMS != 100 ||
		s.MeanMS != 50.5 || s.P50MS != 50 || s.P95MS != 95 || s.P99MS != 99 || s.MaxMS != 100 || s.LastObservedAt.IsZero() {
		t.Fatalf("unexpected statistics: %+v", s)
	}
	for i := 0; i < SampleCapacity; i++ {
		r.Observe(Planning, 2*time.Millisecond, false)
	}
	r.Refresh()
	s = r.Snapshot().Latencies[Planning]
	if s.Count != 100+SampleCapacity || s.Samples != SampleCapacity || s.Errors != 10 ||
		s.MeanMS != 2 || s.MaxMS != 2 || s.P99MS != 2 {
		t.Fatalf("old samples survived bounded window: %+v", s)
	}
}

func TestObserveDropsInsteadOfWaiting(t *testing.T) {
	r := New(time.Now())
	l := &r.metrics[Planning]
	l.mu.Lock()
	done := make(chan struct{})
	go func() { r.Observe(Planning, time.Second, true); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		l.mu.Unlock()
		t.Fatal("Observe blocked on collector")
	}
	l.mu.Unlock()
	r.Refresh()
	s := r.Snapshot().Latencies[Planning]
	if s.Count != 0 || s.Errors != 0 || s.DroppedSamples != 1 {
		t.Fatalf("dropped sample changed accepted counts: %+v", s)
	}
}

func TestDisabledAndInvalidSamples(t *testing.T) {
	var disabled *Recorder
	disabled.Observe(Planning, time.Second, false)
	disabled.ObserveSince(Planning, time.Now(), false)
	disabled.Refresh()
	if !disabled.Start().IsZero() || disabled.Snapshot().Ready {
		t.Fatal("nil recorder enabled")
	}
	r := New(time.Time{})
	r.Observe(-1, time.Second, false)
	r.Observe(metricCount, time.Second, false)
	r.Observe(Planning, -1, false)
	r.ObserveSince(Planning, time.Time{}, false)
	r.ObserveSince(Planning, time.Now().Add(time.Hour), false)
	r.Refresh()
	if r.Snapshot().Latencies[Planning].Count != 0 {
		t.Fatal("invalid sample recorded")
	}
}

func TestCachedSnapshotAndRuntime(t *testing.T) {
	r := New(time.Now())
	calls := 0
	r.SetStateProvider(func() StateCounts { calls++; return StateCounts{Slots: 12} })
	if r.Snapshot().Ready {
		t.Fatal("uncollected snapshot ready")
	}
	r.Observe(Planning, time.Millisecond, false)
	r.Refresh()
	s := r.Snapshot()
	if !s.Ready || s.State.Slots != 12 || s.Runtime.Goroutines == 0 || s.Runtime.HeapGoalBytes == 0 {
		t.Fatalf("incomplete snapshot: %+v", s)
	}
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("invalid runtime JSON: %v", err)
	}
	s.Latencies[Planning].Name = "mutated"
	s.State.Slots = 99
	r.Observe(Planning, time.Second, false)
	next := r.Snapshot()
	if calls != 1 || next.State.Slots != 12 || next.Latencies[Planning].Name != "planning" || next.Latencies[Planning].Count != 1 {
		t.Fatal("snapshot mutated cache or performed collection")
	}
}

func TestConcurrentObserveAndRefresh(t *testing.T) {
	r := New(time.Now())
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				r.Observe(Planning, time.Duration(i), i%2 == 0)
			}
		}()
	}
	for i := 0; i < 20; i++ {
		r.Refresh()
		_ = r.Snapshot()
	}
	wg.Wait()
	r.Refresh()
	s := r.Snapshot().Latencies[Planning]
	if s.Count+s.DroppedSamples != 12000 || s.Samples > SampleCapacity {
		t.Fatalf("lost sample accounting: %+v", s)
	}
}

func TestRunLifecycle(t *testing.T) {
	r := New(time.Now())
	var calls atomic.Int64
	collected := make(chan struct{}, 4)
	r.SetStateProvider(func() StateCounts { calls.Add(1); collected <- struct{}{}; return StateCounts{} })
	canceled, stop := context.WithCancel(context.Background())
	stop()
	r.Run(canceled)
	if calls.Load() != 0 {
		t.Fatal("pre-canceled Run collected state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-collected:
	case <-time.After(2 * time.Second):
		t.Fatal("no initial collection")
	}
	duplicateDone := make(chan struct{})
	go func() { r.Run(ctx); close(duplicateDone) }()
	select {
	case <-duplicateDone:
	case <-time.After(time.Second):
		t.Fatal("duplicate collector started")
	}
	select {
	case <-collected:
	case <-time.After(3 * time.Second):
		t.Fatal("periodic collection missing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collector failed to exit")
	}
	if r.running.Load() {
		t.Fatal("collector still marked running")
	}
}

func BenchmarkObserve(b *testing.B) {
	r := New(time.Now())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.Observe(Planning, time.Millisecond, false)
	}
}

func BenchmarkObserveSince(b *testing.B) {
	r := New(time.Now())
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		started := r.Start()
		r.ObserveSince(Planning, started, false)
	}
}

func BenchmarkRefresh(b *testing.B) {
	r := New(time.Now())
	for metric := Metric(0); metric < metricCount; metric++ {
		for i := 0; i < SampleCapacity; i++ {
			r.Observe(metric, time.Duration((i*37)%SampleCapacity)*time.Microsecond, false)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.Refresh()
	}
}
