// Package telemetry records bounded, read-only performance samples. It has no
// exchange dependency and never performs network or file I/O on a trading path.
package telemetry

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type Metric int

const (
	AdjustTotal Metric = iota
	AdjustLockWait
	Planning
	QuoteToPlanning
	RateLimitWait
	PlaceRequest
	CancelRequest
	OrderUpdateTotal
	OrderUpdateLockWait
	BuyFillToSellSubmit
	SellFillToBuySubmit
	metricCount
)

const (
	SampleCapacity  = 1024
	RefreshInterval = time.Second
)

var metricNames = [metricCount]string{
	"adjust_total", "adjust_lock_wait", "planning", "quote_to_planning",
	"rate_limit_wait", "place_request", "cancel_request", "order_update_total",
	"order_update_lock_wait", "buy_fill_to_sell_submit", "sell_fill_to_buy_submit",
}

type LatencySnapshot struct {
	Name           string    `json:"name"`
	Count          uint64    `json:"count"` // Accepted samples since recorder start.
	Errors         uint64    `json:"errors"`
	DroppedSamples uint64    `json:"droppedSamples"`
	Samples        int       `json:"samples"` // Recent window used by all duration statistics.
	LastObservedAt time.Time `json:"lastObservedAt"`
	LastMS         float64   `json:"lastMs"`
	MeanMS         float64   `json:"meanMs"`
	P50MS          float64   `json:"p50Ms"`
	P95MS          float64   `json:"p95Ms"`
	P99MS          float64   `json:"p99Ms"`
	MaxMS          float64   `json:"maxMs"`
}

type StateCounts struct {
	Slots              int   `json:"slots"`
	FilledDedupKeys    int   `json:"filledDedupKeys"`
	TerminalOrderKeys  int   `json:"terminalOrderKeys"`
	PendingAbsenceKeys int   `json:"pendingAbsenceKeys"`
	FilledOrders       int64 `json:"filledOrders"`
}

type Snapshot struct {
	Ready             bool                         `json:"ready"`
	StartedAt         time.Time                    `json:"startedAt"`
	SampledAt         time.Time                    `json:"sampledAt"`
	RefreshIntervalMS int64                        `json:"refreshIntervalMs"`
	SampleCapacity    int                          `json:"sampleCapacity"`
	Latencies         [metricCount]LatencySnapshot `json:"latencies"`
	Runtime           RuntimeSnapshot              `json:"runtime"`
	State             StateCounts                  `json:"state"`
}

type latency struct {
	mu      sync.Mutex
	samples [SampleCapacity]int64
	next    int
	size    int
	count   uint64
	errors  uint64
	last    int64
	lastAt  time.Time
	dropped atomic.Uint64
}

type stateProvider struct{ read func() StateCounts }

type Recorder struct {
	started   time.Time
	metrics   [metricCount]latency
	provider  atomic.Pointer[stateProvider]
	latest    atomic.Pointer[Snapshot]
	running   atomic.Bool
	refreshMu sync.Mutex
}

func New(started time.Time) *Recorder {
	if started.IsZero() {
		started = time.Now()
	}
	return &Recorder{started: started}
}

// SetStateProvider installs an in-memory, read-only count source. It is sampled
// by the background collector, never by Observe or an HTTP request.
func (r *Recorder) SetStateProvider(read func() StateCounts) {
	if r == nil {
		return
	}
	if read == nil {
		r.provider.Store(nil)
	} else {
		r.provider.Store(&stateProvider{read: read})
	}
}

// Start avoids clock reads for components whose recorder is not enabled.
func (r *Recorder) Start() time.Time {
	if r == nil {
		return time.Time{}
	}
	return time.Now()
}

func (r *Recorder) ObserveSince(metric Metric, started time.Time, failed bool) {
	if r == nil || started.IsZero() {
		return
	}
	r.Observe(metric, time.Since(started), failed)
}

// Observe does not wait for the collector or another writer. Only performance
// samples can be dropped; this function never changes trading state.
func (r *Recorder) Observe(metric Metric, duration time.Duration, failed bool) {
	if r == nil || metric < 0 || metric >= metricCount || duration < 0 {
		return
	}
	l := &r.metrics[metric]
	if !l.mu.TryLock() {
		l.dropped.Add(1)
		return
	}
	l.samples[l.next] = int64(duration)
	l.next = (l.next + 1) % SampleCapacity
	if l.size < SampleCapacity {
		l.size++
	}
	l.count++
	if failed {
		l.errors++
	}
	l.last = int64(duration)
	l.lastAt = time.Now()
	l.mu.Unlock()
}

func (l *latency) snapshot(name string) LatencySnapshot {
	l.mu.Lock()
	values := l.samples
	s := LatencySnapshot{
		Name: name, Count: l.count, Errors: l.errors, Samples: l.size,
		LastMS: float64(l.last) / float64(time.Millisecond), LastObservedAt: l.lastAt,
	}
	l.mu.Unlock()
	s.DroppedSamples = l.dropped.Load()
	if s.Samples == 0 {
		return s
	}
	recent := values[:s.Samples]
	slices.Sort(recent)
	var sum float64
	for _, value := range recent {
		sum += float64(value)
	}
	s.MeanMS = sum / float64(s.Samples) / float64(time.Millisecond)
	percentile := func(p int) float64 {
		index := (len(recent)*p+99)/100 - 1 // Nearest rank, no interpolation.
		return float64(recent[index]) / float64(time.Millisecond)
	}
	s.P50MS, s.P95MS, s.P99MS = percentile(50), percentile(95), percentile(99)
	s.MaxMS = float64(recent[len(recent)-1]) / float64(time.Millisecond)
	return s
}

// Refresh builds a new immutable view. Its work is isolated from hot-path
// sampling; exported for explicit startup sampling and deterministic tests.
func (r *Recorder) Refresh() {
	if r == nil {
		return
	}
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	s := &Snapshot{
		Ready: true, StartedAt: r.started, RefreshIntervalMS: RefreshInterval.Milliseconds(),
		SampleCapacity: SampleCapacity,
	}
	for metric, name := range metricNames {
		s.Latencies[metric] = r.metrics[metric].snapshot(name)
	}
	s.Runtime = readRuntimeSnapshot()
	if provider := r.provider.Load(); provider != nil {
		s.State = provider.read()
	}
	s.SampledAt = time.Now()
	r.latest.Store(s)
}

// Snapshot returns an independent value with no shared mutable slices or maps.
// Reading it neither scans slots nor collects runtime statistics.
func (r *Recorder) Snapshot() Snapshot {
	if r != nil {
		if s := r.latest.Load(); s != nil {
			return *s
		}
	}
	return Snapshot{}
}

func (r *Recorder) Run(ctx context.Context) {
	if r == nil || ctx == nil || !r.running.CompareAndSwap(false, true) {
		return
	}
	defer r.running.Store(false)
	if ctx.Err() != nil {
		return
	}
	r.Refresh()
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Refresh()
		}
	}
}
