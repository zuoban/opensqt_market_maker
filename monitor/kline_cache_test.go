package monitor

import (
	"testing"
	"time"

	"opensqt/exchange"
)

func TestKlineCacheMergesHistoryAndLivePrice(t *testing.T) {
	now := time.Date(2026, 8, 19, 10, 2, 30, 0, time.UTC)
	pm := &PriceMonitor{symbol: "BTCUSDT"}
	pm.klineCache.interval = "1m"
	pm.klineCache.duration = time.Minute
	pm.klineCache.limit = 3
	pm.mergeKlineHistory([]*exchange.Candle{
		{Symbol: "BTCUSDT", Timestamp: now.Add(-2 * time.Minute).Unix(), Open: 99, High: 101, Low: 98, Close: 100, Volume: 12},
		{Symbol: "BTCUSDT", Timestamp: now.Add(-time.Minute).UnixMilli(), Open: 100, High: 103, Low: 99, Close: 102, Volume: 18},
		{Symbol: "BTCUSDT", Timestamp: now.UnixMilli(), Open: 102, High: 104, Low: 101, Close: 103, Volume: 7},
	}, now)
	pm.recordCandlePrice(105, now)

	series := pm.GetKlineSnapshot()
	if !series.HistoryReady || series.HistoryError || len(series.Candles) != 3 {
		t.Fatalf("series = %+v", series)
	}
	latest := series.Candles[2]
	if latest.Open != 102 || latest.High != 105 || latest.Low != 101 || latest.Close != 105 || latest.IsClosed {
		t.Fatalf("latest = %+v", latest)
	}
	if !series.Candles[0].IsClosed || !series.Candles[1].IsClosed {
		t.Fatalf("historical candles should be closed: %+v", series.Candles)
	}

	series.Candles[2].Close = 1
	if got := pm.GetKlineSnapshot().Candles[2].Close; got != 105 {
		t.Fatalf("snapshot mutated cache, close = %v", got)
	}
}

func TestKlineCacheRollsAndLimitsCandles(t *testing.T) {
	start := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	pm := &PriceMonitor{symbol: "ETHUSDT"}
	pm.klineCache.interval = "1m"
	pm.klineCache.duration = time.Minute
	pm.klineCache.limit = 2

	pm.recordCandlePrice(10, start)
	pm.recordCandlePrice(12, start.Add(20*time.Second))
	pm.recordCandlePrice(11, start.Add(time.Minute))
	pm.recordCandlePrice(13, start.Add(2*time.Minute))

	series := pm.GetKlineSnapshot()
	if len(series.Candles) != 2 {
		t.Fatalf("candles = %d, want 2", len(series.Candles))
	}
	if !series.Candles[0].IsClosed || series.Candles[1].IsClosed {
		t.Fatalf("closed flags = %+v", series.Candles)
	}
	if series.Candles[0].Close != 11 || series.Candles[1].Close != 13 {
		t.Fatalf("closes = %+v", series.Candles)
	}
}

func TestCandleIntervalDuration(t *testing.T) {
	for input, want := range map[string]time.Duration{
		"1m": time.Minute,
		"4h": 4 * time.Hour,
		"1D": 24 * time.Hour,
	} {
		got, err := candleIntervalDuration(input)
		if err != nil || got != want {
			t.Fatalf("duration(%s) = %v, %v; want %v", input, got, err, want)
		}
	}
	if _, err := candleIntervalDuration("1M"); err == nil {
		t.Fatal("monthly interval should not be treated as minutes")
	}
}
