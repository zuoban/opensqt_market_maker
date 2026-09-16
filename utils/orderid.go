package utils

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

var pow10Table = [...]float64{
	1e0, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
}

func pow10(decimals int) float64 {
	if decimals >= 0 && decimals < len(pow10Table) {
		return pow10Table[decimals]
	}
	return math.Pow(10, float64(decimals))
}

// OrderIDGenerator 订单ID生成器
// 生成紧凑的 ClientOrderID，最大长度不超过18字符
type OrderIDGenerator struct {
	mu       sync.Mutex
	lastSec  int64
	sequence int
}

var globalIDGen = &OrderIDGenerator{}

// GenerateOrderID 生成紧凑的订单ID
// 格式: {price_int}_{side}_{timestamp}{seq}
//
// 参数:
//
//	price: 订单价格
//	side: 订单方向 (BUY/SELL)
//	priceDecimals: 价格精度
//
// 返回值示例:
//
//	65000_B_1702468800001  (价格65000，买单，约18字符)
//	950_S_1702468800123    (价格0.950，卖单，约16字符)
//
// 注意: 为给 Binance 经纪商前缀预留空间，总长度控制在18字符以内
func GenerateOrderID(price float64, side string, priceDecimals int) string {
	multiplier := pow10(priceDecimals)
	priceInt := int64(math.Round(price * multiplier))

	sideByte := byte('B')
	if side == "SELL" {
		sideByte = 'S'
	}

	globalIDGen.mu.Lock()
	now := time.Now()
	currentSec := now.Unix()

	if currentSec != globalIDGen.lastSec {
		globalIDGen.lastSec = currentSec
		globalIDGen.sequence = 0
	}
	globalIDGen.sequence++
	seq := globalIDGen.sequence
	globalIDGen.mu.Unlock()

	buf := make([]byte, 0, 32)
	buf = strconv.AppendInt(buf, priceInt, 10)
	buf = append(buf, '_', sideByte, '_')
	buf = strconv.AppendInt(buf, currentSec, 10)
	if seq < 10 {
		buf = append(buf, '0', '0', byte('0'+seq))
	} else if seq < 100 {
		buf = append(buf, '0', byte('0'+seq/10), byte('0'+seq%10))
	} else {
		buf = strconv.AppendInt(buf, int64(seq), 10)
	}

	return string(buf)
}

// ParseOrderID 解析紧凑的订单ID
// 返回: price, side, timestamp, valid
func ParseOrderID(clientOrderID string, priceDecimals int) (float64, string, int64, bool) {
	firstUnderscore := strings.IndexByte(clientOrderID, '_')
	if firstUnderscore <= 0 {
		return 0, "", 0, false
	}
	remaining := clientOrderID[firstUnderscore+1:]
	secondUnderscore := strings.IndexByte(remaining, '_')
	if secondUnderscore <= 0 {
		return 0, "", 0, false
	}

	priceStr := clientOrderID[:firstUnderscore]
	sideStr := remaining[:secondUnderscore]
	timestampSeq := remaining[secondUnderscore+1:]

	// 1. 解析价格整数
	priceInt, err := strconv.ParseInt(priceStr, 10, 64)
	if err != nil || priceInt <= 0 {
		return 0, "", 0, false
	}

	multiplier := pow10(priceDecimals)
	price := float64(priceInt) / multiplier

	// 2. 解析方向
	var side string
	switch sideStr {
	case "B":
		side = "BUY"
	case "S":
		side = "SELL"
	default:
		return 0, "", 0, false
	}

	// 3. 解析时间戳（前10位）
	if len(timestampSeq) < 10 {
		return 0, "", 0, false
	}

	timestamp, err := strconv.ParseInt(timestampSeq[:10], 10, 64)
	if err != nil {
		return 0, "", 0, false
	}

	return price, side, timestamp, true
}

// AddBinanceBrokerPrefix 添加 Binance 返佣前缀，并满足 36 字符限制。
func AddBinanceBrokerPrefix(clientOrderID string) string {
	const (
		prefix      = "x-zdfVM8vY"
		maxIDLength = 36
	)
	maxClientIDLength := maxIDLength - len(prefix)
	if len(clientOrderID) > maxClientIDLength {
		clientOrderID = clientOrderID[:maxClientIDLength]
	}
	return prefix + clientOrderID
}

// RemoveBinanceBrokerPrefix 移除 Binance 返佣前缀。
func RemoveBinanceBrokerPrefix(clientOrderID string) string {
	const prefix = "x-zdfVM8vY"
	if strings.HasPrefix(clientOrderID, prefix) {
		return clientOrderID[len(prefix):]
	}
	return clientOrderID
}
