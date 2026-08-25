package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"opensqt/exchange/streamhealth"
	"opensqt/logger"
	"opensqt/utils"

	"github.com/gorilla/websocket"
)

// WebSocketManager Gate.io WebSocket 管理器（用于交易和私有数据）
type WebSocketManager struct {
	apiKey    string
	secretKey string
	userID    int64
	signer    *Signer

	// 连接管理
	conn    *websocket.Conn
	mu      sync.RWMutex
	writeMu sync.Mutex

	// 回调函数
	orderCallback func(interface{})
	priceCallback func(string, float64) // symbol, price

	// 控制
	ctx        context.Context
	cancel     context.CancelFunc
	doneC      chan struct{}
	stopping   bool
	generation uint64

	// 价格缓存
	latestPrice float64
	priceMu     sync.RWMutex

	// 重连控制
	reconnectChan    chan struct{}
	reconnectDelay   time.Duration
	subscribedSymbol string // 记录订阅的交易对，用于重连后重新订阅
	settle           string // usdt 或 btc
	isAuthenticated  bool   // 标记是否已认证
	orderHealth      streamhealth.Tracker
	wsURL            string
	handshakeTTL     time.Duration
	pingInterval     time.Duration
	pongWait         time.Duration
	writeTTL         time.Duration
}

// NewWebSocketManager 创建 WebSocket 管理器
func NewWebSocketManager(apiKey, secretKey, settle string) *WebSocketManager {
	if settle == "" {
		settle = "usdt"
	}
	return &WebSocketManager{
		apiKey:         apiKey,
		secretKey:      secretKey,
		signer:         NewSigner(apiKey, secretKey),
		reconnectChan:  make(chan struct{}, 1),
		reconnectDelay: 5 * time.Second,
		settle:         settle,
		wsURL:          fmt.Sprintf("wss://fx-ws.gateio.ws/v4/ws/%s", settle),
		handshakeTTL:   10 * time.Second,
		pingInterval:   15 * time.Second,
		pongWait:       45 * time.Second,
		writeTTL:       10 * time.Second,
	}
}

// SetPriceCallback 设置价格回调
func (w *WebSocketManager) SetPriceCallback(callback func(string, float64)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.priceCallback = callback
}

// SetOrderCallback 设置订单回调
func (w *WebSocketManager) SetOrderCallback(callback func(interface{})) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.orderCallback = callback
}

// SetUserID 设置 Gate 私有 futures 频道 payload 必需的数值用户 ID。
func (w *WebSocketManager) SetUserID(userID int64) {
	w.mu.Lock()
	w.userID = userID
	w.mu.Unlock()
}

// IsRunning 检查 WebSocket 是否运行中
func (w *WebSocketManager) IsRunning() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.conn != nil
}

// GetLatestPrice 获取最新价格（从缓存）
func (w *WebSocketManager) GetLatestPrice() float64 {
	w.priceMu.RLock()
	defer w.priceMu.RUnlock()
	return w.latestPrice
}

// Start 启动 WebSocket（自动重连）
func (w *WebSocketManager) Start(ctx context.Context, symbol string, callbacks ...func(interface{})) error {
	if ctx == nil {
		return fmt.Errorf("WebSocket 上下文不能为空")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("WebSocket 上下文已结束: %w", err)
	}
	if len(callbacks) > 1 {
		return fmt.Errorf("订单流回调最多只能传入一个")
	}
	var callback func(interface{})
	callbackProvided := len(callbacks) == 1
	if callbackProvided {
		callback = callbacks[0]
		if callback == nil {
			return fmt.Errorf("订单流回调不能为空")
		}
	}
	w.mu.Lock()
	if w.ctx != nil {
		ready := w.orderHealth.Ready()
		state := w.orderHealth.State()
		if w.stopping || w.ctx.Err() != nil {
			w.mu.Unlock()
			return fmt.Errorf("WebSocket 正在退出（订单流状态=%s）", state)
		}
		if ready {
			if callbackProvided {
				w.orderCallback = callback
			}
			w.mu.Unlock()
			return nil
		}
		w.mu.Unlock()
		return fmt.Errorf("WebSocket 已在运行但订单流未就绪（状态=%s）", state)
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.ctx, w.cancel = runCtx, cancel
	w.doneC = make(chan struct{})
	doneC := w.doneC
	w.stopping = false
	w.generation++
	generation := w.generation
	w.subscribedSymbol = symbol
	if callbackProvided {
		w.orderCallback = callback
	}
	w.orderHealth.Set(streamhealth.StateStarting, nil)
	w.mu.Unlock()

	initialResultC := make(chan error, 1)
	go w.connectLoop(runCtx, doneC, generation, initialResultC)
	err := <-initialResultC
	if err != nil {
		<-doneC
	}
	return err
}

// connectLoop 连接循环（自动重连）
func (w *WebSocketManager) connectLoop(ctx context.Context, doneC chan struct{}, generation uint64, initialResultC chan<- error) {
	initialPending := true
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
		if w.ctx == ctx && w.doneC == doneC && w.generation == generation {
			w.conn = nil
			w.ctx = nil
			w.cancel = nil
			w.doneC = nil
			w.stopping = false
			w.isAuthenticated = false
			w.orderCallback = nil
			w.orderHealth.Set(streamhealth.StateStopped, nil)
		}
		w.mu.Unlock()
		close(doneC)
	}()

	for {
		select {
		case <-ctx.Done():
			logger.Info("✅ [Gate WS] 停止连接循环")
			return
		default:
		}

		logger.Info("🔗 [Gate WS] 正在连接...")

		// 连接 Gate.io WebSocket
		dialCtx, cancelDial := context.WithTimeout(ctx, gateHandshakeTTL(w.handshakeTTL))
		conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, w.wsURL, nil)
		cancelDial()
		if err != nil {
			if ctx.Err() != nil {
				reportInitial(ctx.Err())
				return
			}
			wrappedErr := fmt.Errorf("订单流连接失败: %w", err)
			if initialPending {
				w.setOrderHealth(generation, streamhealth.StateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setOrderHealth(generation, streamhealth.StateDegraded, wrappedErr)
			logger.Error("❌ [Gate WS] %v，%v后重试", wrappedErr, w.reconnectDelay)
			if !waitGateReconnect(ctx, w.reconnectDelay) {
				return
			}
			continue
		}

		w.mu.Lock()
		if w.generation != generation || w.doneC != doneC || w.stopping {
			w.mu.Unlock()
			_ = conn.Close()
			reportInitial(fmt.Errorf("订单流已停止"))
			return
		}
		w.conn = conn
		w.isAuthenticated = false
		symbol := w.subscribedSymbol
		w.mu.Unlock()

		logger.Info("✅ [Gate WS] 已连接")

		// Gate.io 不需要单独登录,直接在订阅时携带认证信息
		// 订阅频道
		err = w.subscribeChannels(conn, symbol)
		if err == nil {
			err = w.waitOrderSubscription(conn, generation)
		}
		if err != nil {
			_ = conn.Close()
			w.clearConn(generation, conn)
			if ctx.Err() != nil {
				reportInitial(ctx.Err())
				return
			}
			wrappedErr := fmt.Errorf("订单流订阅失败: %w", err)
			if initialPending {
				w.setOrderHealth(generation, streamhealth.StateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setOrderHealth(generation, streamhealth.StateDegraded, wrappedErr)
			logger.Error("❌ [Gate WS] %v", wrappedErr)
			if !waitGateReconnect(ctx, w.reconnectDelay) {
				return
			}
			continue
		}
		w.mu.Lock()
		if w.conn == conn && w.generation == generation && !w.stopping {
			w.isAuthenticated = true
		}
		w.mu.Unlock()
		if ctx.Err() != nil || !w.setOrderHealth(generation, streamhealth.StateReady, nil) {
			_ = conn.Close()
			stopErr := ctx.Err()
			if stopErr == nil {
				stopErr = fmt.Errorf("订单流已停止")
			}
			reportInitial(stopErr)
			return
		}
		reportInitial(nil)
		logger.Info("✅ [Gate WS] 私有订单频道已认证并订阅")

		// 启动 ping 和读取协程
		done := make(chan struct{})
		stopKeepAliveC := make(chan struct{})
		go func() {
			w.keepAlive(ctx, conn, stopKeepAliveC)
			close(done)
		}()

		// 启动读取循环（阻塞直到连接断开）
		streamErr := w.handleMessages(ctx, conn, generation)
		close(stopKeepAliveC)
		_ = conn.Close()
		if ctx.Err() == nil {
			if streamErr == nil {
				streamErr = fmt.Errorf("订单流连接已断开")
			}
			w.setOrderHealth(generation, streamhealth.StateDegraded, streamErr)
		}

		// 等待 keepAlive 退出
		<-done

		// 连接断开，清理
		w.mu.Lock()
		if w.conn == conn {
			w.conn = nil
			w.isAuthenticated = false
		}
		w.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		logger.Warn("⚠️ [Gate WS] 连接断开: %v，%v后重连...", streamErr, w.reconnectDelay)
		if !waitGateReconnect(ctx, w.reconnectDelay) {
			return
		}
	}
}

// login 登录认证
func (w *WebSocketManager) login() error {
	timestamp := time.Now().Unix()
	channel := "futures.login"
	event := "api"

	// 生成签名
	signature := w.signer.SignWebSocket(channel, event, timestamp)

	// 根据 Gate.io 官方文档,认证信息应该在 auth 字段中
	loginMsg := map[string]interface{}{
		"time":    timestamp,
		"channel": channel,
		"event":   event,
		"auth": map[string]interface{}{
			"method": "api_key",
			"KEY":    w.apiKey,
			"SIGN":   signature,
		},
		"req_header": map[string]string{
			"X-Gate-Channel-Id": GateChannelID, // 渠道返佣 ID
		},
	}

	w.mu.RLock()
	conn := w.conn
	w.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("连接未建立")
	}

	if err := w.writeJSON(conn, loginMsg, gateHandshakeTTL(w.handshakeTTL)); err != nil {
		return fmt.Errorf("发送登录消息失败: %w", err)
	}

	logger.Info("✅ [Gate WS] 已发送登录请求")
	return nil
}

// subscribeChannels 订阅频道
func (w *WebSocketManager) subscribeChannels(conn *websocket.Conn, symbol string) error {
	gateSymbol := convertToGateSymbol(symbol)
	timestamp := time.Now().Unix()
	w.mu.RLock()
	userID := w.userID
	handshakeTTL := gateHandshakeTTL(w.handshakeTTL)
	w.mu.RUnlock()
	if userID <= 0 {
		return fmt.Errorf("Gate futures 用户 ID 未就绪")
	}

	// 订阅订单更新（私有频道需要认证）
	ordersSign := w.signer.SignWebSocket("futures.orders", "subscribe", timestamp)
	ordersMsg := map[string]interface{}{
		"time":    timestamp,
		"channel": "futures.orders",
		"event":   "subscribe",
		"auth": map[string]interface{}{
			"method": "api_key",
			"KEY":    w.apiKey,
			"SIGN":   ordersSign,
		},
		"req_header": map[string]string{
			"X-Gate-Channel-Id": GateChannelID,
		},
		"payload": []any{userID, gateSymbol},
	}

	// 订阅余额更新（私有频道需要认证）
	balanceSign := w.signer.SignWebSocket("futures.balances", "subscribe", timestamp+1)
	balanceMsg := map[string]interface{}{
		"time":    timestamp + 1,
		"channel": "futures.balances",
		"event":   "subscribe",
		"auth": map[string]interface{}{
			"method": "api_key",
			"KEY":    w.apiKey,
			"SIGN":   balanceSign,
		},
		"req_header": map[string]string{
			"X-Gate-Channel-Id": GateChannelID,
		},
		"payload": []any{userID, "!all"},
	}

	// 订阅价格更新（ticker）
	tickerMsg := map[string]interface{}{
		"time":    timestamp + 2,
		"channel": "futures.tickers",
		"event":   "subscribe",
		"payload": []string{gateSymbol},
	}

	if conn == nil {
		return fmt.Errorf("连接未建立")
	}
	// 发送订阅消息
	if err := w.writeJSON(conn, ordersMsg, handshakeTTL); err != nil {
		return fmt.Errorf("订阅订单频道失败: %w", err)
	}

	if err := w.writeJSON(conn, balanceMsg, handshakeTTL); err != nil {
		return fmt.Errorf("订阅余额频道失败: %w", err)
	}

	if err := w.writeJSON(conn, tickerMsg, handshakeTTL); err != nil {
		return fmt.Errorf("订阅价格频道失败: %w", err)
	}

	logger.Info("✅ [Gate WS] 已订阅频道: orders, balances, tickers")
	return nil
}

func (w *WebSocketManager) waitOrderSubscription(conn *websocket.Conn, generation uint64) error {
	ttl := gateHandshakeTTL(w.handshakeTTL)
	if err := conn.SetReadDeadline(time.Now().Add(ttl)); err != nil {
		return err
	}
	defer conn.SetReadDeadline(time.Time{})

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var msg struct {
			Channel string         `json:"channel"`
			Event   string         `json:"event"`
			Result  map[string]any `json:"result"`
			Error   map[string]any `json:"error"`
		}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}
		if len(msg.Error) > 0 {
			return fmt.Errorf("订阅被拒绝: %v", msg.Error)
		}
		if msg.Event == "subscribe" && msg.Channel == "futures.orders" {
			status, _ := msg.Result["status"].(string)
			if status != "success" {
				return fmt.Errorf("订单频道订阅未成功: status=%q", status)
			}
			return nil
		}
		if err := w.handleMessage(message, generation); err != nil {
			return err
		}
	}
}

func gateHandshakeTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 10 * time.Second
	}
	return ttl
}

// keepAlive 保持连接活跃
func (w *WebSocketManager) keepAlive(ctx context.Context, conn *websocket.Conn, stopC <-chan struct{}) {
	w.mu.RLock()
	pingInterval := gatePingInterval(w.pingInterval)
	writeTTL := gateWriteTTL(w.writeTTL)
	w.mu.RUnlock()
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopC:
			return
		case <-ticker.C:
			w.mu.RLock()
			currentConn := w.conn
			w.mu.RUnlock()

			if currentConn != conn {
				return // 连接已更换，退出
			}

			// Gate.io 使用 ping 消息
			pingMsg := map[string]interface{}{
				"time":    time.Now().Unix(),
				"channel": "futures.ping",
			}

			if err := w.writeJSON(conn, pingMsg, writeTTL); err != nil {
				logger.Warn("⚠️ [Gate WS] Ping 失败: %v", err)
				_ = conn.Close()
				return
			}
		}
	}
}

// handleMessages 处理消息循环
func (w *WebSocketManager) handleMessages(ctx context.Context, conn *websocket.Conn, generation uint64) error {
	w.mu.RLock()
	pongWait := gatePongWait(w.pongWait)
	w.mu.RUnlock()
	if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		return fmt.Errorf("设置订单流读超时失败: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			logger.Warn("⚠️ [Gate WS] 读取消息失败: %v", err)
			return err
		}
		if err := conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
			return fmt.Errorf("刷新订单流读超时失败: %w", err)
		}

		if err := w.handleMessage(message, generation); err != nil {
			return err
		}
	}
}

// handleMessage 处理单条消息
func (w *WebSocketManager) handleMessage(message []byte, generation uint64) error {
	w.mu.RLock()
	currentGeneration := w.generation == generation && w.ctx != nil && !w.stopping
	w.mu.RUnlock()
	if !currentGeneration {
		return nil
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(message, &msg); err != nil {
		logger.Warn("⚠️ [Gate WS] 解析消息失败: %v", err)
		return nil
	}

	// 检查错误
	if errObj, ok := msg["error"].(map[string]interface{}); ok {
		logger.Error("❌ [Gate WS] 错误: %v", errObj)
		return fmt.Errorf("WebSocket 返回错误: %v", errObj)
	}

	// 处理不同类型的消息
	event, _ := msg["event"].(string)
	channel, _ := msg["channel"].(string)
	if channel == "futures.pong" {
		return nil
	}

	switch event {
	case "subscribe":
		// 订阅确认
		if result, ok := msg["result"].(map[string]interface{}); ok {
			if status, _ := result["status"].(string); status == "success" {
				logger.Info("✅ [Gate WS] 订阅成功: %s", channel)
			} else if channel == "futures.orders" {
				return fmt.Errorf("订单频道订阅失效: status=%q", status)
			}
		}

	case "update":
		// 数据更新
		switch channel {
		case "futures.orders":
			w.handleOrderUpdate(msg, generation)
		case "futures.balances":
			// 余额更新（可选实现）
			logger.Debug("[Gate WS] 余额更新")
		case "futures.tickers":
			w.handleTickerUpdate(msg, generation)
		}

	case "pong":
		// Pong 响应（静默处理）

	default:
		// 检查是否是登录响应
		if channel == "futures.login" {
			// Gate.io 登录响应在 header.status 中
			if header, ok := msg["header"].(map[string]interface{}); ok {
				status, _ := header["status"].(string)
				if status == "200" {
					w.mu.Lock()
					w.isAuthenticated = true
					w.mu.Unlock()
					logger.Info("✅ [Gate WS] 登录成功")
				} else {
					// 解析错误信息
					errMsg := status
					if data, ok := msg["data"].(map[string]interface{}); ok {
						if errs, ok := data["errs"].(map[string]interface{}); ok {
							if message, ok := errs["message"].(string); ok {
								errMsg = message
							}
						}
					}
					logger.Warn("⚠️ [Gate WS] 登录失败: %s", errMsg)
				}
			}
		} else {
			// 打印未处理的事件用于调试
			logger.Debug("[Gate WS] 未处理的事件: event=%s, channel=%s", event, channel)
		}
	}
	return nil
}

// handleOrderUpdate 处理订单更新
func (w *WebSocketManager) handleOrderUpdate(msg map[string]interface{}, generation uint64) {
	result, ok := msg["result"].([]interface{})
	if !ok || len(result) == 0 {
		return
	}

	for _, item := range result {
		orderData, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		// 解析订单数据
		orderID, _ := orderData["id"].(float64)
		contract, _ := orderData["contract"].(string)
		status, _ := orderData["status"].(string)
		size, _ := orderData["size"].(float64)
		left, _ := orderData["left"].(float64) // 未成交数量
		price, _ := parseFloat(orderData["price"])
		fillPrice, _ := parseFloat(orderData["fill_price"])
		text, _ := orderData["text"].(string)
		finishTime, _ := orderData["finish_time"].(float64)
		realizedPNL, _ := parseFloat(orderData["pnl"])

		// 使用统一的 utils 包去掉 Gate.io 的 t- 前缀
		clientOrderID := utils.RemoveBrokerPrefix("gate", text)

		// 计算成交数量 = 总数量 - 未成交数量
		executedQty := abs(size) - abs(left)
		if executedQty < 0 {
			executedQty = 0
		}

		// 转换为标准格式
		update := OrderUpdate{
			OrderID:       int64(orderID),
			ClientOrderID: clientOrderID,
			Symbol:        convertFromGateSymbol(contract),
			Side:          convertSide(size),
			Status:        convertStatus(status),
			Price:         price,
			Quantity:      abs(size),
			ExecutedQty:   executedQty, // 成交数量 = size - left
			AvgPrice:      fillPrice,
			UpdateTime:    int64(finishTime * 1000), // 转换为毫秒
			RealizedPNL:   realizedPNL,
		}

		w.mu.RLock()
		if w.generation != generation || w.ctx == nil || w.stopping {
			w.mu.RUnlock()
			return
		}
		callback := w.orderCallback
		w.mu.RUnlock()

		if callback != nil {
			callback(update)
		}
	}
}

// handleTickerUpdate 处理价格更新
func (w *WebSocketManager) handleTickerUpdate(msg map[string]interface{}, generation uint64) {
	result, ok := msg["result"].([]interface{})
	if !ok || len(result) == 0 {
		return
	}

	for _, item := range result {
		tickerData, ok := item.(map[string]interface{})
		if !ok {
			continue
		}

		contract, _ := tickerData["contract"].(string)
		last, _ := parseFloat(tickerData["last"])

		symbol := convertFromGateSymbol(contract)

		// 更新缓存
		w.priceMu.Lock()
		w.latestPrice = last
		w.priceMu.Unlock()

		// 触发回调
		w.mu.RLock()
		if w.generation != generation || w.ctx == nil || w.stopping {
			w.mu.RUnlock()
			return
		}
		callback := w.priceCallback
		w.mu.RUnlock()

		if callback != nil {
			callback(symbol, last)
		}
	}
}

// PlaceOrder 通过 WebSocket 下单（带渠道码）
func (w *WebSocketManager) PlaceOrder(order map[string]interface{}) error {
	timestamp := time.Now().Unix()

	// 🔥 重要：构造带渠道码的 Payload
	payload := map[string]interface{}{
		"req_header": map[string]string{
			"X-Gate-Channel-Id": GateChannelID, // 渠道返佣标识
		},
		"req_id":    fmt.Sprintf("order_%d", timestamp),
		"req_param": order,
	}

	orderMsg := map[string]interface{}{
		"time":    timestamp,
		"channel": "futures.order_place",
		"event":   "api",
		"payload": payload,
	}

	w.mu.RLock()
	conn := w.conn
	authenticated := w.isAuthenticated
	w.mu.RUnlock()

	if conn == nil {
		return fmt.Errorf("连接未建立")
	}

	if !authenticated {
		return fmt.Errorf("未认证")
	}

	w.mu.RLock()
	writeTTL := gateWriteTTL(w.writeTTL)
	w.mu.RUnlock()
	if err := w.writeJSON(conn, orderMsg, writeTTL); err != nil {
		return fmt.Errorf("发送下单消息失败: %w", err)
	}

	return nil
}

// Stop 停止 WebSocket
func (w *WebSocketManager) Stop() error {
	w.mu.Lock()
	if w.ctx == nil {
		w.mu.Unlock()
		return nil
	}
	w.stopping = true
	w.orderHealth.Set(streamhealth.StateStopping, nil)
	cancel := w.cancel
	doneC := w.doneC
	conn := w.conn
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if conn != nil {
		_ = conn.Close()
	}
	if doneC != nil {
		select {
		case <-doneC:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("Gate 订单流停止超时")
		}
	}
	return nil
}

func (w *WebSocketManager) clearConn(generation uint64, conn *websocket.Conn) {
	w.mu.Lock()
	if w.generation == generation && w.conn == conn {
		w.conn = nil
		w.isAuthenticated = false
	}
	w.mu.Unlock()
}

func (w *WebSocketManager) setOrderHealth(generation uint64, state streamhealth.State, err error) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.generation != generation || w.ctx == nil || w.stopping {
		return false
	}
	w.orderHealth.Set(state, err)
	return true
}

func (w *WebSocketManager) writeJSON(conn *websocket.Conn, value interface{}, ttl time.Duration) error {
	if conn == nil {
		return fmt.Errorf("连接未建立")
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if err := conn.SetWriteDeadline(time.Now().Add(gateWriteTTL(ttl))); err != nil {
		return err
	}
	err := conn.WriteJSON(value)
	clearErr := conn.SetWriteDeadline(time.Time{})
	if err != nil {
		return err
	}
	return clearErr
}

func gatePingInterval(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 15 * time.Second
	}
	return interval
}

func gatePongWait(wait time.Duration) time.Duration {
	if wait <= 0 {
		return 45 * time.Second
	}
	return wait
}

func gateWriteTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 10 * time.Second
	}
	return ttl
}

func waitGateReconnect(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		delay = time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// convertSide 根据 size 判断方向
func convertSide(size float64) Side {
	if size > 0 {
		return SideBuy
	}
	return SideSell
}

// convertStatus 转换订单状态
func convertStatus(status string) OrderStatus {
	switch status {
	case "open":
		return "NEW"
	case "finished":
		return "FILLED"
	default:
		return OrderStatus(status)
	}
}

// abs 返回绝对值
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
