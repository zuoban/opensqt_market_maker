package order

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	// ErrReduceOnlySellMarginRejected 表示本应降低风险的 SELL 平仓单也因
	// 保证金不足被交易所明确拒绝，必须停止本批并交由门禁恢复处理。
	ErrReduceOnlySellMarginRejected = errors.New("ReduceOnly SELL 因保证金不足被拒绝")
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
	NearTouch     bool   // 距离盘口较近，避免与深度订单共享陈旧批次

	// AcquireSubmissionLease 必须在每次真正的交易所请求前调用，并在该次请求
	// 返回后释放。实现会在 lease 期间持有槽位锁，因此不得从 PlaceOrder 同步
	// 重入同一槽位的订单更新回调；真实订单流应通过异步边界投递。
	AcquireSubmissionLease func() (release func(), ok bool)
	// OnSubmissionUnknown 在请求已进入交易所边界、但结果无法确认时调用。
	OnSubmissionUnknown func()
	// OnDefiniteRejection 将逐笔明确拒绝原因回传到槽位层。回调只记录状态，
	// 不得同步重入订单提交。
	OnDefiniteRejection func(OrderRejectionKind)
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
	makerGuardMu     sync.RWMutex
	marketSnapshot   func() exchange.MarketSnapshot
	priceTickSize    float64
	makerGuardTicks  int
	quoteStaleAfter  time.Duration

	// 时间配置
	rateLimitRetryDelay time.Duration
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

// SetMakerGuard 配置真实提交边界的盘口复检。provider 必须返回全局唯一
// PriceMonitor 的原子快照；nil 表示测试或迁移路径暂不启用盘口复检。
func (oe *ExchangeOrderExecutor) SetMakerGuard(
	provider func() exchange.MarketSnapshot,
	priceTickSize float64,
	guardTicks int,
	quoteStaleAfter time.Duration,
) {
	oe.makerGuardMu.Lock()
	oe.marketSnapshot = provider
	oe.priceTickSize = priceTickSize
	oe.makerGuardTicks = guardTicks
	oe.quoteStaleAfter = quoteStaleAfter
	oe.makerGuardMu.Unlock()
}

func noteDefiniteRejection(req *OrderRequest, kind OrderRejectionKind) {
	if req != nil && req.OnDefiniteRejection != nil {
		req.OnDefiniteRejection(kind)
	}
}

func (oe *ExchangeOrderExecutor) makerGuardSettings() (
	func() exchange.MarketSnapshot,
	float64,
	int,
	time.Duration,
) {
	oe.makerGuardMu.RLock()
	defer oe.makerGuardMu.RUnlock()
	return oe.marketSnapshot, oe.priceTickSize, oe.makerGuardTicks, oe.quoteStaleAfter
}

func checkMakerGuardSnapshot(
	req *OrderRequest,
	snapshot exchange.MarketSnapshot,
	tickSize float64,
	guardTicks int,
	staleAfter time.Duration,
) error {
	if req == nil {
		return NewOrderRejectedError(OrderRejectionMakerMoved, fmt.Errorf("订单请求为空"))
	}
	if req.Price <= 0 || math.IsNaN(req.Price) || math.IsInf(req.Price, 0) {
		return NewOrderRejectedError(OrderRejectionMakerMoved, fmt.Errorf("订单价格无效: %g", req.Price))
	}
	if math.IsNaN(snapshot.BestBid) || math.IsInf(snapshot.BestBid, 0) ||
		math.IsNaN(snapshot.BestAsk) || math.IsInf(snapshot.BestAsk, 0) ||
		!snapshot.Ready || snapshot.BestBid <= 0 || snapshot.BestAsk <= snapshot.BestBid {
		return NewOrderRejectedError(OrderRejectionMarketDataStale,
			fmt.Errorf("完整盘口尚未就绪"))
	}
	if snapshot.Symbol != "" && req.Symbol != "" &&
		!strings.EqualFold(snapshot.Symbol, req.Symbol) {
		return NewOrderRejectedError(OrderRejectionMarketDataStale,
			fmt.Errorf("盘口交易对不匹配: got %s, want %s", snapshot.Symbol, req.Symbol))
	}
	if staleAfter > 0 && (snapshot.QuoteReceivedAt.IsZero() || time.Since(snapshot.QuoteReceivedAt) > staleAfter) {
		return NewOrderRejectedError(OrderRejectionMarketDataStale,
			fmt.Errorf("最优盘口已过期: age=%s", time.Since(snapshot.QuoteReceivedAt).Round(time.Millisecond)))
	}
	if tickSize <= 0 || math.IsNaN(tickSize) || math.IsInf(tickSize, 0) {
		return NewOrderRejectedError(OrderRejectionMarketDataStale,
			fmt.Errorf("交易所价格步长无效: %g", tickSize))
	}
	if guardTicks < 1 {
		guardTicks = 1
	}
	guard := float64(guardTicks) * tickSize
	switch strings.ToUpper(req.Side) {
	case "BUY":
		capPrice := snapshot.BestAsk - guard
		if req.Price > capPrice+tickSize*1e-6 {
			return NewOrderRejectedError(OrderRejectionMakerMoved,
				fmt.Errorf("BUY %.12g 超过 Maker 上限 %.12g (ask=%.12g)", req.Price, capPrice, snapshot.BestAsk))
		}
	case "SELL":
		floorPrice := snapshot.BestBid + guard
		if req.Price < floorPrice-tickSize*1e-6 {
			return NewOrderRejectedError(OrderRejectionMakerMoved,
				fmt.Errorf("SELL %.12g 低于 Maker 下限 %.12g (bid=%.12g)", req.Price, floorPrice, snapshot.BestBid))
		}
	default:
		return NewOrderRejectedError(OrderRejectionMakerMoved,
			fmt.Errorf("未知订单方向 %q", req.Side))
	}
	return nil
}

func (oe *ExchangeOrderExecutor) checkMakerGuard(req *OrderRequest) error {
	provider, tickSize, guardTicks, staleAfter := oe.makerGuardSettings()
	if provider == nil {
		return nil
	}
	return checkMakerGuardSnapshot(req, provider(), tickSize, guardTicks, staleAfter)
}

func (oe *ExchangeOrderExecutor) checkMakerGuardBatch(reqs []leasedMakerRequest) []error {
	provider, tickSize, guardTicks, staleAfter := oe.makerGuardSettings()
	if provider == nil {
		return make([]error, len(reqs))
	}
	snapshot := provider()
	errs := make([]error, len(reqs))
	for i, item := range reqs {
		errs[i] = checkMakerGuardSnapshot(item.req, snapshot, tickSize, guardTicks, staleAfter)
	}
	return errs
}

type leasedMakerRequest struct {
	req         *OrderRequest
	exchangeReq *exchange.OrderRequest
	release     func()
}

// NewExchangeOrderExecutor 创建基于交易所接口的订单执行器
func NewExchangeOrderExecutor(ex exchange.IExchange, symbol string, rateLimitRetryDelay, _ int) *ExchangeOrderExecutor {
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
	// Binance 通常返回 code=-5022；保留文本判断以兼容 SDK 包装后的错误。
	return strings.Contains(errStr, "Post Only") ||
		strings.Contains(errStr, "would immediately match")
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

func nativeExchangeRequest(req *OrderRequest) *exchange.OrderRequest {
	return &exchange.OrderRequest{
		Symbol:        req.Symbol,
		Side:          exchange.Side(req.Side),
		Type:          exchange.OrderTypeLimit,
		TimeInForce:   exchange.TimeInForceGTC,
		Quantity:      req.Quantity,
		Price:         req.Price,
		PriceDecimals: req.PriceDecimals,
		ReduceOnly:    req.ReduceOnly,
		PostOnly:      true,
		ClientOrderID: req.ClientOrderID,
	}
}

func copyExchangeOrder(req *OrderRequest, exchangeOrder *exchange.Order) *Order {
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
	if order.Symbol == "" && req != nil {
		order.Symbol = req.Symbol
	}
	if order.Side == "" && req != nil {
		order.Side = req.Side
	}
	if order.CreatedAt.IsZero() {
		order.CreatedAt = time.Now()
	}
	return order
}

func (oe *ExchangeOrderExecutor) interpretSubmittedResult(req *OrderRequest, exchangeOrder *exchange.Order, err error) (*Order, error) {
	if err == nil {
		if exchangeOrder == nil {
			return nil, unknownSubmissionError(req, "下单返回空订单", fmt.Errorf("empty order"))
		}
		return copyExchangeOrder(req, exchangeOrder), nil
	}
	// 先尊重适配器给出的提交语义：UNKNOWN 始终最高优先；明确 REJECTED
	// 即使保留了 context 取消/超时作为底层原因，也表示请求尚未发送。
	if errors.Is(err, exchange.ErrOrderPlacementUnknown) {
		return nil, unknownSubmissionError(req, "下单结果无法确认，禁止自动重试", err)
	}
	if !exchange.IsOrderPlacementRejected(err) &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return nil, unknownSubmissionError(req, "下单请求已取消或超时",
			errors.Join(err, oe.placementContextError("交易门禁状态", err)))
	}
	rejectionKind, definitelyRejected := classifyDefiniteOrderRejection(err)
	if rejectionKind == OrderRejectionRateLimit {
		return nil, NewOrderRejectedError(rejectionKind, err)
	}
	if definitelyRejected {
		rejectedErr := NewOrderRejectedError(rejectionKind, err)
		if rejectionKind == OrderRejectionPostOnly {
			noteDefiniteRejection(req, rejectionKind)
			logger.Warn("⚠️ [%s] PostOnly被拒: %s %.2f，严格Maker模式不降级且不做同价重试",
				oe.exchange.GetName(), req.Side, req.Price)
		}
		if rejectionKind == OrderRejectionPositionMode {
			return nil, fmt.Errorf("持仓模式不匹配: %w", rejectedErr)
		}
		return nil, rejectedErr
	}
	return nil, unknownSubmissionError(req, "下单返回未分类错误，禁止自动重试", err)
}

// PlaceOrder 下单（严格 PostOnly，仅对明确限流做有界重试）
func (oe *ExchangeOrderExecutor) PlaceOrder(req *OrderRequest) (*Order, error) {
	if req == nil {
		return nil, fmt.Errorf("订单请求不能为空")
	}
	placeCtx, err := oe.newOrderContext()
	if err != nil {
		return nil, err
	}

	const maxRateLimitRetries = 5 // 首次限流失败后最多再重试5次
	var lastErr error

	for i := 0; i <= maxRateLimitRetries; i++ {
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
		if makerErr := oe.checkMakerGuard(req); makerErr != nil {
			release()
			var rejected *OrderRejectedError
			if errors.As(makerErr, &rejected) {
				noteDefiniteRejection(req, rejected.Kind)
			}
			return nil, makerErr
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
		order, submittedErr := oe.interpretSubmittedResult(req, exchangeOrder, err)
		if submittedErr == nil {
			logger.Debug("✅ [%s] 下单成功(PostOnly): %s %.*f 数量: %.4f 订单ID: %d",
				oe.exchange.GetName(), order.Side, req.PriceDecimals, order.Price, order.Quantity, order.OrderID)
			return order, nil
		}

		lastErr = submittedErr
		var rejected *OrderRejectedError
		if errors.As(submittedErr, &rejected) && rejected.Kind == OrderRejectionRateLimit {
			logger.Warn("⚠️ 触发速率限制，等待后重试...")
			if i < maxRateLimitRetries {
				if waitErr := waitWithContext(placeCtx, oe.rateLimitRetryDelay); waitErr != nil {
					return nil, oe.placementContextError("速率限制退避被取消", waitErr)
				}
			}
			continue
		}
		return nil, submittedErr
	}

	return nil, fmt.Errorf("下单失败（限流重试%d次）: %w", maxRateLimitRetries, lastErr)
}

type batchPlaceState struct {
	placed            []*Order
	hasMarginError    bool
	skipRemainingBuys bool
	errs              []error
}

func (s *batchPlaceState) shouldSkipBuy(exchangeName string, req *OrderRequest) bool {
	if s.skipRemainingBuys && strings.EqualFold(req.Side, "BUY") {
		logger.Warn("⏭️ [%s] 本批已出现保证金不足，跳过后续买单 %.2f", exchangeName, req.Price)
		return true
	}
	return false
}

func (s *batchPlaceState) note(exchangeName string, req *OrderRequest, order *Order, err error) bool {
	if err == nil {
		if order != nil {
			s.placed = append(s.placed, order)
		}
		return false
	}
	if errors.Is(err, ErrOrderSubmissionStale) {
		logger.Debug("⏭️ [%s] 跳过已失效 reservation: %.2f %s (ClientOID=%s)",
			exchangeName, req.Price, req.Side, req.ClientOrderID)
		return false
	}
	logger.Warn("⚠️ [%s] 下单失败 %.2f %s: %v", exchangeName, req.Price, req.Side, err)

	mustStopBatch := errors.Is(err, exchange.ErrOrderPlacementUnknown) ||
		errors.Is(err, ErrTradingHealthGuardRejected) ||
		errors.Is(err, ErrNewOrdersStopped) || errors.Is(err, ErrOrderExecutorStopped) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	marginFailure := isMarginError(err)
	var marginRejection *OrderRejectedError
	definiteMarginFailure := IsDefiniteOrderRejection(err) &&
		errors.As(err, &marginRejection) && marginRejection.Kind == OrderRejectionMargin
	if definiteMarginFailure && req.ReduceOnly && strings.EqualFold(req.Side, "SELL") {
		err = errors.Join(ErrReduceOnlySellMarginRejected, err)
		mustStopBatch = true
	}
	if marginFailure {
		s.hasMarginError = true
		logger.Error("❌ [保证金不足] 订单 %.2f %s 因保证金不足失败", req.Price, req.Side)
		if !mustStopBatch {
			s.skipRemainingBuys = true
		}
	}
	if !marginFailure || mustStopBatch {
		s.errs = append(s.errs, fmt.Errorf("订单 %.12g %s 提交失败: %w", req.Price, req.Side, err))
	}
	if mustStopBatch {
		return true
	}
	var rejected *OrderRejectedError
	if IsDefiniteOrderRejection(err) && errors.As(err, &rejected) &&
		rejected.Kind == OrderRejectionRateLimit {
		logger.Warn("⚠️ [%s] 限流重试已耗尽，停止提交本批剩余订单", exchangeName)
		return true
	}
	return false
}

func (oe *ExchangeOrderExecutor) nativeBatcher() (exchange.PlaceOrderBatcher, bool) {
	batcher, ok := oe.exchange.(exchange.PlaceOrderBatcher)
	return batcher, ok && batcher.PlaceOrderBatchSize() > 0
}

// BatchPlaceOrders 批量下单
// 返回：成功下单的订单列表、是否出现保证金不足错误，以及其它下单错误。
// 结果未知等错误必须向上传递，调用方才能关闭门禁并先对账，不能释放槽位后换新 ID 重下。
func (oe *ExchangeOrderExecutor) BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool, error) {
	state := &batchPlaceState{placed: make([]*Order, 0, len(orders))}
	exchangeName := oe.exchange.GetName()
	validOrders := make([]*OrderRequest, 0, len(orders))
	for i, req := range orders {
		if req == nil {
			state.errs = append(state.errs, fmt.Errorf("第 %d 个订单请求为空", i+1))
			continue
		}
		validOrders = append(validOrders, req)
	}
	orders = validOrders
	if batcher, ok := oe.nativeBatcher(); ok {
		size := batcher.PlaceOrderBatchSize()
		for i := 0; i < len(orders); {
			chunkLimit := size
			if orders[i] != nil && orders[i].NearTouch {
				chunkLimit = 1
			}
			chunk := make([]*OrderRequest, 0, chunkLimit)
			for i < len(orders) && len(chunk) < chunkLimit {
				// 深度批次遇到近盘口请求时先提交已有项目，让近盘口请求在
				// 下一轮独立获取最新 quote guard，缩短规划到撮合的窗口。
				if len(chunk) > 0 && orders[i] != nil && orders[i].NearTouch {
					break
				}
				req := orders[i]
				i++
				if state.shouldSkipBuy(exchangeName, req) {
					continue
				}
				chunk = append(chunk, req)
			}
			if len(chunk) == 0 {
				continue
			}
			if oe.placeNativeChunk(state, chunk, batcher) {
				break
			}
		}
		return state.placed, state.hasMarginError, errors.Join(state.errs...)
	}

	for _, orderReq := range orders {
		if state.shouldSkipBuy(exchangeName, orderReq) {
			continue
		}
		order, err := oe.PlaceOrder(orderReq)
		if state.note(exchangeName, orderReq, order, err) {
			break
		}
	}
	return state.placed, state.hasMarginError, errors.Join(state.errs...)
}

func (oe *ExchangeOrderExecutor) placeNativeChunk(state *batchPlaceState, chunk []*OrderRequest, batcher exchange.PlaceOrderBatcher) bool {
	exchangeName := oe.exchange.GetName()
	placeCtx, err := oe.newOrderContext()
	if err != nil {
		stop := false
		for _, req := range chunk {
			if state.note(exchangeName, req, nil, err) {
				stop = true
			}
		}
		return stop
	}
	for range chunk {
		if waitErr := oe.rateLimiter.Wait(placeCtx); waitErr != nil {
			waitErr = oe.placementContextError("速率限制等待失败", waitErr)
			stop := false
			for _, req := range chunk {
				if state.note(exchangeName, req, nil, waitErr) {
					stop = true
				}
			}
			return stop
		}
	}

	submitted := make([]leasedMakerRequest, 0, len(chunk))
	releaseSubmitted := func() {
		for _, item := range submitted {
			item.release()
		}
	}

	for _, req := range chunk {
		if !oe.newOrdersEnabled.Load() {
			gateErr := placeCtx.Err()
			if gateErr == nil {
				gateErr = ErrNewOrdersStopped
			}
			releaseSubmitted()
			return state.note(exchangeName, req, nil, oe.placementContextError("下单前交易门禁已关闭", gateErr))
		}
		release, ok := acquireSubmissionLease(req)
		if !ok {
			if state.note(exchangeName, req, nil, ErrOrderSubmissionStale) {
				releaseSubmitted()
				return true
			}
			continue
		}
		if !oe.newOrdersEnabled.Load() || placeCtx.Err() != nil {
			release()
			releaseSubmitted()
			gateErr := placeCtx.Err()
			if gateErr == nil {
				gateErr = ErrNewOrdersStopped
			}
			return state.note(exchangeName, req, nil, oe.placementContextError("下单前交易门禁已关闭", gateErr))
		}
		submitted = append(submitted, leasedMakerRequest{
			req:         req,
			exchangeReq: nativeExchangeRequest(req),
			release:     release,
		})
	}
	if len(submitted) == 0 {
		return false
	}

	if guardErr := oe.checkSubmissionHealth(); guardErr != nil {
		releaseSubmitted()
		oe.StopNewOrders()
		healthErr := fmt.Errorf("%w: %v", ErrTradingHealthGuardRejected, guardErr)
		stop := false
		for _, item := range submitted {
			if state.note(exchangeName, item.req, nil, healthErr) {
				stop = true
			}
		}
		return stop
	}
	if !oe.newOrdersEnabled.Load() || placeCtx.Err() != nil {
		releaseSubmitted()
		gateErr := placeCtx.Err()
		if gateErr == nil {
			gateErr = ErrNewOrdersStopped
		}
		stop := false
		for _, item := range submitted {
			if state.note(exchangeName, item.req, nil, oe.placementContextError("健康复检后交易门禁已关闭", gateErr)) {
				stop = true
			}
		}
		return stop
	}

	// 批量请求只提交在同一最新盘口下仍满足 Maker 边界的项目。被本地
	// guard 拦截的 reservation 明确尚未进入交易所，可以逐项安全释放。
	guarded := submitted[:0]
	stopAfterGuard := false
	guardErrors := oe.checkMakerGuardBatch(submitted)
	for i, item := range submitted {
		makerErr := guardErrors[i]
		if makerErr == nil {
			guarded = append(guarded, item)
			continue
		}
		item.release()
		var rejected *OrderRejectedError
		if errors.As(makerErr, &rejected) {
			noteDefiniteRejection(item.req, rejected.Kind)
			if rejected.Kind == OrderRejectionMarketDataStale {
				stopAfterGuard = true
			}
		}
		if state.note(exchangeName, item.req, nil, makerErr) {
			stopAfterGuard = true
		}
	}
	submitted = guarded
	if len(submitted) == 0 {
		return stopAfterGuard
	}

	exchangeReqs := make([]*exchange.OrderRequest, len(submitted))
	for i, item := range submitted {
		exchangeReqs[i] = item.exchangeReq
	}
	requestCtx, cancel := oe.requestContext(placeCtx)
	items, batchErr := func() ([]exchange.PlaceOrderBatchItem, error) {
		defer releaseSubmitted()
		return batcher.PlaceOrderBatch(requestCtx, exchangeReqs)
	}()
	cancel()

	stop := false
	if batchErr != nil {
		for _, item := range submitted {
			order, err := oe.interpretSubmittedResult(item.req, nil, batchErr)
			if order != nil && err == nil {
				logger.Debug("✅ [%s] 下单成功(PostOnly): %s %.*f 数量: %.4f 订单ID: %d",
					exchangeName, order.Side, item.req.PriceDecimals, order.Price, order.Quantity, order.OrderID)
			}
			if state.note(exchangeName, item.req, order, err) {
				stop = true
			}
		}
		return true
	}
	if len(items) != len(submitted) {
		mismatch := fmt.Errorf("批量下单结果条数=%d，期望 %d", len(items), len(submitted))
		for _, item := range submitted {
			order, err := oe.interpretSubmittedResult(item.req, nil, mismatch)
			if state.note(exchangeName, item.req, order, err) {
				stop = true
			}
		}
		return true
	}
	for i, item := range submitted {
		order, err := oe.interpretSubmittedResult(item.req, items[i].Order, items[i].Err)
		if order != nil && err == nil {
			logger.Debug("✅ [%s] 下单成功(PostOnly): %s %.*f 数量: %.4f 订单ID: %d",
				exchangeName, order.Side, item.req.PriceDecimals, order.Price, order.Quantity, order.OrderID)
		}
		if state.note(exchangeName, item.req, order, err) {
			stop = true
		}
	}
	return stop || stopAfterGuard
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
			logger.Debug("ℹ️ [%s] 订单 %d 已不存在（可能已成交或已取消），跳过取消", oe.exchange.GetName(), orderID)
			return nil
		}
		return fmt.Errorf("取消订单失败: %w", err)
	}

	logger.Debug("✅ [%s] 取消订单成功: %d", oe.exchange.GetName(), orderID)
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
