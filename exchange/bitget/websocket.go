package bitget

/*
Bitget WebSocket 架构说明：

1. **WebSocket下单**：Bitget不支持WebSocket下单，所有下单操作请使用REST API

2. **WebSocket用途**：
   - 公共频道：订阅价格推送 (ticker)
   - 私有频道：订阅订单更新 (orders)

3. **启动流程**：
   - main.go 中通过 PriceMonitor.Start() 启动价格流
   - main.go 中通过 ex.StartOrderStream() 启动订单流
   - 价格流和订单流共用同一个 WebSocketManager 实例
   - 公共频道和私有频道是两个独立的WebSocket连接

4. **价格获取方式**：
   - 优先从 WebSocket 缓存获取 (GetLatestPrice)
   - 如果缓存为空，降级使用 REST API
*/

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"opensqt/exchange/streamhealth"
	"opensqt/logger"

	"github.com/gorilla/websocket"
)

const (
	// Bitget V2 WebSocket 地址
	BitgetWSPrivate = "wss://ws.bitget.com/v2/ws/private"
	BitgetWSPublic  = "wss://ws.bitget.com/v2/ws/public"

	// API Code - 重要：不要丢失！
	BitgetAPICode = "3xh1b"
)

// WebSocketManager Bitget WebSocket 管理器
type WebSocketManager struct {
	apiKey     string
	secretKey  string
	passphrase string

	// 连接管理
	privateConn *websocket.Conn
	publicConn  *websocket.Conn
	mu          sync.RWMutex

	// 回调函数
	orderCallback func(interface{})
	priceCallback func(string, float64) // symbol, price

	// 控制
	publicCtx         context.Context
	publicCancel      context.CancelFunc
	publicDoneC       chan struct{}
	publicStopping    bool
	privateCtx        context.Context
	privateCancel     context.CancelFunc
	privateDoneC      chan struct{}
	privateStopping   bool
	privateGeneration uint64

	// 价格缓存
	latestPrice float64
	priceMu     sync.RWMutex

	// 🔥 标记消息处理是否已启动
	privateHandlerStarted bool
	publicHandlerStarted  bool
	orderHealth           streamhealth.Tracker

	// 🔥 重连控制
	publicReconnectChan  chan struct{}
	privateReconnectChan chan struct{}
	reconnectDelay       time.Duration
	subscribedSymbol     string // 记录订阅的交易对，用于重连后重新订阅
	subscribedInstType   string
	privateURL           string
	publicURL            string
	handshakeTTL         time.Duration
}

// SetPriceCallback 设置价格回调
func (w *WebSocketManager) SetPriceCallback(callback func(string, float64)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.priceCallback = callback
}

// IsRunning 检查 WebSocket 是否运行中
func (w *WebSocketManager) IsRunning() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.publicConn != nil || w.privateConn != nil
}

// OrderResponse 订单响应
type OrderResponse struct {
	Success   bool
	OrderID   string
	ClientOid string
	Code      string
	Msg       string
}

// WebSocket 消息结构
type WSMessage struct {
	Event string          `json:"event,omitempty"`
	Op    string          `json:"op"`
	Args  []interface{}   `json:"args,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
	Code  json.RawMessage `json:"code,omitempty"` // 可能是字符串或数字
	Msg   string          `json:"msg,omitempty"`
}

// GetCodeString 获取 code 的字符串值
func (m *WSMessage) GetCodeString() string {
	if len(m.Code) == 0 {
		return ""
	}
	// 尝试解析为数字
	var codeNum int
	if err := json.Unmarshal(m.Code, &codeNum); err == nil {
		return fmt.Sprintf("%d", codeNum)
	}
	// 尝试解析为字符串
	var codeStr string
	if err := json.Unmarshal(m.Code, &codeStr); err == nil {
		return codeStr
	}
	return ""
}

// WebSocket 订阅参数
type WSSubscribeArg struct {
	InstType string `json:"instType"`
	Channel  string `json:"channel"`
	InstId   string `json:"instId,omitempty"`
}

// NewWebSocketManager 创建 WebSocket 管理器
func NewWebSocketManager(apiKey, secretKey, passphrase string) *WebSocketManager {
	return &WebSocketManager{
		apiKey:               apiKey,
		secretKey:            secretKey,
		passphrase:           passphrase,
		publicReconnectChan:  make(chan struct{}, 1),
		privateReconnectChan: make(chan struct{}, 1),
		reconnectDelay:       5 * time.Second,
		privateURL:           BitgetWSPrivate,
		publicURL:            BitgetWSPublic,
		handshakeTTL:         10 * time.Second,
	}
}

// publicConnectLoop 公共频道连接循环（自动重连）
func (w *WebSocketManager) publicConnectLoop(ctx context.Context, doneC chan struct{}) {
	defer func() {
		w.mu.Lock()
		if w.publicDoneC == doneC {
			w.publicConn = nil
			w.publicCtx = nil
			w.publicCancel = nil
			w.publicDoneC = nil
			w.publicHandlerStarted = false
			w.publicStopping = false
		}
		w.mu.Unlock()
		close(doneC)
	}()

	for {
		select {
		case <-ctx.Done():
			logger.Info("✅ [Bitget WS公共] 停止连接循环")
			return
		default:
		}

		logger.Info("🔗 [Bitget WS公共] 正在连接...")

		// 连接公共频道
		w.mu.RLock()
		publicURL := w.publicURL
		handshakeTTL := bitgetHandshakeTTL(w.handshakeTTL)
		w.mu.RUnlock()
		dialCtx, cancelDial := context.WithTimeout(ctx, handshakeTTL)
		conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, publicURL, nil)
		cancelDial()
		if err != nil {
			logger.Error("❌ [Bitget WS公共] 连接失败: %v，%v后重试", err, w.reconnectDelay)
			// 使用 select 等待，可以立即响应 context 取消
			select {
			case <-ctx.Done():
				logger.Info("✅ [Bitget WS公共] 停止连接循环")
				return
			case <-time.After(w.reconnectDelay):
			}
			continue
		}

		w.mu.Lock()
		if w.publicDoneC != doneC {
			w.mu.Unlock()
			_ = conn.Close()
			return
		}
		w.publicConn = conn
		symbol := w.subscribedSymbol
		instType := w.subscribedInstType
		w.mu.Unlock()

		logger.Info("✅ [Bitget WS公共] 已连接")

		// 订阅价格更新
		if err := w.subscribeTicker(conn, symbol, instType); err != nil {
			logger.Error("❌ [Bitget WS公共] 订阅失败: %v", err)
			conn.Close()
			// 使用 select 等待，可以立即响应 context 取消
			select {
			case <-ctx.Done():
				logger.Info("✅ [Bitget WS公共] 停止连接循环")
				return
			case <-time.After(w.reconnectDelay):
			}
			continue
		}

		// 启动 ping 和读取协程
		done := make(chan struct{})
		stopKeepAliveC := make(chan struct{})
		go func() {
			w.keepAlive(ctx, conn, "公共", w.publicReconnectChan, stopKeepAliveC)
			close(done)
		}()

		// 启动读取循环（阻塞直到连接断开）
		w.handlePublicMessages(ctx, conn)
		close(stopKeepAliveC)
		_ = conn.Close()

		// 等待 keepAlive 退出（同时监听 context 取消）
		select {
		case <-done:
			// keepAlive 正常退出
		case <-ctx.Done():
			// context 取消，不等待 keepAlive
			logger.Info("✅ [Bitget WS公共] 停止连接循环")
			return
		}

		// 连接断开，清理
		w.mu.Lock()
		if w.publicConn == conn {
			w.publicConn = nil
		}
		w.mu.Unlock()
		conn.Close()

		// 检查是否因为 context 取消而断开，如果是则直接退出
		select {
		case <-ctx.Done():
			logger.Info("✅ [Bitget WS公共] 停止连接循环")
			return
		default:
		}

		logger.Warn("⚠️ [Bitget WS公共] 连接断开，%v后重连...", w.reconnectDelay)
		// 使用 select 等待，可以立即响应 context 取消
		select {
		case <-ctx.Done():
			logger.Info("✅ [Bitget WS公共] 停止连接循环")
			return
		case <-time.After(w.reconnectDelay):
		}
	}
}

// privateConnectLoop 私有频道连接循环（自动重连）
func (w *WebSocketManager) privateConnectLoop(ctx context.Context, doneC chan struct{}, generation uint64, initialResultC chan<- error) {
	initialPending := initialResultC != nil
	reportInitial := func(err error) {
		if !initialPending {
			return
		}
		initialPending = false
		initialResultC <- err
	}
	defer func() {
		if initialPending {
			err := ctx.Err()
			if err == nil {
				err = fmt.Errorf("订单流在握手完成前停止")
			}
			reportInitial(err)
		}
		w.mu.Lock()
		if w.privateDoneC == doneC && w.privateGeneration == generation {
			w.privateHandlerStarted = false
			w.privateStopping = false
			w.privateConn = nil
			w.privateCtx = nil
			w.privateCancel = nil
			w.privateDoneC = nil
			w.orderHealth.Set(streamhealth.StateStopped, nil)
		}
		w.mu.Unlock()
		close(doneC)
	}()

	for {
		select {
		case <-ctx.Done():
			logger.Info("✅ [Bitget WS私有] 停止连接循环")
			return
		default:
		}

		logger.Info("🔗 [Bitget WS私有] 正在连接...")

		// 连接私有频道
		conn, err := w.connectPrivate(ctx, doneC, generation)
		if err != nil {
			if ctx.Err() != nil {
				reportInitial(ctx.Err())
				return
			}
			wrappedErr := fmt.Errorf("私有流登录失败: %w", err)
			if initialPending {
				w.setPrivateHealth(generation, streamhealth.StateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setPrivateHealth(generation, streamhealth.StateDegraded, wrappedErr)
			logger.Error("❌ [Bitget WS私有] %v，%v后重试", wrappedErr, w.reconnectDelay)
			// 使用 select 等待，可以立即响应 context 取消
			select {
			case <-ctx.Done():
				logger.Info("✅ [Bitget WS私有] 停止连接循环")
				return
			case <-time.After(w.reconnectDelay):
			}
			continue
		}

		w.mu.RLock()
		symbol := w.subscribedSymbol
		instType := w.subscribedInstType
		w.mu.RUnlock()

		// 订阅订单更新
		err = w.subscribeOrders(conn, symbol, instType)
		if err == nil {
			err = w.waitPrivateSubscription(conn, instType, generation)
		}
		if err != nil {
			_ = conn.Close()
			w.clearPrivateConn(doneC, generation, conn)
			if ctx.Err() != nil {
				reportInitial(ctx.Err())
				return
			}
			wrappedErr := fmt.Errorf("私有订单频道订阅失败: %w", err)
			if initialPending {
				w.setPrivateHealth(generation, streamhealth.StateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setPrivateHealth(generation, streamhealth.StateDegraded, wrappedErr)
			logger.Error("❌ [Bitget WS私有] %v", wrappedErr)
			// 使用 select 等待，可以立即响应 context 取消
			select {
			case <-ctx.Done():
				logger.Info("✅ [Bitget WS私有] 停止连接循环")
				return
			case <-time.After(w.reconnectDelay):
			}
			continue
		}
		if ctx.Err() != nil || !w.setPrivateHealth(generation, streamhealth.StateReady, nil) {
			_ = conn.Close()
			stopErr := ctx.Err()
			if stopErr == nil {
				stopErr = fmt.Errorf("私有订单流已停止")
			}
			reportInitial(stopErr)
			return
		}
		reportInitial(nil)
		logger.Info("✅ [Bitget WS私有] 订单频道已登录并订阅")

		// 启动 ping 和读取协程
		done := make(chan struct{})
		stopKeepAliveC := make(chan struct{})
		go func() {
			w.keepAlive(ctx, conn, "私有", w.privateReconnectChan, stopKeepAliveC)
			close(done)
		}()

		// 启动读取循环（阻塞直到连接断开）
		streamErr := w.handlePrivateMessages(ctx, conn, generation, instType)
		close(stopKeepAliveC)
		_ = conn.Close()
		if ctx.Err() == nil {
			if streamErr == nil {
				streamErr = fmt.Errorf("私有订单流连接已断开")
			}
			w.setPrivateHealth(generation, streamhealth.StateDegraded, streamErr)
		}
		<-done
		w.clearPrivateConn(doneC, generation, conn)
		if ctx.Err() != nil {
			return
		}

		logger.Warn("⚠️ [Bitget WS私有] 连接断开: %v，%v后重连...", streamErr, w.reconnectDelay)
		// 使用 select 等待，可以立即响应 context 取消
		select {
		case <-ctx.Done():
			logger.Info("✅ [Bitget WS私有] 停止连接循环")
			return
		case <-time.After(w.reconnectDelay):
		}
	}
}

// ConnectAndLogin 已废弃 - 请使用 Start() 方法
// 保留该方法以兼容旧代码，但建议直接调用 Start()
func (w *WebSocketManager) ConnectAndLogin(ctx context.Context, symbol string) error {
	// 直接调用 Start 方法
	return w.Start(ctx, symbol, "USDT-FUTURES", nil)
}

// Start 启动 WebSocket 连接（公共频道+私有频道）
// 订阅价格更新(ticker)和订单更新(orders)
// callback: 订单更新回调函数，为nil时不订阅订单频道
func (w *WebSocketManager) Start(ctx context.Context, symbol, instType string, callback func(interface{})) error {
	if ctx == nil {
		return fmt.Errorf("WebSocket 上下文不能为空")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("WebSocket 上下文已结束: %w", err)
	}
	instType = normalizeBitgetInstType(instType)
	if symbol == "" || instType == "" {
		return fmt.Errorf("订阅交易对和产品类型不能为空")
	}

	w.mu.Lock()
	if w.publicStopping || (callback != nil && w.privateStopping) {
		state := w.orderHealth.State()
		w.mu.Unlock()
		return fmt.Errorf("WebSocket 正在停止（订单流状态=%s）", state)
	}
	if w.publicHandlerStarted && (w.publicCtx == nil || w.publicCtx.Err() != nil) {
		state := w.orderHealth.State()
		w.mu.Unlock()
		return fmt.Errorf("公共 WebSocket 正在退出（订单流状态=%s）", state)
	}
	if callback != nil && w.privateHandlerStarted && (w.privateCtx == nil || w.privateCtx.Err() != nil) {
		state := w.orderHealth.State()
		w.mu.Unlock()
		return fmt.Errorf("私有 WebSocket 正在退出（订单流状态=%s）", state)
	}
	if w.subscribedSymbol != "" && (w.publicHandlerStarted || w.privateHandlerStarted) &&
		(w.subscribedSymbol != symbol || !strings.EqualFold(w.subscribedInstType, instType)) {
		w.mu.Unlock()
		return fmt.Errorf("已运行的 WebSocket 订阅为 %s/%s，不能改为 %s/%s",
			w.subscribedInstType, w.subscribedSymbol, instType, symbol)
	}
	w.subscribedSymbol = symbol // 记录订阅的交易对
	w.subscribedInstType = instType

	startPublic := false
	var publicCtx context.Context
	var publicDoneC chan struct{}
	if !w.publicHandlerStarted {
		publicCtx, w.publicCancel = context.WithCancel(ctx)
		w.publicCtx = publicCtx
		w.publicDoneC = make(chan struct{})
		publicDoneC = w.publicDoneC
		w.publicHandlerStarted = true
		startPublic = true
	}

	startPrivate := false
	var privateCtx context.Context
	var privateDoneC chan struct{}
	var generation uint64
	if callback != nil && !w.privateHandlerStarted {
		w.orderCallback = callback
		privateCtx, w.privateCancel = context.WithCancel(ctx)
		w.privateCtx = privateCtx
		w.privateDoneC = make(chan struct{})
		privateDoneC = w.privateDoneC
		w.privateGeneration++
		generation = w.privateGeneration
		w.privateHandlerStarted = true
		startPrivate = true
		w.orderHealth.Set(streamhealth.StateStarting, nil)
	}
	alreadyPrivateReady := callback != nil && !startPrivate && w.orderHealth.Ready()
	if callback != nil && alreadyPrivateReady {
		w.orderCallback = callback
	}
	w.mu.Unlock()

	if startPublic {
		go w.publicConnectLoop(publicCtx, publicDoneC)
	}
	var initialResultC chan error
	if startPrivate {
		initialResultC = make(chan error, 1)
		go w.privateConnectLoop(privateCtx, privateDoneC, generation, initialResultC)
	}

	if callback != nil {
		if !startPrivate && !alreadyPrivateReady {
			return fmt.Errorf("私有订单流已在运行但未就绪（状态=%s）", w.orderHealth.State())
		}
		if startPrivate {
			if err := <-initialResultC; err != nil {
				<-privateDoneC
				return err
			}
		}
		logger.Info("✅ [Bitget WebSocket] 私有订单流已就绪，并将订阅 %s 的价格更新", symbol)
	} else {
		logger.Info("✅ [Bitget WebSocket] 已启动 %s 的价格连接", symbol)
	}
	return nil
}

// Stop 停止 WebSocket
func (w *WebSocketManager) Stop() {
	w.mu.Lock()
	privateCancel := w.privateCancel
	publicCancel := w.publicCancel
	privateDoneC := w.privateDoneC
	publicDoneC := w.publicDoneC
	privateConn := w.privateConn
	publicConn := w.publicConn
	if w.privateHandlerStarted {
		w.privateStopping = true
		w.orderHealth.Set(streamhealth.StateStopping, nil)
	}
	if w.publicHandlerStarted {
		w.publicStopping = true
	}
	w.mu.Unlock()

	if privateCancel != nil {
		privateCancel()
	}
	if publicCancel != nil {
		publicCancel()
	}
	if privateConn != nil {
		_ = privateConn.Close()
	}
	if publicConn != nil {
		_ = publicConn.Close()
	}
	w.waitStreamStopped(privateDoneC, "私有")
	w.waitStreamStopped(publicDoneC, "公共")
	logger.Info("✅ [Bitget WebSocket] 已停止")
}

func (w *WebSocketManager) waitStreamStopped(doneC <-chan struct{}, name string) {
	if doneC == nil {
		return
	}
	select {
	case <-doneC:
	case <-time.After(10 * time.Second):
		logger.Warn("⚠️ [Bitget WS%s] 停止超时", name)
	}
}

// connectPrivate 连接私有 WebSocket
func (w *WebSocketManager) connectPrivate(ctx context.Context, doneC chan struct{}, generation uint64) (*websocket.Conn, error) {
	w.mu.RLock()
	privateURL := w.privateURL
	handshakeTTL := w.handshakeTTL
	w.mu.RUnlock()
	dialCtx, cancelDial := context.WithTimeout(ctx, bitgetHandshakeTTL(handshakeTTL))
	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, privateURL, nil)
	cancelDial()
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	if w.privateDoneC != doneC || w.privateGeneration != generation || w.privateStopping {
		w.mu.Unlock()
		_ = conn.Close()
		return nil, context.Canceled
	}
	w.privateConn = conn
	w.mu.Unlock()
	handshakeTTL = bitgetHandshakeTTL(handshakeTTL)
	if err := conn.SetReadDeadline(time.Now().Add(handshakeTTL)); err != nil {
		_ = conn.Close()
		w.clearPrivateConn(doneC, generation, conn)
		return nil, err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(handshakeTTL)); err != nil {
		_ = conn.Close()
		w.clearPrivateConn(doneC, generation, conn)
		return nil, err
	}

	// 发送登录认证
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	sign := w.generateSign(timestamp, "GET", "/user/verify")

	loginMsg := map[string]interface{}{
		"op": "login",
		"args": []map[string]string{
			{
				"apiKey":     w.apiKey,
				"passphrase": w.passphrase,
				"timestamp":  timestamp,
				"sign":       sign,
			},
		},
	}

	if err := conn.WriteJSON(loginMsg); err != nil {
		_ = conn.Close()
		w.clearPrivateConn(doneC, generation, conn)
		return nil, fmt.Errorf("发送登录消息失败: %w", err)
	}

	// 等待登录响应
	var resp WSMessage
	if err := conn.ReadJSON(&resp); err != nil {
		_ = conn.Close()
		w.clearPrivateConn(doneC, generation, conn)
		return nil, fmt.Errorf("读取登录响应失败: %w", err)
	}

	codeStr := resp.GetCodeString()
	if resp.Event != "login" || codeStr != "0" {
		_ = conn.Close()
		w.clearPrivateConn(doneC, generation, conn)
		return nil, fmt.Errorf("登录失败: event=%s code=%s, msg=%s", resp.Event, codeStr, resp.Msg)
	}

	logger.Info("✅ [Bitget WebSocket] 私有频道登录成功")
	return conn, nil
}

func bitgetHandshakeTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 10 * time.Second
	}
	return ttl
}

// connectPublic 连接公共 WebSocket
func (w *WebSocketManager) connectPublic() error {
	conn, _, err := websocket.DefaultDialer.Dial(BitgetWSPublic, nil)
	if err != nil {
		return err
	}
	w.publicConn = conn
	logger.Info("✅ [Bitget WebSocket] 公共频道连接成功")
	return nil
}

// subscribeOrders 订阅订单更新
func (w *WebSocketManager) subscribeOrders(conn *websocket.Conn, symbol, instType string) error {
	subMsg := map[string]interface{}{
		"op": "subscribe",
		"args": []WSSubscribeArg{
			{
				InstType: instType,
				Channel:  "orders",
				InstId:   "default", // 订阅所有交易对
			},
		},
	}

	logger.Info("📡 [Bitget WS] 订阅私有频道: %s/orders (%s)", instType, symbol)
	return conn.WriteJSON(subMsg)
}

func (w *WebSocketManager) waitPrivateSubscription(conn *websocket.Conn, expectedInstType string, generation uint64) error {
	clearDeadlines := func() error {
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			return err
		}
		return conn.SetWriteDeadline(time.Time{})
	}
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		if string(message) == "pong" {
			continue
		}
		var msg struct {
			Event string          `json:"event"`
			Arg   WSSubscribeArg  `json:"arg"`
			Data  json.RawMessage `json:"data"`
			Code  json.RawMessage `json:"code"`
			Msg   string          `json:"msg"`
		}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}
		if msg.Event == "error" {
			return fmt.Errorf("订单订阅被拒绝: code=%s msg=%s", string(msg.Code), msg.Msg)
		}
		if msg.Event == "subscribe" && msg.Arg.Channel == "orders" {
			if !strings.EqualFold(msg.Arg.InstType, expectedInstType) {
				return fmt.Errorf("订单订阅确认产品类型不匹配: got=%s want=%s", msg.Arg.InstType, expectedInstType)
			}
			return clearDeadlines()
		}
		if msg.Arg.Channel == "orders" && len(msg.Data) > 0 {
			if !strings.EqualFold(msg.Arg.InstType, expectedInstType) {
				return fmt.Errorf("订单首帧产品类型不匹配: got=%s want=%s", msg.Arg.InstType, expectedInstType)
			}
			w.handleOrderUpdate(msg.Data, generation)
			return clearDeadlines()
		}
	}
}

// subscribeTicker 订阅价格更新
func (w *WebSocketManager) subscribeTicker(conn *websocket.Conn, symbol, instType string) error {
	subMsg := map[string]interface{}{
		"op": "subscribe",
		"args": []WSSubscribeArg{
			{
				InstType: instType,
				Channel:  "ticker",
				InstId:   symbol,
			},
		},
	}

	ttl := bitgetHandshakeTTL(w.handshakeTTL)
	if err := conn.SetWriteDeadline(time.Now().Add(ttl)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	return conn.WriteJSON(subMsg)
}

// handlePrivateMessages 处理私有频道消息（订单更新和成交明细）
func (w *WebSocketManager) handlePrivateMessages(ctx context.Context, conn *websocket.Conn, generation uint64, expectedInstType string) error {
	// 🔥 设置读取超时：90秒
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			_, message, err := conn.ReadMessage()
			if err != nil {
				logger.Warn("⚠️ [Bitget WebSocket] 读取私有消息失败: %v", err)
				// 🔥 关键：触发重连
				select {
				case w.privateReconnectChan <- struct{}{}:
				default:
				}
				return err
			}

			// 🔥 收到消息后更新读取超时
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))

			// 忽略 pong 响应
			if string(message) == "pong" {
				logger.Debug("💓 [Bitget WS私有] 收到 pong")
				continue
			}

			var msg struct {
				Event  string          `json:"event"`  // subscribe / error / login
				Op     string          `json:"op"`     // trade (下单响应)
				Action string          `json:"action"` // snapshot / update
				Arg    WSSubscribeArg  `json:"arg"`
				Data   json.RawMessage `json:"data"`
				Code   json.RawMessage `json:"code"`
				Msg    string          `json:"msg"`
			}

			if err := json.Unmarshal(message, &msg); err != nil {
				logger.Warn("⚠️ [Bitget WebSocket] 解析私有消息失败: %v", err)
				continue
			}

			// 🔍 调试：打印收到的消息类型
			logger.Debug("🔍 [Bitget WS私有] event=%s, op=%s, action=%s, channel=%s",
				msg.Event, msg.Op, msg.Action, msg.Arg.Channel)

			// 处理订阅确认
			if msg.Event == "subscribe" {
				logger.Debug("✅ [Bitget WS] 订阅成功: %s", msg.Arg.Channel)
				continue
			}

			// 处理错误消息
			if msg.Event == "error" {
				logger.Error("❌ [Bitget WS] 错误: %s", msg.Msg)
				return fmt.Errorf("私有订单流错误: %s", msg.Msg)
			}

			// 处理订单推送 (channel="orders")
			if msg.Arg.Channel == "orders" && len(msg.Data) > 0 {
				if !strings.EqualFold(msg.Arg.InstType, expectedInstType) {
					return fmt.Errorf("订单推送产品类型不匹配: got=%s want=%s", msg.Arg.InstType, expectedInstType)
				}
				logger.Debug("🔍 [Bitget WS订单] 推送数据: %s", string(msg.Data))
				w.handleOrderUpdate(msg.Data, generation)
				continue
			}
		}
	}
}

// handlePublicMessages 处理公共频道消息（价格更新）
func (w *WebSocketManager) handlePublicMessages(ctx context.Context, conn *websocket.Conn) {
	// 🔥 设置读取超时：90秒（大于3倍ping间隔）
	conn.SetReadDeadline(time.Now().Add(90 * time.Second))

	for {
		select {
		case <-ctx.Done():
			return
		default:
			_, message, err := conn.ReadMessage()
			if err != nil {
				logger.Warn("⚠️ [Bitget WebSocket] 读取公共消息失败: %v", err)
				// 🔥 关键：触发重连
				select {
				case w.publicReconnectChan <- struct{}{}:
				default:
				}
				return
			}

			// 🔥 收到消息后更新读取超时
			conn.SetReadDeadline(time.Now().Add(90 * time.Second))

			// 忽略 pong 响应
			if string(message) == "pong" {
				logger.Debug("💓 [Bitget WS公共] 收到 pong")
				continue
			}

			var msg struct {
				Arg    WSSubscribeArg  `json:"arg"`
				Action string          `json:"action"`
				Data   json.RawMessage `json:"data"`
			}

			if err := json.Unmarshal(message, &msg); err != nil {
				logger.Warn("⚠️ [Bitget WebSocket] 解析公共消息失败: %v", err)
				continue
			}

			// 处理价格更新
			// Bitget V2 推送格式: {"action":"snapshot","arg":{"instType":"USDT-FUTURES","channel":"ticker","instId":"ETHUSDT"},"data":[...]}
			if msg.Arg.Channel == "ticker" && len(msg.Data) > 0 {
				w.handlePriceUpdate(msg.Data)
			}
		}
	}
}

// handleOrderUpdate 处理订单更新
func (w *WebSocketManager) handleOrderUpdate(data json.RawMessage, generation uint64) {
	var updates []map[string]interface{}
	if err := json.Unmarshal(data, &updates); err != nil {
		logger.Warn("⚠️ [Bitget WebSocket] 解析订单更新失败: %v", err)
		return
	}

	//logger.Info("🔍 [Bitget WS] 收到 %d 条订单更新", len(updates))

	for _, update := range updates {
		// 🔍 调试：打印原始订单数据的关键字段
		orderID, _ := update["orderId"].(string)
		status, _ := update["status"].(string)
		side, _ := update["side"].(string)
		accBaseVolume, _ := update["accBaseVolume"].(string)

		// 🔥 关键诊断：如果订单被撤销，打印完整的原始数据
		if status == "cancelled" || status == "canceled" {
			//updateBytes, _ := json.Marshal(update)
			//logger.Warn("⚠️ [Bitget WS订单撤销] 完整数据: %s", string(updateBytes))
			//2025/12/07 20:46:12 [WARN] ⚠️ [Bitget WS订单撤销] 完整数据: {"accBaseVolume":"0","cTime":"1765101259950","cancelReason":"normal_cancel","clientOid":"sqt_302711_B_1765101259932571318","enterPointSource":"API","feeDetail":[{"fee":"0.00000000","feeCoin":"USDT"}],"force":"post_only","instId":"ETHUSDT","leverage":"10","marginCoin":"USDT","marginMode":"crossed","notionalUsd":"30.2711","orderId":"1381500303017938945","orderType":"limit","posMode":"hedge_mode","posSide":"long","presetStopLossExecutePrice":"","presetStopLossType":"","presetStopSurplusExecutePrice":"","presetStopSurplusType":"","price":"3027.11","reduceOnly":"no","side":"buy","size":"0.01","status":"canceled","stpMode":"none","totalProfits":"0","tradeSide":"open","uTime":"1765111572322"}
			logger.Warn("⚠️ [Bitget 订单被交易所撤销] ")
		}

		logger.Debug("🔍 [Bitget WS订单] ID=%s, 状态=%s, 方向=%s, 成交量=%s",
			orderID, status, side, accBaseVolume)

		w.mu.RLock()
		if w.privateGeneration != generation || !w.privateHandlerStarted || w.privateStopping {
			w.mu.RUnlock()
			return
		}
		callback := w.orderCallback
		w.mu.RUnlock()
		if callback != nil {
			// 转换为 OrderUpdate 格式
			orderUpdate := w.parseOrderUpdate(update)
			if orderUpdate != nil {
				logger.Debug("🔍 [Bitget WS订单] 解析后: ID=%d, Status=%s, ExecutedQty=%.4f",
					orderUpdate.OrderID, orderUpdate.Status, orderUpdate.ExecutedQty)
				callback(orderUpdate)
			}
		}
	}
}

// handlePriceUpdate 处理价格更新
func (w *WebSocketManager) handlePriceUpdate(data json.RawMessage) {
	var updates []map[string]interface{}
	if err := json.Unmarshal(data, &updates); err != nil {
		logger.Warn("⚠️ [Bitget WebSocket] 解析价格更新失败: %v", err)
		return
	}

	for _, update := range updates {
		// Bitget V2 Ticker 字段是 lastPr
		lastStr, ok := update["lastPr"].(string)
		if !ok {
			// 尝试兼容旧字段
			lastStr, ok = update["last"].(string)
		}

		if ok {
			price, _ := strconv.ParseFloat(lastStr, 64)
			if price > 0 {
				w.priceMu.Lock()
				w.latestPrice = price
				w.priceMu.Unlock()

				w.mu.RLock()
				callback := w.priceCallback
				w.mu.RUnlock()
				if callback != nil {
					// instId 是交易对名称
					symbol, _ := update["instId"].(string)
					callback(symbol, price)
				}
			}
		}
	}
}

// parseOrderUpdate 解析订单更新
func (w *WebSocketManager) parseOrderUpdate(data map[string]interface{}) *OrderUpdate {
	orderIDStr, _ := data["orderId"].(string)
	orderID, _ := strconv.ParseInt(orderIDStr, 10, 64)

	clientOrderID, _ := data["clientOid"].(string) // 🔥 解析 ClientOrderID

	symbol, _ := data["instId"].(string)
	sideStr, _ := data["side"].(string)
	statusStr, _ := data["status"].(string)
	priceStr, _ := data["price"].(string)
	qtyStr, _ := data["size"].(string)
	filledQtyStr, _ := data["accBaseVolume"].(string)
	avgPriceStr, _ := data["priceAvg"].(string)
	updateTimeStr, _ := data["uTime"].(string)
	tradeSideStr, _ := data["tradeSide"].(string)
	posSideStr, _ := data["posSide"].(string)
	realizedPNL := parseFlexibleFloat(data["totalProfits"])
	if realizedPNL == 0 {
		realizedPNL = parseFlexibleFloat(data["pnl"])
	}

	// 🔍 调试：打印关键字段的原始值
	logger.Debug("🔍 [parseOrderUpdate] accBaseVolume=%v (type=%T), priceAvg=%v (type=%T)",
		data["accBaseVolume"], data["accBaseVolume"], data["priceAvg"], data["priceAvg"])

	price, _ := strconv.ParseFloat(priceStr, 64)
	quantity, _ := strconv.ParseFloat(qtyStr, 64)
	executedQty, _ := strconv.ParseFloat(filledQtyStr, 64)
	avgPrice, _ := strconv.ParseFloat(avgPriceStr, 64)
	updateTime, _ := strconv.ParseInt(updateTimeStr, 10, 64)

	// 🔍 调试：打印解析后的值
	logger.Debug("🔍 [parseOrderUpdate] 解析结果: executedQty=%.4f, avgPrice=%.2f, Price=%.2f", executedQty, avgPrice, price)

	side := SideBuy
	lowerSide := strings.ToLower(strings.TrimSpace(sideStr))
	if lowerSide == "sell" {
		side = SideSell
	} else if lowerSide == "buy" {
		side = SideBuy
	} else {
		lowerTrade := strings.ToLower(strings.TrimSpace(tradeSideStr))
		lowerPos := strings.ToLower(strings.TrimSpace(posSideStr))
		if strings.Contains(lowerTrade, "close") || lowerPos == "short" {
			side = SideSell
		} else if strings.Contains(lowerTrade, "open") || lowerPos == "long" {
			side = SideBuy
		} else if lowerSide != "" {
			logger.Warn("⚠️ [Bitget WS] 未知 side 值: %s (tradeSide=%s, posSide=%s), 默认按买单处理", sideStr, tradeSideStr, posSideStr)
		}
	}

	// 🔥 关键修复：Bitget V2 WebSocket 订单推送的状态值
	// 根据官方文档：live=挂单中, partially_filled=部分成交, filled=完全成交, cancelled=已撤销
	var status OrderStatus = "NEW"
	switch statusStr {
	case "new", "live": // live 表示订单挂单中
		status = "NEW"
	case "partial_filled", "partial-fill", "partially_filled":
		status = "PARTIALLY_FILLED"
	case "filled", "full-fill":
		status = "FILLED"
	case "cancelled", "canceled":
		status = "CANCELED"
	default:
		// 🔍 如果遇到未知状态，记录日志
		logger.Warn("⚠️ [Bitget WS] 未知订单状态: %s, 订单ID: %s", statusStr, orderIDStr)
		status = OrderStatus(statusStr) // 保留原始状态
	}

	return &OrderUpdate{
		OrderID:       orderID,
		ClientOrderID: clientOrderID, // 🔥 包含 ClientOrderID
		Symbol:        symbol,
		Side:          side,
		Type:          OrderTypeLimit,
		Status:        status,
		Price:         price,
		Quantity:      quantity,
		ExecutedQty:   executedQty,
		AvgPrice:      avgPrice,
		UpdateTime:    updateTime,
		RealizedPNL:   realizedPNL, // Bitget totalProfits 为该订单累计已实现盈亏
	}
}

func parseFlexibleFloat(v interface{}) float64 {
	switch t := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(t, 64)
		return f
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	default:
		return 0
	}
}

// PlaceOrderWS 已废弃 - Bitget不支持WebSocket下单，请使用REST API
// 保留方法签名以兼容旧代码，但返回错误
func (w *WebSocketManager) PlaceOrderWS(symbol string, side string, price, quantity float64, priceDecimals int) (string, error) {
	return "", fmt.Errorf("Bitget不支持WebSocket下单，请使用REST API")
}

// GetLatestPrice 获取最新价格
func (w *WebSocketManager) GetLatestPrice() float64 {
	w.priceMu.RLock()
	defer w.priceMu.RUnlock()
	return w.latestPrice
}

// keepAlive WebSocket 保活（每15秒发送 ping）
func (w *WebSocketManager) keepAlive(ctx context.Context, conn *websocket.Conn, connType string, reconnectChan chan struct{}, stopC <-chan struct{}) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopC:
			return
		case <-ticker.C:
			if conn != nil {
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				err := conn.WriteMessage(websocket.TextMessage, []byte("ping"))
				if err != nil {
					logger.Warn("⚠️ [Bitget WS%s] 发送 ping 失败: %v", connType, err)
					// 🔥 关键：ping 失败说明连接已断开，触发重连并退出
					select {
					case reconnectChan <- struct{}{}:
					default:
					}
					_ = conn.Close()
					return
				}
				logger.Debug("💓 [Bitget WS%s] Ping已发送", connType)
			}
		}
	}
}

func (w *WebSocketManager) clearPrivateConn(doneC chan struct{}, generation uint64, conn *websocket.Conn) {
	w.mu.Lock()
	if w.privateDoneC == doneC && w.privateGeneration == generation && w.privateConn == conn {
		w.privateConn = nil
	}
	w.mu.Unlock()
}

func (w *WebSocketManager) setPrivateHealth(generation uint64, state streamhealth.State, err error) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.privateGeneration != generation || !w.privateHandlerStarted || w.privateStopping {
		return false
	}
	w.orderHealth.Set(state, err)
	return true
}

func normalizeBitgetInstType(productType string) string {
	return strings.ToUpper(strings.TrimSpace(productType))
}

// generateSign 生成签名
func (w *WebSocketManager) generateSign(timestamp, method, requestPath string) string {
	message := timestamp + method + requestPath
	mac := hmac.New(sha256.New, []byte(w.secretKey))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}
