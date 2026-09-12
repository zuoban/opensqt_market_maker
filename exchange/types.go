package exchange

import "time"

// Side 交易方向
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType 订单类型
type OrderType string

const (
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeMarket OrderType = "MARKET"
)

// OrderStatus 订单状态
type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "NEW"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCanceled        OrderStatus = "CANCELED"
	OrderStatusRejected        OrderStatus = "REJECTED"
	OrderStatusExpired         OrderStatus = "EXPIRED"
)

// TimeInForce 订单有效期
type TimeInForce string

const (
	TimeInForceGTC TimeInForce = "GTC" // Good Till Cancel
	TimeInForceIOC TimeInForce = "IOC" // Immediate or Cancel
	TimeInForceFOK TimeInForce = "FOK" // Fill or Kill
	TimeInForceGTX TimeInForce = "GTX" // Good Till Crossing (Post Only)
)

// OrderRequest 下单请求（通用）
type OrderRequest struct {
	Symbol        string
	Side          Side
	Type          OrderType
	TimeInForce   TimeInForce
	Quantity      float64
	Price         float64 // 市价单可为0
	ReduceOnly    bool    // 是否只减仓
	PostOnly      bool    // 是否只做 Maker（Post Only）
	PriceDecimals int     // 价格精度（用于格式化）
	ClientOrderID string  // 自定义订单ID
}

// Order 订单信息（通用）
type Order struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          Side
	Type          OrderType
	Price         float64
	Quantity      float64
	ExecutedQty   float64
	AvgPrice      float64
	Status        OrderStatus
	CreatedAt     time.Time
	UpdateTime    int64
}

// Position 持仓信息（通用）
type Position struct {
	Symbol         string
	Size           float64 // 正数表示多仓，负数表示空仓
	EntryPrice     float64
	MarkPrice      float64
	UnrealizedPNL  float64
	Leverage       int
	MarginType     string
	IsolatedMargin float64
}

// Account 账户信息（通用）
type Account struct {
	TotalWalletBalance float64
	TotalMarginBalance float64
	AvailableBalance   float64
	Positions          []*Position
	AccountLeverage    int // 账户级别的杠杆倍数（部分交易所支持）
}

// MarketUpdate 是唯一市场数据 WebSocket 送入 PriceMonitor 的增量事件。
// LastPrice 来自真实逐笔成交；BestBid/BestAsk 来自最优盘口。Reset 表示底层
// 连接已切换到新的 epoch，消费者必须先使旧盘口失效，等新连接同时收到
// 成交价与盘口后再恢复下单。
type MarketUpdate struct {
	Symbol       string
	LastPrice    float64
	BestBid      float64
	BestAsk      float64
	EventTime    time.Time
	ReceivedAt   time.Time
	QuoteVersion uint64
	StreamEpoch  uint64
	Reset        bool
}

// MarketSnapshot 是全局 PriceMonitor 发布的原子市场快照。
// 网格定位使用 LastPrice；Maker 可提交边界必须使用 BestBid/BestAsk。
type MarketSnapshot struct {
	Symbol          string
	LastPrice       float64
	BestBid         float64
	BestAsk         float64
	TradeTime       time.Time
	QuoteTime       time.Time
	QuoteReceivedAt time.Time
	ReceivedAt      time.Time
	QuoteVersion    uint64
	StreamEpoch     uint64
	Ready           bool
}

// LastEventAt 是本进程最近一次收到该 epoch 市场数据的本地时间。
// Binance bookTicker 只在最优价/量变化时推送；成交仍证明同一条 combined 流活着。
func (s MarketSnapshot) LastEventAt() time.Time {
	latest := s.ReceivedAt
	if s.QuoteReceivedAt.After(latest) {
		latest = s.QuoteReceivedAt
	}
	if s.TradeTime.After(latest) {
		latest = s.TradeTime
	}
	return latest
}

// BookAgreesWithLast 判断买卖一是否和最近成交价属于同一价格量级。
// Binance bookTicker 同时带 b/B、a/A；解析若把数量写进价格，会出现 ask≈1 而 last≈736。
func (s MarketSnapshot) BookAgreesWithLast() bool {
	if s.LastPrice <= 0 || s.BestBid <= 0 || s.BestAsk <= s.BestBid {
		return s.BestBid > 0 && s.BestAsk > s.BestBid
	}
	low := s.LastPrice * 0.5
	high := s.LastPrice * 1.5
	return s.BestBid >= low && s.BestBid <= high && s.BestAsk >= low && s.BestAsk <= high
}

// MakerBookUsable 表示当前 epoch 已有合法买卖一，且市场数据流仍在活动。
// 没有新的 bookTicker 只说明最优盘口没变，不能当成盘口失效。
func (s MarketSnapshot) MakerBookUsable(now time.Time, staleAfter time.Duration) bool {
	if !s.Ready || s.BestBid <= 0 || s.BestAsk <= s.BestBid || s.QuoteReceivedAt.IsZero() {
		return false
	}
	if staleAfter <= 0 {
		return true
	}
	last := s.LastEventAt()
	if last.IsZero() {
		return false
	}
	if now.Before(last) {
		return true
	}
	return now.Sub(last) <= staleAfter
}

// OrderUpdate WebSocket 订单更新事件（通用）
type OrderUpdate struct {
	OrderID                int64
	ClientOrderID          string
	Symbol                 string
	Side                   Side
	Type                   OrderType
	Status                 OrderStatus
	Price                  float64
	Quantity               float64
	ExecutedQty            float64
	AvgPrice               float64
	UpdateTime             int64
	RealizedPNL            float64
	RealizedPNLIncremental bool
}

// OrderUpdateCallback 订单更新回调函数
type OrderUpdateCallback func(update OrderUpdate)

// Candle K线数据
type Candle struct {
	Symbol    string
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
	Timestamp int64
	IsClosed  bool // K线是否完结
}

// CandleUpdateCallback K线更新回调函数
type CandleUpdateCallback func(candle *Candle)
