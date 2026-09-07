package exchange

import (
	"opensqt/config"
	"opensqt/exchange/binance"
)

// NewBinance 创建 Binance 交易所实例。
func NewBinance(cfg *config.Config) (IExchange, error) {
	exchangeCfg := cfg.Exchanges.Binance
	adapter, err := binance.NewBinanceAdapter(exchangeCfg.APIKey, exchangeCfg.SecretKey, cfg.Trading.Symbol)
	if err != nil {
		return nil, err
	}
	return &binanceWrapper{adapter: adapter}, nil
}
