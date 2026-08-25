package exchange

import "context"

// MakerFeeRateProvider 是交易所可选实现的账户实际 Maker 手续费率能力。
// 返回值使用小数表示，例如 0.0002 表示 0.02%。
type MakerFeeRateProvider interface {
	GetMakerFeeRate(ctx context.Context, symbol string) (float64, error)
}
