package main

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
	"opensqt/monitor"
	"opensqt/safety"
)

const (
	minimumPriceStaleAfter = 30 * time.Second
	gateHealthPollInterval = 200 * time.Millisecond
	recoveryReconcileDelay = 5 * time.Second
	cancelConfirmInterval  = 250 * time.Millisecond
	startupCancelTimeout   = 10 * time.Second
)

var errTradingGateShuttingDown = errors.New("交易门禁正在关闭")

// tradingGateHealth 是交易放行所需条件的不可变快照。
type tradingGateHealth struct {
	OrderStreamReady bool
	OrderStreamState string
	RiskReady        bool
	RiskTriggered    bool
	ReconcilerReady  bool
	PriceFresh       bool
}

func (h tradingGateHealth) CanTrade() bool {
	return h.OrderStreamReady && h.RiskReady && !h.RiskTriggered && h.ReconcilerReady && h.PriceFresh
}

func (h tradingGateHealth) Reasons() []string {
	reasons := make([]string, 0, 5)
	if !h.OrderStreamReady {
		state := h.OrderStreamState
		if state == "" {
			state = "UNKNOWN"
		}
		reasons = append(reasons, "订单流="+state)
	}
	if !h.RiskReady {
		reasons = append(reasons, "风控未就绪")
	}
	if h.RiskTriggered {
		reasons = append(reasons, "风控已触发")
	}
	if !h.ReconcilerReady {
		reasons = append(reasons, "对账不健康")
	}
	if !h.PriceFresh {
		reasons = append(reasons, "价格流陈旧")
	}
	return reasons
}

func priceIsFresh(last time.Time, now time.Time, staleAfter time.Duration) bool {
	if last.IsZero() || now.IsZero() {
		return false
	}
	if staleAfter <= 0 {
		staleAfter = minimumPriceStaleAfter
	}
	age := now.Sub(last)
	return age >= 0 && age <= staleAfter
}

func configuredPriceStaleAfter(priceSendIntervalMS int) time.Duration {
	staleAfter := 20 * time.Duration(priceSendIntervalMS) * time.Millisecond
	if staleAfter < minimumPriceStaleAfter {
		return minimumPriceStaleAfter
	}
	return staleAfter
}

// currentOrderStreamHealth 对实现健康接口的交易所读取真实状态。
// 订单流无法观测时必须 fail-closed，禁止以交易所名称猜测就绪。
type namedExchange interface {
	GetName() string
}

func currentOrderStreamHealth(ex namedExchange) (bool, string) {
	if provider, ok := ex.(exchange.OrderStreamHealthProvider); ok {
		ready := provider.IsOrderStreamReady()
		state := strings.ToUpper(strings.TrimSpace(provider.GetOrderStreamState()))
		if state == "" {
			state = "UNKNOWN"
		}
		// Ready 与 State 来自两次独立读取，状态可能在两者之间
		// 变化。只有两个证据同时明确指向 READY 才能放行。
		return ready && state == "READY", state
	}
	return false, "HEALTH_UNAVAILABLE"
}

type cancelAllOrdersExchange interface {
	CancelAllOrders(ctx context.Context, symbol string) error
	GetOpenOrders(ctx context.Context, symbol string) ([]*exchange.Order, error)
}

type newOrderGate interface {
	StopNewOrders()
	EnableNewOrders() error
	SetSubmissionHealthGuard(func() error)
}

type tradingPositionManager interface {
	CancelAllBuyOrders() error
	AdjustOrders(currentPrice float64) error
	GetSymbol() string
}

// serializedOrderGate 串行化 Enable/Stop 与停机，防止健康监控和价格协程交错后重新放单。
type serializedOrderGate struct {
	mu           sync.Mutex
	executor     newOrderGate
	enabled      bool
	shuttingDown bool
	generation   uint64
}

func newSerializedOrderGate(executor newOrderGate) *serializedOrderGate {
	return &serializedOrderGate{executor: executor}
}

func (g *serializedOrderGate) Enable() (uint64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shuttingDown {
		return g.generation, errTradingGateShuttingDown
	}
	if g.enabled {
		return g.generation, nil
	}
	if err := g.executor.EnableNewOrders(); err != nil {
		return g.generation, err
	}
	g.generation++
	g.enabled = true
	return g.generation, nil
}

// Disable 返回本次调用是否真的把门禁从开启切到关闭。
func (g *serializedOrderGate) Disable() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.enabled {
		return false
	}
	g.executor.StopNewOrders()
	g.generation++
	g.enabled = false
	return true
}

func (g *serializedOrderGate) Enabled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enabled && !g.shuttingDown
}

func (g *serializedOrderGate) StillEnabled(generation uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enabled && !g.shuttingDown && g.generation == generation
}

func (g *serializedOrderGate) BeginShutdown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.shuttingDown {
		return
	}
	g.shuttingDown = true
	g.generation++
	g.enabled = false
	// 即使内部状态已经是关闭，也再次落到执行器边界，确保停机 fail-closed。
	g.executor.StopNewOrders()
}

type tradingGateRuntime struct {
	gate       *serializedOrderGate
	exchange   exchange.IExchange
	price      *monitor.PriceMonitor
	risk       *safety.RiskMonitor
	reconciler *safety.Reconciler
	position   tradingPositionManager

	priceStaleAfter  time.Duration
	reconcileEvery   time.Duration
	wake             chan struct{}
	needsReconcile   atomic.Bool
	withdrawRequired atomic.Bool

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}

	// 以下字段仅由协调协程访问。
	nextRecoveryReconcile time.Time
	lastHealth            tradingGateHealth
	hasLastHealth         bool
}

func newTradingGateRuntime(
	gate *serializedOrderGate,
	ex exchange.IExchange,
	price *monitor.PriceMonitor,
	risk *safety.RiskMonitor,
	reconciler *safety.Reconciler,
	positionManager tradingPositionManager,
	priceStaleAfter time.Duration,
	reconcileEvery time.Duration,
) *tradingGateRuntime {
	if priceStaleAfter <= 0 {
		priceStaleAfter = minimumPriceStaleAfter
	}
	if reconcileEvery <= 0 {
		reconcileEvery = 30 * time.Second
	}
	runtime := &tradingGateRuntime{
		gate:            gate,
		exchange:        ex,
		price:           price,
		risk:            risk,
		reconciler:      reconciler,
		position:        positionManager,
		priceStaleAfter: priceStaleAfter,
		reconcileEvery:  reconcileEvery,
		wake:            make(chan struct{}, 1),
	}
	if gate != nil && gate.executor != nil {
		gate.executor.SetSubmissionHealthGuard(runtime.submissionHealthGuard)
	}
	reconciler.SetHealthHandler(runtime.handleReconcileHealth)
	return runtime
}

func (r *tradingGateRuntime) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// disableForRecovery 只在门禁真的从开启切到关闭时标记撤买单和恢复对账。
// invalidate=false 仅用于 Reconciler 自身已经报告不健康的回调，避免递归通知。
func (r *tradingGateRuntime) disableForRecovery(reason error, invalidate bool) bool {
	if !r.gate.Disable() {
		return false
	}
	r.withdrawRequired.Store(true)
	r.needsReconcile.Store(true)
	if invalidate {
		if reason == nil {
			reason = fmt.Errorf("交易门禁健康恶化")
		}
		r.reconciler.Invalidate(reason)
	}
	r.signal()
	return true
}

// handleReconcileHealth 必须保持非阻塞；不健康时先在执行边界立即停新单。
func (r *tradingGateRuntime) handleReconcileHealth(healthy bool, err error) {
	if !healthy {
		r.disableForRecovery(err, false)
	}
	r.signal()
}

func (r *tradingGateRuntime) snapshot() tradingGateHealth {
	orderReady, orderState := currentOrderStreamHealth(r.exchange)
	return tradingGateHealth{
		OrderStreamReady: orderReady,
		OrderStreamState: orderState,
		RiskReady:        r.risk.IsReady(),
		RiskTriggered:    r.risk.IsTriggered(),
		ReconcilerReady:  r.reconciler.IsHealthy(),
		PriceFresh:       r.price.GetLastPrice() > 0 && priceIsFresh(r.price.GetLastPriceTime(), time.Now(), r.priceStaleAfter),
	}
}

func submissionHealthError(health tradingGateHealth, needsReconcile bool) error {
	reasons := health.Reasons()
	if needsReconcile {
		reasons = append(reasons, "等待恢复对账")
	}
	if len(reasons) == 0 {
		return nil
	}
	return fmt.Errorf("交易健康条件不满足: %s", strings.Join(reasons, ", "))
}

// submissionHealthGuard 由订单执行器在每次真实 PlaceOrder 前同步调用。
// 它将周期健康观察与实际提交之间的窗口收窄到最终交易所边界。
func (r *tradingGateRuntime) submissionHealthGuard() error {
	if r == nil {
		return fmt.Errorf("交易门禁运行时未初始化")
	}
	return r.enforceSubmissionHealth(r.snapshot(), r.needsReconcile.Load())
}

// enforceSubmissionHealth 不只拒绝当前请求，还在 guard 返回执行器之前
// 同步将整个门禁切入恢复路径。这避免健康状态瞬时恢复时出现
// serialized gate 仍显示 enabled，但 executor 已自停且未强制对账的分裂状态。
func (r *tradingGateRuntime) enforceSubmissionHealth(health tradingGateHealth, needsReconcile bool) error {
	guardErr := submissionHealthError(health, needsReconcile)
	if guardErr == nil {
		return nil
	}
	recoveryErr := fmt.Errorf("下单边界健康复检失败: %w", guardErr)
	r.disableForRecovery(recoveryErr, health.ReconcilerReady)
	return recoveryErr
}

// observeHealth 独立于可能阻塞的 Adjust/Reconcile，健康恶化时仍能立即取消在途下单。
func (r *tradingGateRuntime) observeHealth(ctx context.Context) {
	observe := func() {
		health := r.snapshot()
		if !health.OrderStreamReady && r.needsReconcile.CompareAndSwap(false, true) {
			r.reconciler.Invalidate(fmt.Errorf("订单流不健康: %s", health.OrderStreamState))
		}
		if !health.CanTrade() {
			r.disableForRecovery(
				fmt.Errorf("交易健康恶化: %s", strings.Join(health.Reasons(), ", ")),
				health.ReconcilerReady,
			)
		}
		r.signal()
	}

	observe()
	ticker := time.NewTicker(gateHealthPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			observe()
		}
	}
}

func (r *tradingGateRuntime) Start(parent context.Context) error {
	r.lifecycleMu.Lock()
	if r.done != nil {
		r.lifecycleMu.Unlock()
		return fmt.Errorf("交易门禁已启动")
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	initialResult := make(chan error, 1)
	r.cancel = cancel
	r.done = done
	r.lifecycleMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		r.observeHealth(ctx)
	}()
	go func() {
		defer wg.Done()
		r.run(ctx, initialResult)
	}()
	go func() {
		wg.Wait()
		close(done)
	}()

	if err := <-initialResult; err != nil {
		cancel()
		<-done
		return err
	}
	return nil
}

func (r *tradingGateRuntime) Stop() {
	r.gate.BeginShutdown()
	r.lifecycleMu.Lock()
	cancel := r.cancel
	done := r.done
	r.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (r *tradingGateRuntime) run(ctx context.Context, initialResult chan<- error) {
	priceChanges := r.price.Subscribe()
	reconcileTicker := time.NewTicker(r.reconcileEvery)
	defer reconcileTicker.Stop()

	if err := r.evaluate(ctx, false); err != nil {
		initialResult <- r.rollbackFailedStart(err)
		return
	}
	if !r.gate.Enabled() {
		initialResult <- r.rollbackFailedStart(
			fmt.Errorf("交易门禁初始条件不满足: %s", strings.Join(r.snapshot().Reasons(), ", ")),
		)
		return
	}
	initialResult <- nil

	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-priceChanges:
			if !ok {
				return
			}
			if err := r.evaluate(ctx, true); err != nil && ctx.Err() == nil {
				logger.Error("❌ 交易门禁价格调整失败: %v", err)
			}
		case <-r.wake:
			if err := r.evaluate(ctx, false); err != nil && ctx.Err() == nil {
				logger.Error("❌ 交易门禁健康评估失败: %v", err)
			}
		case <-reconcileTicker.C:
			r.periodicReconcile(ctx)
			if err := r.evaluate(ctx, false); err != nil && ctx.Err() == nil {
				logger.Error("❌ 定期对账后门禁评估失败: %v", err)
			}
		}
	}
}

func (r *tradingGateRuntime) rollbackFailedStart(startErr error) error {
	// 首次 AdjustOrders 可能已经部分成功；返回启动错误前必须永久关门并确认远端为空。
	r.gate.BeginShutdown()
	r.withdrawRequired.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), startupCancelTimeout)
	defer cancel()
	if err := cancelAllOrdersAndConfirm(ctx, r.exchange, r.position.GetSymbol()); err != nil {
		return errors.Join(startErr, fmt.Errorf("交易门禁启动回滚失败: %w", err))
	}
	return startErr
}

func (r *tradingGateRuntime) periodicReconcile(ctx context.Context) {
	if ctx.Err() != nil || r.needsReconcile.Load() {
		return
	}
	ready, _ := currentOrderStreamHealth(r.exchange)
	if !ready {
		return
	}
	if err := r.reconciler.Reconcile(); err != nil {
		logger.Error("❌ [定期对账失败] %v", err)
	}
}

func (r *tradingGateRuntime) evaluate(ctx context.Context, priceChanged bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	if r.withdrawRequired.Load() {
		logger.Warn("🚨 交易健康恶化，停止新单并撤销全部买单")
		if err := r.position.CancelAllBuyOrders(); err != nil {
			withdrawErr := fmt.Errorf("撤销并确认全部买单失败: %w", err)
			r.needsReconcile.Store(true)
			r.reconciler.Invalidate(withdrawErr)
			return withdrawErr
		}
		// 只有远端撤单与本地槽位都已确认收敛，才允许进入恢复对账。
		r.withdrawRequired.Store(false)
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	health := r.snapshot()
	if r.hasLastHealth && r.lastHealth.CanTrade() && !health.CanTrade() {
		logger.Warn("⛔ 交易门禁关闭: %s", strings.Join(health.Reasons(), ", "))
	}
	r.lastHealth = health
	r.hasLastHealth = true

	if r.needsReconcile.Load() && health.OrderStreamReady && !time.Now().Before(r.nextRecoveryReconcile) {
		logger.Info("🔄 订单流恢复，放行前执行强制对账...")
		err := r.reconciler.Reconcile()
		if err != nil || !r.reconciler.IsHealthy() {
			if err == nil {
				err = fmt.Errorf("恢复对账未达到健康状态")
			}
			r.nextRecoveryReconcile = time.Now().Add(recoveryReconcileDelay)
			return fmt.Errorf("订单流恢复对账失败: %w", err)
		}
		r.needsReconcile.Store(false)
		r.nextRecoveryReconcile = time.Time{}
		logger.Info("✅ 订单流恢复对账通过")
		health = r.snapshot()
	}

	if !health.CanTrade() || r.needsReconcile.Load() {
		r.disableForRecovery(
			fmt.Errorf("交易健康恶化: %s", strings.Join(health.Reasons(), ", ")),
			health.ReconcilerReady,
		)
		return nil
	}

	if !r.gate.Enabled() {
		generation, err := r.gate.Enable()
		if err != nil {
			return fmt.Errorf("开启新下单门禁失败: %w", err)
		}
		if err := r.position.AdjustOrders(r.price.GetLastPrice()); err != nil {
			r.disableForRecovery(fmt.Errorf("首次订单调整失败: %w", err), true)
			return fmt.Errorf("门禁放行后的首次订单调整失败: %w", err)
		}
		if !r.gate.StillEnabled(generation) {
			return nil
		}
		if latest := r.snapshot(); !latest.CanTrade() {
			r.disableForRecovery(
				fmt.Errorf("放行后健康复检失败: %s", strings.Join(latest.Reasons(), ", ")),
				latest.ReconcilerReady,
			)
			return nil
		}
		logger.Info("✅ 订单流、风控、对账和价格均健康，交易门禁已放行")
		return nil
	}

	if priceChanged {
		if err := r.position.AdjustOrders(r.price.GetLastPrice()); err != nil {
			r.disableForRecovery(fmt.Errorf("实时订单调整失败: %w", err), true)
			return fmt.Errorf("实时订单调整失败: %w", err)
		}
	}
	return nil
}

// cancelAllOrdersAndConfirm 在上下文期限内持续全撤并查询，只有远端挂单确认为空才成功。
func cancelAllOrdersAndConfirm(ctx context.Context, ex cancelAllOrdersExchange, symbol string) error {
	if ctx == nil {
		return fmt.Errorf("全撤上下文不能为空")
	}
	if ex == nil {
		return fmt.Errorf("全撤交易所不能为空")
	}
	var lastCancelErr error
	var lastQueryErr error
	remaining := -1

	for {
		if err := ex.CancelAllOrders(ctx, symbol); err != nil {
			lastCancelErr = err
		}
		openOrders, err := ex.GetOpenOrders(ctx, symbol)
		if err != nil {
			lastQueryErr = err
		} else {
			lastQueryErr = nil
			remaining = len(openOrders)
			if remaining == 0 {
				return nil
			}
		}

		timer := time.NewTimer(cancelConfirmInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(
				fmt.Errorf("全撤确认超时，剩余挂单=%d: %w", remaining, ctx.Err()),
				lastCancelErr,
				lastQueryErr,
			)
		case <-timer.C:
		}
	}
}
