package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"opensqt/exchange/exchangeerr"
	"opensqt/logger"
	"opensqt/utils"

	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
)

const (
	binanceInitializationTimeout       = 10 * time.Second
	defaultCancelAllConfirmAttempts    = 5
	defaultCancelAllConfirmDelay       = 100 * time.Millisecond
	defaultOrderConfirmationTimeout    = 3 * time.Second
	defaultUnknownPlacementMaxAttempts = 2
)

// 为了避免循环导入，在这里定义需要的类型
type Side string
type OrderType string
type OrderStatus string
type TimeInForce string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

const (
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeMarket OrderType = "MARKET"
)

const (
	OrderStatusNew             OrderStatus = "NEW"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCanceled        OrderStatus = "CANCELED"
	OrderStatusRejected        OrderStatus = "REJECTED"
	OrderStatusExpired         OrderStatus = "EXPIRED"
)

const (
	TimeInForceGTC TimeInForce = "GTC"
	TimeInForceGTX TimeInForce = "GTX" // Post Only - 无法成为挂单方就撤销
)

type OrderRequest struct {
	Symbol        string
	Side          Side
	Type          OrderType
	TimeInForce   TimeInForce
	Quantity      float64
	Price         float64
	ReduceOnly    bool
	PostOnly      bool // 是否只做 Maker（使用 GTX）
	PriceDecimals int
	ClientOrderID string // 自定义订单ID
}

type Order struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          Side
	Type          OrderType
	Price         float64
	Quantity      float64
	ExecutedQty   float64
	AvgPrice      float64
	Status        OrderStatus
	CreatedAt     time.Time
	UpdateTime    int64
}

type Position struct {
	Symbol         string
	Size           float64
	EntryPrice     float64
	MarkPrice      float64
	UnrealizedPNL  float64
	Leverage       int
	MarginType     string
	IsolatedMargin float64
}

type Account struct {
	TotalWalletBalance float64
	TotalMarginBalance float64
	AvailableBalance   float64
	Positions          []*Position
}

type OrderUpdate struct {
	OrderID                int64
	ClientOrderID          string
	Symbol                 string
	Side                   Side
	Type                   OrderType
	Status                 OrderStatus
	Price                  float64
	Quantity               float64
	ExecutedQty            float64
	AvgPrice               float64
	UpdateTime             int64
	RealizedPNL            float64
	RealizedPNLIncremental bool
}

type OrderUpdateCallback func(update OrderUpdate)

// BinanceAdapter 币安交易所适配器
type BinanceAdapter struct {
	client           *futures.Client
	symbol           string
	wsManager        *WebSocketManager
	klineWSManager   *KlineWebSocketManager
	contractSpec     *ContractSpec
	priceDecimals    int    // 价格精度（小数位数）
	quantityDecimals int    // 数量精度（小数位数）
	baseAsset        string // 基础资产（交易币种），如 BTC
	quoteAsset       string // 计价资产（结算币种），如 USDT、USD

	httpTransportOnce        sync.Once
	cancelAllConfirmAttempts int
	cancelAllConfirmDelay    time.Duration
	orderConfirmationTimeout time.Duration
	unknownPlacementAttempts int
}

// NewBinanceAdapter 创建币安适配器
func NewBinanceAdapter(cfg map[string]string, symbol string) (*BinanceAdapter, error) {
	apiKey := cfg["api_key"]
	secretKey := cfg["secret_key"]

	if apiKey == "" || secretKey == "" {
		return nil, fmt.Errorf("Binance API 配置不完整")
	}

	client := futures.NewClient(apiKey, secretKey)

	wsManager := NewWebSocketManager(apiKey, secretKey)
	installStatusAwareTransport(wsManager.client)
	wsManager.SetExpectedSymbol(symbol)

	adapter := &BinanceAdapter{
		client:    client,
		symbol:    symbol,
		wsManager: wsManager,
	}
	adapter.ensureStatusAwareTransport()

	// 签名请求依赖准确的服务器时间；启动时失败必须阻止程序继续交易。
	ctxTime, cancelTime := context.WithTimeout(context.Background(), binanceInitializationTimeout)
	_, err := client.NewSetServerTimeService().Do(ctxTime)
	cancelTime()
	if err != nil {
		return nil, fmt.Errorf("同步 Binance 服务器时间失败: %w", err)
	}

	// 获取合约信息（价格精度、数量精度等）。
	ctxSpec, cancelSpec := context.WithTimeout(context.Background(), binanceInitializationTimeout)
	err = adapter.fetchExchangeInfo(ctxSpec)
	cancelSpec()
	if err != nil {
		return nil, fmt.Errorf("初始化 Binance 合约规格失败: %w", err)
	}

	// 合约规格初始化后，用独立的有界上下文验证账户交易权限、持仓模式和方向。
	ctxAccount, cancelAccount := context.WithTimeout(context.Background(), binanceInitializationTimeout)
	err = adapter.ValidateAccount(ctxAccount)
	cancelAccount()
	if err != nil {
		return nil, fmt.Errorf("验证 Binance 账户失败: %w", err)
	}

	return adapter, nil
}

func (b *BinanceAdapter) ensureStatusAwareTransport() {
	b.httpTransportOnce.Do(func() {
		installStatusAwareTransport(b.client)
	})
}

func installStatusAwareTransport(client *futures.Client) {
	if client == nil {
		return
	}
	httpClient := client.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	clientCopy := *httpClient
	transport := clientCopy.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if _, ok := transport.(*statusAwareTransport); !ok {
		clientCopy.Transport = &statusAwareTransport{base: transport}
	}
	client.HTTPClient = &clientCopy
}

// GetName 获取交易所名称
func (b *BinanceAdapter) GetName() string {
	return "Binance"
}

// fetchExchangeInfo 获取合约信息（价格精度、数量精度等）
func (b *BinanceAdapter) fetchExchangeInfo(ctx context.Context) error {
	exchangeInfo, err := b.client.NewExchangeInfoService().Do(ctx)
	if err != nil {
		return fmt.Errorf("获取交易所信息失败: %w", err)
	}

	// 查找并严格校验指定交易对的信息。Binance 明确要求价格和数量按
	// PRICE_FILTER/LOT_SIZE 处理，不能把展示精度当成 tickSize/stepSize。
	for _, symbol := range exchangeInfo.Symbols {
		if symbol.Symbol == b.symbol {
			spec, err := contractSpecFromSymbol(symbol)
			if err != nil {
				return err
			}

			b.contractSpec = spec
			b.priceDecimals = spec.PricePrecision
			b.quantityDecimals = spec.QuantityPrecision
			b.baseAsset = spec.BaseAsset
			b.quoteAsset = spec.QuoteAsset

			logger.Info("ℹ️ [Binance 合约信息] %s - tick:%s, step:%s, 数量范围:%s~%s, 最小名义价值:%s %s, 保证金币种:%s",
				b.symbol, spec.TickSize, spec.StepSize, spec.MinQuantity, spec.MaxQuantity,
				spec.MinNotional, spec.QuoteAsset, spec.MarginAsset)
			return nil
		}
	}

	return fmt.Errorf("未找到合约信息: %s", b.symbol)
}

// PlaceOrder 下单
func (b *BinanceAdapter) PlaceOrder(ctx context.Context, req *OrderRequest) (*Order, error) {
	if b.contractSpec == nil {
		return nil, exchangeerr.WrapOrderPlacementRejected(
			fmt.Errorf("Binance 合约规格未初始化"))
	}
	normalized, err := b.contractSpec.normalizeOrder(req)
	if err != nil {
		return nil, exchangeerr.WrapOrderPlacementRejected(
			fmt.Errorf("Binance 订单参数无效: %w", err))
	}

	// 根据 PostOnly 参数选择 TimeInForce
	timeInForce := futures.TimeInForceTypeGTC
	if req.PostOnly {
		timeInForce = futures.TimeInForceTypeGTX // Post Only - 只做 Maker
	}

	// ClientOrderID 必须只加一次经纪商前缀，并在下单重试、结果确认时保持完全一致。
	clientOrderID := req.ClientOrderID
	if clientOrderID != "" {
		clientOrderID = utils.AddBrokerPrefix("binance", clientOrderID)
	}

	b.ensureStatusAwareTransport()
	maxAttempts := b.unknownPlacementAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultUnknownPlacementMaxAttempts
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		orderService := b.client.NewCreateOrderService().
			Symbol(b.contractSpec.Symbol).
			Side(futures.SideType(req.Side)).
			Type(futures.OrderTypeLimit).
			TimeInForce(timeInForce).
			Quantity(normalized.QuantityText).
			Price(normalized.PriceText)
		if clientOrderID != "" {
			orderService = orderService.NewClientOrderID(clientOrderID)
		}
		// Binance 单向持仓模式下，平仓单使用 ReduceOnly。
		if req.ReduceOnly {
			orderService = orderService.ReduceOnly(true)
		}

		resp, createErr := orderService.Do(ctx)
		invalidSuccessResponse := isCreateResponseDecodeError(createErr)
		if createErr == nil {
			if responseErr := validateCreateOrderResponse(resp, b.contractSpec.Symbol, clientOrderID); responseErr != nil {
				// HTTP 2xx 但响应不完整时，订单仍可能已经落库，必须进入 clientOrderID 对账流程。
				createErr = responseErr
				invalidSuccessResponse = true
			} else {
				return &Order{
					OrderID:       resp.OrderID,
					ClientOrderID: resp.ClientOrderID,
					Symbol:        b.contractSpec.Symbol,
					Side:          req.Side,
					Type:          req.Type,
					Price:         normalized.Price,
					Quantity:      normalized.Quantity,
					Status:        OrderStatus(resp.Status),
					CreatedAt:     time.Now(),
					UpdateTime:    resp.UpdateTime,
				}, nil
			}
		}
		duplicateClientOrderID := isDuplicateClientOrderIDError(createErr)
		if !invalidSuccessResponse && !isUnknownPlacementResult(createErr) && !duplicateClientOrderID {
			// 创建订单的 HTTP 5xx 已由 status-aware transport 保留并在上方
			// 进入 UNKNOWN；走到这里的 SDK APIError 是交易所明确业务拒绝。
			var apiErr *common.APIError
			if errors.As(createErr, &apiErr) && apiErr != nil {
				return nil, exchangeerr.WrapOrderPlacementRejected(createErr)
			}
			return nil, createErr
		}
		if clientOrderID == "" {
			return nil, fmt.Errorf("%w: Binance 下单失败且未提供 clientOrderID，无法确认是否已受理: %v",
				exchangeerr.ErrOrderPlacementUnknown, createErr)
		}

		confirmed, confirmErr := b.confirmOrderByClientID(ctx, b.contractSpec.Symbol, clientOrderID)
		if confirmErr == nil {
			return confirmed, nil
		}
		if !isBinanceAPIErrorCode(confirmErr, -2013) {
			return nil, fmt.Errorf("%w: Binance 下单返回不确定结果 (%v)，按 clientOrderID=%s 确认失败: %v",
				exchangeerr.ErrOrderPlacementUnknown, createErr, clientOrderID, confirmErr)
		}
		if invalidSuccessResponse {
			return nil, fmt.Errorf("%w: Binance 下单收到无效的 HTTP 2xx 响应 (%v)，但按 clientOrderID=%s 查询又返回 -2013，状态互相矛盾",
				exchangeerr.ErrOrderPlacementUnknown, createErr, clientOrderID)
		}
		if duplicateClientOrderID {
			return nil, fmt.Errorf("%w: Binance 返回 clientOrderID=%s 已重复，但随后查询又返回 -2013，状态互相矛盾",
				exchangeerr.ErrOrderPlacementUnknown, clientOrderID)
		}

		// 只有 Binance 明确返回 -2013（订单不存在）才允许用同一 clientOrderID 重试。
		if attempt == maxAttempts || ctx.Err() != nil {
			return nil, fmt.Errorf("Binance 下单结果不确定，但已用 clientOrderID=%s 确认订单不存在（尝试 %d/%d）: %w",
				clientOrderID, attempt, maxAttempts, createErr)
		}
		logger.Warn("⚠️ [Binance] 下单结果不确定，但 clientOrderID=%s 明确不存在；使用相同 ID 安全重试 (%d/%d)",
			clientOrderID, attempt+1, maxAttempts)
	}

	return nil, fmt.Errorf("Binance 下单重试状态异常")
}

func (b *BinanceAdapter) confirmOrderByClientID(ctx context.Context, symbol, clientOrderID string) (*Order, error) {
	timeout := b.orderConfirmationTimeout
	if timeout <= 0 {
		timeout = defaultOrderConfirmationTimeout
	}
	// 原下单上下文可能已经超时。确认查询必须使用新的有界上下文，且只做只读对账。
	confirmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()

	order, err := b.client.NewGetOrderService().
		Symbol(symbol).
		OrigClientOrderID(clientOrderID).
		Do(confirmCtx)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, fmt.Errorf("Binance clientOrderID=%s 查询返回空订单", clientOrderID)
	}
	if order.ClientOrderID != clientOrderID {
		return nil, fmt.Errorf("Binance clientOrderID 查询结果不匹配: got %s, want %s", order.ClientOrderID, clientOrderID)
	}
	if order.Symbol != symbol {
		return nil, fmt.Errorf("Binance clientOrderID=%s 查询交易对不匹配: got %s, want %s", clientOrderID, order.Symbol, symbol)
	}
	return orderFromBinance(order)
}

func isBinanceAPIErrorCode(err error, code int64) bool {
	var apiErr *common.APIError
	return errors.As(err, &apiErr) && apiErr != nil && apiErr.Code == code
}

func validateCreateOrderResponse(resp *futures.CreateOrderResponse, symbol, clientOrderID string) error {
	if resp == nil {
		return fmt.Errorf("Binance 创建订单返回空响应")
	}
	if resp.OrderID <= 0 {
		return fmt.Errorf("Binance 创建订单返回无效 orderId=%d", resp.OrderID)
	}
	if resp.Symbol != symbol {
		return fmt.Errorf("Binance 创建订单返回交易对不匹配: got %q, want %q", resp.Symbol, symbol)
	}
	if strings.TrimSpace(resp.ClientOrderID) == "" {
		return fmt.Errorf("Binance 创建订单响应缺少 clientOrderId")
	}
	if clientOrderID != "" && resp.ClientOrderID != clientOrderID {
		return fmt.Errorf("Binance 创建订单返回 clientOrderId 不匹配: got %q, want %q", resp.ClientOrderID, clientOrderID)
	}
	if strings.TrimSpace(string(resp.Status)) == "" {
		return fmt.Errorf("Binance 创建订单响应缺少 status")
	}
	return nil
}

func isDuplicateClientOrderIDError(err error) bool {
	if err == nil {
		return false
	}
	if isBinanceAPIErrorCode(err, -4111) || isBinanceAPIErrorCode(err, -4116) {
		return true
	}
	message := strings.ToLower(err.Error())
	return (strings.Contains(message, "duplicate") && strings.Contains(message, "client") &&
		strings.Contains(message, "order") && strings.Contains(message, "id")) ||
		strings.Contains(message, "duplicate client order id") ||
		strings.Contains(message, "duplicated client order id") ||
		strings.Contains(message, "client order id is not unique") ||
		strings.Contains(message, "new client order id is used")
}

func isUnknownPlacementResult(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}

	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr != nil &&
		(statusErr.StatusCode == http.StatusRequestTimeout || statusErr.StatusCode == http.StatusConflict ||
			(statusErr.StatusCode >= http.StatusInternalServerError && statusErr.StatusCode <= 599)) {
		return true
	}
	var bodyLimitErr *responseBodyTooLargeError
	if errors.As(err, &bodyLimitErr) && bodyLimitErr.Method == http.MethodPost && bodyLimitErr.Path == createOrderEndpoint {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if isCreateResponseDecodeError(err) {
		// CreateOrderService 只会在 HTTP 2xx 后反序列化创建响应；解码失败时订单可能已经落库。
		return true
	}
	var apiErr *common.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		switch apiErr.Code {
		case -1000, -1001, -1006, -1007:
			return true
		}
	}

	message := strings.ToLower(err.Error())
	unknownFragments := []string{
		"timeout", "deadline exceeded", "connection reset", "connection aborted",
		"broken pipe", "server disconnected", "disconnected", "connection closed", "closed network connection", "unexpected eof",
		"service unavailable", "http 503", "status code 503",
		"unknown error, please check your request",
	}
	for _, fragment := range unknownFragments {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func isCreateResponseDecodeError(err error) bool {
	if err == nil {
		return false
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &syntaxErr) || errors.As(err, &typeErr)
}

// BatchPlaceOrders 批量下单
func (b *BinanceAdapter) BatchPlaceOrders(ctx context.Context, orders []*OrderRequest) ([]*Order, bool) {
	placedOrders := make([]*Order, 0, len(orders))
	hasMarginError := false

	for _, orderReq := range orders {
		order, err := b.PlaceOrder(ctx, orderReq)
		if err != nil {
			logger.Warn("⚠️ [Binance] 下单失败 %.2f %s: %v",
				orderReq.Price, orderReq.Side, err)

			if strings.Contains(err.Error(), "-2019") || strings.Contains(err.Error(), "insufficient") {
				hasMarginError = true
			}
			continue
		}
		placedOrders = append(placedOrders, order)
	}

	return placedOrders, hasMarginError
}

// CancelOrder 取消订单
func (b *BinanceAdapter) CancelOrder(ctx context.Context, symbol string, orderID int64) error {
	_, err := b.client.NewCancelOrderService().
		Symbol(symbol).
		OrderID(orderID).
		Do(ctx)

	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "-2011") || strings.Contains(errStr, "Unknown order") {
			logger.Info("ℹ️ [Binance] 订单 %d 已不存在，跳过取消", orderID)
			return nil
		}
		return err
	}

	logger.Info("✅ [Binance] 取消订单成功: %d", orderID)
	return nil
}

// BatchCancelOrders 批量撤单
func (b *BinanceAdapter) BatchCancelOrders(ctx context.Context, symbol string, orderIDs []int64) error {
	if len(orderIDs) == 0 {
		return nil
	}

	var cancelErrs []error
	// 🔥 Binance 批量撤单限制：最多10个
	batchSize := 10
	for i := 0; i < len(orderIDs); i += batchSize {
		end := i + batchSize
		if end > len(orderIDs) {
			end = len(orderIDs)
		}

		batch := orderIDs[i:end]

		// 🔥 如果只有1个订单，直接用单个撤单接口
		if len(batch) == 1 {
			if err := b.CancelOrder(ctx, symbol, batch[0]); err != nil {
				logger.Warn("⚠️ [Binance] 取消订单失败 %d: %v", batch[0], err)
				cancelErrs = append(cancelErrs, fmt.Errorf("取消 Binance 订单 %d 失败: %w", batch[0], err))
			}
			continue
		}

		_, err := b.client.NewCancelMultipleOrdersService().
			Symbol(symbol).
			OrderIDList(batch).
			Do(ctx)

		if err != nil {
			logger.Warn("⚠️ [Binance] 批量撤单失败 (共%d个): %v", len(batch), err)
			cancelErrs = append(cancelErrs, fmt.Errorf("Binance 批量撤单失败（%d 个）: %w", len(batch), err))
			if ctx.Err() != nil {
				return errors.Join(cancelErrs...)
			}
			// 失败时尝试单个撤单
			logger.Info("🔄 [Binance] 改为逐个撤单...")
			for index, orderID := range batch {
				if fallbackErr := b.CancelOrder(ctx, symbol, orderID); fallbackErr != nil {
					cancelErrs = append(cancelErrs, fmt.Errorf("Binance 批量撤单回退取消订单 %d 失败: %w", orderID, fallbackErr))
				}
				if index < len(batch)-1 {
					if waitErr := waitWithContext(ctx, 100*time.Millisecond); waitErr != nil {
						cancelErrs = append(cancelErrs, fmt.Errorf("Binance 批量撤单回退等待被取消: %w", waitErr))
						return errors.Join(cancelErrs...)
					}
				}
			}
		} else {
			logger.Info("✅ [Binance] 批量撤单成功: %d 个订单", len(batch))
		}

		// 避免限频
		if i+batchSize < len(orderIDs) {
			if err := waitWithContext(ctx, 100*time.Millisecond); err != nil {
				cancelErrs = append(cancelErrs, fmt.Errorf("Binance 分批撤单等待被取消: %w", err))
				return errors.Join(cancelErrs...)
			}
		}
	}

	return errors.Join(cancelErrs...)
}

// CancelAllOrders 使用 Binance 原生一键全撤接口，并在返回成功后确认未完成订单已经清空。
func (b *BinanceAdapter) CancelAllOrders(ctx context.Context, symbol string) error {
	if strings.TrimSpace(symbol) == "" {
		return fmt.Errorf("Binance 一键全撤缺少交易对")
	}
	if err := b.client.NewCancelAllOpenOrdersService().Symbol(symbol).Do(ctx); err != nil {
		return fmt.Errorf("Binance 一键全撤 %s 失败: %w", symbol, err)
	}

	attempts := b.cancelAllConfirmAttempts
	if attempts <= 0 {
		attempts = defaultCancelAllConfirmAttempts
	}
	delay := b.cancelAllConfirmDelay
	if delay <= 0 {
		delay = defaultCancelAllConfirmDelay
	}

	var remaining []*Order
	for attempt := 1; attempt <= attempts; attempt++ {
		openOrders, err := b.GetOpenOrders(ctx, symbol)
		if err != nil {
			return fmt.Errorf("Binance 一键全撤后第 %d 次确认失败: %w", attempt, err)
		}
		if len(openOrders) == 0 {
			return nil
		}
		remaining = openOrders
		if attempt < attempts {
			if err := waitWithContext(ctx, delay); err != nil {
				return fmt.Errorf("Binance 一键全撤确认等待被取消: %w", err)
			}
		}
	}

	orderIDs := make([]string, 0, len(remaining))
	for _, order := range remaining {
		orderIDs = append(orderIDs, strconv.FormatInt(order.OrderID, 10))
	}
	return fmt.Errorf("Binance 一键全撤后仍残留 %d 个 %s 订单（orderIDs=%s）",
		len(remaining), symbol, strings.Join(orderIDs, ","))
}

func waitWithContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// GetOrder 查询订单
func (b *BinanceAdapter) GetOrder(ctx context.Context, symbol string, orderID int64) (*Order, error) {
	order, err := b.client.NewGetOrderService().
		Symbol(symbol).
		OrderID(orderID).
		Do(ctx)

	if err != nil {
		return nil, err
	}

	return orderFromBinance(order)
}

// GetOpenOrders 查询未完成订单
func (b *BinanceAdapter) GetOpenOrders(ctx context.Context, symbol string) ([]*Order, error) {
	orders, err := b.client.NewListOpenOrdersService().
		Symbol(symbol).
		Do(ctx)

	if err != nil {
		return nil, err
	}

	result := make([]*Order, 0, len(orders))
	for index, order := range orders {
		converted, err := orderFromBinance(order)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance 未完成订单第 %d 条失败: %w", index+1, err)
		}
		result = append(result, converted)
	}

	return result, nil
}

func orderFromBinance(order *futures.Order) (*Order, error) {
	if order == nil {
		return nil, fmt.Errorf("Binance 订单为空")
	}
	if order.OrderID <= 0 {
		return nil, fmt.Errorf("Binance 订单 orderId=%d 无效", order.OrderID)
	}
	if strings.TrimSpace(order.Symbol) == "" {
		return nil, fmt.Errorf("Binance 订单缺少 symbol")
	}
	if strings.TrimSpace(order.ClientOrderID) == "" {
		return nil, fmt.Errorf("Binance 订单缺少 clientOrderId")
	}
	if strings.TrimSpace(string(order.Status)) == "" {
		return nil, fmt.Errorf("Binance 订单缺少 status")
	}
	if order.Side != futures.SideTypeBuy && order.Side != futures.SideTypeSell {
		return nil, fmt.Errorf("Binance 订单 side=%q 无效", order.Side)
	}
	if strings.TrimSpace(string(order.Type)) == "" {
		return nil, fmt.Errorf("Binance 订单缺少 type")
	}
	price, err := parseFiniteFloat("price", order.Price)
	if err != nil {
		return nil, err
	}
	quantity, err := parseFiniteFloat("origQty", order.OrigQuantity)
	if err != nil {
		return nil, err
	}
	executedQty, err := parseFiniteFloat("executedQty", order.ExecutedQuantity)
	if err != nil {
		return nil, err
	}
	avgPrice, err := parseFiniteFloat("avgPrice", order.AvgPrice)
	if err != nil {
		return nil, err
	}
	if price < 0 || quantity < 0 || executedQty < 0 || avgPrice < 0 {
		return nil, fmt.Errorf("Binance 订单数值不能为负: price=%g origQty=%g executedQty=%g avgPrice=%g",
			price, quantity, executedQty, avgPrice)
	}
	if executedQty > quantity {
		return nil, fmt.Errorf("Binance 订单 executedQty=%g 大于 origQty=%g", executedQty, quantity)
	}

	createdAt := time.Time{}
	if order.Time > 0 {
		createdAt = time.UnixMilli(order.Time)
	}
	return &Order{
		OrderID:       order.OrderID,
		ClientOrderID: order.ClientOrderID,
		Symbol:        order.Symbol,
		Side:          Side(order.Side),
		Type:          OrderType(order.Type),
		Price:         price,
		Quantity:      quantity,
		ExecutedQty:   executedQty,
		AvgPrice:      avgPrice,
		Status:        OrderStatus(order.Status),
		CreatedAt:     createdAt,
		UpdateTime:    order.UpdateTime,
	}, nil
}

// GetAccount 获取账户信息（合约账户）
func (b *BinanceAdapter) GetAccount(ctx context.Context) (*Account, error) {
	if b.contractSpec == nil || strings.TrimSpace(b.contractSpec.MarginAsset) == "" {
		return nil, fmt.Errorf("Binance 合约保证金币种未初始化")
	}
	account, err := b.fetchAccount(ctx)
	if err != nil {
		return nil, err
	}
	marginAsset, err := findAccountAsset(account, b.contractSpec.MarginAsset)
	if err != nil {
		return nil, err
	}
	totalWalletBalance, err := parseFiniteFloat("walletBalance", marginAsset.WalletBalance)
	if err != nil {
		return nil, fmt.Errorf("解析 Binance %s 余额失败: %w", marginAsset.Asset, err)
	}
	totalMarginBalance, err := parseFiniteFloat("marginBalance", marginAsset.MarginBalance)
	if err != nil {
		return nil, fmt.Errorf("解析 Binance %s 余额失败: %w", marginAsset.Asset, err)
	}
	availableBalance, err := parseFiniteFloat("availableBalance", marginAsset.AvailableBalance)
	if err != nil {
		return nil, fmt.Errorf("解析 Binance %s 余额失败: %w", marginAsset.Asset, err)
	}

	positions := make([]*Position, 0, len(account.Positions))
	for _, pos := range account.Positions {
		converted, err := accountPositionFromBinance(pos)
		if err != nil {
			return nil, err
		}
		if converted == nil {
			continue
		}
		positions = append(positions, converted)
	}

	return &Account{
		TotalWalletBalance: totalWalletBalance,
		TotalMarginBalance: totalMarginBalance,
		AvailableBalance:   availableBalance,
		Positions:          positions,
	}, nil
}

// ValidateAccount 在任何下单前验证 Binance 账户是否满足本程序的单向做多约束。
func (b *BinanceAdapter) ValidateAccount(ctx context.Context) error {
	if strings.TrimSpace(b.symbol) == "" {
		return fmt.Errorf("Binance 配置交易对为空")
	}
	b.ensureStatusAwareTransport()
	account, err := b.fetchAccount(ctx)
	if err != nil {
		return fmt.Errorf("查询 Binance 账户失败: %w", err)
	}
	if !account.CanTrade {
		return fmt.Errorf("Binance 账户 canTrade=false，禁止启动交易")
	}

	positionMode, err := b.client.NewGetPositionModeService().Do(ctx)
	if err != nil {
		return fmt.Errorf("查询 Binance 持仓模式失败: %w", err)
	}
	if positionMode == nil {
		return fmt.Errorf("查询 Binance 持仓模式返回空结果")
	}
	if positionMode.DualSidePosition {
		return fmt.Errorf("Binance 账户处于双向持仓模式，程序要求单向持仓模式；不会自动修改账户设置")
	}

	positions, err := b.GetPositions(ctx, b.symbol)
	if err != nil {
		return fmt.Errorf("查询 Binance %s 持仓失败: %w", b.symbol, err)
	}
	foundSymbol := false
	for _, position := range positions {
		if strings.EqualFold(position.Symbol, b.symbol) {
			foundSymbol = true
		}
		if strings.EqualFold(position.Symbol, b.symbol) && position.Size < 0 {
			return fmt.Errorf("Binance %s 存在空头持仓 %g，单向做多程序拒绝启动", b.symbol, position.Size)
		}
	}
	if !foundSymbol {
		return fmt.Errorf("Binance 持仓接口未返回配置交易对 %s，无法确认账户方向", b.symbol)
	}
	return nil
}

// GetPositions 获取持仓信息（使用PositionRisk API获取准确的杠杆倍数）
func (b *BinanceAdapter) GetPositions(ctx context.Context, symbol string) ([]*Position, error) {
	positionRisks, err := b.client.NewGetPositionRiskService().Symbol(symbol).Do(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*Position, 0)
	for _, pos := range positionRisks {
		if pos == nil {
			return nil, fmt.Errorf("Binance %s 持仓响应包含空记录", symbol)
		}
		posAmt, err := parseFiniteFloat("positionAmt", pos.PositionAmt)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance %s 持仓失败: %w", pos.Symbol, err)
		}
		entryPrice, err := parseFiniteFloat("entryPrice", pos.EntryPrice)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance %s 持仓失败: %w", pos.Symbol, err)
		}
		unrealizedPNL, err := parseFiniteFloat("unRealizedProfit", pos.UnRealizedProfit)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance %s 持仓失败: %w", pos.Symbol, err)
		}
		markPrice, err := parseFiniteFloat("markPrice", pos.MarkPrice)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance %s 持仓失败: %w", pos.Symbol, err)
		}
		isolatedMargin, err := parseFiniteFloat("isolatedMargin", pos.IsolatedMargin)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance %s 持仓失败: %w", pos.Symbol, err)
		}
		leverage, err := strconv.Atoi(strings.TrimSpace(pos.Leverage))
		if err != nil {
			return nil, fmt.Errorf("解析 Binance %s 持仓 leverage=%q 失败: %w", pos.Symbol, pos.Leverage, err)
		}

		result = append(result, &Position{
			Symbol:         pos.Symbol,
			Size:           posAmt,
			EntryPrice:     entryPrice,
			MarkPrice:      markPrice,
			UnrealizedPNL:  unrealizedPNL,
			Leverage:       leverage,
			MarginType:     pos.MarginType,
			IsolatedMargin: isolatedMargin,
		})
	}

	return result, nil
}

// GetBalance 获取余额
func (b *BinanceAdapter) GetBalance(ctx context.Context, asset string) (float64, error) {
	asset = strings.TrimSpace(asset)
	if asset == "" {
		return 0, fmt.Errorf("查询 Binance 余额时资产不能为空")
	}
	account, err := b.fetchAccount(ctx)
	if err != nil {
		return 0, err
	}
	accountAsset, err := findAccountAsset(account, asset)
	if err != nil {
		return 0, err
	}
	available, err := parseFiniteFloat("availableBalance", accountAsset.AvailableBalance)
	if err != nil {
		return 0, fmt.Errorf("解析 Binance %s 余额失败: %w", accountAsset.Asset, err)
	}
	return available, nil
}

func (b *BinanceAdapter) fetchAccount(ctx context.Context) (*futures.Account, error) {
	account, err := b.client.NewGetAccountService().Do(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "Service unavailable from a restricted location") {
			return nil, fmt.Errorf("你的网络连接在限制服务区域，请检查网络或使用代理: %w", err)
		}
		return nil, err
	}
	if account == nil {
		return nil, fmt.Errorf("Binance 账户接口返回空结果")
	}
	return account, nil
}

func findAccountAsset(account *futures.Account, wanted string) (*futures.AccountAsset, error) {
	if account == nil {
		return nil, fmt.Errorf("Binance 账户为空")
	}
	wanted = strings.TrimSpace(wanted)
	for _, asset := range account.Assets {
		if asset != nil && strings.EqualFold(asset.Asset, wanted) {
			return asset, nil
		}
	}
	return nil, fmt.Errorf("Binance 合约账户缺少资产 %s", wanted)
}

func accountPositionFromBinance(pos *futures.AccountPosition) (*Position, error) {
	if pos == nil {
		return nil, fmt.Errorf("Binance 账户持仓包含空记录")
	}
	posAmt, err := parseFiniteFloat("positionAmt", pos.PositionAmt)
	if err != nil {
		return nil, fmt.Errorf("解析 Binance %s 账户持仓失败: %w", pos.Symbol, err)
	}
	if posAmt == 0 {
		return nil, nil
	}
	entryPrice, err := parseFiniteFloat("entryPrice", pos.EntryPrice)
	if err != nil {
		return nil, fmt.Errorf("解析 Binance %s 账户持仓失败: %w", pos.Symbol, err)
	}
	unrealizedPNL, err := parseFiniteFloat("unrealizedProfit", pos.UnrealizedProfit)
	if err != nil {
		return nil, fmt.Errorf("解析 Binance %s 账户持仓失败: %w", pos.Symbol, err)
	}
	leverage, err := strconv.Atoi(strings.TrimSpace(pos.Leverage))
	if err != nil {
		return nil, fmt.Errorf("解析 Binance %s 账户持仓 leverage=%q 失败: %w", pos.Symbol, pos.Leverage, err)
	}
	return &Position{
		Symbol:        pos.Symbol,
		Size:          posAmt,
		EntryPrice:    entryPrice,
		UnrealizedPNL: unrealizedPNL,
		Leverage:      leverage,
	}, nil
}

func parseFiniteFloat(field, value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q 不是有效数字: %w", field, value, err)
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, fmt.Errorf("%s=%q 不是有限数字", field, value)
	}
	return parsed, nil
}

// StartOrderStream 启动订单流（WebSocket）
func (b *BinanceAdapter) StartOrderStream(ctx context.Context, callback func(interface{})) error {
	// 转换回调函数：将 binance.OrderUpdate 转换为通用格式
	localCallback := func(update OrderUpdate) {
		// 构造通用的 OrderUpdate 结构（避免导入 exchange 包）
		genericUpdate := struct {
			OrderID                int64
			ClientOrderID          string
			Symbol                 string
			Side                   string
			Type                   string
			Status                 string
			Price                  float64
			Quantity               float64
			ExecutedQty            float64
			AvgPrice               float64
			UpdateTime             int64
			RealizedPNL            float64
			RealizedPNLIncremental bool
		}{
			OrderID:                update.OrderID,
			ClientOrderID:          update.ClientOrderID, // 🔥 关键：传递 ClientOrderID
			Symbol:                 update.Symbol,
			Side:                   string(update.Side),
			Type:                   string(update.Type),
			Status:                 string(update.Status),
			Price:                  update.Price,
			Quantity:               update.Quantity,
			ExecutedQty:            update.ExecutedQty,
			AvgPrice:               update.AvgPrice,
			UpdateTime:             update.UpdateTime,
			RealizedPNL:            update.RealizedPNL,
			RealizedPNLIncremental: update.RealizedPNLIncremental,
		}
		callback(genericUpdate)
	}
	return b.wsManager.Start(ctx, localCallback)
}

// StopOrderStream 停止订单流
func (b *BinanceAdapter) StopOrderStream() error {
	b.wsManager.Stop()
	return nil
}

// GetLatestPrice 获取最新价格（仅从 WebSocket 缓存读取）
// 架构说明：
// - 各组件不应直接调用此方法获取实时价格
// - 实时价格应该通过 PriceMonitor.GetLastPrice() 获取（订阅模式）
// - 此方法仅用于下单时的价格诊断（检查订单价格与市场价格的偏离）
// - WebSocket 是唯一的价格来源，不使用 REST API
// - 如果 WebSocket 未启动或断开，返回错误
func (b *BinanceAdapter) GetLatestPrice(ctx context.Context, symbol string) (float64, error) {
	// 从 WebSocket 缓存读取价格
	if b.wsManager != nil {
		price := b.wsManager.GetLatestPrice()
		if price > 0 {
			return price, nil
		}
	}

	// WebSocket 未启动或无价格数据
	return 0, fmt.Errorf("WebSocket 价格流未就绪或无价格数据")
}

// StartPriceStream 启动价格流（WebSocket）
func (b *BinanceAdapter) StartPriceStream(ctx context.Context, symbol string, callback func(price float64)) error {
	// 启动价格流
	return b.wsManager.StartPriceStream(ctx, symbol, callback)
}

// StartKlineStream 启动K线流（WebSocket）
func (b *BinanceAdapter) StartKlineStream(ctx context.Context, symbols []string, interval string, callback func(candle interface{})) error {
	if b.klineWSManager == nil {
		b.klineWSManager = NewKlineWebSocketManager()
	}
	return b.klineWSManager.Start(ctx, symbols, interval, callback)
}

// StopKlineStream 停止K线流
func (b *BinanceAdapter) StopKlineStream() error {
	if b.klineWSManager != nil {
		b.klineWSManager.Stop()
	}
	return nil
}

// GetHistoricalKlines 获取历史K线数据
func (b *BinanceAdapter) GetHistoricalKlines(ctx context.Context, symbol string, interval string, limit int) ([]*Candle, error) {
	klines, err := b.client.NewKlinesService().
		Symbol(symbol).
		Interval(interval).
		Limit(limit).
		Do(ctx)

	if err != nil {
		return nil, fmt.Errorf("获取历史K线失败: %w", err)
	}

	candles := make([]*Candle, 0, len(klines))
	nowMillis := time.Now().UnixMilli()
	for _, k := range klines {
		open, err := parseFiniteFloat("open", k.Open)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance K线 openTime=%d 失败: %w", k.OpenTime, err)
		}
		high, err := parseFiniteFloat("high", k.High)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance K线 openTime=%d 失败: %w", k.OpenTime, err)
		}
		low, err := parseFiniteFloat("low", k.Low)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance K线 openTime=%d 失败: %w", k.OpenTime, err)
		}
		close, err := parseFiniteFloat("close", k.Close)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance K线 openTime=%d 失败: %w", k.OpenTime, err)
		}
		volume, err := parseFiniteFloat("volume", k.Volume)
		if err != nil {
			return nil, fmt.Errorf("解析 Binance K线 openTime=%d 失败: %w", k.OpenTime, err)
		}

		candles = append(candles, &Candle{
			Symbol:    symbol,
			Open:      open,
			High:      high,
			Low:       low,
			Close:     close,
			Volume:    volume,
			Timestamp: k.OpenTime,
			IsClosed:  k.CloseTime <= nowMillis,
		})
	}

	return candles, nil
}

// GetPriceDecimals 获取价格精度（小数位数）
func (b *BinanceAdapter) GetPriceDecimals() int {
	return b.priceDecimals
}

// GetQuantityDecimals 获取数量精度（小数位数）
func (b *BinanceAdapter) GetQuantityDecimals() int {
	return b.quantityDecimals
}

// GetBaseAsset 获取基础资产（交易币种）
func (b *BinanceAdapter) GetBaseAsset() string {
	return b.baseAsset
}

// GetQuoteAsset 获取计价资产（结算币种）
func (b *BinanceAdapter) GetQuoteAsset() string {
	return b.quoteAsset
}
