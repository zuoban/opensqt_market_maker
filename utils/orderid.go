package utils

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	globalIDGen.mu.Lock()
	defer globalIDGen.mu.Unlock()

	// 1. 将价格转为整数字符串（避免浮点数）
	multiplier := math.Pow(10, float64(priceDecimals))
	priceInt := int64(math.Round(price * multiplier))

	// 2. 方向编码（单字符）
	sideCode := "B"
	if side == "SELL" {
		sideCode = "S"
	}

	// 3. 生成紧凑的时间戳 + 序列号
	now := time.Now()
	currentSec := now.Unix()

	// 重置序列号（每秒重置）
	if currentSec != globalIDGen.lastSec {
		globalIDGen.lastSec = currentSec
		globalIDGen.sequence = 0
	}

	globalIDGen.sequence++

	// 时间戳(10位) + 序列号(3位) = 13字符
	timestampSeq := fmt.Sprintf("%d%03d", currentSec, globalIDGen.sequence)

	// 最终格式: {price}_{side}_{timestamp}{seq}
	// 例如: 65000_B_1702468800001 (约18字符)
	return fmt.Sprintf("%d_%s_%s", priceInt, sideCode, timestampSeq)
}

// ParseOrderID 解析紧凑的订单ID
// 返回: price, side, timestamp, valid
func ParseOrderID(clientOrderID string, priceDecimals int) (float64, string, int64, bool) {
	parts := strings.Split(clientOrderID, "_")
	if len(parts) != 3 {
		return 0, "", 0, false
	}

	// 1. 解析价格整数
	priceInt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || priceInt <= 0 {
		return 0, "", 0, false
	}

	// 还原为浮点数价格
	multiplier := math.Pow(10, float64(priceDecimals))
	price := float64(priceInt) / multiplier

	// 2. 解析方向
	var side string
	switch parts[1] {
	case "B":
		side = "BUY"
	case "S":
		side = "SELL"
	default:
		return 0, "", 0, false
	}

	// 3. 解析时间戳（前10位）
	timestampSeq := parts[2]
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
