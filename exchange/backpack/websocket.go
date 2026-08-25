package backpack

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"opensqt/exchange/streamhealth"
	"opensqt/logger"

	"github.com/gorilla/websocket"
)

type WebSocketManager struct {
	client        *Client
	marketSymbol  string
	displaySymbol string
	idMapper      *clientIDMapper
	priceDecimals int

	mu              sync.RWMutex
	callbacks       []OrderUpdateCallback
	orderStopC      chan struct{}
	orderDoneC      chan struct{}
	orderConn       *websocket.Conn
	orderActive     bool
	orderStopping   bool
	orderGeneration uint64
	orderHealth     streamhealth.Tracker

	priceMu      sync.RWMutex
	latestPrice  float64
	reconnectGap time.Duration
	handshakeTTL time.Duration
	orderURL     string
	orderDial    func(context.Context, string) (*websocket.Conn, error)
}

func NewWebSocketManager(client *Client, marketSymbol, displaySymbol string, idMapper *clientIDMapper, priceDecimals int) *WebSocketManager {
	w := &WebSocketManager{
		client:        client,
		marketSymbol:  marketSymbol,
		displaySymbol: displaySymbol,
		idMapper:      idMapper,
		priceDecimals: priceDecimals,
		reconnectGap:  5 * time.Second,
		handshakeTTL:  10 * time.Second,
		orderURL:      backpackWSURL,
	}
	w.orderDial = func(ctx context.Context, url string) (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
		return conn, err
	}
	return w
}

func (w *WebSocketManager) Start(ctx context.Context, callback OrderUpdateCallback) error {
	if ctx == nil {
		return fmt.Errorf("订单流上下文不能为空")
	}
	if callback == nil {
		return fmt.Errorf("订单流回调不能为空")
	}

	w.mu.Lock()
	if w.orderActive {
		if w.orderStopping || !w.orderHealth.Ready() {
			state := w.orderHealth.State()
			w.mu.Unlock()
			return fmt.Errorf("订单流已在运行但未就绪（状态=%s）", state)
		}
		w.callbacks = append(w.callbacks, callback)
		w.mu.Unlock()
		return nil
	}

	w.callbacks = []OrderUpdateCallback{callback}
	w.orderStopC = make(chan struct{})
	w.orderDoneC = make(chan struct{})
	w.orderActive = true
	w.orderStopping = false
	w.orderGeneration++
	generation := w.orderGeneration
	stopC := w.orderStopC
	doneC := w.orderDoneC
	initialResultC := make(chan error, 1)
	w.orderHealth.Set(streamhealth.StateStarting, nil)
	w.mu.Unlock()

	go w.runOrderLoop(ctx, stopC, doneC, generation, initialResultC)
	err := <-initialResultC
	if err != nil {
		<-doneC
	}
	return err
}

func (w *WebSocketManager) Stop() {
	w.mu.Lock()
	if !w.orderActive {
		w.mu.Unlock()
		return
	}
	if w.orderStopping {
		doneC := w.orderDoneC
		w.mu.Unlock()
		w.waitOrderStopped(doneC)
		return
	}
	stopC := w.orderStopC
	doneC := w.orderDoneC
	conn := w.orderConn
	w.orderStopping = true
	w.orderHealth.Set(streamhealth.StateStopping, nil)
	w.mu.Unlock()

	close(stopC)
	if conn != nil {
		_ = conn.Close()
	}
	w.waitOrderStopped(doneC)
}

func (w *WebSocketManager) waitOrderStopped(doneC <-chan struct{}) {
	if doneC == nil {
		return
	}
	select {
	case <-doneC:
	case <-time.After(10 * time.Second):
		logger.Warn("⚠️ [Backpack] 停止订单流超时")
	}
}

func (w *WebSocketManager) StartPriceStream(ctx context.Context, marketSymbol string, callback func(price float64)) error {
	firstPriceCh := make(chan struct{})
	var once sync.Once

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			conn, _, err := websocket.DefaultDialer.Dial(backpackWSURL, nil)
			if err != nil {
				logger.Warn("⚠️ [Backpack] 价格流连接失败: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(w.reconnectGap):
				}
				continue
			}

			subscribe := map[string]any{
				"method": "SUBSCRIBE",
				"params": []string{fmt.Sprintf("markPrice.%s", marketSymbol)},
			}
			if err := conn.WriteJSON(subscribe); err != nil {
				conn.Close()
				logger.Warn("⚠️ [Backpack] 价格流订阅失败: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(w.reconnectGap):
				}
				continue
			}

			for {
				_, message, err := conn.ReadMessage()
				if err != nil {
					conn.Close()
					logger.Warn("⚠️ [Backpack] 价格流断开，准备重连: %v", err)
					break
				}

				var payload struct {
					Stream string `json:"stream"`
					Data   struct {
						MarkPrice string `json:"p"`
					} `json:"data"`
				}
				if err := json.Unmarshal(message, &payload); err != nil {
					continue
				}

				price, err := strconv.ParseFloat(payload.Data.MarkPrice, 64)
				if err != nil || price <= 0 {
					continue
				}

				w.priceMu.Lock()
				w.latestPrice = price
				w.priceMu.Unlock()

				once.Do(func() { close(firstPriceCh) })
				callback(price)
			}
		}
	}()

	select {
	case <-firstPriceCh:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("上下文已取消")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("等待 Backpack 首个价格超时")
	}
}

func (w *WebSocketManager) GetLatestPrice() float64 {
	w.priceMu.RLock()
	defer w.priceMu.RUnlock()
	return w.latestPrice
}

func (w *WebSocketManager) runOrderLoop(ctx context.Context, stopC, doneC chan struct{}, generation uint64, initialResultC chan<- error) {
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
		if w.orderDoneC == doneC && w.orderGeneration == generation {
			w.orderConn = nil
			w.orderActive = false
			w.orderStopping = false
			w.orderStopC = nil
			w.orderDoneC = nil
			w.callbacks = nil
			w.orderHealth.Set(streamhealth.StateStopped, nil)
		}
		w.mu.Unlock()
		close(doneC)
	}()

	for {
		if backpackOrderRunCanceled(ctx, stopC) {
			return
		}

		dialCtx, cancelDial := context.WithTimeout(ctx, backpackHandshakeTTL(w.handshakeTTL))
		conn, err := w.orderDial(dialCtx, w.orderURL)
		cancelDial()
		if err != nil {
			if backpackOrderRunCanceled(ctx, stopC) {
				reportInitial(backpackOrderCancellationError(ctx))
				return
			}
			wrappedErr := fmt.Errorf("订单流连接失败: %w", err)
			if initialPending {
				w.setOrderHealth(generation, streamhealth.StateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setOrderHealth(generation, streamhealth.StateDegraded, wrappedErr)
			logger.Warn("⚠️ [Backpack] %v", wrappedErr)
			if !waitBackpackOrderReconnect(ctx, stopC, w.reconnectGap) {
				return
			}
			continue
		}

		w.setOrderConn(doneC, conn)
		watchDoneC := make(chan struct{})
		go closeBackpackOrderConnOnStop(ctx, stopC, watchDoneC, conn)

		err = w.performOrderHandshake(conn)
		if err != nil {
			close(watchDoneC)
			_ = conn.Close()
			w.clearOrderConn(doneC, conn)
			if backpackOrderRunCanceled(ctx, stopC) {
				reportInitial(backpackOrderCancellationError(ctx))
				return
			}
			wrappedErr := fmt.Errorf("订单流握手失败: %w", err)
			if initialPending {
				w.setOrderHealth(generation, streamhealth.StateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setOrderHealth(generation, streamhealth.StateDegraded, wrappedErr)
			logger.Warn("⚠️ [Backpack] %v", wrappedErr)
			if !waitBackpackOrderReconnect(ctx, stopC, w.reconnectGap) {
				return
			}
			continue
		}

		if backpackOrderRunCanceled(ctx, stopC) || !w.setOrderHealth(generation, streamhealth.StateReady, nil) {
			close(watchDoneC)
			_ = conn.Close()
			reportInitial(backpackOrderCancellationError(ctx))
			return
		}
		reportInitial(nil)
		logger.Info("✅ [Backpack] 私有订单流已鉴权并订阅")

		for {
			if backpackOrderRunCanceled(ctx, stopC) {
				close(watchDoneC)
				_ = conn.Close()
				return
			}

			_, message, err := conn.ReadMessage()
			if err != nil {
				close(watchDoneC)
				_ = conn.Close()
				w.clearOrderConn(doneC, conn)
				if backpackOrderRunCanceled(ctx, stopC) {
					return
				}
				w.setOrderHealth(generation, streamhealth.StateDegraded, err)
				logger.Warn("⚠️ [Backpack] 订单流断开，准备重连: %v", err)
				break
			}
			if err := backpackOrderStreamMessageError(message); err != nil {
				close(watchDoneC)
				_ = conn.Close()
				w.clearOrderConn(doneC, conn)
				w.setOrderHealth(generation, streamhealth.StateDegraded, err)
				logger.Warn("⚠️ [Backpack] 订单流返回错误，准备重连: %v", err)
				break
			}

			w.handleOrderMessage(message, generation)
		}

		if !waitBackpackOrderReconnect(ctx, stopC, w.reconnectGap) {
			return
		}
	}
}

func (w *WebSocketManager) performOrderHandshake(conn *websocket.Conn) error {
	ttl := backpackHandshakeTTL(w.handshakeTTL)
	if err := conn.SetWriteDeadline(time.Now().Add(ttl)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})

	signature, err := w.client.SubscribeSignature()
	if err != nil {
		return fmt.Errorf("生成订单流签名失败: %w", err)
	}
	subscribe := map[string]any{
		"method":    "SUBSCRIBE",
		"params":    []string{fmt.Sprintf("account.orderUpdate.%s", w.marketSymbol)},
		"signature": signature,
	}
	if err := conn.WriteJSON(subscribe); err != nil {
		return fmt.Errorf("发送订单订阅失败: %w", err)
	}
	// Backpack 的流协议没有正向订阅 ACK。WebSocket 握手成功且签名订阅帧
	// 完整写入即是该协议能提供的最强同步启动保证；随后任何错误帧或断线
	// 都会立即把健康状态降为 DEGRADED。
	return nil
}

func (w *WebSocketManager) setOrderHealth(generation uint64, state streamhealth.State, err error) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.orderGeneration != generation || !w.orderActive || w.orderStopping {
		return false
	}
	w.orderHealth.Set(state, err)
	return true
}

func backpackHandshakeTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 10 * time.Second
	}
	return ttl
}

func backpackOrderStreamMessageError(message []byte) error {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(message, &response); err != nil {
		return nil
	}
	rawErr, ok := response["error"]
	if !ok || string(rawErr) == "null" {
		return nil
	}
	return fmt.Errorf("私有订单流错误: %s", string(rawErr))
}

func (w *WebSocketManager) setOrderConn(doneC chan struct{}, conn *websocket.Conn) {
	w.mu.Lock()
	if w.orderDoneC == doneC {
		w.orderConn = conn
	}
	w.mu.Unlock()
}

func (w *WebSocketManager) clearOrderConn(doneC chan struct{}, conn *websocket.Conn) {
	w.mu.Lock()
	if w.orderDoneC == doneC && w.orderConn == conn {
		w.orderConn = nil
	}
	w.mu.Unlock()
}

func backpackOrderRunCanceled(ctx context.Context, stopC <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	case <-stopC:
		return true
	default:
		return false
	}
}

func backpackOrderCancellationError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("订单流已停止")
}

func closeBackpackOrderConnOnStop(ctx context.Context, stopC, doneC <-chan struct{}, conn *websocket.Conn) {
	select {
	case <-ctx.Done():
		_ = conn.Close()
	case <-stopC:
		_ = conn.Close()
	case <-doneC:
	}
}

func waitBackpackOrderReconnect(ctx context.Context, stopC <-chan struct{}, delay time.Duration) bool {
	if delay <= 0 {
		delay = time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-stopC:
		return false
	case <-timer.C:
		return true
	}
}

func (w *WebSocketManager) handleOrderMessage(message []byte, generation uint64) {
	var payload struct {
		Stream string `json:"stream"`
		Data   struct {
			EventTime             int64  `json:"E"`
			EngineTime            int64  `json:"T"`
			OrderID               string `json:"i"`
			ClientID              uint32 `json:"c"`
			Side                  string `json:"S"`
			Type                  string `json:"o"`
			Status                string `json:"X"`
			Price                 string `json:"p"`
			Quantity              string `json:"q"`
			ExecutedQty           string `json:"z"`
			ExecutedQuoteQuantity string `json:"Z"`
			FillPrice             string `json:"L"`
			RealizedPNL           string `json:"rp"`
		} `json:"data"`
	}

	if err := json.Unmarshal(message, &payload); err != nil {
		return
	}

	price := parseFloat(payload.Data.Price)
	fillPrice := parseFloat(payload.Data.FillPrice)
	if price == 0 {
		price = fillPrice
	}

	executedQty := parseFloat(payload.Data.ExecutedQty)
	avgPrice := fillPrice
	if executedQty > 0 {
		executedQuoteQty := parseFloat(payload.Data.ExecutedQuoteQuantity)
		if executedQuoteQty > 0 {
			avgPrice = executedQuoteQty / executedQty
		}
	}
	if avgPrice == 0 {
		avgPrice = price
	}

	clientOrderID, ok := w.idMapper.lookup(payload.Data.ClientID)
	if !ok && payload.Data.ClientID != 0 {
		clientOrderID = syntheticClientOrderID(price, mapSideFromBackpack(payload.Data.Side), w.priceDecimals)
	}

	update := OrderUpdate{
		OrderID:                parseInt64(payload.Data.OrderID),
		ClientOrderID:          clientOrderID,
		Symbol:                 w.displaySymbol,
		Side:                   mapSideFromBackpack(payload.Data.Side),
		Type:                   mapOrderTypeFromBackpack(payload.Data.Type),
		Status:                 mapStatusFromBackpack(payload.Data.Status),
		Price:                  price,
		Quantity:               parseFloat(payload.Data.Quantity),
		ExecutedQty:            executedQty,
		AvgPrice:               avgPrice,
		UpdateTime:             normalizeTimestamp(payload.Data.EngineTime),
		RealizedPNL:            parseFloat(payload.Data.RealizedPNL),
		RealizedPNLIncremental: payload.Data.RealizedPNL != "",
	}

	w.mu.RLock()
	if w.orderGeneration != generation || !w.orderActive || w.orderStopping {
		w.mu.RUnlock()
		return
	}
	callbacks := append([]OrderUpdateCallback(nil), w.callbacks...)
	w.mu.RUnlock()

	for _, callback := range callbacks {
		callback(update)
	}
}
