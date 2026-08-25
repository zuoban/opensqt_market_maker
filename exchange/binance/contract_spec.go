package binance

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/adshao/go-binance/v2/futures"
)

const quantizeToleranceDenominator int64 = 1_000_000_000

// fixedDecimal 保存来自 Binance 过滤器的十进制定点数。
// rat 用于精确比较和运算，scale 仅用于生成不会带科学计数法的 API 参数。
type fixedDecimal struct {
	rat   *big.Rat
	scale int
	text  string
}

func parseFixedDecimal(field, raw string, allowZero bool) (fixedDecimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fixedDecimal{}, fmt.Errorf("%s 不能为空", field)
	}

	unsigned := raw
	if unsigned[0] == '+' || unsigned[0] == '-' {
		unsigned = unsigned[1:]
	}
	parts := strings.Split(unsigned, ".")
	if len(parts) > 2 || (parts[0] == "" && (len(parts) == 1 || parts[1] == "")) {
		return fixedDecimal{}, fmt.Errorf("%s 不是有效十进制数: %q", field, raw)
	}
	for _, part := range parts {
		for _, r := range part {
			if r < '0' || r > '9' {
				return fixedDecimal{}, fmt.Errorf("%s 不是有效十进制数: %q", field, raw)
			}
		}
	}

	rat, ok := new(big.Rat).SetString(raw)
	if !ok {
		return fixedDecimal{}, fmt.Errorf("%s 不是有效十进制数: %q", field, raw)
	}
	if rat.Sign() < 0 || (!allowZero && rat.Sign() == 0) {
		relation := ">"
		if allowZero {
			relation = ">="
		}
		return fixedDecimal{}, fmt.Errorf("%s 必须%s0: %q", field, relation, raw)
	}

	scale := 0
	if len(parts) == 2 {
		scale = len(strings.TrimRight(parts[1], "0"))
	}
	text := rat.FloatString(scale)
	return fixedDecimal{rat: rat, scale: scale, text: text}, nil
}

func (d fixedDecimal) String() string {
	return d.text
}

func (d fixedDecimal) isZero() bool {
	return d.rat == nil || d.rat.Sign() == 0
}

// ContractSpec 是 Binance USD-M 永续合约真实的交易过滤器。
// PricePrecision/QuantityPrecision 只为兼容现有展示和 ClientOrderID 编码；
// 下单合法性一律以 TickSize/StepSize 等过滤器为准。
type ContractSpec struct {
	Symbol            string
	Status            string
	ContractType      futures.ContractType
	BaseAsset         string
	QuoteAsset        string
	MarginAsset       string
	PricePrecision    int
	QuantityPrecision int

	TickSize    fixedDecimal
	MinPrice    fixedDecimal
	MaxPrice    fixedDecimal
	StepSize    fixedDecimal
	MinQuantity fixedDecimal
	MaxQuantity fixedDecimal
	MinNotional fixedDecimal
}

func contractSpecFromSymbol(symbol futures.Symbol) (*ContractSpec, error) {
	if symbol.Symbol == "" {
		return nil, fmt.Errorf("合约 symbol 不能为空")
	}
	if symbol.Status != string(futures.SymbolStatusTypeTrading) {
		return nil, fmt.Errorf("合约 %s 状态不是 TRADING: %s", symbol.Symbol, symbol.Status)
	}
	if symbol.ContractType != futures.ContractTypePerpetual {
		return nil, fmt.Errorf("合约 %s 不是 PERPETUAL: %s", symbol.Symbol, symbol.ContractType)
	}
	if symbol.BaseAsset == "" || symbol.QuoteAsset == "" || symbol.MarginAsset == "" {
		return nil, fmt.Errorf("合约 %s 资产信息不完整: base=%q quote=%q margin=%q",
			symbol.Symbol, symbol.BaseAsset, symbol.QuoteAsset, symbol.MarginAsset)
	}
	if symbol.PricePrecision < 0 || symbol.QuantityPrecision < 0 {
		return nil, fmt.Errorf("合约 %s 精度无效: price=%d quantity=%d",
			symbol.Symbol, symbol.PricePrecision, symbol.QuantityPrecision)
	}

	priceFilter, err := uniqueFilter(symbol.Filters, string(futures.SymbolFilterTypePrice))
	if err != nil {
		return nil, fmt.Errorf("合约 %s: %w", symbol.Symbol, err)
	}
	lotSizeFilter, err := uniqueFilter(symbol.Filters, string(futures.SymbolFilterTypeLotSize))
	if err != nil {
		return nil, fmt.Errorf("合约 %s: %w", symbol.Symbol, err)
	}
	minNotionalFilter, err := uniqueFilter(symbol.Filters, string(futures.SymbolFilterTypeMinNotional))
	if err != nil {
		return nil, fmt.Errorf("合约 %s: %w", symbol.Symbol, err)
	}

	tickSize, err := positiveFilterDecimal(priceFilter, "tickSize")
	if err != nil {
		return nil, fmt.Errorf("合约 %s PRICE_FILTER: %w", symbol.Symbol, err)
	}
	minPrice, err := nonNegativeFilterDecimal(priceFilter, "minPrice")
	if err != nil {
		return nil, fmt.Errorf("合约 %s PRICE_FILTER: %w", symbol.Symbol, err)
	}
	maxPrice, err := nonNegativeFilterDecimal(priceFilter, "maxPrice")
	if err != nil {
		return nil, fmt.Errorf("合约 %s PRICE_FILTER: %w", symbol.Symbol, err)
	}
	if !minPrice.isZero() && !maxPrice.isZero() && minPrice.rat.Cmp(maxPrice.rat) > 0 {
		return nil, fmt.Errorf("合约 %s PRICE_FILTER minPrice 大于 maxPrice", symbol.Symbol)
	}

	stepSize, err := positiveFilterDecimal(lotSizeFilter, "stepSize")
	if err != nil {
		return nil, fmt.Errorf("合约 %s LOT_SIZE: %w", symbol.Symbol, err)
	}
	minQuantity, err := positiveFilterDecimal(lotSizeFilter, "minQty")
	if err != nil {
		return nil, fmt.Errorf("合约 %s LOT_SIZE: %w", symbol.Symbol, err)
	}
	maxQuantity, err := positiveFilterDecimal(lotSizeFilter, "maxQty")
	if err != nil {
		return nil, fmt.Errorf("合约 %s LOT_SIZE: %w", symbol.Symbol, err)
	}
	if minQuantity.rat.Cmp(maxQuantity.rat) > 0 {
		return nil, fmt.Errorf("合约 %s LOT_SIZE minQty 大于 maxQty", symbol.Symbol)
	}

	minNotional, err := positiveFilterDecimal(minNotionalFilter, "notional")
	if err != nil {
		return nil, fmt.Errorf("合约 %s MIN_NOTIONAL: %w", symbol.Symbol, err)
	}

	return &ContractSpec{
		Symbol:            symbol.Symbol,
		Status:            symbol.Status,
		ContractType:      symbol.ContractType,
		BaseAsset:         symbol.BaseAsset,
		QuoteAsset:        symbol.QuoteAsset,
		MarginAsset:       symbol.MarginAsset,
		PricePrecision:    symbol.PricePrecision,
		QuantityPrecision: symbol.QuantityPrecision,
		TickSize:          tickSize,
		MinPrice:          minPrice,
		MaxPrice:          maxPrice,
		StepSize:          stepSize,
		MinQuantity:       minQuantity,
		MaxQuantity:       maxQuantity,
		MinNotional:       minNotional,
	}, nil
}

func uniqueFilter(filters []map[string]interface{}, filterType string) (map[string]interface{}, error) {
	var found map[string]interface{}
	for _, filter := range filters {
		rawType, ok := filter["filterType"]
		if !ok {
			continue
		}
		typeName, ok := rawType.(string)
		if !ok {
			return nil, fmt.Errorf("filterType 类型无效: %T", rawType)
		}
		if typeName != filterType {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("重复的 %s 过滤器", filterType)
		}
		found = filter
	}
	if found == nil {
		return nil, fmt.Errorf("缺少 %s 过滤器", filterType)
	}
	return found, nil
}

func filterDecimal(filter map[string]interface{}, key string, allowZero bool) (fixedDecimal, error) {
	raw, ok := filter[key]
	if !ok {
		return fixedDecimal{}, fmt.Errorf("缺少 %s", key)
	}
	text, ok := raw.(string)
	if !ok {
		return fixedDecimal{}, fmt.Errorf("%s 类型无效: %T", key, raw)
	}
	return parseFixedDecimal(key, text, allowZero)
}

func positiveFilterDecimal(filter map[string]interface{}, key string) (fixedDecimal, error) {
	return filterDecimal(filter, key, false)
}

func nonNegativeFilterDecimal(filter map[string]interface{}, key string) (fixedDecimal, error) {
	return filterDecimal(filter, key, true)
}

type quantizeDirection int

const (
	quantizeDown quantizeDirection = iota
	quantizeUp
)

type normalizedOrder struct {
	PriceText    string
	QuantityText string
	Price        float64
	Quantity     float64
}

func (s *ContractSpec) normalizeOrder(req *OrderRequest) (normalizedOrder, error) {
	if s == nil {
		return normalizedOrder{}, fmt.Errorf("Binance 合约规格未初始化")
	}
	if req == nil {
		return normalizedOrder{}, fmt.Errorf("订单请求不能为空")
	}
	if !strings.EqualFold(strings.TrimSpace(req.Symbol), s.Symbol) {
		return normalizedOrder{}, fmt.Errorf("订单交易对 %q 与合约规格 %q 不匹配", req.Symbol, s.Symbol)
	}

	priceDirection := quantizeDown
	switch req.Side {
	case SideBuy:
		priceDirection = quantizeDown
	case SideSell:
		priceDirection = quantizeUp
	default:
		return normalizedOrder{}, fmt.Errorf("不支持的订单方向: %q", req.Side)
	}

	priceText, price, priceRat, err := quantizeFloat(req.Price, s.TickSize, priceDirection)
	if err != nil {
		return normalizedOrder{}, fmt.Errorf("价格量化失败: %w", err)
	}
	quantityText, quantity, quantityRat, err := quantizeFloat(req.Quantity, s.StepSize, quantizeDown)
	if err != nil {
		return normalizedOrder{}, fmt.Errorf("数量量化失败: %w", err)
	}

	if !s.MinPrice.isZero() && priceRat.Cmp(s.MinPrice.rat) < 0 {
		return normalizedOrder{}, fmt.Errorf("量化后价格 %s 小于 minPrice %s", priceText, s.MinPrice)
	}
	if !s.MaxPrice.isZero() && priceRat.Cmp(s.MaxPrice.rat) > 0 {
		return normalizedOrder{}, fmt.Errorf("量化后价格 %s 大于 maxPrice %s", priceText, s.MaxPrice)
	}
	if quantityRat.Cmp(s.MinQuantity.rat) < 0 {
		return normalizedOrder{}, fmt.Errorf("量化后数量 %s 小于 minQty %s", quantityText, s.MinQuantity)
	}
	if quantityRat.Cmp(s.MaxQuantity.rat) > 0 {
		return normalizedOrder{}, fmt.Errorf("量化后数量 %s 大于 maxQty %s", quantityText, s.MaxQuantity)
	}

	notional := new(big.Rat).Mul(priceRat, quantityRat)
	if notional.Cmp(s.MinNotional.rat) < 0 {
		return normalizedOrder{}, fmt.Errorf("量化后名义价值 %s 小于 minNotional %s",
			notional.FloatString(maxInt(s.MinNotional.scale, 8)), s.MinNotional)
	}

	return normalizedOrder{
		PriceText:    priceText,
		QuantityText: quantityText,
		Price:        price,
		Quantity:     quantity,
	}, nil
}

func quantizeFloat(value float64, quantum fixedDecimal, direction quantizeDirection) (string, float64, *big.Rat, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return "", 0, nil, fmt.Errorf("数值必须为有限正数: %v", value)
	}
	if quantum.rat == nil || quantum.rat.Sign() <= 0 {
		return "", 0, nil, fmt.Errorf("量化步长无效")
	}

	valueRat, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'f', -1, 64))
	if !ok {
		return "", 0, nil, fmt.Errorf("无法解析数值: %v", value)
	}
	ratio := new(big.Rat).Quo(valueRat, quantum.rat)
	units, snapped := integerRatio(ratio)
	if !snapped && direction == quantizeUp {
		units.Add(units, big.NewInt(1))
	}
	if units.Sign() <= 0 {
		return "", 0, nil, fmt.Errorf("量化后数值为 0")
	}

	resultRat := new(big.Rat).Mul(new(big.Rat).SetInt(units), quantum.rat)
	text := resultRat.FloatString(quantum.scale)
	result, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return "", 0, nil, fmt.Errorf("量化结果无法转换为 float64: %w", err)
	}
	return text, result, resultRat, nil
}

// integerRatio 将非常接近整数的浮点计算结果吸附到整数，避免诸如
// 100.12 在 float64 中变成 100.11999999999999 后被错误向下多量化一个 tick。
func integerRatio(ratio *big.Rat) (*big.Int, bool) {
	numerator := ratio.Num()
	denominator := ratio.Denom()
	floor := new(big.Int).Quo(numerator, denominator)
	remainder := new(big.Int).Rem(numerator, denominator)
	if remainder.Sign() == 0 {
		return floor, true
	}

	scaledRemainder := new(big.Int).Mul(new(big.Int).Set(remainder), big.NewInt(quantizeToleranceDenominator))
	if scaledRemainder.Cmp(denominator) <= 0 {
		return floor, true
	}
	gap := new(big.Int).Sub(denominator, remainder)
	scaledGap := new(big.Int).Mul(gap, big.NewInt(quantizeToleranceDenominator))
	if scaledGap.Cmp(denominator) <= 0 {
		return floor.Add(floor, big.NewInt(1)), true
	}
	return floor, false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
