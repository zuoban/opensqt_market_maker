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
	UptimeSec float64                   `json:"uptimeSec"`
	App       SafeAppConfig             `json:"app"`
	Price     PriceView                 `json:"price"`
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
	snap := &Snapshot{
		Time:      now,
		Version:   a.version,
		UptimeSec: now.Sub(a.started).Seconds(),
		App:       safeAppView(a.cfg),
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
