package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opensqt/exchange"
	"opensqt/logger"
	"opensqt/monitor"
	"opensqt/order"
	"opensqt/safety"
)

const (
	minimumPriceStaleAfter  = 30 * time.Second
	gateHealthPollInterval  = 200 * time.Millisecond
	recoveryReconcileDelay  = 5 * time.Second
	cancelConfirmInterval   = 250 * time.Millisecond
	startupCancelTimeout    = 10 * time.Second
	marginCancelStableReads = 3
	marginAuditInterval     = 2 * time.Second
	marginAuditWindow       = 15 * time.Second
)

var errTradingGateShuttingDown = errors.New("交易门禁正在关闭")

// tradingGateHealth 是交易放行所需条件的不可变快照。
type tradingGateHealth struct {
	OrderStreamReady bool
	OrderStreamState string
	RiskReady        bool
	RiskTriggered    bool
	MarginReady      bool
	MarginTriggered  bool
	ReconcilerReady  bool
	PriceFresh       bool
}

func (h tradingGateHealth) CanTrade() bool {
	return h.OrderStreamReady && h.RiskReady && !h.RiskTriggered && h.MarginReady &&
		!h.MarginTriggered && h.ReconcilerReady && h.PriceFresh
}

func (h tradingGateHealth) Reasons() []string {
	reasons := make([]string, 0, 7)
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
	if !h.MarginReady {
		reasons = append(reasons, "保证金守卫未就绪")
	}
	if h.MarginTriggered {
		reasons = append(reasons, "保证金限制已触发")
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

type adjustmentNotifierPosition interface {
	SetAdjustmentNotifier(func(delay time.Duration))
}

type unchangedGridPosition interface {
	ShouldSkipUnchangedGrid(currentPrice float64) bool
}

func shouldSkipUnchangedGridPriceTick(position tradingPositionManager, price float64) bool {
	skipper, ok := position.(unchangedGridPosition)
	return ok && skipper.ShouldSkipUnchangedGrid(price)
}

type marginGateMonitor interface {
	IsReady() bool
	IsTriggered() bool
	Snapshot() safety.MarginSnapshot
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
	margin     marginGateMonitor
	reconciler *safety.Reconciler
	position   tradingPositionManager

	priceStaleAfter   time.Duration
	reconcileEvery    time.Duration
	wake              chan struct{}
	needsReconcile    atomic.Bool
	withdrawRequired  atomic.Bool
	cancelAllRequired atomic.Bool
	marginCancelDone  atomic.Bool
	adjustStopped     atomic.Bool

	adjustMu        sync.Mutex
	adjustImmediate bool
	adjustDeadlines []time.Time

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}

	// 以下字段仅由协调协程访问。
	nextRecoveryReconcile time.Time
	nextMarginAudit       time.Time
	marginAuditUntil      time.Time
	lastHealth            tradingGateHealth
	hasLastHealth         bool
}

func newTradingGateRuntime(
	gate *serializedOrderGate,
	ex exchange.IExchange,
	price *monitor.PriceMonitor,
	risk *safety.RiskMonitor,
	margin marginGateMonitor,
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
		margin:          margin,
		reconciler:      reconciler,
		position:        positionManager,
		priceStaleAfter: priceStaleAfter,
		reconcileEvery:  reconcileEvery,
		wake:            make(chan struct{}, 1),
	}
	if gate != nil && gate.executor != nil {
		gate.executor.SetSubmissionHealthGuard(runtime.submissionHealthGuard)
	}
	if notifier, ok := positionManager.(adjustmentNotifierPosition); ok {
		notifier.SetAdjustmentNotifier(func(delay time.Duration) {
			runtime.RequestAdjustOrders(delay)
		})
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

// RequestAdjustOrders 请求协调协程再次执行一次 AdjustOrders。
// delay<=0 的请求合并为一个立即标志；未来 deadline 有序保留，避免较早的
// 普通调整误吞仍处于冷却期的重试。实际订单调整仍只由 run 协程串行执行。
func (r *tradingGateRuntime) RequestAdjustOrders(delay time.Duration) bool {
	if r == nil || r.adjustStopped.Load() {
		return false
	}

	immediate := delay <= 0

	r.adjustMu.Lock()
	if r.adjustStopped.Load() {
		r.adjustMu.Unlock()
		return false
	}
	if immediate {
		r.adjustImmediate = true
	} else {
		deadline := time.Now().Add(delay)
		index := sort.Search(len(r.adjustDeadlines), func(i int) bool {
			return !r.adjustDeadlines[i].Before(deadline)
		})
		// 完全相同的 deadline 可以合并；不同冷却期限必须分别保留。
		if index == len(r.adjustDeadlines) || !r.adjustDeadlines[index].Equal(deadline) {
			r.adjustDeadlines = append(r.adjustDeadlines, time.Time{})
			copy(r.adjustDeadlines[index+1:], r.adjustDeadlines[index:])
			r.adjustDeadlines[index] = deadline
		}
	}
	r.adjustMu.Unlock()

	// 即使立即请求被合并，也唤醒协调协程重新观察健康状态；延迟请求则需要
	// 让协调循环按新的最早 deadline 重新武装唯一的定时器。
	r.signal()
	return true
}

// nextAdjustDeadline 返回仍在未来队列中的最早调整时间。已到期项会在事件
// 处理或 evaluate 时提升为 immediate 并从队列移除，因此不会反复得到零时长。
func (r *tradingGateRuntime) nextAdjustDeadline() (time.Time, bool) {
	r.adjustMu.Lock()
	defer r.adjustMu.Unlock()
	if r.adjustStopped.Load() || len(r.adjustDeadlines) == 0 {
		return time.Time{}, false
	}
	return r.adjustDeadlines[0], true
}

func (r *tradingGateRuntime) promoteDueAdjustRequestsLocked(now time.Time) bool {
	due := sort.Search(len(r.adjustDeadlines), func(i int) bool {
		return now.Before(r.adjustDeadlines[i])
	})
	if due == 0 {
		return false
	}
	r.adjustImmediate = true
	if due == len(r.adjustDeadlines) {
		r.adjustDeadlines = nil
	} else {
		copy(r.adjustDeadlines, r.adjustDeadlines[due:])
		r.adjustDeadlines = r.adjustDeadlines[:len(r.adjustDeadlines)-due]
	}
	return true
}

// promoteDueAdjustRequests 将当前所有已到期 deadline 合并为一个 immediate。
// 即使健康门禁暂时关闭，immediate 也会保留；未来 deadline 继续由定时器管理。
func (r *tradingGateRuntime) promoteDueAdjustRequests(now time.Time) bool {
	r.adjustMu.Lock()
	defer r.adjustMu.Unlock()
	if r.adjustStopped.Load() {
		return false
	}
	return r.promoteDueAdjustRequestsLocked(now)
}

// takeReadyAdjustRequest 在真正调用 AdjustOrders 前只取走 immediate 和已到期
// deadline。未来 deadline 始终保留；调用期间新到达的请求也形成下一轮 pending。
func (r *tradingGateRuntime) takeReadyAdjustRequest(now time.Time) bool {
	r.adjustMu.Lock()
	defer r.adjustMu.Unlock()
	if r.adjustStopped.Load() {
		return false
	}
	r.promoteDueAdjustRequestsLocked(now)
	if !r.adjustImmediate {
		return false
	}
	r.adjustImmediate = false
	return true
}

func (r *tradingGateRuntime) stopAdjustRequests() {
	if r == nil {
		return
	}
	r.adjustStopped.Store(true)
	r.adjustMu.Lock()
	r.adjustImmediate = false
	r.adjustDeadlines = nil
	r.adjustMu.Unlock()
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

// handleMarginLimit 将撤单要求单调升级为 ALL。即使门禁已经因其它健康条件
// 关闭，也必须保留这次全撤请求；全撤确认后本次运行仍保持锁存停单。
func (r *tradingGateRuntime) handleMarginLimit(snap safety.MarginSnapshot) {
	if r == nil {
		return
	}
	if r.gate != nil {
		r.gate.Disable()
	}
	r.withdrawRequired.Store(false)
	r.needsReconcile.Store(true)
	if r.marginCancelDone.Load() {
		// done 一旦锁存，required 就不再有合法的 true 状态。
		// 同时自愈旧健康观察可能遗留的过期请求。
		r.cancelAllRequired.Store(false)
	} else if r.cancelAllRequired.CompareAndSwap(false, true) {
		// 全撤可能在上面的 done 读取与 CAS 之间完成。
		// 成功结果一旦锁存，旧健康观察不得重新武装全撤。
		if r.marginCancelDone.Load() {
			r.cancelAllRequired.Store(false)
		} else {
			reason := fmt.Errorf("保证金占用 %.2f%% 超过限制 %.2f%%", snap.UsagePercent, snap.LimitPercent)
			if r.reconciler != nil {
				r.reconciler.Invalidate(reason)
			}
		}
	}
	r.signal()
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
	marginReady := false
	marginTriggered := false
	if r.margin != nil {
		marginReady = r.margin.IsReady()
		marginTriggered = r.margin.IsTriggered()
	}
	market := r.price.GetMarketSnapshot()
	now := time.Now()
	return tradingGateHealth{
		OrderStreamReady: orderReady,
		OrderStreamState: orderState,
		RiskReady:        r.risk.IsReady(),
		RiskTriggered:    r.risk.IsTriggered(),
		MarginReady:      marginReady,
		MarginTriggered:  marginTriggered,
		ReconcilerReady:  r.reconciler.IsHealthy(),
		PriceFresh: market.Ready && market.LastPrice > 0 &&
			priceIsFresh(r.price.GetLastPriceTime(), now, r.priceStaleAfter) &&
			priceIsFresh(market.QuoteReceivedAt, now, r.priceStaleAfter),
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
	if health.MarginTriggered {
		snap := safety.MarginSnapshot{Triggered: true}
		if r.margin != nil {
			snap = r.margin.Snapshot()
		}
		r.handleMarginLimit(snap)
		return recoveryErr
	}
	r.disableForRecovery(recoveryErr, health.ReconcilerReady)
	return recoveryErr
}

// observeHealth 独立于可能阻塞的 Adjust/Reconcile，健康恶化时仍能立即取消在途下单。
func (r *tradingGateRuntime) observeHealth(ctx context.Context) {
	observe := func() {
		health := r.snapshot()
		if health.MarginTriggered {
			snap := safety.MarginSnapshot{Triggered: true}
			if r.margin != nil {
				snap = r.margin.Snapshot()
			}
			r.handleMarginLimit(snap)
		}
		if !health.OrderStreamReady && r.needsReconcile.CompareAndSwap(false, true) {
			r.reconciler.Invalidate(fmt.Errorf("订单流不健康: %s", health.OrderStreamState))
		}
		if !health.CanTrade() && !health.MarginTriggered {
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
	r.stopAdjustRequests()
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
	defer r.stopAdjustRequests()
	priceChanges := r.price.Subscribe()
	reconcileTicker := time.NewTicker(r.reconcileEvery)
	defer reconcileTicker.Stop()
	adjustTimer := time.NewTimer(time.Hour)
	adjustTimer.Stop()
	defer adjustTimer.Stop()

	if err := r.evaluate(ctx, false); err != nil {
		initialResult <- r.rollbackFailedStart(err)
		return
	}
	if !r.gate.Enabled() {
		health := r.snapshot()
		marginDormant := health.MarginTriggered &&
			r.marginCancelDone.Load() &&
			!r.cancelAllRequired.Load()
		if !marginDormant {
			initialResult <- r.rollbackFailedStart(
				fmt.Errorf("交易门禁初始条件不满足: %s", strings.Join(health.Reasons(), ", ")),
			)
			return
		}
		logger.Warn("⛔ 首轮账户读数已触发保证金限制，交易门禁保持关闭，程序以锁存停单态继续运行")
	}
	initialResult <- nil

	for {
		var adjustTimerC <-chan time.Time
		// 项目要求 Go 1.25；channel timer 在 Stop/Reset 返回后保证不会再交付
		// 旧设置的 tick，因此每轮可安全按当前队首 deadline 重新武装。
		adjustTimer.Stop()
		if deadline, ok := r.nextAdjustDeadline(); ok {
			wait := time.Until(deadline)
			if wait < 0 {
				wait = 0
			}
			adjustTimer.Reset(wait)
			adjustTimerC = adjustTimer.C
		}

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
		case <-adjustTimerC:
			if r.promoteDueAdjustRequests(time.Now()) {
				if err := r.evaluate(ctx, false); err != nil && ctx.Err() == nil {
					logger.Error("❌ 交易门禁延迟调整失败: %v", err)
				}
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
	r.stopAdjustRequests()
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

	if r.cancelAllRequired.Load() {
		if err := r.processMarginFullCancel(ctx); err != nil {
			return err
		}
	}
	if err := r.auditLateMarginOrders(ctx, time.Now()); err != nil {
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

	if health.MarginTriggered {
		r.handleMarginLimit(r.margin.Snapshot())
		return nil
	}

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
		// 首次放行本身必定执行 AdjustOrders，只消费 immediate 和已经到期的
		// 请求；仍在冷却期的 deadline 必须保留到期后再触发。
		r.takeReadyAdjustRequest(time.Now())
		if err := r.position.AdjustOrders(r.price.GetLastPrice()); err != nil {
			if order.IsDefiniteOrderRejection(err) {
				logger.Warn("⚠️ 门禁放行后的首次订单调整包含明确未受理订单，保留已确认订单并继续交易: %v", err)
			} else {
				r.disableForRecovery(fmt.Errorf("首次订单调整失败: %w", err), true)
				return fmt.Errorf("门禁放行后的首次订单调整失败: %w", err)
			}
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
		logger.Info("✅ 订单流、风控、保证金、对账和价格均健康，交易门禁已放行")
		return nil
	}

	adjustRequested := r.takeReadyAdjustRequest(time.Now())
	if !adjustRequested && priceChanged && shouldSkipUnchangedGridPriceTick(r.position, r.price.GetLastPrice()) {
		return nil
	}
	if priceChanged || adjustRequested {
		if err := r.position.AdjustOrders(r.price.GetLastPrice()); err != nil {
			if order.IsDefiniteOrderRejection(err) {
				logger.Warn("⚠️ 实时订单调整包含明确未受理订单，保留已确认订单并继续交易: %v", err)
				return nil
			}
			r.disableForRecovery(fmt.Errorf("实时订单调整失败: %w", err), true)
			return fmt.Errorf("实时订单调整失败: %w", err)
		}
	}
	return nil
}

func (r *tradingGateRuntime) processMarginFullCancel(ctx context.Context) error {
	if r == nil || !r.cancelAllRequired.Load() {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("保证金限制全撤上下文不能为空")
	}
	if r.exchange == nil || r.position == nil {
		return fmt.Errorf("保证金限制全撤依赖未初始化")
	}

	logger.Warn("🚨 保证金限制已触发，停止所有新单并撤销当前交易对全部挂单")
	cancelCtx, cancel := context.WithTimeout(ctx, startupCancelTimeout)
	err := cancelAllOrdersAndConfirmStable(cancelCtx, r.exchange, r.position.GetSymbol(), marginCancelStableReads)
	cancel()
	if err != nil {
		cancelErr := fmt.Errorf("保证金限制全撤确认失败: %w", err)
		r.needsReconcile.Store(true)
		if r.reconciler != nil {
			r.reconciler.Invalidate(cancelErr)
		}
		return cancelErr
	}
	r.marginCancelDone.Store(true)
	r.cancelAllRequired.Store(false)
	r.withdrawRequired.Store(false)
	now := time.Now()
	r.nextMarginAudit = now.Add(marginAuditInterval)
	r.marginAuditUntil = now.Add(marginAuditWindow)
	logger.Warn("⛔ 保证金限制已锁存，当前交易对远端挂单已确认为空；本次运行不会自动恢复挂单")
	return ctx.Err()
}

// auditLateMarginOrders 在首次全撤后的有界窗口内低频检查迟到受理订单。
// 空结果只消耗一次查询；只有发现残单才再次执行当前交易对全撤。
func (r *tradingGateRuntime) auditLateMarginOrders(ctx context.Context, now time.Time) error {
	if r == nil || !r.marginCancelDone.Load() || r.exchange == nil || r.position == nil {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("保证金限制迟到订单复查上下文不能为空")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if r.marginAuditUntil.IsZero() || !now.Before(r.marginAuditUntil) || now.Before(r.nextMarginAudit) {
		return nil
	}
	r.nextMarginAudit = now.Add(marginAuditInterval)

	auditCtx, cancel := context.WithTimeout(ctx, startupCancelTimeout)
	defer cancel()
	openOrders, err := r.exchange.GetOpenOrders(auditCtx, r.position.GetSymbol())
	if err != nil {
		return fmt.Errorf("保证金限制迟到订单复查失败: %w", err)
	}
	if len(openOrders) == 0 {
		return nil
	}

	// 先延长窗口，确保本次撤单失败时后续仍会继续复查和重试。
	r.marginAuditUntil = now.Add(marginAuditWindow)
	logger.Warn("🚨 保证金限制锁存后发现 %d 个迟到挂单，再次执行当前交易对全撤", len(openOrders))
	if err := cancelAllOrdersAndConfirm(auditCtx, r.exchange, r.position.GetSymbol()); err != nil {
		return fmt.Errorf("保证金限制迟到订单全撤失败: %w", err)
	}
	return nil
}

// cancelAllOrdersAndConfirm 在上下文期限内持续全撤并查询，只有远端挂单确认为空才成功。
func cancelAllOrdersAndConfirm(ctx context.Context, ex cancelAllOrdersExchange, symbol string) error {
	return cancelAllOrdersAndConfirmStable(ctx, ex, symbol, 1)
}

// cancelAllOrdersAndConfirmStable 要求连续多次观测远端为空。保证金触发时
// 执行器会取消在途下单，但交易所仍可能迟到受理已经发出的请求；连续全撤与
// 空结果确认可以覆盖这段竞态窗口。
func cancelAllOrdersAndConfirmStable(
	ctx context.Context,
	ex cancelAllOrdersExchange,
	symbol string,
	stableReads int,
) error {
	if ctx == nil {
		return fmt.Errorf("全撤上下文不能为空")
	}
	if ex == nil {
		return fmt.Errorf("全撤交易所不能为空")
	}
	if stableReads <= 0 {
		stableReads = 1
	}
	var lastCancelErr error
	var lastQueryErr error
	remaining := -1
	emptyReads := 0

	for {
		if err := ex.CancelAllOrders(ctx, symbol); err != nil {
			lastCancelErr = err
		}
		openOrders, err := ex.GetOpenOrders(ctx, symbol)
		if err != nil {
			lastQueryErr = err
			emptyReads = 0
		} else {
			lastQueryErr = nil
			remaining = len(openOrders)
			if remaining == 0 {
				emptyReads++
				if emptyReads >= stableReads {
					return nil
				}
			} else {
				emptyReads = 0
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
