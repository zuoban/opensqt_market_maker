package safety

import (
	"testing"
	"time"

	"opensqt/config"
	"opensqt/exchange"
)

func TestRiskMonitorFailsClosedUntilReady(t *testing.T) {
	cfg := &config.Config{}
	cfg.RiskControl.Enabled = true
	cfg.RiskControl.MonitorSymbols = []string{"BTCUSDT"}
	r := NewRiskMonitor(cfg, nil)
	if r.IsReady() {
		t.Fatal("new risk monitor should not be ready")
	}
	if !r.IsTriggered() {
		t.Fatal("unready risk monitor must fail closed")
	}

	r.setReady(true, "ready")
	if !r.IsReady() || r.IsTriggered() {
		t.Fatal("ready monitor without market signal should allow trading")
	}
}

func TestMergeRiskCandleReplacesAndBounds(t *testing.T) {
	var candles []*exchange.Candle
	mergeRiskCandle(&candles, &exchange.Candle{Timestamp: 1000, Close: 1, IsClosed: false}, 3)
	mergeRiskCandle(&candles, &exchange.Candle{Timestamp: 1000, Close: 2, IsClosed: true}, 3)
	mergeRiskCandle(&candles, &exchange.Candle{Timestamp: 1000, Close: 3, IsClosed: false}, 3)
	if len(candles) != 1 {
		t.Fatalf("candles len = %d, want 1", len(candles))
	}
	if !candles[0].IsClosed || candles[0].Close != 2 {
		t.Fatalf("closed candle was overwritten: %+v", candles[0])
	}

	for i := int64(2); i <= 5; i++ {
		mergeRiskCandle(&candles, &exchange.Candle{Timestamp: i * 1000, Close: float64(i), IsClosed: true}, 3)
	}
	if len(candles) != 3 {
		t.Fatalf("bounded len = %d, want 3", len(candles))
	}
	if candles[0].Timestamp != 3000 || candles[2].Timestamp != 5000 {
		t.Fatalf("unexpected bounded candles: %+v", candles)
	}
}

func TestParseRiskInterval(t *testing.T) {
	tests := map[string]time.Duration{
		"1m":  time.Minute,
		"5m":  5 * time.Minute,
		"2h":  2 * time.Hour,
		"1d":  24 * time.Hour,
		"bad": 0,
	}
	for input, want := range tests {
		if got := parseRiskInterval(input); got != want {
			t.Errorf("parseRiskInterval(%q) = %v, want %v", input, got, want)
		}
	}
}
