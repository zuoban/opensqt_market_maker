package web

import "opensqt/config"

// SafeAppConfig 给面板看的策略参数，不含任何密钥。
type SafeAppConfig struct {
	Exchange                string   `json:"exchange"`
	Symbol                  string   `json:"symbol"`
	PriceInterval           float64  `json:"priceInterval"`
	OrderQuantity           float64  `json:"orderQuantity"`
	MinOrderValue           float64  `json:"minOrderValue"`
	MaxMarginUsage          float64  `json:"maxMarginUsagePercent"`
	BuyWindowSize           int      `json:"buyWindowSize"`
	SellWindowSize          int      `json:"sellWindowSize"`
	FeeRate                 float64  `json:"feeRate"`
	RiskEnabled             bool     `json:"riskEnabled"`
	MonitorSymbols          []string `json:"monitorSymbols"`
	DashboardPushIntervalMS int      `json:"dashboardPushIntervalMs"`
	MakerGuardTicks         int      `json:"makerGuardTicks"`
	QuoteStaleMS            int      `json:"quoteStaleMs"`
	CatchUpMode             string   `json:"catchUpMode"`
}

func safeAppView(cfg *config.Config) SafeAppConfig {
	if cfg == nil {
		return SafeAppConfig{}
	}
	view := SafeAppConfig{
		Exchange:                "binance",
		Symbol:                  cfg.Trading.Symbol,
		PriceInterval:           cfg.Trading.PriceInterval,
		OrderQuantity:           cfg.Trading.OrderQuantity,
		MinOrderValue:           cfg.Trading.MinOrderValue,
		MaxMarginUsage:          cfg.Trading.MaxMarginUsagePercent,
		BuyWindowSize:           cfg.Trading.BuyWindowSize,
		SellWindowSize:          cfg.Trading.SellWindowSize,
		RiskEnabled:             cfg.RiskControl.Enabled,
		DashboardPushIntervalMS: cfg.Dashboard.PushIntervalMS,
		MakerGuardTicks:         cfg.Execution.MakerGuardTicks,
		QuoteStaleMS:            cfg.Execution.QuoteStaleMS,
		CatchUpMode:             cfg.Execution.CatchUpMode,
	}
	if len(cfg.RiskControl.MonitorSymbols) > 0 {
		view.MonitorSymbols = append([]string(nil), cfg.RiskControl.MonitorSymbols...)
	}
	view.FeeRate = cfg.Exchanges.Binance.FeeRate
	return view
}
