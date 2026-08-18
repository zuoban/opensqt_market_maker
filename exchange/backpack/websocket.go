package backpack

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"opensqt/logger"

	"github.com/gorilla/websocket"
)

type WebSocketManager struct {
	client        *Client
	marketSymbol  string
	displaySymbol string
	idMapper      *clientIDMapper
	priceDecimals int

	mu          sync.RWMutex
	callbacks   []OrderUpdateCallback
	orderStopC  chan struct{}
	orderDoneC  chan struct{}
	orderActive bool

	priceMu      sync.RWMutex
	latestPrice  float64
	reconnectGap time.Duration
}

func NewWebSocketManager(client *Client, marketSymbol, displaySymbol string, idMapper *clientIDMapper, priceDecimals int) *WebSocketManager {
	return &WebSocketManager{
		client:        client,
		marketSymbol:  marketSymbol,
		displaySymbol: displaySymbol,
		idMapper:      idMapper,
		priceDecimals: priceDecimals,
		reconnectGap:  5 * time.Second,
	}
}

func (w *WebSocketManager) Start(ctx context.Context, callback OrderUpdateCallback) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.orderActive {
		w.callbacks = append(w.callbacks, callback)
		return nil
	}

	w.callbacks = append(w.callbacks, callback)
	w.orderStopC = make(chan struct{})
	w.orderDoneC = make(chan struct{})
	w.orderActive = true

	go w.runOrderLoop(ctx, w.orderStopC, w.orderDoneC)
	return nil
}

func (w *WebSocketManager) Stop() {
	w.mu.Lock()
	if !w.orderActive {
		w.mu.Unlock()
		return
	}
	stopC := w.orderStopC
	doneC := w.orderDoneC
	w.orderActive = false
	w.mu.Unlock()

	close(stopC)
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

func (w *WebSocketManager) runOrderLoop(ctx context.Context, stopC, doneC chan struct{}) {
	defer close(doneC)

	for {
		select {
		case <-ctx.Done():
			return
		case <-stopC:
			return
		default:
		}

		conn, _, err := websocket.DefaultDialer.Dial(backpackWSURL, nil)
		if err != nil {
			logger.Warn("⚠️ [Backpack] 订单流连接失败: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-stopC:
				return
			case <-time.After(w.reconnectGap):
			}
			continue
		}

		signature, err := w.client.SubscribeSignature()
		if err != nil {
			conn.Close()
			logger.Error("❌ [Backpack] 生成订单流签名失败: %v", err)
			return
		}

		subscribe := map[string]any{
			"method":    "SUBSCRIBE",
			"params":    []string{fmt.Sprintf("account.orderUpdate.%s", w.marketSymbol)},
			"signature": signature,
		}
		if err := conn.WriteJSON(subscribe); err != nil {
			conn.Close()
			logger.Warn("⚠️ [Backpack] 订单流订阅失败: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-stopC:
				return
			case <-time.After(w.reconnectGap):
			}
			continue
		}

		for {
			select {
			case <-ctx.Done():
				conn.Close()
				return
			case <-stopC:
				conn.Close()
				return
			default:
			}

			_, message, err := conn.ReadMessage()
			if err != nil {
				conn.Close()
				logger.Warn("⚠️ [Backpack] 订单流断开，准备重连: %v", err)
				break
			}

			w.handleOrderMessage(message)
		}
	}
}

func (w *WebSocketManager) handleOrderMessage(message []byte) {
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
	callbacks := append([]OrderUpdateCallback(nil), w.callbacks...)
	w.mu.RUnlock()

	for _, callback := range callbacks {
		callback(update)
	}
}
