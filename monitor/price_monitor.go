package monitor

import (
	"context"
	"fmt"
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
	symbol        string
	exchange      exchange.IExchange // 依赖交易所接口
	lastPrice     atomic.Value       // float64
	lastPriceStr  atomic.Value       // string - 原始价格字符串（用于检测小数位数）
	lastPriceTime atomic.Value       // time.Time
	marketMu      sync.Mutex
	market        atomic.Value // exchange.MarketSnapshot，整份原子发布

	priceChangeCh     chan PriceChange
	latestPriceChange atomic.Value // *PriceChange - 保存最新的价格更新（不阻塞）
	isRunning         atomic.Bool
	ctx               context.Context
	cancel            context.CancelFunc

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
	pm.lastPrice.Store(0.0)
	pm.lastPriceStr.Store("")
	pm.lastPriceTime.Store(time.Time{})
	pm.market.Store(exchange.MarketSnapshot{Symbol: symbol})
	pm.latestPriceChange.Store((*PriceChange)(nil))
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

// updateMarket 合并成交价与盘口增量，并以一份原子快照发布。Reset 会立即
// 丢弃旧连接的盘口；重连后必须重新收到成交价和 bookTicker 才 Ready。
func (pm *PriceMonitor) updateMarket(update exchange.MarketUpdate) {
	pm.marketMu.Lock()
	defer pm.marketMu.Unlock()

	current := pm.GetMarketSnapshot()
	if update.StreamEpoch > 0 && current.StreamEpoch > update.StreamEpoch {
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
		pm.market.Store(reset)
		pm.latestPriceChange.Store(&PriceChange{
			OldPrice: current.LastPrice, Timestamp: now, QuoteChanged: true, Reset: true, Market: reset,
		})
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
		pm.lastPrice.Store(update.LastPrice)
		pm.lastPriceStr.Store(fmt.Sprintf("%f", update.LastPrice))
		pm.lastPriceTime.Store(now)
		pm.recordCandlePrice(update.LastPrice, now)
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
	pm.market.Store(current)

	if update.LastPrice > 0 || quoteChanged {
		pm.latestPriceChange.Store(&PriceChange{
			OldPrice: oldPrice, NewPrice: current.LastPrice,
			Change: current.LastPrice - oldPrice, Timestamp: now,
			QuoteChanged: quoteChanged, Market: current,
		})
	}
}

// GetMarketSnapshot 返回成交价与最优盘口来自同一市场数据连接的原子快照。
func (pm *PriceMonitor) GetMarketSnapshot() exchange.MarketSnapshot {
	if pm == nil {
		return exchange.MarketSnapshot{}
	}
	if value := pm.market.Load(); value != nil {
		if snapshot, ok := value.(exchange.MarketSnapshot); ok {
			return snapshot
		}
	}
	return exchange.MarketSnapshot{}
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
			// 获取最新价格更新
			if latestVal := pm.latestPriceChange.Load(); latestVal != nil {
				latestChange := latestVal.(*PriceChange)
				if latestChange != nil {
					// 尝试非阻塞发送
					select {
					case pm.priceChangeCh <- *latestChange:
						// 只清掉本次实际发送的版本。若发送期间又到了更新，
						// CAS 会失败并保留新版本，避免旧发送覆盖新盘口。
						pm.latestPriceChange.CompareAndSwap(latestChange, (*PriceChange)(nil))
					default:
						// channel已满，保留最新价格等待下次机会
					}
				}
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
	if val := pm.lastPrice.Load(); val != nil {
		return val.(float64)
	}
	return 0
}

// GetLastPriceString 获取最新价格的原始字符串（用于检测小数位数）
func (pm *PriceMonitor) GetLastPriceString() string {
	if val := pm.lastPriceStr.Load(); val != nil {
		return val.(string)
	}
	return ""
}

// GetLastPriceTime 最近一次收到价格推送的时间
func (pm *PriceMonitor) GetLastPriceTime() time.Time {
	if val := pm.lastPriceTime.Load(); val != nil {
		if t, ok := val.(time.Time); ok {
			return t
		}
	}
	return time.Time{}
}

// Subscribe 订阅价格变化
func (pm *PriceMonitor) Subscribe() <-chan PriceChange {
	outCh := make(chan PriceChange, 10)
	go func() {
		defer close(outCh)
		var latestChange *PriceChange // 保存最新的价格更新

		for {
			select {
			case <-pm.ctx.Done():
				// 尝试发送最后保存的更新（如果有）
				if latestChange != nil {
					select {
					case outCh <- *latestChange:
					default:
					}
				}
				return
			case change, ok := <-pm.priceChangeCh:
				if !ok {
					// priceChangeCh已关闭，尝试发送最后保存的更新（如果有）
					if latestChange != nil {
						select {
						case outCh <- *latestChange:
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
					latestChange = nil
				default:
					// outCh已满，保存最新的价格更新，丢弃旧数据
					// 这样确保消费者总是能收到最新的价格，而不是被旧数据阻塞
					latestChange = &change
				}
			}
		}
	}()
	return outCh
}
