package web

import "opensqt/config"

// SafeAppConfig 给面板看的策略参数，不含任何密钥。
type SafeAppConfig struct {
	Exchange       string   `json:"exchange"`
	Symbol         string   `json:"symbol"`
	PriceInterval  float64  `json:"priceInterval"`
	OrderQuantity  float64  `json:"orderQuantity"`
	MinOrderValue  float64  `json:"minOrderValue"`
	BuyWindowSize  int      `json:"buyWindowSize"`
	SellWindowSize int      `json:"sellWindowSize"`
	FeeRate        float64  `json:"feeRate"`
	RiskEnabled    bool     `json:"riskEnabled"`
	MonitorSymbols []string `json:"monitorSymbols"`
}

func safeAppView(cfg *config.Config) SafeAppConfig {
	if cfg == nil {
		return SafeAppConfig{}
	}
	view := SafeAppConfig{
		Exchange:       cfg.App.CurrentExchange,
		Symbol:         cfg.Trading.Symbol,
		PriceInterval:  cfg.Trading.PriceInterval,
		OrderQuantity:  cfg.Trading.OrderQuantity,
		MinOrderValue:  cfg.Trading.MinOrderValue,
		BuyWindowSize:  cfg.Trading.BuyWindowSize,
		SellWindowSize: cfg.Trading.SellWindowSize,
		RiskEnabled:    cfg.RiskControl.Enabled,
	}
	if len(cfg.RiskControl.MonitorSymbols) > 0 {
		view.MonitorSymbols = append([]string(nil), cfg.RiskControl.MonitorSymbols...)
	}
	if ex, ok := cfg.Exchanges[cfg.App.CurrentExchange]; ok {
		view.FeeRate = ex.FeeRate
	}
	return view
}
