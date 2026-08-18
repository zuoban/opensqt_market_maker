package safety

import (
	"fmt"
	"time"

	"opensqt/exchange"
)

// SymbolRiskSnapshot 单个监控币种的风控读数
type SymbolRiskSnapshot struct {
	Symbol           string  `json:"symbol"`
	HasData          bool    `json:"hasData"`
	DataInsufficient bool    `json:"dataInsufficient"`
	SkipReason       string  `json:"skipReason,omitempty"`
	CurrentPrice     float64 `json:"currentPrice"`
	AvgPrice         float64 `json:"avgPrice"`
	PriceDeviation   float64 `json:"priceDeviation"`
	CurrentVolume    float64 `json:"currentVolume"`
	AvgVolume        float64 `json:"avgVolume"`
	VolumeRatio      float64 `json:"volumeRatio"`
	PriceBelowMA     bool    `json:"priceBelowMA"`
	VolumeAbnormal   bool    `json:"volumeAbnormal"`
	Abnormal         bool    `json:"abnormal"`
	CandleClosed     bool    `json:"candleClosed"`
	CandleAgeSec     float64 `json:"candleAgeSec"`
	Status           string  `json:"status"`
}

// RiskSnapshot 风控监视器只读快照
type RiskSnapshot struct {
	Enabled            bool                 `json:"enabled"`
	Triggered          bool                 `json:"triggered"`
	LastMsg            string               `json:"lastMsg"`
	Interval           string               `json:"interval"`
	VolumeMultiplier   float64              `json:"volumeMultiplier"`
	AverageWindow      int                  `json:"averageWindow"`
	RecoveryThreshold  int                  `json:"recoveryThreshold"`
	MonitorSymbolCount int                  `json:"monitorSymbolCount"`
	Symbols            []SymbolRiskSnapshot `json:"symbols"`
}

type symbolMetrics struct {
	Ready          bool
	SkipReason     string
	CurrentPrice   float64
	CurrentVolume  float64
	AvgPrice       float64
	AvgVolume      float64
	PriceDeviation float64
	VolumeRatio    float64
	PriceAboveMA   bool
	VolumeNormal   bool
	Abnormal       bool
	CandleClosed   bool
	CandleAge      time.Duration
	KlineStatus    string
	KlineAgeStr    string
}

func cloneCandles(src []*exchange.Candle) []*exchange.Candle {
	out := make([]*exchange.Candle, len(src))
	for i, c := range src {
		if c == nil {
			continue
		}
		cc := *c
		out[i] = &cc
	}
	return out
}

func candleTime(ts int64) time.Time {
	if ts > 10000000000 {
		return time.Unix(ts/1000, 0)
	}
	return time.Unix(ts, 0)
}

func formatKlineAge(age time.Duration) string {
	if age > time.Minute {
		return fmt.Sprintf("%.0f分前", age.Minutes())
	}
	return fmt.Sprintf("%.0f秒前", age.Seconds())
}

// analyzeSymbolCandles 与 printMovingAverages / 触发判断同一套口径。
func analyzeSymbolCandles(candles []*exchange.Candle, window int, volMult float64, inRiskControl bool) symbolMetrics {
	if len(candles) < window+1 {
		return symbolMetrics{SkipReason: fmt.Sprintf("数据不足 (当前%d根, 需要%d根)", len(candles), window+1)}
	}

	var currentCandle *exchange.Candle
	if inRiskControl {
		for i := len(candles) - 1; i >= 0; i-- {
			if candles[i] != nil && candles[i].IsClosed {
				currentCandle = candles[i]
				break
			}
		}
		if currentCandle == nil {
			return symbolMetrics{SkipReason: "无完结K线"}
		}
	} else {
		currentCandle = candles[len(candles)-1]
		if currentCandle == nil {
			return symbolMetrics{SkipReason: "无数据"}
		}
	}

	currentPrice := currentCandle.Close
	currentVol := currentCandle.Volume

	var totalPrice, totalVol float64
	var validCount int
	for i := len(candles) - 1; i >= 0 && validCount < window; i-- {
		if candles[i] != nil && candles[i].IsClosed && candles[i] != currentCandle {
			totalPrice += candles[i].Close
			totalVol += candles[i].Volume
			validCount++
		}
	}
	if validCount < window {
		return symbolMetrics{SkipReason: fmt.Sprintf("完结K线不足 (当前%d根, 需要%d根)", validCount, window)}
	}

	avgPrice := totalPrice / float64(validCount)
	avgVol := totalVol / float64(validCount)
	priceDeviation := 0.0
	if avgPrice != 0 {
		priceDeviation = (currentPrice - avgPrice) / avgPrice * 100
	}
	volRatio := 0.0
	if avgVol != 0 {
		volRatio = currentVol / avgVol
	}

	priceAboveMA := currentPrice > avgPrice
	volNormal := currentVol < avgVol*volMult
	klineStatus := "完结"
	if !currentCandle.IsClosed {
		klineStatus = "未完结"
	}
	age := time.Since(candleTime(currentCandle.Timestamp))

	return symbolMetrics{
		Ready:          true,
		CurrentPrice:   currentPrice,
		CurrentVolume:  currentVol,
		AvgPrice:       avgPrice,
		AvgVolume:      avgVol,
		PriceDeviation: priceDeviation,
		VolumeRatio:    volRatio,
		PriceAboveMA:   priceAboveMA,
		VolumeNormal:   volNormal,
		Abnormal:       !priceAboveMA && !volNormal,
		CandleClosed:   currentCandle.IsClosed,
		CandleAge:      age,
		KlineStatus:    klineStatus,
		KlineAgeStr:    formatKlineAge(age),
	}
}

func (m symbolMetrics) toSnapshot(symbol string) SymbolRiskSnapshot {
	s := SymbolRiskSnapshot{
		Symbol:           symbol,
		HasData:          m.Ready,
		DataInsufficient: !m.Ready,
		SkipReason:       m.SkipReason,
		CurrentPrice:     m.CurrentPrice,
		AvgPrice:         m.AvgPrice,
		PriceDeviation:   m.PriceDeviation,
		CurrentVolume:    m.CurrentVolume,
		AvgVolume:        m.AvgVolume,
		VolumeRatio:      m.VolumeRatio,
		PriceBelowMA:     m.Ready && !m.PriceAboveMA,
		VolumeAbnormal:   m.Ready && !m.VolumeNormal,
		Abnormal:         m.Abnormal,
		CandleClosed:     m.CandleClosed,
		CandleAgeSec:     m.CandleAge.Seconds(),
	}
	if !m.Ready {
		s.Status = m.SkipReason
		return s
	}
	if m.Abnormal {
		s.Status = "异常"
	} else {
		s.Status = "正常"
	}
	return s
}

func (r *RiskMonitor) metricsForSymbol(symbol string, inRiskControl bool) symbolMetrics {
	r.mu.RLock()
	symbolData, exists := r.symbolDataMap[symbol]
	r.mu.RUnlock()
	if !exists || symbolData == nil {
		return symbolMetrics{SkipReason: "无数据"}
	}

	symbolData.mu.RLock()
	candles := cloneCandles(symbolData.candles)
	symbolData.mu.RUnlock()

	window := 20
	volMult := 3.0
	if r.cfg != nil {
		if r.cfg.RiskControl.AverageWindow > 0 {
			window = r.cfg.RiskControl.AverageWindow
		}
		if r.cfg.RiskControl.VolumeMultiplier > 0 {
			volMult = r.cfg.RiskControl.VolumeMultiplier
		}
	}
	return analyzeSymbolCandles(candles, window, volMult, inRiskControl)
}

// Snapshot 风控只读状态
func (r *RiskMonitor) Snapshot() RiskSnapshot {
	if r == nil || r.cfg == nil {
		return RiskSnapshot{}
	}

	r.mu.RLock()
	triggered := r.triggered
	lastMsg := r.lastMsg
	r.mu.RUnlock()

	snap := RiskSnapshot{
		Enabled:            r.cfg.RiskControl.Enabled,
		Triggered:          triggered,
		LastMsg:            lastMsg,
		Interval:           r.cfg.RiskControl.Interval,
		VolumeMultiplier:   r.cfg.RiskControl.VolumeMultiplier,
		AverageWindow:      r.cfg.RiskControl.AverageWindow,
		RecoveryThreshold:  r.cfg.RiskControl.RecoveryThreshold,
		MonitorSymbolCount: len(r.cfg.RiskControl.MonitorSymbols),
		Symbols:            make([]SymbolRiskSnapshot, 0, len(r.cfg.RiskControl.MonitorSymbols)),
	}

	for _, symbol := range r.cfg.RiskControl.MonitorSymbols {
		m := r.metricsForSymbol(symbol, triggered)
		snap.Symbols = append(snap.Symbols, m.toSnapshot(symbol))
	}
	return snap
}
