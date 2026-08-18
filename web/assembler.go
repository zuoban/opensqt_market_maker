package web

import (
	"fmt"
	"time"

	"opensqt/config"
	"opensqt/logger"
	"opensqt/monitor"
	"opensqt/position"
	"opensqt/safety"
)

// Snapshot 面板完整读数
type Snapshot struct {
	Time      time.Time                 `json:"time"`
	Version   string                    `json:"version"`
	StartedAt time.Time                 `json:"startedAt"`
	UptimeSec float64                   `json:"uptimeSec"`
	App       SafeAppConfig             `json:"app"`
	Price     PriceView                 `json:"price"`
	Kline     KlineView                 `json:"kline"`
	Position  position.PositionSnapshot `json:"position"`
	Risk      safety.RiskSnapshot       `json:"risk"`
	Account   AccountView               `json:"account"`
	Logs      []logger.LogEntry         `json:"logs"`
}

// PriceView 最新价格
type PriceView struct {
	Last      float64   `json:"last"`
	LastText  string    `json:"lastText"`
	UpdatedAt time.Time `json:"updatedAt"`
	AgeMs     int64     `json:"ageMs"`
}

// KlineView 面板使用的真实 OHLC 序列。
type KlineView struct {
	Interval     string       `json:"interval"`
	UpdatedAt    time.Time    `json:"updatedAt"`
	HistoryReady bool         `json:"historyReady"`
	Degraded     bool         `json:"degraded"`
	Candles      []CandleView `json:"candles"`
}

// CandleView 单根蜡烛；Time 为 UTC 毫秒时间戳。
type CandleView struct {
	Time     int64   `json:"time"`
	Open     float64 `json:"open"`
	High     float64 `json:"high"`
	Low      float64 `json:"low"`
	Close    float64 `json:"close"`
	Volume   float64 `json:"volume"`
	IsClosed bool    `json:"isClosed"`
}

type assembler struct {
	cfg     *config.Config
	version string
	started time.Time
	price   *monitor.PriceMonitor
	pos     *position.SuperPositionManager
	risk    *safety.RiskMonitor
	account *AccountCache
}

func (a *assembler) Build() *Snapshot {
	now := time.Now()
	started := a.started
	if started.IsZero() {
		started = now
	}
	snap := &Snapshot{
		Time:      now,
		Version:   a.version,
		StartedAt: started,
		UptimeSec: now.Sub(started).Seconds(),
		App:       safeAppView(a.cfg),
		Kline:     KlineView{Candles: make([]CandleView, 0)},
		Logs:      logger.RecentLogs(80),
	}
	if a.account != nil {
		snap.Account = a.account.View()
	}
	if a.pos != nil {
		snap.Position = a.pos.Snapshot()
	}
	if a.risk != nil {
		snap.Risk = a.risk.Snapshot()
	}
	if a.price != nil {
		last := a.price.GetLastPrice()
		updated := a.price.GetLastPriceTime()
		text := a.price.GetLastPriceString()
		if snap.Position.PriceDecimals > 0 && last > 0 {
			text = formatDec(last, snap.Position.PriceDecimals)
		}
		age := int64(0)
		if !updated.IsZero() {
			age = now.Sub(updated).Milliseconds()
		}
		snap.Price = PriceView{Last: last, LastText: text, UpdatedAt: updated, AgeMs: age}
		series := a.price.GetKlineSnapshot()
		snap.Kline.Interval = series.Interval
		snap.Kline.UpdatedAt = series.UpdatedAt
		snap.Kline.HistoryReady = series.HistoryReady
		snap.Kline.Degraded = series.HistoryError
		snap.Kline.Candles = make([]CandleView, 0, len(series.Candles))
		for _, candle := range series.Candles {
			snap.Kline.Candles = append(snap.Kline.Candles, CandleView{
				Time:     candle.Timestamp,
				Open:     candle.Open,
				High:     candle.High,
				Low:      candle.Low,
				Close:    candle.Close,
				Volume:   candle.Volume,
				IsClosed: candle.IsClosed,
			})
		}
	} else if snap.Position.LastPrice > 0 {
		snap.Price = PriceView{
			Last:     snap.Position.LastPrice,
			LastText: formatDec(snap.Position.LastPrice, snap.Position.PriceDecimals),
		}
	}
	return snap
}

func formatDec(v float64, decimals int) string {
	if decimals < 0 {
		decimals = 2
	}
	return fmt.Sprintf("%.*f", decimals, v)
}
