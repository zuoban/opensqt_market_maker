package order

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opensqt/exchange"
	"opensqt/logger"

	"github.com/adshao/go-binance/v2/common"
	"golang.org/x/time/rate"
)

const defaultRequestTimeout = 10 * time.Second

var (
	// ErrNewOrdersStopped 表示订单执行器的新下单门禁已关闭。
	ErrNewOrdersStopped = errors.New("新下单已停止")
	// ErrOrderExecutorStopped 表示订单执行器的根上下文已关闭，不能再次启用。
	ErrOrderExecutorStopped = errors.New("订单执行器已停止")
	// ErrOrderSubmissionStale 表示槽位 reservation 在实际提交前已失效。
	ErrOrderSubmissionStale = errors.New("订单提交 reservation 已失效")
	// ErrTradingHealthGuardRejected 表示订单在最终交易所边界前的
	// 同步健康复检失败，请求尚未发送到交易所。
	ErrTradingHealthGuardRejected = errors.New("下单前交易健康复检失败")
)

type contextWaiter interface {
	Wait(context.Context) error
}

// OrderRequest 订单请求
type OrderRequest struct {
	Symbol        string
	Side          string
	Price         float64
	Quantity      float64
	PriceDecimals int    // 价格小数位数（用于格式化价格字符串）
	ReduceOnly    bool   // 是否只减仓（平仓单）
	PostOnly      bool   // 是否只做 Maker（Post Only）
	ClientOrderID string // 自定义订单ID

	// AcquireSubmissionLease 必须在每次真正的交易所请求前调用，并在该次请求
	// 返回后释放。实现会在 lease 期间持有槽位锁，因此不得从 PlaceOrder 同步
	// 重入同一槽位的订单更新回调；真实订单流应通过异步边界投递。
	AcquireSubmissionLease func() (release func(), ok bool)
	// OnSubmissionUnknown 在请求已进入交易所边界、但结果无法确认时调用。
	OnSubmissionUnknown func()
}

// Order 订单信息
type Order struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          string
	Type          string
	Price         float64
	Quantity      float64
	ExecutedQty   float64
	AvgPrice      float64
	Status        string
	CreatedAt     time.Time
	UpdateTime    int64
}

// ExchangeOrderExecutor 基于 exchange.IExchange 的订单执行器
type ExchangeOrderExecutor struct {
	exchange    exchange.IExchange
	symbol      string
	rateLimiter contextWaiter

	rootCtx    context.Context
	rootCancel context.CancelFunc

	newOrdersMu      sync.Mutex
	newOrdersCtx     context.Context
	newOrdersCancel  context.CancelFunc
	newOrdersEnabled atomic.Bool
	healthGuardMu    sync.RWMutex
	healthGuard      func() error

	// 时间配置
	rateLimitRetryDelay time.Duration
	orderRetryDelay     time.Duration
	requestTimeout      time.Duration
}

// SetSubmissionHealthGuard 设置每次真实交易所请求前的同步健康复检。
// guard 会在限流完成且已获取槽位 submission lease 后执行；它不得
// 重入订单提交或阻塞等待槽位回调。
func (oe *ExchangeOrderExecutor) SetSubmissionHealthGuard(guard func() error) {
	oe.healthGuardMu.Lock()
	oe.healthGuard = guard
	oe.healthGuardMu.Unlock()
}

func (oe *ExchangeOrderExecutor) checkSubmissionHealth() error {
	oe.healthGuardMu.RLock()
	guard := oe.healthGuard
	oe.healthGuardMu.RUnlock()
	if guard == nil {
		return nil
	}
	return guard()
}

// NewExchangeOrderExecutor 创建基于交易所接口的订单执行器
func NewExchangeOrderExecutor(ex exchange.IExchange, symbol string, rateLimitRetryDelay, orderRetryDelay int) *ExchangeOrderExecutor {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	newOrdersCtx, newOrdersCancel := context.WithCancel(rootCtx)
	oe := &ExchangeOrderExecutor{
		exchange:            ex,
		symbol:              symbol,
		rateLimiter:         rate.NewLimiter(rate.Limit(25), 30), // 25单/秒，突发30
		rootCtx:             rootCtx,
		rootCancel:          rootCancel,
		newOrdersCtx:        newOrdersCtx,
		newOrdersCancel:     newOrdersCancel,
		rateLimitRetryDelay: time.Duration(rateLimitRetryDelay) * time.Second,
		orderRetryDelay:     time.Duration(orderRetryDelay) * time.Millisecond,
		requestTimeout:      defaultRequestTimeout,
	}
	oe.newOrdersEnabled.Store(true)
	return oe
}

// StopNewOrders 关闭新下单门禁，并取消正在等待限流、退避或交易所响应的下单。
// 撤单使用根上下文，不受此门禁影响。
func (oe *ExchangeOrderExecutor) StopNewOrders() {
	oe.newOrdersMu.Lock()
	oe.newOrdersEnabled.Store(false)
	oe.newOrdersCancel()
	oe.newOrdersMu.Unlock()
}

// EnableNewOrders 重新开启新下单门禁。执行器整体停止后不能重新开启。
func (oe *ExchangeOrderExecutor) EnableNewOrders() error {
	oe.newOrdersMu.Lock()
	defer oe.newOrdersMu.Unlock()

	if err := oe.rootCtx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrOrderExecutorStopped, err)
	}
	if oe.newOrdersEnabled.Load() && oe.newOrdersCtx.Err() == nil {
		return nil
	}

	oe.newOrdersCtx, oe.newOrdersCancel = context.WithCancel(oe.rootCtx)
	oe.newOrdersEnabled.Store(true)
	return nil
}

// Shutdown 关闭新下单门禁，并取消执行器的所有在途操作。
func (oe *ExchangeOrderExecutor) Shutdown() {
	oe.newOrdersMu.Lock()
	oe.newOrdersEnabled.Store(false)
	oe.newOrdersCancel()
	oe.rootCancel()
	oe.newOrdersMu.Unlock()
}

func (oe *ExchangeOrderExecutor) newOrderContext() (context.Context, error) {
	oe.newOrdersMu.Lock()
	defer oe.newOrdersMu.Unlock()

	if !oe.newOrdersEnabled.Load() {
		return nil, ErrNewOrdersStopped
	}
	if err := oe.rootCtx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOrderExecutorStopped, err)
	}
	return oe.newOrdersCtx, nil
}

func (oe *ExchangeOrderExecutor) requestContext(parent context.Context) (context.Context, context.CancelFunc) {
	if oe.requestTimeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, oe.requestTimeout)
}

func waitWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func apiErrorCode(err error) (int64, bool) {
	var apiErr *common.APIError
	if errors.As(err, &apiErr) && apiErr != nil {
		return apiErr.Code, true
	}
	return 0, false
}

func hasErrorCode(err error, code int64) bool {
	if actual, ok := apiErrorCode(err); ok {
		return actual == code
	}
	return err != nil && strings.Contains(err.Error(), fmt.Sprintf("%d", code))
}

func isMarginError(err error) bool {
	if hasErrorCode(err, -2019) {
		return true
	}
	errStr := strings.ToLower(err.Error())
	return strings.Contains(errStr, "保证金不足") || strings.Contains(errStr, "insufficient")
}

func (oe *ExchangeOrderExecutor) placementContextError(prefix string, err error) error {
	if !oe.newOrdersEnabled.Load() {
		return fmt.Errorf("%s: %w", prefix, errors.Join(ErrNewOrdersStopped, err))
	}
	return fmt.Errorf("%s: %w", prefix, err)
}

// isPostOnlyError 检查是否为PostOnly错误
func isPostOnlyError(err error) bool {
	if err == nil {
		return false
	}
	if hasErrorCode(err, -5022) {
		return true
	}
	errStr := err.Error()
	// Binance: code=-5022, Bitget: Post Only order will be rejected, Gate.io: ORDER_POC_IMMEDIATE
	return strings.Contains(errStr, "Post Only") ||
		strings.Contains(errStr, "post_only") ||
		strings.Contains(errStr, "would immediately match") ||
		strings.Contains(errStr, "ORDER_POC_IMMEDIATE")
}

func classifyDefiniteOrderRejection(err error) (OrderRejectionKind, bool) {
	if err == nil {
		return "", false
	}
	// UNKNOWN 的安全优先级最高。即使错误文本同时带有保证金、PostOnly
	// 等字样，也不能把可能已经落库的请求降级成“明确未受理”。
	if errors.Is(err, exchange.ErrOrderPlacementUnknown) {
		return "", false
	}
	if isPostOnlyError(err) {
		return OrderRejectionPostOnly, true
	}
	if isMarginError(err) {
		return OrderRejectionMargin, true
	}
	if hasErrorCode(err, -4061) {
		return OrderRejectionPositionMode, true
	}
	if hasErrorCode(err, -1021) {
		return OrderRejectionTimestamp, true
	}
	errStr := strings.ToLower(err.Error())
	if hasErrorCode(err, -1003) || strings.Contains(errStr, "rate limit") {
		return OrderRejectionRateLimit, true
	}
	if exchange.IsOrderPlacementRejected(err) {
		return OrderRejectionExchangeRejected, true
	}
	return "", false
}

func markSubmissionUnknown(req *OrderRequest) {
	if req != nil && req.OnSubmissionUnknown != nil {
		req.OnSubmissionUnknown()
	}
}

func unknownSubmissionError(req *OrderRequest, prefix string, err error) error {
	markSubmissionUnknown(req)
	return fmt.Errorf("%s，提交结果无法确认: %w", prefix,
		errors.Join(exchange.ErrOrderPlacementUnknown, err))
}

func acquireSubmissionLease(req *OrderRequest) (func(), bool) {
	if req == nil || req.AcquireSubmissionLease == nil {
		return func() {}, true
	}
	release, ok := req.AcquireSubmissionLease()
	if !ok {
		return nil, false
	}
	if release == nil {
		release = func() {}
	}
	return release, true
}

// PlaceOrder 下单（严格 PostOnly，带重试）
func (oe *ExchangeOrderExecutor) PlaceOrder(req *OrderRequest) (*Order, error) {
	placeCtx, err := oe.newOrderContext()
	if err != nil {
		return nil, err
	}

	const maxRetries = 5 // 首次尝试失败后最多再重试5次
	var lastErr error

	for i := 0; i <= maxRetries; i++ {
		if !oe.newOrdersEnabled.Load() {
			return nil, ErrNewOrdersStopped
		}
		// 每次实际尝试都重新限流，重试不能绕过订单预算。
		if err := oe.rateLimiter.Wait(placeCtx); err != nil {
			return nil, oe.placementContextError("速率限制等待失败", err)
		}

		// 限流完成后才获取槽位 lease。lease 将“最终状态复检”和本次真实
		// PlaceOrder 调用线性化，终态修正无法插入二者之间使请求过期。
		release, ok := acquireSubmissionLease(req)
		if !ok {
			return nil, ErrOrderSubmissionStale
		}
		if !oe.newOrdersEnabled.Load() || placeCtx.Err() != nil {
			release()
			gateErr := placeCtx.Err()
			if gateErr == nil {
				gateErr = ErrNewOrdersStopped
			}
			return nil, oe.placementContextError("下单前交易门禁已关闭", gateErr)
		}
		if guardErr := oe.checkSubmissionHealth(); guardErr != nil {
			// 健康复检失败时请求尚未进入交易所边界。先释放槽位
			// lease，再立即取消执行器的整代新单上下文，确保本批不会
			// 继续提交其余订单。
			release()
			oe.StopNewOrders()
			return nil, fmt.Errorf("%w: %v", ErrTradingHealthGuardRejected, guardErr)
		}
		// guard 执行期间健康观察器也可能已关闭执行器；真正
		// 进入交易所前再复检一次可取消门禁。
		if !oe.newOrdersEnabled.Load() || placeCtx.Err() != nil {
			release()
			gateErr := placeCtx.Err()
			if gateErr == nil {
				gateErr = ErrNewOrdersStopped
			}
			return nil, oe.placementContextError("健康复检后交易门禁已关闭", gateErr)
		}

		exchangeReq := &exchange.OrderRequest{
			Symbol:        req.Symbol,
			Side:          exchange.Side(req.Side),
			Type:          exchange.OrderTypeLimit,
			TimeInForce:   exchange.TimeInForceGTC,
			Quantity:      req.Quantity,
			Price:         req.Price,
			PriceDecimals: req.PriceDecimals,
			ReduceOnly:    req.ReduceOnly,
			// 在最终下单边界强制 PostOnly，防止上层遗漏导致 Taker 成交。
			PostOnly:      true,
			ClientOrderID: req.ClientOrderID,
		}

		// lease 必须覆盖实际交易所调用，但在返回后立即释放，以免占用后续
		// 重试等待。订单流是异步边界，会在此处短暂等待同一槽位锁。
		requestCtx, cancel := oe.requestContext(placeCtx)
		exchangeOrder, err := func() (*exchange.Order, error) {
			defer release()
			return oe.exchange.PlaceOrder(requestCtx, exchangeReq)
		}()
		cancel()
		if err == nil {
			// 转换回 Order 格式
			order := &Order{
				OrderID:       exchangeOrder.OrderID,
				ClientOrderID: exchangeOrder.ClientOrderID,
				Symbol:        exchangeOrder.Symbol,
				Side:          string(exchangeOrder.Side),
				Type:          string(exchangeOrder.Type),
				Price:         exchangeOrder.Price,
				Quantity:      exchangeOrder.Quantity,
				ExecutedQty:   exchangeOrder.ExecutedQty,
				AvgPrice:      exchangeOrder.AvgPrice,
				Status:        string(exchangeOrder.Status),
				CreatedAt:     exchangeOrder.CreatedAt,
				UpdateTime:    exchangeOrder.UpdateTime,
			}
			if order.Symbol == "" {
				order.Symbol = req.Symbol
			}
			if order.Side == "" {
				order.Side = req.Side
			}
			if order.CreatedAt.IsZero() {
				order.CreatedAt = time.Now()
			}

			logger.Info("✅ [%s] 下单成功(PostOnly): %s %.*f 数量: %.4f 订单ID: %d",
				oe.exchange.GetName(), order.Side, req.PriceDecimals, order.Price, order.Quantity, exchangeOrder.OrderID)
			return order, nil
		}

		lastErr = err
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// 已进入交易所调用边界后才收到取消/超时，无法证明请求没有被接受。
			return nil, unknownSubmissionError(req, "下单请求已取消或超时",
				errors.Join(err, oe.placementContextError("交易门禁状态", err)))
		}
		if errors.Is(err, exchange.ErrOrderPlacementUnknown) {
			// 交易所可能已经接受订单。此时绝不能由执行器再次提交，否则会重复下单。
			return nil, unknownSubmissionError(req, "下单结果无法确认，禁止自动重试", err)
		}

		// 明确业务拒绝可以安全地告诉上层“远端没有生成订单”。限流保留
		// 既有退避重试；PostOnly 同价重试没有意义，首次拒绝即返回。
		rejectionKind, definitelyRejected := classifyDefiniteOrderRejection(err)
		if rejectionKind == OrderRejectionRateLimit {
			lastErr = NewOrderRejectedError(rejectionKind, err)
			// 速率限制，等待后重试
			logger.Warn("⚠️ 触发速率限制，等待后重试...")
			if i < maxRetries {
				if err := waitWithContext(placeCtx, oe.rateLimitRetryDelay); err != nil {
					return nil, oe.placementContextError("速率限制退避被取消", err)
				}
			}
			continue
		}
		if definitelyRejected {
			rejectedErr := NewOrderRejectedError(rejectionKind, err)
			switch rejectionKind {
			case OrderRejectionPostOnly:
				logger.Warn("⚠️ [%s] PostOnly被拒: %s %.2f，严格Maker模式不降级且不做同价重试",
					oe.exchange.GetName(), req.Side, req.Price)
			case OrderRejectionPositionMode:
				return nil, fmt.Errorf("持仓模式不匹配: %w", rejectedErr)
			}
			return nil, rejectedErr
		}

		// 其他错误，短暂等待后重试
		if i < maxRetries {
			if err := waitWithContext(placeCtx, oe.orderRetryDelay); err != nil {
				return nil, oe.placementContextError("下单重试等待被取消", err)
			}
		}
	}

	return nil, fmt.Errorf("下单失败（重试%d次）: %w", maxRetries, lastErr)
}

// BatchPlaceOrders 批量下单
// 返回：成功下单的订单列表、是否出现保证金不足错误，以及其它下单错误。
// 结果未知等错误必须向上传递，调用方才能关闭门禁并先对账，不能释放槽位后换新 ID 重下。
func (oe *ExchangeOrderExecutor) BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool, error) {
	placedOrders := make([]*Order, 0, len(orders))
	hasMarginError := false
	skipRemainingBuys := false
	var placementErrors []error

	for _, orderReq := range orders {
		// 一次明确的保证金拒绝足以暂停本批剩余 BUY；继续扫描是为了让
		// 非标准调用顺序中的 ReduceOnly SELL 仍有机会提交。
		if skipRemainingBuys && strings.EqualFold(orderReq.Side, "BUY") {
			logger.Warn("⏭️ [%s] 本批已出现保证金不足，跳过后续买单 %.2f",
				oe.exchange.GetName(), orderReq.Price)
			continue
		}
		order, err := oe.PlaceOrder(orderReq)
		if err != nil {
			if errors.Is(err, ErrOrderSubmissionStale) {
				logger.Debug("⏭️ [%s] 跳过已失效 reservation: %.2f %s (ClientOID=%s)",
					oe.exchange.GetName(), orderReq.Price, orderReq.Side, orderReq.ClientOrderID)
				continue
			}
			logger.Warn("⚠️ [%s] 下单失败 %.2f %s: %v",
				oe.exchange.GetName(), orderReq.Price, orderReq.Side, err)

			// UNKNOWN、门禁和上下文错误必须无条件向上传播。错误文本即使
			// 同时含有 insufficient，也不能被保证金分支吞掉，否则槽位会
			// 保持 PENDING 而门禁却不会进入对账恢复。
			mustStopBatch := errors.Is(err, exchange.ErrOrderPlacementUnknown) ||
				errors.Is(err, ErrTradingHealthGuardRejected) ||
				errors.Is(err, ErrNewOrdersStopped) || errors.Is(err, ErrOrderExecutorStopped) ||
				errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
			marginFailure := isMarginError(err)
			if marginFailure {
				hasMarginError = true
				logger.Error("❌ [保证金不足] 订单 %.2f %s 因保证金不足失败", orderReq.Price, orderReq.Side)
				if !mustStopBatch {
					skipRemainingBuys = true
				}
			}
			if !marginFailure || mustStopBatch {
				placementErrors = append(placementErrors,
					fmt.Errorf("订单 %.12g %s 提交失败: %w", orderReq.Price, orderReq.Side, err))
			}

			// 结果未知或门禁已关闭时必须立即停止本批次，避免继续扩大不确定状态。
			if mustStopBatch {
				break
			}
			// 单笔限流重试已经耗尽时对整个批次施加背压，避免后续每个订单
			// 再分别消耗完整的重试预算。PostOnly 等其它明确拒绝仍可逐单继续。
			var rejected *OrderRejectedError
			if IsDefiniteOrderRejection(err) && errors.As(err, &rejected) &&
				rejected.Kind == OrderRejectionRateLimit {
				logger.Warn("⚠️ [%s] 限流重试已耗尽，停止提交本批剩余订单", oe.exchange.GetName())
				break
			}
			continue
		}
		placedOrders = append(placedOrders, order)
	}

	return placedOrders, hasMarginError, errors.Join(placementErrors...)
}

// CancelOrder 取消订单
func (oe *ExchangeOrderExecutor) CancelOrder(orderID int64) error {
	// 限流
	if err := oe.rateLimiter.Wait(oe.rootCtx); err != nil {
		return fmt.Errorf("速率限制等待失败: %w", err)
	}

	requestCtx, cancel := oe.requestContext(oe.rootCtx)
	err := oe.exchange.CancelOrder(requestCtx, oe.symbol, orderID)
	cancel()
	if err != nil {
		// 如果是"Unknown order"错误，说明订单已经不存在（可能已成交或已取消），不算错误
		errStr := err.Error()
		if hasErrorCode(err, -2011) || strings.Contains(errStr, "Unknown order") || strings.Contains(errStr, "does not exist") {
			logger.Info("ℹ️ [%s] 订单 %d 已不存在（可能已成交或已取消），跳过取消", oe.exchange.GetName(), orderID)
			return nil
		}
		return fmt.Errorf("取消订单失败: %w", err)
	}

	logger.Info("✅ [%s] 取消订单成功: %d", oe.exchange.GetName(), orderID)
	return nil
}

// BatchCancelOrders 批量撤单
func (oe *ExchangeOrderExecutor) BatchCancelOrders(orderIDs []int64) error {
	if len(orderIDs) == 0 {
		return nil
	}

	if err := oe.rateLimiter.Wait(oe.rootCtx); err != nil {
		return fmt.Errorf("批量撤单限流等待失败: %w", err)
	}

	// 使用交易所的批量撤单接口
	requestCtx, cancel := oe.requestContext(oe.rootCtx)
	err := oe.exchange.BatchCancelOrders(requestCtx, oe.symbol, orderIDs)
	cancel()
	if err != nil {
		logger.Warn("⚠️ [%s] 批量撤单失败: %v，尝试单个撤单", oe.exchange.GetName(), err)
		errs := []error{fmt.Errorf("批量撤单失败: %w", err)}
		// 如果批量撤单失败，尝试单个撤单
		for _, orderID := range orderIDs {
			if cancelErr := oe.CancelOrder(orderID); cancelErr != nil {
				logger.Warn("⚠️ [%s] 取消订单 %d 失败: %v", oe.exchange.GetName(), orderID, cancelErr)
				errs = append(errs, fmt.Errorf("取消订单 %d 失败: %w", orderID, cancelErr))
			}
		}
		return errors.Join(errs...)
	}

	return nil
}

// CheckOrderStatus 检查订单状态
func (oe *ExchangeOrderExecutor) CheckOrderStatus(orderID int64) (string, float64, error) {
	requestCtx, cancel := oe.requestContext(oe.rootCtx)
	order, err := oe.exchange.GetOrder(requestCtx, oe.symbol, orderID)
	cancel()
	if err != nil {
		return "", 0, err
	}

	return string(order.Status), order.ExecutedQty, nil
}

// GetOpenOrders 获取未完成订单
func (oe *ExchangeOrderExecutor) GetOpenOrders() ([]interface{}, error) {
	requestCtx, cancel := oe.requestContext(oe.rootCtx)
	orders, err := oe.exchange.GetOpenOrders(requestCtx, oe.symbol)
	cancel()
	if err != nil {
		return nil, err
	}

	// 转换为 interface{} 列表（为了兼容现有代码）
	result := make([]interface{}, len(orders))
	for i, order := range orders {
		result[i] = order
	}

	return result, nil
}
