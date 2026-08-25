package binance

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// GetMakerFeeRate 获取当前 Binance 账户在指定永续合约上的实际 Maker 费率。
func (b *BinanceAdapter) GetMakerFeeRate(ctx context.Context, symbol string) (float64, error) {
	if b == nil || b.client == nil {
		return 0, fmt.Errorf("Binance 客户端未初始化")
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		symbol = strings.ToUpper(strings.TrimSpace(b.symbol))
	}
	if symbol == "" {
		return 0, fmt.Errorf("Binance 手续费查询交易对不能为空")
	}
	if b.contractSpec != nil && symbol != b.contractSpec.Symbol {
		return 0, fmt.Errorf("Binance 手续费查询交易对 %s 与当前合约 %s 不匹配", symbol, b.contractSpec.Symbol)
	}

	rate, err := b.client.NewCommissionRateService().Symbol(symbol).Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("查询 Binance 实际 Maker 手续费率失败: %w", err)
	}
	if rate == nil {
		return 0, fmt.Errorf("查询 Binance 实际 Maker 手续费率返回空响应")
	}
	if !strings.EqualFold(strings.TrimSpace(rate.Symbol), symbol) {
		return 0, fmt.Errorf("Binance 手续费率响应交易对不匹配: got %q, want %q", rate.Symbol, symbol)
	}
	return parseMakerFeeRate(rate.MakerCommissionRate)
}

func parseMakerFeeRate(raw string) (float64, error) {
	raw = strings.TrimSpace(raw)
	feeRate, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("解析 Binance Maker 手续费率 %q 失败: %w", raw, err)
	}
	if math.IsNaN(feeRate) || math.IsInf(feeRate, 0) || feeRate < 0 || feeRate >= 1 {
		return 0, fmt.Errorf("Binance Maker 手续费率无效: %q", raw)
	}
	return feeRate, nil
}
