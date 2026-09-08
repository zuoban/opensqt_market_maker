package monitor

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"opensqt/exchange"
	"opensqt/logger"
)

/*
PriceMonitor 架构说明：

1. **全局唯一的价格流**：
   - 整个系统中只有一个 PriceMonitor 实例（在 main.go 中创建）
   - 所有组件需要价格时，应该通过 priceMonitor.GetLastPrice() 获取
   - 不要在其他地方独立启动价格流

2. **价格获取方式**：
   - 必须使用 WebSocket 推送（毫秒级量化系统要求）
   - WebSocket 失败时系统将停止运行，不会降级
   - 价格缓存在内存中，读取无阻塞

3. **依赖关系**：
   - 依赖 exchange.IExchange 接口
   - 通过 exchange.StartPriceStream() 启动 WebSocket
   - WebSocket 是唯一的价格来源
*/

// PriceChange 价格变化事件
type PriceChange struct {
	OldPrice     float64
	NewPrice     float64
	Change       float64
	Timestamp    time.Time
	QuoteChanged bool
	Reset        bool
	Market       exchange.MarketSnapshot
}

// PriceMonitor 价格监控器
type PriceMonitor struct {
	symbol            string
	exchange          exchange.IExchange // 依赖交易所接口
	lastPriceBits     atomic.Uint64      // float64 bits
	lastPriceUnixNano atomic.Int64       // 最近成交价的本地接收时间
	marketMu          sync.RWMutex
	market            exchange.MarketSnapshot
	pendingChange     PriceChange
	hasPendingChange  bool

	priceChangeCh chan PriceChange
	isRunning     atomic.Bool
	ctx           context.Context
	cancel        context.CancelFunc

	// 时间配置
	priceSendInterval time.Duration
	klineCache        candleCache
}

// NewPriceMonitor 创建价格监控器
// 参数说明：
// - ex: 交易所接口（仅用于启动全局唯一 WebSocket 市场数据流）
// - symbol: 交易对符号
// - priceSendInterval: 价格推送间隔（毫秒）
func NewPriceMonitor(ex exchange.IExchange, symbol string, priceSendInterval int) *PriceMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	pm := &PriceMonitor{
		symbol:            symbol,
		exchange:          ex,
		priceChangeCh:     make(chan PriceChange, 10),
		ctx:               ctx,
		cancel:            cancel,
		priceSendInterval: time.Duration(priceSendInterval) * time.Millisecond,
	}
	pm.market = exchange.MarketSnapshot{Symbol: symbol}
	return pm
}

// Start 启动价格监控
func (pm *PriceMonitor) Start() error {
	if pm.isRunning.Load() {
		return fmt.Errorf("价格监控已在运行")
	}

	pm.isRunning.Store(true)

	// 启动价格流（WebSocket）- 这是唯一的价格来源
	// 注意：毫秒级量化系统不能容忍 REST API 轮询的延迟
	err := pm.exchange.StartPriceStream(pm.ctx, pm.symbol, func(update exchange.MarketUpdate) {
		pm.updateMarket(update)
	})
	if err != nil {
		// WebSocket 失败时直接返回错误，系统将停止
		pm.isRunning.Store(false)
		return fmt.Errorf("启动价格流失败（WebSocket 是唯一价格来源）: %w", err)
	}

	logger.Info("✅ 价格监控已启动 (WebSocket 推送)")
	go pm.periodicPriceSender() // 启动定期发送协程

	return nil
}

// pollPrice 已移除 - 毫秒级量化系统不使用 REST API 轮询
// WebSocket 是唯一的价格来源，失败时系统应该停止运行

// updateMarket 合并成交价与盘口增量，并以一份一致快照发布。Reset 会立即
// 丢弃旧连接的盘口；重连后必须重新收到成交价和 bookTicker 才 Ready。
func (pm *PriceMonitor) updateMarket(update exchange.MarketUpdate) {
	pm.marketMu.Lock()

	current := pm.market
	if update.StreamEpoch > 0 && current.StreamEpoch > update.StreamEpoch {
		pm.marketMu.Unlock()
		return
	}
	if update.Reset {
		now := update.ReceivedAt
		if now.IsZero() {
			now = time.Now()
		}
		reset := exchange.MarketSnapshot{
			Symbol: update.Symbol, StreamEpoch: update.StreamEpoch, ReceivedAt: now,
		}
		pm.market = reset
		pm.pendingChange = PriceChange{
			OldPrice: current.LastPrice, Timestamp: now, QuoteChanged: true, Reset: true, Market: reset,
		}
		pm.hasPendingChange = true
		pm.marketMu.Unlock()
		return
	}
	if update.StreamEpoch > current.StreamEpoch {
		current = exchange.MarketSnapshot{Symbol: update.Symbol, StreamEpoch: update.StreamEpoch}
	}
	if update.Symbol != "" {
		current.Symbol = update.Symbol
	}
	now := update.ReceivedAt
	if now.IsZero() {
		now = time.Now()
	}
	oldPrice := current.LastPrice
	quoteChanged := false
	if update.LastPrice > 0 {
		current.LastPrice = update.LastPrice
		current.TradeTime = update.EventTime
		if current.TradeTime.IsZero() {
			current.TradeTime = now
		}
		pm.lastPriceBits.Store(math.Float64bits(update.LastPrice))
		pm.lastPriceUnixNano.Store(now.UnixNano())
	}
	if update.BestBid > 0 && update.BestAsk > update.BestBid {
		quoteChanged = current.BestBid != update.BestBid || current.BestAsk != update.BestAsk ||
			current.QuoteVersion != update.QuoteVersion
		current.BestBid = update.BestBid
		current.BestAsk = update.BestAsk
		current.QuoteVersion = update.QuoteVersion
		current.QuoteTime = update.EventTime
		if current.QuoteTime.IsZero() {
			current.QuoteTime = now
		}
		current.QuoteReceivedAt = now
	}
	current.ReceivedAt = now
	current.Ready = current.LastPrice > 0 && current.BestBid > 0 && current.BestAsk > current.BestBid
	pm.market = current

	if update.LastPrice > 0 || quoteChanged {
		pm.pendingChange = PriceChange{
			OldPrice: oldPrice, NewPrice: current.LastPrice,
			Change: current.LastPrice - oldPrice, Timestamp: now,
			QuoteChanged: quoteChanged, Market: current,
		}
		pm.hasPendingChange = true
	}
	pm.marketMu.Unlock()

	if update.LastPrice > 0 {
		pm.recordCandlePrice(update.LastPrice, now)
	}
}

// GetMarketSnapshot 返回成交价与最优盘口来自同一市场数据连接的一致快照。
func (pm *PriceMonitor) GetMarketSnapshot() exchange.MarketSnapshot {
	if pm == nil {
		return exchange.MarketSnapshot{}
	}
	pm.marketMu.RLock()
	snapshot := pm.market
	pm.marketMu.RUnlock()
	return snapshot
}

func (pm *PriceMonitor) takePendingChange() (PriceChange, bool) {
	pm.marketMu.Lock()
	defer pm.marketMu.Unlock()
	if !pm.hasPendingChange {
		return PriceChange{}, false
	}
	change := pm.pendingChange
	pm.hasPendingChange = false
	return change, true
}

func (pm *PriceMonitor) restorePendingChange(change PriceChange) {
	pm.marketMu.Lock()
	if !pm.hasPendingChange {
		pm.pendingChange = change
		pm.hasPendingChange = true
	}
	pm.marketMu.Unlock()
}

// periodicPriceSender 定期发送最新价格
func (pm *PriceMonitor) periodicPriceSender() {
	ticker := time.NewTicker(pm.priceSendInterval)
	defer ticker.Stop()

	for {
		select {
		case <-pm.ctx.Done():
			return
		case <-ticker.C:
			change, ok := pm.takePendingChange()
			if !ok {
				continue
			}
			select {
			case pm.priceChangeCh <- change:
			default:
				// 若发送期间已有更新到达，restore 会保留更新后的版本。
				pm.restorePendingChange(change)
			}
		}
	}
}

// Stop 停止价格监控
func (pm *PriceMonitor) Stop() {
	pm.cancel()
	pm.isRunning.Store(false)
	// priceChangeCh 只有 periodicPriceSender 写入。这里不主动关闭，订阅者会由
	// ctx.Done() 退出；这样可避免 Stop 与 ticker 同时命中时发生 send-on-closed-channel。
}

// GetLastPrice 获取最新价格
func (pm *PriceMonitor) GetLastPrice() float64 {
	if pm == nil {
		return 0
	}
	return math.Float64frombits(pm.lastPriceBits.Load())
}

// GetLastPriceString 返回最新价格文本。交易精度来自合约规格，不再在行情
// 热路径为每笔成交预先分配字符串。
func (pm *PriceMonitor) GetLastPriceString() string {
	price := pm.GetLastPrice()
	if price <= 0 {
		return ""
	}
	return strconv.FormatFloat(price, 'f', -1, 64)
}

// GetLastPriceTime 最近一次收到价格推送的时间
func (pm *PriceMonitor) GetLastPriceTime() time.Time {
	if pm == nil {
		return time.Time{}
	}
	nanos := pm.lastPriceUnixNano.Load()
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, nanos)
}

// Subscribe 订阅价格变化
func (pm *PriceMonitor) Subscribe() <-chan PriceChange {
	outCh := make(chan PriceChange, 10)
	go func() {
		defer close(outCh)
		var latestChange PriceChange
		hasLatestChange := false

		for {
			select {
			case <-pm.ctx.Done():
				// 尝试发送最后保存的更新（如果有）
				if hasLatestChange {
					select {
					case outCh <- latestChange:
					default:
					}
				}
				return
			case change, ok := <-pm.priceChangeCh:
				if !ok {
					// priceChangeCh已关闭，尝试发送最后保存的更新（如果有）
					if hasLatestChange {
						select {
						case outCh <- latestChange:
						default:
						}
					}
					return
				}
				// Reset 和纯盘口变化也必须唤醒交易协调器。Reset 的
				// NewPrice 会是 0，但它需要立即使交易门禁 fail-closed；
				// bookTicker 变化则负责驱动同 QuoteVersion 去重后的重试。
				if change.NewPrice <= 0 && !change.QuoteChanged && !change.Reset {
					continue
				}

				// 尝试非阻塞发送
				select {
				case outCh <- change:
					// 成功发送，清空latestChange
					hasLatestChange = false
				default:
					// outCh已满，保存最新的价格更新，丢弃旧数据
					// 这样确保消费者总是能收到最新的价格，而不是被旧数据阻塞
					latestChange = change
					hasLatestChange = true
				}
			}
		}
	}()
	return outCh
}
