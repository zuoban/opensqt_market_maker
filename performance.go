package main

import (
	"fmt"

	"opensqt/logger"
	"opensqt/telemetry"
)

func performanceP95(s telemetry.Snapshot, metric telemetry.Metric) string {
	m := s.Latencies[metric]
	if m.Samples == 0 {
		return "--(n=0)"
	}
	return fmt.Sprintf("%.2fms(n=%d)", m.P95MS, m.Samples)
}

func logPerformance(s telemetry.Snapshot) {
	if !s.Ready {
		return
	}
	logger.Info("📊 [性能] P95 规划=%s 限流=%s 下单=%s 回调锁等待=%s 买成→卖提交=%s 卖成→买提交=%s 堆=%.2fMiB GC=%d 槽位=%d 成交去重键=%d",
		performanceP95(s, telemetry.Planning), performanceP95(s, telemetry.RateLimitWait),
		performanceP95(s, telemetry.PlaceRequest), performanceP95(s, telemetry.OrderUpdateLockWait),
		performanceP95(s, telemetry.BuyFillToSellSubmit), performanceP95(s, telemetry.SellFillToBuySubmit),
		float64(s.Runtime.HeapObjectsBytes)/(1024*1024), s.Runtime.GCCycles, s.State.Slots, s.State.FilledDedupKeys)
}
