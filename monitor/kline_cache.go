package monitor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"opensqt/exchange"
)

const maxVisibleCandles = 500

// CandleSeries 是 PriceMonitor 内部行情流聚合出的只读 K 线快照。
// 历史数据来自交易所 REST，当前未完结蜡烛由全局唯一的价格流持续更新。
type CandleSeries struct {
	Interval     string
	UpdatedAt    time.Time
	HistoryReady bool
	HistoryError bool
	Candles      []exchange.Candle
}

type candleCache struct {
	mu           sync.RWMutex
	interval     string
	duration     time.Duration
	limit        int
	updatedAt    time.Time
	historyReady bool
	historyError bool
	candles      []exchange.Candle
}

// EnableKlines 开启 K 线缓存并加载一段真实历史数据。
// 该方法不会启动新的 WebSocket；当前蜡烛由 PriceMonitor 已有的价格流更新。
func (pm *PriceMonitor) EnableKlines(ctx context.Context, interval string, limit int) error {
	if pm == nil {
		return fmt.Errorf("价格监控不可用")
	}
	duration, err := candleIntervalDuration(interval)
	if err != nil {
		return err
	}
	if limit <= 0 {
		limit = 60
	}
	if limit > maxVisibleCandles {
		limit = maxVisibleCandles
	}

	pm.klineCache.mu.Lock()
	pm.klineCache.interval = interval
	pm.klineCache.duration = duration
	pm.klineCache.limit = limit
	pm.klineCache.historyReady = false
	pm.klineCache.historyError = false
	pm.klineCache.mu.Unlock()

	if pm.exchange == nil {
		pm.markKlineHistoryError()
		pm.recordCandlePrice(pm.GetLastPrice(), time.Now())
		return fmt.Errorf("交易所行情接口不可用")
	}

	history, err := pm.exchange.GetHistoricalKlines(ctx, pm.symbol, interval, limit)
	if err != nil {
		pm.markKlineHistoryError()
		pm.recordCandlePrice(pm.GetLastPrice(), time.Now())
		return fmt.Errorf("加载历史 K 线失败: %w", err)
	}
	pm.mergeKlineHistory(history, time.Now())
	pm.recordCandlePrice(pm.GetLastPrice(), time.Now())
	return nil
}

// GetKlineSnapshot 返回不会被后续行情更新修改的 K 线副本。
func (pm *PriceMonitor) GetKlineSnapshot() CandleSeries {
	if pm == nil {
		return CandleSeries{}
	}
	pm.klineCache.mu.RLock()
	defer pm.klineCache.mu.RUnlock()
	return CandleSeries{
		Interval:     pm.klineCache.interval,
		UpdatedAt:    pm.klineCache.updatedAt,
		HistoryReady: pm.klineCache.historyReady,
		HistoryError: pm.klineCache.historyError,
		Candles:      append([]exchange.Candle(nil), pm.klineCache.candles...),
	}
}

func (pm *PriceMonitor) markKlineHistoryError() {
	pm.klineCache.mu.Lock()
	pm.klineCache.historyReady = false
	pm.klineCache.historyError = true
	pm.klineCache.mu.Unlock()
}

func (pm *PriceMonitor) mergeKlineHistory(history []*exchange.Candle, now time.Time) {
	pm.klineCache.mu.Lock()
	defer pm.klineCache.mu.Unlock()
	if pm.klineCache.duration <= 0 {
		return
	}

	byTime := make(map[int64]exchange.Candle, len(history)+len(pm.klineCache.candles))
	for _, item := range history {
		if candle, ok := normalizeCandle(item, pm.klineCache.duration, now); ok {
			byTime[candle.Timestamp] = candle
		}
	}
	// 价格流可能在历史请求期间已经生成了当前蜡烛；保留其最新收盘价和范围。
	for _, live := range pm.klineCache.candles {
		if historical, ok := byTime[live.Timestamp]; ok {
			historical.High = maxFloat(historical.High, live.High)
			historical.Low = minPositive(historical.Low, live.Low)
			historical.Close = live.Close
			historical.IsClosed = live.IsClosed
			byTime[live.Timestamp] = historical
		} else {
			byTime[live.Timestamp] = live
		}
	}

	times := make([]int64, 0, len(byTime))
	for timestamp := range byTime {
		times = append(times, timestamp)
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	start := 0
	if len(times) > pm.klineCache.limit {
		start = len(times) - pm.klineCache.limit
	}
	merged := make([]exchange.Candle, 0, len(times)-start)
	for _, timestamp := range times[start:] {
		merged = append(merged, byTime[timestamp])
	}
	pm.klineCache.candles = merged
	pm.klineCache.updatedAt = now
	pm.klineCache.historyReady = len(merged) > 0
	pm.klineCache.historyError = false
}

func (pm *PriceMonitor) recordCandlePrice(price float64, now time.Time) {
	if pm == nil || price <= 0 || now.IsZero() {
		return
	}
	pm.klineCache.mu.Lock()
	defer pm.klineCache.mu.Unlock()
	duration := pm.klineCache.duration
	if duration <= 0 {
		return
	}
	bucket := now.UTC().Truncate(duration).UnixMilli()

	for i := len(pm.klineCache.candles) - 1; i >= 0; i-- {
		if pm.klineCache.candles[i].Timestamp == bucket {
			candle := &pm.klineCache.candles[i]
			candle.High = maxFloat(candle.High, price)
			candle.Low = minPositive(candle.Low, price)
			candle.Close = price
			candle.IsClosed = false
			pm.klineCache.updatedAt = now
			return
		}
		if pm.klineCache.candles[i].Timestamp < bucket {
			break
		}
	}

	if n := len(pm.klineCache.candles); n > 0 && pm.klineCache.candles[n-1].Timestamp < bucket {
		pm.klineCache.candles[n-1].IsClosed = true
	}
	pm.klineCache.candles = append(pm.klineCache.candles, exchange.Candle{
		Symbol:    pm.symbol,
		Open:      price,
		High:      price,
		Low:       price,
		Close:     price,
		Timestamp: bucket,
		IsClosed:  false,
	})
	sort.Slice(pm.klineCache.candles, func(i, j int) bool {
		return pm.klineCache.candles[i].Timestamp < pm.klineCache.candles[j].Timestamp
	})
	if len(pm.klineCache.candles) > pm.klineCache.limit {
		pm.klineCache.candles = append([]exchange.Candle(nil), pm.klineCache.candles[len(pm.klineCache.candles)-pm.klineCache.limit:]...)
	}
	pm.klineCache.updatedAt = now
}

func normalizeCandle(item *exchange.Candle, duration time.Duration, now time.Time) (exchange.Candle, bool) {
	if item == nil || item.Open <= 0 || item.High <= 0 || item.Low <= 0 || item.Close <= 0 || item.Timestamp <= 0 {
		return exchange.Candle{}, false
	}
	timestamp := item.Timestamp
	if timestamp < 100_000_000_000 {
		timestamp *= 1000
	} else if timestamp > 100_000_000_000_000_000 {
		timestamp /= 1_000_000
	} else if timestamp > 100_000_000_000_000 {
		timestamp /= 1000
	}
	timestamp = time.UnixMilli(timestamp).UTC().Truncate(duration).UnixMilli()
	currentBucket := now.UTC().Truncate(duration).UnixMilli()
	candle := *item
	candle.Timestamp = timestamp
	candle.High = maxFloat(candle.High, maxFloat(candle.Open, candle.Close))
	candle.Low = minPositive(candle.Low, minPositive(candle.Open, candle.Close))
	candle.IsClosed = timestamp < currentBucket
	return candle, true
}

func candleIntervalDuration(interval string) (time.Duration, error) {
	value := strings.TrimSpace(interval)
	if len(value) < 2 {
		return 0, fmt.Errorf("无效 K 线周期 %q", interval)
	}
	count, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("无效 K 线周期 %q", interval)
	}
	var unit time.Duration
	switch value[len(value)-1] {
	case 'm':
		unit = time.Minute
	case 'h', 'H':
		unit = time.Hour
	case 'd', 'D':
		unit = 24 * time.Hour
	case 'w', 'W':
		unit = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("不支持的 K 线周期 %q", interval)
	}
	return time.Duration(count) * unit, nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minPositive(a, b float64) float64 {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}
