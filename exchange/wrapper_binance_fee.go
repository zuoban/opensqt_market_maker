package exchange

import (
	"context"
	"fmt"
)

var _ MakerFeeRateProvider = (*binanceWrapper)(nil)

func (w *binanceWrapper) GetMakerFeeRate(ctx context.Context, symbol string) (float64, error) {
	if w == nil || w.adapter == nil {
		return 0, fmt.Errorf("Binance 适配器未初始化")
	}
	return w.adapter.GetMakerFeeRate(ctx, symbol)
}
