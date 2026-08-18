package safety

import (
	"testing"
	"time"

	"opensqt/config"
	"opensqt/exchange"
)

func makeWindow(avgClose, avgVol, lastClose, lastVol float64, closedLast bool) []*exchange.Candle {
	now := time.Now().UnixMilli()
	out := make([]*exchange.Candle, 0, 21)
	for i := 0; i < 20; i++ {
		out = append(out, &exchange.Candle{
			Symbol:    "ETHUSDT",
			Open:      avgClose,
			High:      avgClose,
			Low:       avgClose,
			Close:     avgClose,
			Volume:    avgVol,
			Timestamp: now - int64(21-i)*60000,
			IsClosed:  true,
		})
	}
	out = append(out, &exchange.Candle{
		Symbol:    "ETHUSDT",
		Close:     lastClose,
		Volume:    lastVol,
		Timestamp: now,
		IsClosed:  closedLast,
	})
	return out
}

func TestAnalyzeAbnormalRequiresBoth(t *testing.T) {
	// 价低于均线且放量 → 异常
	m := analyzeSymbolCandles(makeWindow(100, 10, 90, 40, false), 20, 3, false)
	if !m.Ready || !m.Abnormal {
		t.Fatalf("expected abnormal, got ready=%v abnormal=%v skip=%s", m.Ready, m.Abnormal, m.SkipReason)
	}

	// 只低于均线 → 正常
	onlyPrice := analyzeSymbolCandles(makeWindow(100, 10, 90, 10, false), 20, 3, false)
	if !onlyPrice.Ready || onlyPrice.Abnormal {
		t.Fatalf("price-only should be normal: %+v", onlyPrice)
	}

	// 只放量 → 正常
	onlyVol := analyzeSymbolCandles(makeWindow(100, 10, 101, 40, false), 20, 3, false)
	if !onlyVol.Ready || onlyVol.Abnormal {
		t.Fatalf("volume-only should be normal: %+v", onlyVol)
	}
}

func TestRiskMonitorSnapshot(t *testing.T) {
	cfg := &config.Config{}
	cfg.RiskControl.Enabled = true
	cfg.RiskControl.MonitorSymbols = []string{"ETHUSDT"}
	cfg.RiskControl.AverageWindow = 20
	cfg.RiskControl.VolumeMultiplier = 3
	cfg.RiskControl.Interval = "1m"
	r := NewRiskMonitor(cfg, nil)
	r.symbolDataMap["ETHUSDT"].candles = makeWindow(100, 10, 90, 40, false)

	snap := r.Snapshot()
	if !snap.Enabled || len(snap.Symbols) != 1 {
		t.Fatalf("snap = %+v", snap)
	}
	if !snap.Symbols[0].Abnormal || !snap.Symbols[0].PriceBelowMA || !snap.Symbols[0].VolumeAbnormal {
		t.Fatalf("symbol snap = %+v", snap.Symbols[0])
	}
}
