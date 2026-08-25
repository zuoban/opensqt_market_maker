package bybit

import (
	"context"
	"encoding/json"
	"fmt"
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
	orderIDs      *orderIDMapper

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

func NewWebSocketManager(client *Client, marketSymbol, displaySymbol string, orderIDs *orderIDMapper) *WebSocketManager {
	w := &WebSocketManager{
		client:        client,
		marketSymbol:  marketSymbol,
		displaySymbol: displaySymbol,
		orderIDs:      orderIDs,
		reconnectGap:  5 * time.Second,
		handshakeTTL:  10 * time.Second,
		orderURL:      bybitPrivateWS,
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
		logger.Warn("⚠️ [Bybit] 停止订单流超时")
	}
}

func (w *WebSocketManager) StartPriceStream(ctx context.Context, symbol string, callback func(price float64)) error {
	firstPriceCh := make(chan struct{})
	var once sync.Once

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			conn, _, err := websocket.DefaultDialer.Dial(bybitPublicLinear, nil)
			if err != nil {
				logger.Warn("⚠️ [Bybit] 价格流连接失败: %v", err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(w.reconnectGap):
				}
				continue
			}

			subscribe := map[string]any{
				"op":   "subscribe",
				"args": []string{fmt.Sprintf("tickers.%s", symbol)},
			}
			if err := conn.WriteJSON(subscribe); err != nil {
				conn.Close()
				logger.Warn("⚠️ [Bybit] 价格流订阅失败: %v", err)
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
					logger.Warn("⚠️ [Bybit] 价格流断开，准备重连: %v", err)
					break
				}

				var payload struct {
					Topic string `json:"topic"`
					Data  struct {
						LastPrice string `json:"lastPrice"`
						MarkPrice string `json:"markPrice"`
					} `json:"data"`
				}
				if err := json.Unmarshal(message, &payload); err != nil {
					continue
				}

				price := parseFloat(payload.Data.LastPrice)
				if price <= 0 {
					price = parseFloat(payload.Data.MarkPrice)
				}
				if price <= 0 {
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
	case <-ctx.Done():
		return fmt.Errorf("上下文已取消")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("等待 Bybit 首个价格超时")
	case <-firstPriceCh:
		return nil
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
		if orderRunCanceled(ctx, stopC) {
			return
		}

		dialCtx, cancelDial := context.WithTimeout(ctx, orderHandshakeTTL(w.handshakeTTL))
		conn, err := w.orderDial(dialCtx, w.orderURL)
		cancelDial()
		if err != nil {
			if orderRunCanceled(ctx, stopC) {
				reportInitial(orderCancellationError(ctx))
				return
			}
			wrappedErr := fmt.Errorf("订单流连接失败: %w", err)
			if initialPending {
				w.setOrderHealth(generation, streamhealth.StateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setOrderHealth(generation, streamhealth.StateDegraded, wrappedErr)
			logger.Warn("⚠️ [Bybit] %v", wrappedErr)
			if !waitOrderReconnect(ctx, stopC, w.reconnectGap) {
				return
			}
			continue
		}

		w.setOrderConn(doneC, conn)
		watchDoneC := make(chan struct{})
		go closeOrderConnOnStop(ctx, stopC, watchDoneC, conn)

		err = w.performOrderHandshake(conn, generation)
		if err != nil {
			close(watchDoneC)
			_ = conn.Close()
			w.clearOrderConn(doneC, conn)
			if orderRunCanceled(ctx, stopC) {
				reportInitial(orderCancellationError(ctx))
				return
			}
			wrappedErr := fmt.Errorf("订单流握手失败: %w", err)
			if initialPending {
				w.setOrderHealth(generation, streamhealth.StateStopped, wrappedErr)
				reportInitial(wrappedErr)
				return
			}
			w.setOrderHealth(generation, streamhealth.StateDegraded, wrappedErr)
			logger.Warn("⚠️ [Bybit] %v", wrappedErr)
			if !waitOrderReconnect(ctx, stopC, w.reconnectGap) {
				return
			}
			continue
		}

		if orderRunCanceled(ctx, stopC) || !w.setOrderHealth(generation, streamhealth.StateReady, nil) {
			close(watchDoneC)
			_ = conn.Close()
			reportInitial(orderCancellationError(ctx))
			return
		}
		reportInitial(nil)
		logger.Info("✅ [Bybit] 私有订单流已鉴权并订阅")

		for {
			if orderRunCanceled(ctx, stopC) {
				close(watchDoneC)
				_ = conn.Close()
				return
			}

			_, message, err := conn.ReadMessage()
			if err != nil {
				close(watchDoneC)
				_ = conn.Close()
				w.clearOrderConn(doneC, conn)
				if orderRunCanceled(ctx, stopC) {
					return
				}
				w.setOrderHealth(generation, streamhealth.StateDegraded, err)
				logger.Warn("⚠️ [Bybit] 订单流断开，准备重连: %v", err)
				break
			}

			w.handleOrderMessage(message, generation)
		}

		if !waitOrderReconnect(ctx, stopC, w.reconnectGap) {
			return
		}
	}
}

func (w *WebSocketManager) performOrderHandshake(conn *websocket.Conn, generation uint64) error {
	ttl := orderHandshakeTTL(w.handshakeTTL)
	if err := conn.SetReadDeadline(time.Now().Add(ttl)); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(ttl)); err != nil {
		return err
	}
	defer conn.SetReadDeadline(time.Time{})
	defer conn.SetWriteDeadline(time.Time{})

	if err := conn.WriteJSON(map[string]any{"op": "auth", "args": w.client.WebSocketAuthArgs()}); err != nil {
		return fmt.Errorf("发送鉴权请求失败: %w", err)
	}
	if err := w.waitOperationAck(conn, "auth", generation); err != nil {
		return err
	}
	if err := conn.WriteJSON(map[string]any{"op": "subscribe", "args": []string{"order"}}); err != nil {
		return fmt.Errorf("发送订单订阅失败: %w", err)
	}
	return w.waitOperationAck(conn, "subscribe", generation)
}

func orderHandshakeTTL(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return 10 * time.Second
	}
	return ttl
}

func (w *WebSocketManager) waitOperationAck(conn *websocket.Conn, wantOp string, generation uint64) error {
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var ack struct {
			Op      string `json:"op"`
			Success *bool  `json:"success"`
			RetMsg  string `json:"ret_msg"`
			RetCode *int   `json:"retCode"`
			Topic   string `json:"topic"`
		}
		if err := json.Unmarshal(message, &ack); err != nil {
			continue
		}
		if ack.Topic == "order" {
			w.handleOrderMessage(message, generation)
			continue
		}
		if ack.Op != wantOp {
			continue
		}
		if ack.Success != nil && !*ack.Success {
			return fmt.Errorf("%s 被拒绝: %s", wantOp, ack.RetMsg)
		}
		if ack.RetCode != nil && *ack.RetCode != 0 {
			return fmt.Errorf("%s 被拒绝: retCode=%d %s", wantOp, *ack.RetCode, ack.RetMsg)
		}
		if ack.Success == nil && ack.RetCode == nil {
			return fmt.Errorf("%s 确认缺少成功状态", wantOp)
		}
		return nil
	}
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

func orderRunCanceled(ctx context.Context, stopC <-chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	case <-stopC:
		return true
	default:
		return false
	}
}

func orderCancellationError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("订单流已停止")
}

func closeOrderConnOnStop(ctx context.Context, stopC, doneC <-chan struct{}, conn *websocket.Conn) {
	select {
	case <-ctx.Done():
		_ = conn.Close()
	case <-stopC:
		_ = conn.Close()
	case <-doneC:
	}
}

func waitOrderReconnect(ctx context.Context, stopC <-chan struct{}, delay time.Duration) bool {
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
		Topic string `json:"topic"`
		Data  []struct {
			Category    string `json:"category"`
			OrderID     string `json:"orderId"`
			OrderLinkID string `json:"orderLinkId"`
			Symbol      string `json:"symbol"`
			Side        string `json:"side"`
			OrderType   string `json:"orderType"`
			OrderStatus string `json:"orderStatus"`
			Price       string `json:"price"`
			Qty         string `json:"qty"`
			CumExecQty  string `json:"cumExecQty"`
			AvgPrice    string `json:"avgPrice"`
			UpdatedTime string `json:"updatedTime"`
			ClosedPnl   string `json:"closedPnl"`
		} `json:"data"`
	}

	if err := json.Unmarshal(message, &payload); err != nil || payload.Topic != "order" {
		return
	}

	for _, item := range payload.Data {
		if item.Category != "linear" {
			continue
		}
		if item.Symbol != "" && item.Symbol != w.marketSymbol {
			continue
		}

		update := OrderUpdate{
			OrderID:       w.orderIDs.encode(item.OrderID),
			ClientOrderID: item.OrderLinkID,
			Symbol:        w.displaySymbol,
			Side:          mapSideFromBybit(item.Side),
			Type:          mapOrderTypeFromBybit(item.OrderType),
			Status:        mapStatusFromBybit(item.OrderStatus),
			Price:         parseFloat(item.Price),
			Quantity:      parseFloat(item.Qty),
			ExecutedQty:   parseFloat(item.CumExecQty),
			AvgPrice:      parseFloat(item.AvgPrice),
			UpdateTime:    parseInt64(item.UpdatedTime),
			RealizedPNL:   parseFloat(item.ClosedPnl),
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
}
