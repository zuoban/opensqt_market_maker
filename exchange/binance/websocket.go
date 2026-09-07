package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"opensqt/logger"
	"opensqt/utils"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/gorilla/websocket"
)

// WebSocketManager 币安 WebSocket 订单流管理器
type WebSocketManager struct {
	client         *futures.Client
	apiKey         string
	secretKey      string
	listenKey      string
	doneC          chan struct{}
	cancel         context.CancelFunc
	mu             sync.RWMutex
	callbacks      []OrderUpdateCallback
	isRunning      bool
	state          OrderStreamState
	lastError      string
	expiredC       chan struct{}
	streamErrC     chan error
	expectedSymbol string

	startUserStreamFn func(context.Context) (string, error)
	keepAliveFn       func(context.Context, string) error
	serveUserStreamFn func(string, futures.WsUserDataHandler, futures.ErrHandler) (chan struct{}, chan struct{}, error)

	// 价格缓存
	latestPrice float64
	priceMu     sync.RWMutex

	// 时间配置
	reconnectDelay    time.Duration
	keepAliveInterval time.Duration
	closeTimeout      time.Duration
}

// OrderStreamState 描述 Binance 用户订单流当前所处的生命周期状态。
type OrderStreamState string

const (
	OrderStreamStateStopped  OrderStreamState = "STOPPED"
	OrderStreamStateStarting OrderStreamState = "STARTING"
	OrderStreamStateReady    OrderStreamState = "READY"
	OrderStreamStateDegraded OrderStreamState = "DEGRADED"
	OrderStreamStateStopping OrderStreamState = "STOPPING"
)

// OrderStreamHealth 是供上层交易门禁读取的线程安全健康快照。
type OrderStreamHealth struct {
	State     OrderStreamState
	Ready     bool
	LastError string
}

// NewWebSocketManager 创建 WebSocket 管理器
func NewWebSocketManager(apiKey, secretKey string) *WebSocketManager {
	w := &WebSocketManager{
		client:            futures.NewClient(apiKey, secretKey),
		apiKey:            apiKey,
		secretKey:         secretKey,
		callbacks:         make([]OrderUpdateCallback, 0),
		state:             OrderStreamStateStopped,
		reconnectDelay:    5 * time.Second,
		keepAliveInterval: 30 * time.Minute,
		closeTimeout:      10 * time.Second,
	}
	w.startUserStreamFn = func(ctx context.Context) (string, error) {
		return w.client.NewStartUserStreamService().Do(ctx)
	}
	w.keepAliveFn = func(ctx context.Context, listenKey string) error {
		return w.client.NewKeepaliveUserStreamService().ListenKey(listenKey).Do(ctx)
	}
	w.serveUserStreamFn = futures.WsUserDataServe
	return w
}

// SetExpectedSymbol 将账户级用户流限定到本策略交易对。其它人工订单事件会被忽略，
// 带 OpenSQT ClientOrderID 却落在错误交易对的事件会使订单流 fail-closed。
func (w *WebSocketManager) SetExpectedSymbol(symbol string) {
	w.mu.Lock()
	w.expectedSymbol = strings.ToUpper(strings.TrimSpace(symbol))
	w.mu.Unlock()
}

// Start 启动WebSocket连接
func (w *WebSocketManager) Start(ctx context.Context, callback OrderUpdateCallback) error {
	if ctx == nil {
		return fmt.Errorf("订单流上下文不能为空")
	}
	if callback == nil {
		return fmt.Errorf("订单流回调不能为空")
	}

	w.mu.Lock()
	if w.isRunning {
		w.mu.Unlock()
		return fmt.Errorf("订单流已在运行")
	}
	runCtx, cancel := context.WithCancel(ctx)
	runDoneC := make(chan struct{})
	expiredC := make(chan struct{}, 1)
	streamErrC := make(chan error, 1)
	initialResultC := make(chan error, 1)
	w.isRunning = true
	w.state = OrderStreamStateStarting
	w.lastError = ""
	w.listenKey = ""
	w.cancel = cancel
	w.doneC = runDoneC
	w.expiredC = expiredC
	w.streamErrC = streamErrC
	w.callbacks = []OrderUpdateCallback{callback}
	w.mu.Unlock()

	go w.runUserDataStream(runCtx, runDoneC, expiredC, streamErrC, initialResultC)

	// 只有 listenKey 获取成功且 WebSocket 握手完成，Start 才返回成功。
	err := <-initialResultC
	if err != nil {
		// 等待生命周期协程完成回滚，确保调用方可立即安全重试 Start。
		<-runDoneC
		return err
	}
	return nil
}

// StartPriceStream 启动价格流
func (w *WebSocketManager) StartPriceStream(ctx context.Context, symbol string, callback func(price float64)) error {
	if ctx == nil {
		return fmt.Errorf("价格流上下文不能为空")
	}
	if strings.TrimSpace(symbol) == "" {
		return fmt.Errorf("价格流交易对不能为空")
	}
	if callback == nil {
		return fmt.Errorf("价格流回调不能为空")
	}

	// 使用原生 WebSocket 连接（go-binance 的 WsAggTradeServe 有 Bug）
	// 新路由优先，旧地址保留为回退，避免 Binance 路由迁移时直接断流

	symbolLower := strings.ToLower(symbol)
	streamName := fmt.Sprintf("%s@aggTrade", symbolLower)
	urls := []string{
		fmt.Sprintf("wss://fstream.binance.com/market/ws/%s", streamName),
		fmt.Sprintf("wss://fstream.binance.com/ws/%s", streamName),
	}

	// 使用通道等待首个价格
	firstPriceCh := make(chan struct{})
	firstPriceReceived := false

	go func() {
		for {
			select {
			case <-ctx.Done():
				logger.Info("✅ [Binance] 价格流已停止")
				return
			default:
			}

			var (
				conn      *websocket.Conn
				activeURL string
				err       error
			)

			for i, url := range urls {
				logger.Debug("🔗 [Binance] 正在连接价格 WebSocket: %s", url)
				conn, _, err = websocket.DefaultDialer.DialContext(ctx, url, nil)
				if err == nil {
					activeURL = url
					if i > 0 {
						logger.Warn("⚠️ [Binance] 价格流已回退到兼容地址: %s", url)
					}
					break
				}
				logger.Warn("⚠️ [Binance] 价格 WebSocket 连接失败: %s, err=%v", url, err)
			}

			if conn == nil {
				if ctx.Err() != nil {
					logger.Info("✅ [Binance] 价格流已停止")
					return
				}
				logger.Error("❌ [Binance] 所有价格 WebSocket 地址连接失败，5秒后重试")
				if !waitPriceReconnect(ctx, 5*time.Second) {
					logger.Info("✅ [Binance] 价格流已停止")
					return
				}
				continue
			}

			// ReadMessage 会阻塞；上下文取消时主动关闭连接以唤醒读取，确保退出不挂住。
			cancelWatchDone := make(chan struct{})
			go func(activeConn *websocket.Conn) {
				select {
				case <-ctx.Done():
					_ = activeConn.Close()
				case <-cancelWatchDone:
				}
			}(conn)

			logger.Info("✅ [Binance] WebSocket 已连接: %s", activeURL) // 读取消息循环
			for {
				select {
				case <-ctx.Done():
					conn.Close()
					logger.Info("✅ [Binance] 价格流已停止")
					return
				default:
				}

				_, message, err := conn.ReadMessage()
				if err != nil {
					close(cancelWatchDone)
					if ctx.Err() != nil {
						logger.Info("✅ [Binance] 价格流已停止")
						return
					}
					logger.Warn("⚠️ [Binance] WebSocket 读取错误: %v，正在重连", err)
					_ = conn.Close()
					if !waitPriceReconnect(ctx, 2*time.Second) {
						logger.Info("✅ [Binance] 价格流已停止")
						return
					}
					break // 跳出内层循环，重新连接
				}

				// 解析消息（只提取必要字段）
				var event struct {
					Symbol string `json:"s"`
					Price  string `json:"p"`
				}

				if err := json.Unmarshal(message, &event); err != nil {
					logger.Debug("解析消息失败: %v", err)
					continue
				}
				if !strings.EqualFold(strings.TrimSpace(event.Symbol), strings.TrimSpace(symbol)) {
					logger.Warn("⚠️ [Binance] 忽略交易对不匹配的价格推送: got=%q, want=%q", event.Symbol, symbol)
					continue
				}

				price, err := parseFiniteFloat("aggTrade.price", event.Price)
				if err != nil || price <= 0 {
					logger.Debug("解析价格失败: %v", err)
					continue
				} // 更新价格缓存
				w.priceMu.Lock()
				w.latestPrice = price
				w.priceMu.Unlock()

				// 通知首个价格已接收
				if !firstPriceReceived {
					firstPriceReceived = true
					logger.Debug("✅ [Binance] 收到首个价格: %.2f", price)
					close(firstPriceCh)
				}

				// 调用回调
				callback(price)
			}
		}
	}()

	// 等待接收首个价格（最多10秒）
	select {
	case <-firstPriceCh:
		logger.Debug("✅ [Binance] 价格流已启动: %s", streamName)
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("等待首个价格超时（10秒）")
	case <-ctx.Done():
		return fmt.Errorf("上下文已取消")
	}
}

func waitPriceReconnect(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Ready 返回订单流是否已经完成握手且可供上层交易门禁放行。
func (w *WebSocketManager) Ready() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.isRunning && w.state == OrderStreamStateReady
}

// Health 返回订单流当前的线程安全健康快照。
func (w *WebSocketManager) Health() OrderStreamHealth {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return OrderStreamHealth{
		State:     w.state,
		Ready:     w.isRunning && w.state == OrderStreamStateReady,
		LastError: w.lastError,
	}
}

// Stop 停止订单流。取消函数和等待动作都在锁外执行，避免与订单回调互锁。
func (w *WebSocketManager) Stop() {
	w.mu.Lock()
	if !w.isRunning {
		w.mu.Unlock()
		return
	}
	w.state = OrderStreamStateStopping
	cancel := w.cancel
	doneC := w.doneC
	closeTimeout := w.closeTimeout
	w.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	if closeTimeout <= 0 {
		closeTimeout = 10 * time.Second
	}
	timer := time.NewTimer(closeTimeout)
	defer timer.Stop()
	select {
	case <-doneC:
		logger.Info("✅ [Binance] 订单流已停止")
	case <-timer.C:
		logger.Warn("⚠️ [Binance] 订单流停止超时")
	}
}

// runUserDataStream 管理 listenKey、WebSocket 和重连的完整生命周期。
func (w *WebSocketManager) runUserDataStream(
	ctx context.Context,
	runDoneC chan struct{},
	expiredC <-chan struct{},
	streamErrC <-chan error,
	initialResultC chan<- error,
) {
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
		w.finishRun(runDoneC)
	}()

	for {
		if err := ctx.Err(); err != nil {
			reportInitial(err)
			return
		}

		listenKey, err := w.startUserStreamFn(ctx)
		if err != nil {
			if ctx.Err() != nil {
				reportInitial(ctx.Err())
				return
			}
			wrappedErr := fmt.Errorf("获取listenKey失败: %w", err)
			if initialPending {
				w.setRunState(runDoneC, OrderStreamStateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setRunState(runDoneC, OrderStreamStateDegraded, wrappedErr)
			logger.Error("❌ [Binance] %v，等待重新获取", wrappedErr)
			if !w.waitReconnect(ctx) {
				return
			}
			continue
		}

		w.setListenKey(runDoneC, listenKey)
		logger.Debug("✅ [Binance] 已获取订单流listenKey（已隐藏）")
		w.drainExpired(expiredC)
		w.drainStreamErrors(streamErrC)
		if ctx.Err() != nil {
			reportInitial(ctx.Err())
			return
		}
		logger.Info("🔗 [Binance] 连接WebSocket订单流...")

		// SDK 可能在连接关闭后仍迟到派发事件。为每一代订单流捕获独立 token，
		// 防止 Stop -> Start 后旧连接污染新一代回调或健康状态。
		eventHandler := func(event *futures.WsUserDataEvent) {
			w.handleUserDataEventForRun(runDoneC, event)
		}
		errHandler := func(streamErr error) {
			w.handleErrorForRun(runDoneC, streamErr)
		}
		connectionDoneC, connectionStopC, err := w.serveUserStreamFn(listenKey, eventHandler, errHandler)
		if err == nil && (connectionDoneC == nil || connectionStopC == nil) {
			err = fmt.Errorf("WebSocket连接返回了无效生命周期通道")
		}
		if err == nil && ctx.Err() != nil {
			w.stopConnection(connectionDoneC, connectionStopC)
			reportInitial(ctx.Err())
			return
		}
		if err != nil {
			if ctx.Err() != nil {
				reportInitial(ctx.Err())
				return
			}
			wrappedErr := fmt.Errorf("WebSocket订单流握手失败: %w", err)
			if initialPending {
				w.setRunState(runDoneC, OrderStreamStateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setRunState(runDoneC, OrderStreamStateDegraded, wrappedErr)
			logger.Error("❌ [Binance] %v", wrappedErr)
			if !w.waitReconnect(ctx) {
				return
			}
			continue
		}

		w.setRunState(runDoneC, OrderStreamStateReady, nil)
		logger.Info("✅ [Binance] WebSocket订单流已连接并就绪")
		reportInitial(nil)

		err = w.monitorConnection(ctx, runDoneC, listenKey, expiredC, streamErrC, connectionDoneC, connectionStopC)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("WebSocket订单流连接已断开")
		}
		w.setRunState(runDoneC, OrderStreamStateDegraded, err)
		logger.Warn("⚠️ [Binance] %v，等待重建订单流...", err)
		if !w.waitReconnect(ctx) {
			return
		}
	}
}

func (w *WebSocketManager) monitorConnection(
	ctx context.Context,
	runDoneC chan struct{},
	listenKey string,
	expiredC <-chan struct{},
	streamErrC <-chan error,
	doneC, stopC chan struct{},
) error {
	keepAliveInterval := w.keepAliveInterval
	if keepAliveInterval <= 0 {
		keepAliveInterval = 30 * time.Minute
	}
	ticker := time.NewTicker(keepAliveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.stopConnection(doneC, stopC)
			return ctx.Err()
		case <-expiredC:
			if ctx.Err() != nil {
				w.stopConnection(doneC, stopC)
				return ctx.Err()
			}
			err := fmt.Errorf("listenKey已失效")
			w.setRunState(runDoneC, OrderStreamStateDegraded, err)
			w.stopConnection(doneC, stopC)
			return err
		case streamErr := <-streamErrC:
			if ctx.Err() != nil {
				w.stopConnection(doneC, stopC)
				return ctx.Err()
			}
			if streamErr == nil {
				streamErr = fmt.Errorf("订单流数据异常")
			}
			w.setRunState(runDoneC, OrderStreamStateDegraded, streamErr)
			w.stopConnection(doneC, stopC)
			return streamErr
		case <-doneC:
			return fmt.Errorf("WebSocket订单流连接已断开")
		case <-ticker.C:
			if err := w.keepAliveFn(ctx, listenKey); err != nil {
				if ctx.Err() != nil {
					w.stopConnection(doneC, stopC)
					return ctx.Err()
				}
				wrappedErr := fmt.Errorf("listenKey保活失败: %w", err)
				w.setRunState(runDoneC, OrderStreamStateDegraded, wrappedErr)
				w.stopConnection(doneC, stopC)
				return wrappedErr
			}
			logger.Debug("✅ [Binance] listenKey保活成功")
		}
	}
}

func (w *WebSocketManager) stopConnection(doneC, stopC chan struct{}) {
	close(stopC)
	closeTimeout := w.closeTimeout
	if closeTimeout <= 0 {
		closeTimeout = 10 * time.Second
	}
	timer := time.NewTimer(closeTimeout)
	defer timer.Stop()
	select {
	case <-doneC:
	case <-timer.C:
		logger.Warn("⚠️ [Binance] WebSocket订单连接关闭超时")
	}
}

func (w *WebSocketManager) waitReconnect(ctx context.Context) bool {
	delay := w.reconnectDelay
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
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

func (w *WebSocketManager) drainExpired(expiredC <-chan struct{}) {
	for {
		select {
		case <-expiredC:
		default:
			return
		}
	}
}

func (w *WebSocketManager) drainStreamErrors(streamErrC <-chan error) {
	for {
		select {
		case <-streamErrC:
		default:
			return
		}
	}
}

func (w *WebSocketManager) setListenKey(runDoneC chan struct{}, listenKey string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.doneC == runDoneC {
		w.listenKey = listenKey
	}
}

func (w *WebSocketManager) setRunState(runDoneC chan struct{}, state OrderStreamState, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.doneC != runDoneC {
		return
	}
	w.state = state
	if err != nil {
		w.lastError = err.Error()
	} else if state == OrderStreamStateReady || state == OrderStreamStateStarting {
		w.lastError = ""
	}
}

func (w *WebSocketManager) finishRun(runDoneC chan struct{}) {
	w.mu.Lock()
	if w.doneC == runDoneC {
		w.isRunning = false
		w.state = OrderStreamStateStopped
		w.listenKey = ""
		w.cancel = nil
		w.expiredC = nil
		w.streamErrC = nil
		w.callbacks = nil
	}
	w.mu.Unlock()
	close(runDoneC)
}

func (w *WebSocketManager) signalStreamError(runDoneC chan struct{}, err error) {
	if err == nil {
		err = fmt.Errorf("订单流数据异常")
	}
	w.mu.Lock()
	if runDoneC == nil || w.doneC != runDoneC || !w.isRunning ||
		w.state == OrderStreamStateStopping || w.state == OrderStreamStateStopped {
		w.mu.Unlock()
		return
	}
	w.state = OrderStreamStateDegraded
	w.lastError = err.Error()
	streamErrC := w.streamErrC
	w.mu.Unlock()

	if streamErrC != nil {
		select {
		case streamErrC <- err:
		default:
		}
	}
}

func (w *WebSocketManager) signalListenKeyExpired(runDoneC chan struct{}) {
	err := fmt.Errorf("listenKey已失效")
	w.mu.Lock()
	if runDoneC == nil || w.doneC != runDoneC || !w.isRunning ||
		w.state == OrderStreamStateStopping || w.state == OrderStreamStateStopped {
		w.mu.Unlock()
		return
	}
	w.state = OrderStreamStateDegraded
	w.lastError = err.Error()
	expiredC := w.expiredC
	w.mu.Unlock()

	if expiredC != nil {
		select {
		case expiredC <- struct{}{}:
		default:
		}
	}
}

// handleUserDataEvent 处理用户数据事件
func (w *WebSocketManager) handleUserDataEvent(event *futures.WsUserDataEvent) {
	w.mu.RLock()
	runDoneC := w.doneC
	w.mu.RUnlock()
	w.handleUserDataEventForRun(runDoneC, event)
}

func (w *WebSocketManager) handleUserDataEventForRun(runDoneC chan struct{}, event *futures.WsUserDataEvent) {
	if event == nil {
		return
	}
	w.mu.RLock()
	currentRun := runDoneC != nil && w.doneC == runDoneC && w.isRunning &&
		w.state != OrderStreamStateStopping && w.state != OrderStreamStateStopped
	w.mu.RUnlock()
	if !currentRun {
		return
	}
	if event.Event == futures.UserDataEventTypeListenKeyExpired {
		logger.Warn("⚠️ [Binance] 收到listenKey过期事件，订单流进入降级并重建")
		w.signalListenKeyExpired(runDoneC)
		return
	}
	if event.Event != futures.UserDataEventTypeOrderTradeUpdate {
		return
	}
	order := event.OrderTradeUpdate
	_, strategyOrder := strategyOrderSide(order.ClientOrderID)
	if strings.TrimSpace(order.ClientOrderID) != "" && !strategyOrder {
		logger.Debug("⏭️ [Binance] 忽略非 OpenSQT 订单推送: ID=%d, ClientOID=%s", order.ID, order.ClientOrderID)
		return
	}

	update, err := parseOrderTradeUpdate(order)
	if err != nil {
		wrappedErr := fmt.Errorf("Binance ORDER_TRADE_UPDATE 无效: %w", err)
		logger.Error("❌ [Binance] %v", wrappedErr)
		w.signalStreamError(runDoneC, wrappedErr)
		return
	}
	w.mu.RLock()
	expectedSymbol := w.expectedSymbol
	currentRun = w.doneC == runDoneC && w.isRunning &&
		w.state != OrderStreamStateStopping && w.state != OrderStreamStateStopped
	w.mu.RUnlock()
	if !currentRun {
		return
	}
	if expectedSymbol != "" && !strings.EqualFold(update.Symbol, expectedSymbol) {
		wrappedErr := fmt.Errorf("Binance OpenSQT 订单交易对不匹配: got=%q, want=%q", update.Symbol, expectedSymbol)
		logger.Error("❌ [Binance] %v", wrappedErr)
		w.signalStreamError(runDoneC, wrappedErr)
		return
	}

	// 🔍 调试日志：记录收到的订单更新
	logger.Debug("🔍 [WebSocket回调] 收到订单更新: ID=%d, ClientOID=%s, Side=%s, Status=%s, ExecutedQty=%.4f, Price=%.2f",
		update.OrderID, update.ClientOrderID, update.Side, update.Status, update.ExecutedQty, update.Price)

	// 调用所有注册的回调
	w.mu.RLock()
	currentRun = w.doneC == runDoneC && w.isRunning &&
		w.state != OrderStreamStateStopping && w.state != OrderStreamStateStopped
	callbacks := append([]OrderUpdateCallback(nil), w.callbacks...)
	w.mu.RUnlock()
	if !currentRun {
		return
	}

	for _, callback := range callbacks {
		callback(update)
	}
}

func parseOrderTradeUpdate(order futures.WsOrderTradeUpdate) (OrderUpdate, error) {
	if order.ID <= 0 {
		return OrderUpdate{}, fmt.Errorf("orderId=%d 无效", order.ID)
	}
	if strings.TrimSpace(order.Symbol) == "" {
		return OrderUpdate{}, fmt.Errorf("symbol 为空")
	}
	if strings.TrimSpace(order.ClientOrderID) == "" {
		return OrderUpdate{}, fmt.Errorf("clientOrderId 为空")
	}
	if order.Side != futures.SideTypeBuy && order.Side != futures.SideTypeSell {
		return OrderUpdate{}, fmt.Errorf("side=%q 无效", order.Side)
	}
	if order.Type != futures.OrderTypeLimit {
		return OrderUpdate{}, fmt.Errorf("type=%q 不是 OpenSQT LIMIT 订单", order.Type)
	}
	switch order.ExecutionType {
	case futures.OrderExecutionTypeNew, futures.OrderExecutionTypeTrade,
		futures.OrderExecutionTypeCanceled, futures.OrderExecutionTypeExpired,
		futures.OrderExecutionTypePartialFill, futures.OrderExecutionTypeFill:
	default:
		return OrderUpdate{}, fmt.Errorf("executionType=%q 无法安全处理", order.ExecutionType)
	}
	if order.TimeInForce != futures.TimeInForceTypeGTX {
		return OrderUpdate{}, fmt.Errorf("timeInForce=%q 不是严格 PostOnly GTX", order.TimeInForce)
	}
	if order.PositionSide != futures.PositionSideTypeBoth {
		return OrderUpdate{}, fmt.Errorf("positionSide=%q 不是单向持仓模式 BOTH", order.PositionSide)
	}
	if order.TradeTime <= 0 {
		return OrderUpdate{}, fmt.Errorf("tradeTime=%d 无效", order.TradeTime)
	}
	clientSide, strategyOrder := strategyOrderSide(order.ClientOrderID)
	if !strategyOrder {
		return OrderUpdate{}, fmt.Errorf("clientOrderId=%q 不是有效的 OpenSQT 订单", order.ClientOrderID)
	}
	if clientSide != string(order.Side) {
		return OrderUpdate{}, fmt.Errorf("clientOrderId 方向=%s 与推送 side=%s 不一致", clientSide, order.Side)
	}
	if order.Side == futures.SideTypeSell && !order.IsReduceOnly {
		return OrderUpdate{}, fmt.Errorf("OpenSQT SELL 订单缺少 reduceOnly")
	}
	if order.Side == futures.SideTypeBuy && order.IsReduceOnly {
		return OrderUpdate{}, fmt.Errorf("OpenSQT BUY 订单不应设置 reduceOnly")
	}

	status := OrderStatus(order.Status)
	switch status {
	case OrderStatusNew, OrderStatusPartiallyFilled, OrderStatusFilled,
		OrderStatusCanceled, OrderStatusRejected, OrderStatusExpired:
	case OrderStatus("EXPIRED_IN_MATCH"):
		// Binance 的 STP 终止状态对槽位而言等价于 EXPIRED，必须释放而不能静默忽略。
		status = OrderStatusExpired
	default:
		return OrderUpdate{}, fmt.Errorf("status=%q 无法安全处理", order.Status)
	}

	quantity, err := parseFiniteFloat("originalQty", order.OriginalQty)
	if err != nil || quantity <= 0 {
		if err == nil {
			err = fmt.Errorf("必须大于 0")
		}
		return OrderUpdate{}, fmt.Errorf("originalQty=%q 无效: %w", order.OriginalQty, err)
	}
	executedQty, err := parseFiniteFloat("accumulatedFilledQty", order.AccumulatedFilledQty)
	if err != nil || executedQty < 0 {
		if err == nil {
			err = fmt.Errorf("不能小于 0")
		}
		return OrderUpdate{}, fmt.Errorf("accumulatedFilledQty=%q 无效: %w", order.AccumulatedFilledQty, err)
	}
	if executedQty > quantity+1e-12 {
		return OrderUpdate{}, fmt.Errorf("累计成交量 %.12f 超过原始数量 %.12f", executedQty, quantity)
	}
	if (status == OrderStatusPartiallyFilled || status == OrderStatusFilled) && executedQty <= 0 {
		return OrderUpdate{}, fmt.Errorf("status=%s 但累计成交量为 0", status)
	}
	if status == OrderStatusFilled && executedQty+1e-12 < quantity {
		return OrderUpdate{}, fmt.Errorf("status=FILLED 但累计成交量 %.12f 小于原始数量 %.12f", executedQty, quantity)
	}
	if status == OrderStatusPartiallyFilled && executedQty+1e-12 >= quantity {
		return OrderUpdate{}, fmt.Errorf("status=PARTIALLY_FILLED 但累计成交量 %.12f 未小于原始数量 %.12f", executedQty, quantity)
	}

	lastFilledQty, err := parseFiniteFloat("lastFilledQty", order.LastFilledQty)
	if err != nil || lastFilledQty < 0 {
		if err == nil {
			err = fmt.Errorf("不能小于 0")
		}
		return OrderUpdate{}, fmt.Errorf("lastFilledQty=%q 无效: %w", order.LastFilledQty, err)
	}
	if lastFilledQty > executedQty+1e-12 {
		return OrderUpdate{}, fmt.Errorf("本次成交量 %.12f 超过累计成交量 %.12f", lastFilledQty, executedQty)
	}

	price, err := parseFiniteFloat("originalPrice", order.OriginalPrice)
	if err != nil || price <= 0 {
		if err == nil {
			err = fmt.Errorf("必须大于 0")
		}
		return OrderUpdate{}, fmt.Errorf("originalPrice=%q 无效: %w", order.OriginalPrice, err)
	}
	avgPrice, err := parseFiniteFloat("averagePrice", order.AveragePrice)
	if err != nil || avgPrice < 0 {
		if err == nil {
			err = fmt.Errorf("不能小于 0")
		}
		return OrderUpdate{}, fmt.Errorf("averagePrice=%q 无效: %w", order.AveragePrice, err)
	}
	if executedQty > 0 && avgPrice <= 0 {
		return OrderUpdate{}, fmt.Errorf("已有成交 %.12f 但 averagePrice=%q", executedQty, order.AveragePrice)
	}
	lastFilledPrice, err := parseFiniteFloat("lastFilledPrice", order.LastFilledPrice)
	if err != nil || lastFilledPrice < 0 {
		if err == nil {
			err = fmt.Errorf("不能小于 0")
		}
		return OrderUpdate{}, fmt.Errorf("lastFilledPrice=%q 无效: %w", order.LastFilledPrice, err)
	}
	if lastFilledQty > 0 && lastFilledPrice <= 0 {
		return OrderUpdate{}, fmt.Errorf("本次成交量 %.12f 大于 0 但 lastFilledPrice=%q", lastFilledQty, order.LastFilledPrice)
	}
	if order.ExecutionType == futures.OrderExecutionTypeTrade {
		if lastFilledQty <= 0 {
			return OrderUpdate{}, fmt.Errorf("executionType=TRADE 但 lastFilledQty=%q", order.LastFilledQty)
		}
		if order.TradeID <= 0 {
			return OrderUpdate{}, fmt.Errorf("executionType=TRADE 但 tradeId=%d 无效", order.TradeID)
		}
	}
	realizedPNL, err := parseFiniteFloat("realizedPnL", order.RealizedPnL)
	if err != nil {
		return OrderUpdate{}, err
	}
	if strings.TrimSpace(order.Commission) != "" {
		if _, err := parseFiniteFloat("commission", order.Commission); err != nil {
			return OrderUpdate{}, err
		}
	}

	return OrderUpdate{
		OrderID:                order.ID,
		ClientOrderID:          order.ClientOrderID,
		Symbol:                 order.Symbol,
		Status:                 status,
		Quantity:               quantity,
		ExecutedQty:            executedQty,
		Price:                  price,
		AvgPrice:               avgPrice,
		Side:                   Side(order.Side),
		Type:                   OrderType(order.Type),
		UpdateTime:             order.TradeTime,
		RealizedPNL:            realizedPNL,
		RealizedPNLIncremental: true, // Binance rp 是本笔成交利润
	}, nil
}

func strategyOrderSide(clientOrderID string) (string, bool) {
	cleanID := utils.RemoveBinanceBrokerPrefix(strings.TrimSpace(clientOrderID))
	_, side, _, valid := utils.ParseOrderID(cleanID, 0)
	return side, valid
}

// handleError 处理错误
func (w *WebSocketManager) handleError(err error) {
	w.mu.RLock()
	runDoneC := w.doneC
	w.mu.RUnlock()
	w.handleErrorForRun(runDoneC, err)
}

func (w *WebSocketManager) handleErrorForRun(runDoneC chan struct{}, err error) {
	w.mu.RLock()
	currentRun := runDoneC != nil && w.doneC == runDoneC && w.isRunning &&
		w.state != OrderStreamStateStopping && w.state != OrderStreamStateStopped
	w.mu.RUnlock()
	if !currentRun {
		return
	}
	logger.Error("❌ [Binance] WebSocket错误: %v", err)
	if err != nil {
		w.signalStreamError(runDoneC, fmt.Errorf("Binance WebSocket 订单流错误: %w", err))
	}
}

// GetLatestPrice 获取最新价格（从缓存读取）
func (w *WebSocketManager) GetLatestPrice() float64 {
	w.priceMu.RLock()
	defer w.priceMu.RUnlock()
	return w.latestPrice
}
