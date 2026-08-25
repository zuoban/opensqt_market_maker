package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"opensqt/logger"

	"github.com/gorilla/websocket"
)

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

type klineStreamEnvelope struct {
	Stream string `json:"stream"`
	Data   struct {
		EventType string `json:"e"`
		Symbol    string `json:"s"`
		K         struct {
			Timestamp int64  `json:"t"`
			Symbol    string `json:"s"`
			Interval  string `json:"i"`
			Open      string `json:"o"`
			Close     string `json:"c"`
			High      string `json:"h"`
			Low       string `json:"l"`
			Volume    string `json:"v"`
			IsClosed  bool   `json:"x"`
		} `json:"k"`
	} `json:"data"`
}

func parseKlineStreamMessage(message []byte, symbols []string, interval string) (*Candle, error) {
	var msg klineStreamEnvelope
	if err := json.Unmarshal(message, &msg); err != nil {
		return nil, fmt.Errorf("解析 K线 JSON 失败: %w", err)
	}
	if msg.Data.EventType != "kline" {
		return nil, fmt.Errorf("K线事件类型无效: %q", msg.Data.EventType)
	}
	symbol := strings.ToUpper(strings.TrimSpace(msg.Data.K.Symbol))
	if symbol == "" || (msg.Data.Symbol != "" && !strings.EqualFold(msg.Data.Symbol, symbol)) {
		return nil, fmt.Errorf("K线交易对字段无效: event=%q kline=%q", msg.Data.Symbol, msg.Data.K.Symbol)
	}
	allowed := false
	for _, configured := range symbols {
		if strings.EqualFold(strings.TrimSpace(configured), symbol) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("收到未订阅的 K线交易对: %s", symbol)
	}
	if strings.TrimSpace(msg.Data.K.Interval) != strings.TrimSpace(interval) {
		return nil, fmt.Errorf("K线周期不匹配: got %q, want %q", msg.Data.K.Interval, interval)
	}
	if msg.Data.K.Timestamp <= 0 {
		return nil, fmt.Errorf("K线开始时间无效: %d", msg.Data.K.Timestamp)
	}

	open, err := parseFiniteFloat("open", msg.Data.K.Open)
	if err != nil {
		return nil, err
	}
	high, err := parseFiniteFloat("high", msg.Data.K.High)
	if err != nil {
		return nil, err
	}
	low, err := parseFiniteFloat("low", msg.Data.K.Low)
	if err != nil {
		return nil, err
	}
	closePrice, err := parseFiniteFloat("close", msg.Data.K.Close)
	if err != nil {
		return nil, err
	}
	volume, err := parseFiniteFloat("volume", msg.Data.K.Volume)
	if err != nil {
		return nil, err
	}
	if open <= 0 || high <= 0 || low <= 0 || closePrice <= 0 || volume < 0 {
		return nil, fmt.Errorf("K线数值范围无效: O=%g H=%g L=%g C=%g V=%g", open, high, low, closePrice, volume)
	}
	if high < open || high < closePrice || high < low || low > open || low > closePrice {
		return nil, fmt.Errorf("K线 OHLC 关系无效: O=%g H=%g L=%g C=%g", open, high, low, closePrice)
	}

	return &Candle{
		Symbol:    symbol,
		Open:      open,
		High:      high,
		Low:       low,
		Close:     closePrice,
		Volume:    volume,
		Timestamp: msg.Data.K.Timestamp,
		IsClosed:  msg.Data.K.IsClosed,
	}, nil
}

// KlineWebSocketManager Binance K线WebSocket管理器
type KlineWebSocketManager struct {
	conn           *websocket.Conn
	mu             sync.RWMutex
	done           chan struct{}
	callback       func(candle interface{})
	symbols        []string
	interval       string
	reconnectDelay time.Duration
	pingInterval   time.Duration
	pongWait       time.Duration
	isRunning      bool
}

// NewKlineWebSocketManager 创建K线WebSocket管理器
func NewKlineWebSocketManager() *KlineWebSocketManager {
	return &KlineWebSocketManager{
		done:           make(chan struct{}),
		reconnectDelay: 5 * time.Second,  // 重连延迟
		pingInterval:   30 * time.Second, // Ping间隔
		pongWait:       60 * time.Second, // Pong等待超时
	}
}

// Start 启动K线流（带自动重连）
func (k *KlineWebSocketManager) Start(ctx context.Context, symbols []string, interval string, callback func(candle interface{})) error {
	k.mu.Lock()
	if k.isRunning {
		k.mu.Unlock()
		return fmt.Errorf("K线流已在运行")
	}
	k.callback = callback
	k.symbols = symbols
	k.interval = interval
	k.isRunning = true
	k.mu.Unlock()

	// 启动连接和重连协程
	go k.connectLoop(ctx)

	return nil
}

// connectLoop 连接循环（自动重连）
func (k *KlineWebSocketManager) connectLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			logger.Info("✅ K线WebSocket已停止（上下文取消）")
			return
		case <-k.done:
			logger.Info("✅ K线WebSocket已停止")
			return
		default:
		}

		// 构建WebSocket URL
		streams := make([]string, len(k.symbols))
		for i, symbol := range k.symbols {
			streams[i] = fmt.Sprintf("%s@kline_%s", strings.ToLower(symbol), k.interval)
		}
		wsURL := fmt.Sprintf("wss://fstream.binance.com/market/stream?streams=%s", strings.Join(streams, "/"))

		logger.Info("🔗 正在连接 Binance K线WebSocket...")

		conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if err != nil {
			logger.Error("❌ K线WebSocket连接失败: %v，%v后重试", err, k.reconnectDelay)
			// 使用 select 等待，可以立即响应 context 取消
			select {
			case <-ctx.Done():
				logger.Info("✅ K线WebSocket已停止（上下文取消）")
				return
			case <-k.done:
				logger.Info("✅ K线WebSocket已停止")
				return
			case <-time.After(k.reconnectDelay):
			}
			continue
		}

		k.mu.Lock()
		k.conn = conn
		k.mu.Unlock()

		logger.Info("✅ Binance K线WebSocket已连接")

		// 启动心跳保活
		go k.pingLoop(ctx, conn)

		// 启动读取循环（阻塞直到连接断开）
		k.readLoop(ctx, conn)

		// 连接断开，清理并准备重连
		k.mu.Lock()
		if k.conn == conn {
			k.conn = nil
		}
		k.mu.Unlock()

		// 检查是否因为 context 取消而断开，如果是则直接退出
		select {
		case <-ctx.Done():
			logger.Info("✅ K线WebSocket已停止（上下文取消）")
			return
		case <-k.done:
			logger.Info("✅ K线WebSocket已停止")
			return
		default:
		}

		logger.Warn("⚠️ K线WebSocket连接断开，%v后重连...", k.reconnectDelay)
		// 使用 select 等待，可以立即响应 context 取消
		select {
		case <-ctx.Done():
			logger.Info("✅ K线WebSocket已停止（上下文取消）")
			return
		case <-k.done:
			logger.Info("✅ K线WebSocket已停止")
			return
		case <-time.After(k.reconnectDelay):
		}
	}
}

// pingLoop 心跳保活循环
func (k *KlineWebSocketManager) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(k.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-k.done:
			return
		case <-ticker.C:
			k.mu.RLock()
			currentConn := k.conn
			k.mu.RUnlock()

			// 检查连接是否还是当前连接
			if currentConn != conn {
				return
			}

			// 发送Ping
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				logger.Warn("⚠️ K线WebSocket发送Ping失败: %v", err)
				conn.Close()
				return
			}
			logger.Debug("💓 K线WebSocket Ping已发送")
		}
	}
}

// Stop 停止K线流
func (k *KlineWebSocketManager) Stop() {
	k.mu.Lock()
	defer k.mu.Unlock()

	if !k.isRunning {
		return
	}

	k.isRunning = false
	close(k.done)

	if k.conn != nil {
		k.conn.Close()
		k.conn = nil
	}

	logger.Info("✅ Binance K线WebSocket已停止")
}

// readLoop 读取消息循环
func (k *KlineWebSocketManager) readLoop(ctx context.Context, conn *websocket.Conn) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("❌ K线WebSocket读取协程panic: %v", r)
		}
		conn.Close()
	}()

	// 设置Pong处理器
	conn.SetReadDeadline(time.Now().Add(k.pongWait))
	conn.SetPongHandler(func(string) error {
		logger.Debug("💓 K线WebSocket收到Pong")
		conn.SetReadDeadline(time.Now().Add(k.pongWait))
		return nil
	})

	for {
		select {
		case <-k.done:
			return
		case <-ctx.Done():
			return
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				logger.Warn("⚠️ K线WebSocket异常关闭: %v", err)
			} else {
				logger.Debug("K线WebSocket读取错误: %v", err)
			}
			return
		}

		// 重置读取超时
		conn.SetReadDeadline(time.Now().Add(k.pongWait))

		// 首次收到消息时打印，确认WebSocket连接正常
		//logger.Debug("收到K线WebSocket原始消息: %s", string(message))

		candle, err := parseKlineStreamMessage(message, k.symbols, k.interval)
		if err != nil {
			logger.Warn("⚠️ 忽略无效 Binance K线消息: %v", err)
			continue
		}

		// 调用回调（无论K线是否完结都回调）
		if k.callback != nil {
			k.callback(candle)
		}
	}
}
