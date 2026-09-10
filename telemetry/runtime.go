package telemetry

import (
	"math"
	"runtime/metrics"
)

type RuntimeSnapshot struct {
	HeapObjectsBytes       uint64  `json:"heapObjectsBytes"`
	HeapLiveBytes          uint64  `json:"heapLiveBytes"`
	HeapGoalBytes          uint64  `json:"heapGoalBytes"`
	TotalAllocatedBytes    uint64  `json:"totalAllocatedBytes"`
	GCCycles               uint64  `json:"gcCycles"`
	Goroutines             uint64  `json:"goroutines"`
	GCPauseSamples         uint64  `json:"gcPauseSamples"`
	GCPauseP99MSUpperBound float64 `json:"gcPauseP99MsUpperBound"`
	GCPauseQuantileReady   bool    `json:"gcPauseQuantileReady"`
}

func readRuntimeSnapshot() RuntimeSnapshot {
	// runtime/metrics reads aggregate runtime counters without a forced GC or
	// runtime.ReadMemStats' global heap-statistics synchronization.
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/gc/heap/live:bytes"},
		{Name: "/gc/heap/goal:bytes"},
		{Name: "/gc/heap/allocs:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/gc/pauses:seconds"},
	}
	metrics.Read(samples)
	integer := func(i int) uint64 {
		if samples[i].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return samples[i].Value.Uint64()
	}
	s := RuntimeSnapshot{
		HeapObjectsBytes: integer(0), HeapLiveBytes: integer(1), HeapGoalBytes: integer(2),
		TotalAllocatedBytes: integer(3), GCCycles: integer(4), Goroutines: integer(5),
	}
	if samples[6].Value.Kind() == metrics.KindFloat64Histogram {
		h := samples[6].Value.Float64Histogram()
		for _, count := range h.Counts {
			s.GCPauseSamples += count
		}
		if s.GCPauseSamples > 0 {
			target := s.GCPauseSamples - s.GCPauseSamples/100
			var count uint64
			for i, n := range h.Counts {
				count += n
				if count >= target {
					upper := h.Buckets[i+1] * 1000
					if !math.IsInf(upper, 0) && !math.IsNaN(upper) && upper >= 0 {
						s.GCPauseP99MSUpperBound = upper
						s.GCPauseQuantileReady = true
					}
					break
				}
			}
		}
	}
	return s
}
