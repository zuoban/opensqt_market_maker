package backpack

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"opensqt/exchange/exchangeerr"
	"opensqt/logger"

	"encoding/json"
)

type BackpackAdapter struct {
	client           *Client
	symbol           string
	marketSymbol     string
	wsManager        *WebSocketManager
	klineWSManager   *KlineWebSocketManager
	idMapper         *clientIDMapper
	priceDecimals    int
	quantityDecimals int
	baseAsset        string
	quoteAsset       string
	accountLeverage  int
}

type clientIDMapper struct {
	mu       sync.RWMutex
	byString map[string]uint32
	byUint   map[uint32]string
}

func newClientIDMapper() *clientIDMapper {
	return &clientIDMapper{
		byString: make(map[string]uint32),
		byUint:   make(map[uint32]string),
	}
}

func (m *clientIDMapper) encode(clientOrderID string) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()

	if encoded, ok := m.byString[clientOrderID]; ok {
		return encoded
	}

	encoded := crc32.ChecksumIEEE([]byte(clientOrderID))
	if encoded == 0 {
		encoded = 1
	}

	for {
		if existing, ok := m.byUint[encoded]; !ok {
			m.byUint[encoded] = clientOrderID
			m.byString[clientOrderID] = encoded
			return encoded
		} else if existing == clientOrderID {
			m.byString[clientOrderID] = encoded
			return encoded
		}
		encoded++
		if encoded == 0 {
			encoded = 1
		}
	}
}

func (m *clientIDMapper) lookup(clientID uint32) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.byUint[clientID]
	return value, ok
}

func NewBackpackAdapter(cfg map[string]string, symbol string) (*BackpackAdapter, error) {
	apiKey := cfg["api_key"]
	secretKey := cfg["secret_key"]
	if apiKey == "" || secretKey == "" {
		return nil, fmt.Errorf("Backpack API 配置不完整")
	}

	client, err := NewClient(apiKey, secretKey)
	if err != nil {
		return nil, err
	}

	idMapper := newClientIDMapper()
	marketSymbol := normalizeBackpackSymbol(symbol)
	adapter := &BackpackAdapter{
		client:         client,
		symbol:         symbol,
		marketSymbol:   marketSymbol,
		idMapper:       idMapper,
		klineWSManager: NewKlineWebSocketManager(),
	}
	adapter.wsManager = NewWebSocketManager(client, marketSymbol, symbol, idMapper, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := adapter.fetchMarketInfo(ctx); err != nil {
		return nil, err
	}
	adapter.wsManager = NewWebSocketManager(client, marketSymbol, symbol, idMapper, adapter.priceDecimals)

	if account, err := adapter.GetAccount(ctx); err == nil {
		adapter.accountLeverage = account.AccountLeverage
	}

	return adapter, nil
}

func (b *BackpackAdapter) GetName() string {
	return "Backpack"
}

func (b *BackpackAdapter) fetchMarketInfo(ctx context.Context) error {
	respBody, err := b.client.DoPublicRequest(ctx, "GET", "/api/v1/market", map[string]string{"symbol": b.marketSymbol})
	if err != nil {
		return fmt.Errorf("获取 Backpack 市场信息失败: %w", err)
	}

	var market struct {
		Symbol      string `json:"symbol"`
		BaseSymbol  string `json:"baseSymbol"`
		QuoteSymbol string `json:"quoteSymbol"`
		Filters     struct {
			Price struct {
				TickSize string `json:"tickSize"`
			} `json:"price"`
			Quantity struct {
				StepSize string `json:"stepSize"`
			} `json:"quantity"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(respBody, &market); err != nil {
		return fmt.Errorf("解析 Backpack 市场信息失败: %w", err)
	}

	b.baseAsset = market.BaseSymbol
	b.quoteAsset = market.QuoteSymbol
	b.priceDecimals = decimalsFromStep(market.Filters.Price.TickSize)
	b.quantityDecimals = decimalsFromStep(market.Filters.Quantity.StepSize)
	if b.priceDecimals == 0 {
		b.priceDecimals = 2
	}
	if b.quantityDecimals == 0 {
		b.quantityDecimals = 3
	}

	logger.Info("ℹ️ [Backpack 合约信息] %s - 数量精度:%d, 价格精度:%d, 基础币种:%s, 计价币种:%s",
		b.marketSymbol, b.quantityDecimals, b.priceDecimals, b.baseAsset, b.quoteAsset)
	return nil
}

func (b *BackpackAdapter) PlaceOrder(ctx context.Context, req *OrderRequest) (*Order, error) {
	priceDecimals := req.PriceDecimals
	if priceDecimals <= 0 {
		priceDecimals = b.priceDecimals
	}

	body := map[string]any{
		"symbol":    normalizeBackpackSymbol(req.Symbol),
		"side":      sideToBackpack(req.Side),
		"orderType": orderTypeToBackpack(req.Type),
		"quantity":  formatWithDecimals(req.Quantity, b.quantityDecimals),
	}

	if req.Type == OrderTypeLimit {
		body["price"] = formatWithDecimals(req.Price, priceDecimals)
	}
	if tif := timeInForceToBackpack(req.TimeInForce); tif != "" {
		body["timeInForce"] = tif
	}
	if req.PostOnly {
		body["postOnly"] = true
		if _, ok := body["timeInForce"]; !ok {
			body["timeInForce"] = "GTC"
		}
	}
	if req.ReduceOnly {
		body["reduceOnly"] = true
	}
	if req.ClientOrderID != "" {
		body["clientId"] = b.idMapper.encode(req.ClientOrderID)
	}

	respBody, err := b.client.DoSignedRequest(ctx, "POST", "/api/v1/order", "orderExecute", nil, body, map[string]string{"X-BROKER-ID": backpackBroker})
	if err != nil {
		return nil, classifyBackpackPlacementError(err)
	}

	var response orderResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, exchangeerr.WrapOrderPlacementUnknown(
			fmt.Errorf("解析 Backpack 下单响应失败: %w", err))
	}
	if strings.TrimSpace(response.ID) == "" || parseInt64(response.ID) <= 0 {
		return nil, exchangeerr.WrapOrderPlacementUnknown(
			fmt.Errorf("Backpack 下单响应缺少有效 orderId"))
	}
	if req.ClientOrderID != "" {
		clientOrderID, ok := b.idMapper.lookup(response.ClientID)
		if !ok || clientOrderID != req.ClientOrderID {
			return nil, exchangeerr.WrapOrderPlacementUnknown(
				fmt.Errorf("Backpack 下单响应 clientId=%d 无法匹配请求 %q", response.ClientID, req.ClientOrderID))
		}
	}

	return b.convertOrderResponse(&response, req.Symbol), nil
}

func classifyBackpackPlacementError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		if apiErr.StatusCode == http.StatusRequestTimeout ||
			apiErr.StatusCode == http.StatusConflict ||
			exchangeerr.LooksLikeAmbiguousOrderPlacementFailure(apiErr.Code, apiErr.Message) ||
			isBackpackDuplicateClientOrderIDError(apiErr) {
			return exchangeerr.WrapOrderPlacementUnknown(err)
		}
		if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
			return exchangeerr.WrapOrderPlacementRejected(err)
		}
		return exchangeerr.WrapOrderPlacementUnknown(err)
	}

	message := err.Error()
	if hasBackpackDuplicateClientOrderIDMessage(message) {
		return exchangeerr.WrapOrderPlacementUnknown(err)
	}
	// 这些错误发生在 HTTP 请求进入网络边界之前，能够证明远端未创建订单。
	if strings.Contains(message, "序列化 Backpack 请求体失败") ||
		strings.Contains(message, "创建 Backpack 请求失败") {
		return exchangeerr.WrapOrderPlacementRejected(err)
	}
	return exchangeerr.WrapOrderPlacementUnknown(err)
}

func isBackpackDuplicateClientOrderIDError(err *APIError) bool {
	if err == nil {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(err.Code)) {
	case "DUPLICATE_CLIENT_ID", "CLIENT_ID_ALREADY_USED", "CLIENT_ID_NOT_UNIQUE":
		return true
	}
	return hasBackpackDuplicateClientOrderIDMessage(err.Code + " " + err.Message)
}

func hasBackpackDuplicateClientOrderIDMessage(message string) bool {
	message = strings.ToLower(message)
	duplicate := strings.Contains(message, "duplicate") ||
		strings.Contains(message, "already used") ||
		strings.Contains(message, "already been used") ||
		strings.Contains(message, "has been used") ||
		strings.Contains(message, "already in use") ||
		strings.Contains(message, "already exists") ||
		strings.Contains(message, "not unique")
	identifier := strings.Contains(message, "clientid") ||
		strings.Contains(message, "client id") ||
		strings.Contains(message, "clientoid") ||
		strings.Contains(message, "client oid") ||
		strings.Contains(message, "orderid") ||
		strings.Contains(message, "order id")
	return duplicate && identifier
}

func (b *BackpackAdapter) BatchPlaceOrders(ctx context.Context, orders []*OrderRequest) ([]*Order, bool) {
	placedOrders := make([]*Order, 0, len(orders))
	hasMarginError := false

	for _, orderReq := range orders {
		order, err := b.PlaceOrder(ctx, orderReq)
		if err != nil {
			logger.Warn("⚠️ [Backpack] 下单失败 %.6f %s: %v", orderReq.Price, orderReq.Side, err)
			errMsg := strings.ToLower(err.Error())
			if strings.Contains(errMsg, "insufficient") || strings.Contains(errMsg, "margin") {
				hasMarginError = true
			}
			continue
		}
		placedOrders = append(placedOrders, order)
	}

	return placedOrders, hasMarginError
}

func (b *BackpackAdapter) CancelOrder(ctx context.Context, symbol string, orderID int64) error {
	body := map[string]any{
		"symbol":  normalizeBackpackSymbol(symbol),
		"orderId": strconv.FormatInt(orderID, 10),
	}
	_, err := b.client.DoSignedRequest(ctx, "DELETE", "/api/v1/order", "orderCancel", nil, body, nil)
	if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == 404 {
		return fmt.Errorf("order does not exist")
	}
	return err
}

func (b *BackpackAdapter) BatchCancelOrders(ctx context.Context, symbol string, orderIDs []int64) error {
	for _, orderID := range orderIDs {
		if err := b.CancelOrder(ctx, symbol, orderID); err != nil {
			return err
		}
	}
	return nil
}

func (b *BackpackAdapter) GetOrder(ctx context.Context, symbol string, orderID int64) (*Order, error) {
	respBody, err := b.client.DoSignedRequest(ctx, "GET", "/api/v1/order", "orderQuery", map[string]string{
		"symbol":  normalizeBackpackSymbol(symbol),
		"orderId": strconv.FormatInt(orderID, 10),
	}, nil, nil)
	if err != nil {
		return nil, err
	}

	var response orderResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("解析 Backpack 订单信息失败: %w", err)
	}

	return b.convertOrderResponse(&response, symbol), nil
}

func (b *BackpackAdapter) GetOpenOrders(ctx context.Context, symbol string) ([]*Order, error) {
	query := map[string]string{"marketType": "PERP"}
	if symbol != "" {
		query["symbol"] = normalizeBackpackSymbol(symbol)
	}

	respBody, err := b.client.DoSignedRequest(ctx, "GET", "/api/v1/orders", "orderQueryAll", query, nil, nil)
	if err != nil {
		return nil, err
	}

	var response []orderResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("解析 Backpack 未完成订单失败: %w", err)
	}

	orders := make([]*Order, 0, len(response))
	for _, item := range response {
		orders = append(orders, b.convertOrderResponse(&item, symbol))
	}

	sort.SliceStable(orders, func(i, j int) bool {
		return orders[i].Price < orders[j].Price
	})
	return orders, nil
}

func (b *BackpackAdapter) CancelAllOrders(ctx context.Context, symbol string) error {
	body := map[string]any{"symbol": normalizeBackpackSymbol(symbol)}
	_, err := b.client.DoSignedRequest(ctx, "DELETE", "/api/v1/orders", "orderCancelAll", nil, body, nil)
	return err
}

func (b *BackpackAdapter) GetAccount(ctx context.Context) (*Account, error) {
	accountBody, err := b.client.DoSignedRequest(ctx, "GET", "/api/v1/account", "accountQuery", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var accountResp struct {
		LeverageLimit string `json:"leverageLimit"`
	}
	if err := json.Unmarshal(accountBody, &accountResp); err != nil {
		return nil, fmt.Errorf("解析 Backpack 账户信息失败: %w", err)
	}

	collateralBody, err := b.client.DoSignedRequest(ctx, "GET", "/api/v1/capital/collateral", "collateralQuery", nil, nil, nil)
	if err != nil {
		return nil, err
	}
	var collateralResp struct {
		NetEquity          string `json:"netEquity"`
		NetEquityAvailable string `json:"netEquityAvailable"`
		NetEquityLocked    string `json:"netEquityLocked"`
	}
	if err := json.Unmarshal(collateralBody, &collateralResp); err != nil {
		return nil, fmt.Errorf("解析 Backpack 抵押品信息失败: %w", err)
	}

	positions, err := b.GetPositions(ctx, b.symbol)
	if err != nil {
		return nil, err
	}

	leverage := int(math.Round(parseFloat(accountResp.LeverageLimit)))
	if leverage <= 0 {
		leverage = b.accountLeverage
	}
	b.accountLeverage = leverage

	return &Account{
		TotalWalletBalance: parseFloat(collateralResp.NetEquity),
		TotalMarginBalance: parseFloat(collateralResp.NetEquityLocked),
		AvailableBalance:   parseFloat(collateralResp.NetEquityAvailable),
		Positions:          positions,
		AccountLeverage:    leverage,
	}, nil
}

func (b *BackpackAdapter) GetPositions(ctx context.Context, symbol string) ([]*Position, error) {
	query := map[string]string{"marketType": "PERP"}
	if symbol != "" {
		query["symbol"] = normalizeBackpackSymbol(symbol)
	}

	respBody, err := b.client.DoSignedRequest(ctx, "GET", "/api/v1/position", "positionQuery", query, nil, nil)
	if apiErr, ok := err.(*APIError); ok && apiErr.StatusCode == 404 {
		return []*Position{}, nil
	} else if err != nil {
		return nil, err
	}

	var response []positionResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("解析 Backpack 持仓失败: %w", err)
	}

	positions := make([]*Position, 0, len(response))
	for _, item := range response {
		displaySymbol := symbol
		if displaySymbol == "" {
			displaySymbol = compactBackpackSymbol(item.Symbol)
		}
		positions = append(positions, &Position{
			Symbol:         displaySymbol,
			Size:           parseFloat(item.NetQuantity),
			EntryPrice:     parseFloat(item.EntryPrice),
			MarkPrice:      parseFloat(item.MarkPrice),
			UnrealizedPNL:  parseFloat(item.PnlUnrealized),
			Leverage:       b.accountLeverage,
			MarginType:     "cross",
			IsolatedMargin: 0,
		})
	}

	return positions, nil
}

func (b *BackpackAdapter) GetBalance(ctx context.Context, asset string) (float64, error) {
	respBody, err := b.client.DoSignedRequest(ctx, "GET", "/api/v1/capital", "balanceQuery", nil, nil, nil)
	if err != nil {
		return 0, err
	}

	var response map[string]struct {
		Available string `json:"available"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return 0, fmt.Errorf("解析 Backpack 余额失败: %w", err)
	}

	balance, ok := response[strings.ToUpper(asset)]
	if !ok {
		return 0, nil
	}
	return parseFloat(balance.Available), nil
}

func (b *BackpackAdapter) StartOrderStream(ctx context.Context, callback func(interface{})) error {
	return b.wsManager.Start(ctx, func(update OrderUpdate) {
		callback(update)
	})
}

func (b *BackpackAdapter) StopOrderStream() error {
	b.wsManager.Stop()
	return nil
}

func (b *BackpackAdapter) GetLatestPrice(ctx context.Context, symbol string) (float64, error) {
	respBody, err := b.client.DoPublicRequest(ctx, "GET", "/api/v1/markPrices", map[string]string{"symbol": normalizeBackpackSymbol(symbol)})
	if err != nil {
		return 0, err
	}

	var response []struct {
		MarkPrice string `json:"markPrice"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return 0, fmt.Errorf("解析 Backpack 标记价格失败: %w", err)
	}
	if len(response) == 0 {
		return 0, fmt.Errorf("Backpack 未返回标记价格")
	}

	return parseFloat(response[0].MarkPrice), nil
}

func (b *BackpackAdapter) StartPriceStream(ctx context.Context, symbol string, callback func(price float64)) error {
	return b.wsManager.StartPriceStream(ctx, normalizeBackpackSymbol(symbol), callback)
}

func (b *BackpackAdapter) StartKlineStream(ctx context.Context, symbols []string, interval string, callback func(candle interface{})) error {
	symbolMap := make(map[string]string, len(symbols))
	for _, symbol := range symbols {
		symbolMap[normalizeBackpackSymbol(symbol)] = symbol
	}
	return b.klineWSManager.Start(ctx, symbolMap, interval, callback)
}

func (b *BackpackAdapter) StopKlineStream() error {
	b.klineWSManager.Stop()
	return nil
}

func (b *BackpackAdapter) GetHistoricalKlines(ctx context.Context, symbol string, interval string, limit int) ([]*Candle, error) {
	if limit <= 0 {
		return []*Candle{}, nil
	}

	intervalDuration, err := intervalToDuration(interval)
	if err != nil {
		return nil, err
	}

	endTime := time.Now().UTC()
	startTime := endTime.Add(-time.Duration(limit) * intervalDuration)
	respBody, err := b.client.DoPublicRequest(ctx, "GET", "/api/v1/klines", map[string]string{
		"symbol":    normalizeBackpackSymbol(symbol),
		"interval":  interval,
		"startTime": strconv.FormatInt(startTime.Unix(), 10),
		"endTime":   strconv.FormatInt(endTime.Unix(), 10),
	})
	if err != nil {
		return nil, err
	}

	var response []struct {
		Start  string `json:"start"`
		Open   string `json:"open"`
		High   string `json:"high"`
		Low    string `json:"low"`
		Close  string `json:"close"`
		Volume string `json:"volume"`
	}
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("解析 Backpack 历史K线失败: %w", err)
	}

	if len(response) > limit {
		response = response[len(response)-limit:]
	}

	candles := make([]*Candle, 0, len(response))
	for _, item := range response {
		startAt, err := parseBackpackTime(item.Start)
		if err != nil {
			continue
		}
		candles = append(candles, &Candle{
			Symbol:    symbol,
			Open:      parseFloat(item.Open),
			High:      parseFloat(item.High),
			Low:       parseFloat(item.Low),
			Close:     parseFloat(item.Close),
			Volume:    parseFloat(item.Volume),
			Timestamp: startAt.UnixMilli(),
			IsClosed:  true,
		})
	}

	return candles, nil
}

func (b *BackpackAdapter) GetPriceDecimals() int {
	return b.priceDecimals
}

func (b *BackpackAdapter) GetQuantityDecimals() int {
	return b.quantityDecimals
}

func (b *BackpackAdapter) GetBaseAsset() string {
	return b.baseAsset
}

func (b *BackpackAdapter) GetQuoteAsset() string {
	return b.quoteAsset
}

type orderResponse struct {
	ID                    string `json:"id"`
	ClientID              uint32 `json:"clientId"`
	CreatedAt             int64  `json:"createdAt"`
	ExecutedQuantity      string `json:"executedQuantity"`
	ExecutedQuoteQuantity string `json:"executedQuoteQuantity"`
	OrderType             string `json:"orderType"`
	Price                 string `json:"price"`
	Quantity              string `json:"quantity"`
	Side                  string `json:"side"`
	Status                string `json:"status"`
	Symbol                string `json:"symbol"`
	TimeInForce           string `json:"timeInForce"`
}

type positionResponse struct {
	Symbol        string `json:"symbol"`
	NetQuantity   string `json:"netQuantity"`
	EntryPrice    string `json:"entryPrice"`
	MarkPrice     string `json:"markPrice"`
	PnlUnrealized string `json:"pnlUnrealized"`
}

func (b *BackpackAdapter) convertOrderResponse(response *orderResponse, symbol string) *Order {
	price := parseFloat(response.Price)
	quantity := parseFloat(response.Quantity)
	executedQty := parseFloat(response.ExecutedQuantity)
	avgPrice := price
	if executedQty > 0 {
		executedQuoteQty := parseFloat(response.ExecutedQuoteQuantity)
		if executedQuoteQty > 0 {
			avgPrice = executedQuoteQty / executedQty
		}
	}

	clientOrderID, ok := b.idMapper.lookup(response.ClientID)
	if !ok && response.ClientID != 0 {
		clientOrderID = syntheticClientOrderID(price, mapSideFromBackpack(response.Side), b.priceDecimals)
	}

	displaySymbol := symbol
	if displaySymbol == "" {
		displaySymbol = compactBackpackSymbol(response.Symbol)
	}

	return &Order{
		OrderID:       parseInt64(response.ID),
		ClientOrderID: clientOrderID,
		Symbol:        displaySymbol,
		Side:          mapSideFromBackpack(response.Side),
		Type:          mapOrderTypeFromBackpack(response.OrderType),
		Price:         price,
		Quantity:      quantity,
		ExecutedQty:   executedQty,
		AvgPrice:      avgPrice,
		Status:        mapStatusFromBackpack(response.Status),
		CreatedAt:     time.UnixMilli(normalizeTimestamp(response.CreatedAt)),
		UpdateTime:    normalizeTimestamp(response.CreatedAt),
	}
}

func normalizeBackpackSymbol(symbol string) string {
	if strings.Contains(symbol, "_") {
		if strings.HasSuffix(symbol, "_PERP") {
			return symbol
		}
		return symbol + "_PERP"
	}

	quotes := []string{"USDC", "USDT", "USD"}
	upper := strings.ToUpper(symbol)
	for _, quote := range quotes {
		if strings.HasSuffix(upper, quote) {
			base := strings.TrimSuffix(upper, quote)
			return base + "_" + quote + "_PERP"
		}
	}

	return upper
}

func compactBackpackSymbol(symbol string) string {
	upper := strings.ToUpper(symbol)
	upper = strings.TrimSuffix(upper, "_PERP")
	return strings.ReplaceAll(upper, "_", "")
}

func sideToBackpack(side Side) string {
	if side == SideSell {
		return "Ask"
	}
	return "Bid"
}

func mapSideFromBackpack(side string) Side {
	if strings.EqualFold(side, "Ask") {
		return SideSell
	}
	return SideBuy
}

func orderTypeToBackpack(orderType OrderType) string {
	if orderType == OrderTypeMarket {
		return "Market"
	}
	return "Limit"
}

func mapOrderTypeFromBackpack(orderType string) OrderType {
	if strings.EqualFold(orderType, "Market") {
		return OrderTypeMarket
	}
	return OrderTypeLimit
}

func timeInForceToBackpack(timeInForce TimeInForce) string {
	switch timeInForce {
	case TimeInForceIOC:
		return "IOC"
	case TimeInForceFOK:
		return "FOK"
	case TimeInForceGTX:
		return "GTC"
	default:
		return "GTC"
	}
}

func mapStatusFromBackpack(status string) OrderStatus {
	switch strings.ToLower(status) {
	case "new":
		return OrderStatusNew
	case "partiallyfilled":
		return OrderStatusPartiallyFilled
	case "filled":
		return OrderStatusFilled
	case "cancelled":
		return OrderStatusCanceled
	case "expired":
		return OrderStatusExpired
	case "rejected", "triggerfailed":
		return OrderStatusRejected
	default:
		return OrderStatusNew
	}
}

func decimalsFromStep(step string) int {
	if step == "" || !strings.Contains(step, ".") {
		return 0
	}
	trimmed := strings.TrimRight(step, "0")
	parts := strings.Split(trimmed, ".")
	if len(parts) != 2 {
		return 0
	}
	return len(parts[1])
}

func formatWithDecimals(value float64, decimals int) string {
	if decimals < 0 {
		decimals = 0
	}
	return strconv.FormatFloat(value, 'f', decimals, 64)
}

func parseFloat(value string) float64 {
	parsed, _ := strconv.ParseFloat(value, 64)
	return parsed
}

func parseInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(value, 10, 64)
	return parsed
}

func normalizeTimestamp(timestamp int64) int64 {
	if timestamp > 1_000_000_000_000_000 {
		return timestamp / 1000
	}
	if timestamp > 1_000_000_000_000 {
		return timestamp
	}
	if timestamp > 1_000_000_000 {
		return timestamp * 1000
	}
	return timestamp
}

func syntheticClientOrderID(price float64, side Side, priceDecimals int) string {
	priceInt := int64(math.Round(price * math.Pow10(priceDecimals)))
	sideCode := "B"
	if side == SideSell {
		sideCode = "S"
	}
	return fmt.Sprintf("%d_%s_%013d", priceInt, sideCode, 0)
}

func parseBackpackTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}

	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}

	if numeric, err := strconv.ParseInt(value, 10, 64); err == nil {
		switch {
		case numeric > 1_000_000_000_000_000:
			return time.UnixMicro(numeric).UTC(), nil
		case numeric > 1_000_000_000_000:
			return time.UnixMilli(numeric).UTC(), nil
		default:
			return time.Unix(numeric, 0).UTC(), nil
		}
	}

	return time.Time{}, fmt.Errorf("unsupported backpack time: %s", value)
}

func intervalToDuration(interval string) (time.Duration, error) {
	switch interval {
	case "1s":
		return time.Second, nil
	case "1m":
		return time.Minute, nil
	case "3m":
		return 3 * time.Minute, nil
	case "5m":
		return 5 * time.Minute, nil
	case "15m":
		return 15 * time.Minute, nil
	case "30m":
		return 30 * time.Minute, nil
	case "1h":
		return time.Hour, nil
	case "2h":
		return 2 * time.Hour, nil
	case "4h":
		return 4 * time.Hour, nil
	case "6h":
		return 6 * time.Hour, nil
	case "8h":
		return 8 * time.Hour, nil
	case "12h":
		return 12 * time.Hour, nil
	case "1d":
		return 24 * time.Hour, nil
	case "3d":
		return 72 * time.Hour, nil
	case "1w":
		return 7 * 24 * time.Hour, nil
	case "1month":
		return 30 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("不支持的 Backpack K线周期: %s", interval)
	}
}
