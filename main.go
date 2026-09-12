package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/logger"
	"opensqt/monitor"
	"opensqt/notification"
	"opensqt/order"
	"opensqt/position"
	"opensqt/safety"
	"opensqt/telemetry"
	"opensqt/web"
)

// Version 版本号
var Version = "v3.5.21"

func main() {
	programStartedAt := time.Now()
	logger.Info("🚀 www.OpenSQT.com 做市商系统启动...")
	logger.Info("📦 版本号: %s", Version)

	// 1. 加载配置：命令行路径 > OPENSQT_CONFIG > config.yaml
	if err := config.LoadDotEnv(); err != nil {
		logger.Fatalf("❌ 加载 .env 失败: %v", err)
	}
	configPath := "config.yaml"
	if v := strings.TrimSpace(os.Getenv("OPENSQT_CONFIG")); v != "" {
		configPath = v
	}
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		logger.Info("未找到配置文件 %s，仅使用环境变量", configPath)
	}

	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		logger.Fatalf("❌ 加载配置失败: %v", err)
	}

	// 初始化日志级别
	logLevel := logger.ParseLogLevel(cfg.System.LogLevel)
	logger.SetLevel(logLevel)
	logger.Info("日志级别设置为: %s", logLevel.String())

	logger.Info("✅ 配置加载成功: 交易对=%s, 窗口大小=%d, 交易所=Binance",
		cfg.Trading.Symbol, cfg.Trading.BuyWindowSize)
	logger.Info("🧮 保证金占用上限: %.2f%%", cfg.Trading.MaxMarginUsagePercent)
	if !cfg.System.CancelOnExit {
		logger.Warn("⚠️ system.cancel_on_exit=false：普通退出会保留交易所挂单；保证金硬限制已锁存时仍会强制全撤")
	}

	// 2. 创建 Binance 交易所实例
	ex, err := exchange.NewBinance(cfg)
	if err != nil {
		logger.Fatalf("❌ 创建交易所实例失败: %v", err)
	}
	logger.Info("✅ 使用交易所: %s", ex.GetName())

	// 3. 创建价格监控组件（全局唯一的价格来源）
	// 架构说明：
	// - 这是整个系统中唯一的价格流启动点
	// - WebSocket 是唯一的价格来源，不使用 REST API 轮询
	// - 所有组件需要价格时，都应该通过 priceMonitor.GetLastPrice() 获取
	// - 必须在其他组件初始化前启动，确保价格数据就绪
	priceMonitor := monitor.NewPriceMonitor(
		ex,
		cfg.Trading.Symbol,
		cfg.Timing.PriceSendInterval,
	)

	// 4. 启动价格监控（WebSocket 必须成功）
	logger.Info("🔗 启动 WebSocket 价格流...")
	if err := priceMonitor.Start(); err != nil {
		logger.Fatalf("❌ 启动价格流失败（WebSocket 是唯一价格来源）: %v", err)
	}

	// 5. 等待从 WebSocket 获取初始价格
	logger.Debugln("⏳ 等待 WebSocket 推送初始价格...")
	var currentPrice float64
	var currentPriceStr string
	pollInterval := time.Duration(cfg.Timing.PricePollInterval) * time.Millisecond
	for i := 0; i < 10; i++ {
		currentPrice = priceMonitor.GetLastPrice()
		currentPriceStr = priceMonitor.GetLastPriceString()
		if currentPrice > 0 {
			break
		}
		time.Sleep(pollInterval)
	}

	if currentPrice <= 0 {
		logger.Fatalf("❌ 无法从 WebSocket 获取价格（超时），系统无法启动")
	}

	// 从交易所获取精度信息
	priceDecimals := ex.GetPriceDecimals()
	priceTickSize := ex.GetPriceTickSize()
	quantityDecimals := ex.GetQuantityDecimals()
	if priceTickSize <= 0 || math.IsNaN(priceTickSize) || math.IsInf(priceTickSize, 0) {
		logger.Fatalf("❌ 交易所返回无效价格步长: %v", priceTickSize)
	}
	if err := position.ValidateGridPriceInterval(cfg.Trading.PriceInterval, priceTickSize); err != nil {
		logger.Fatalf("❌ %v", err)
	}
	logger.Info("ℹ️ 交易精度 - 价格精度:%d, 价格步长:%g, 数量精度:%d",
		priceDecimals, priceTickSize, quantityDecimals)
	logger.Debug("📊 当前价格: %.*f", priceDecimals, currentPrice)

	// 6. 持仓安全性检查（必须在开始交易之前执行）
	requiredPositions := cfg.Trading.PositionSafetyCheck
	if requiredPositions <= 0 {
		requiredPositions = 100 // 默认100
	}

	// 获取当前交易所的手续费率
	exchangeCfg := cfg.Exchanges.Binance
	feeRate := exchangeCfg.FeeRate
	if feeProvider, ok := ex.(exchange.MakerFeeRateProvider); ok {
		feeCtx, feeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		actualFeeRate, feeErr := feeProvider.GetMakerFeeRate(feeCtx, cfg.Trading.Symbol)
		feeCancel()
		if feeErr != nil {
			logger.Fatalf("❌ 获取 %s 实际 Maker 手续费率失败，已拒绝启动交易: %v", ex.GetName(), feeErr)
		}

		feeRate = actualFeeRate
		exchangeCfg.FeeRate = actualFeeRate
		cfg.Exchanges.Binance = exchangeCfg
		logger.Info("✅ 已获取 %s 实际 Maker 手续费率: %.6f", ex.GetName(), actualFeeRate)
	}
	// 注意：支持0费率，不需要特殊处理

	// 执行持仓安全性检查（使用独立的 safety 包）
	if err := safety.CheckAccountSafety(
		ex,
		cfg.Trading.Symbol,
		currentPrice,
		cfg.Trading.OrderQuantity,
		cfg.Trading.PriceInterval,
		feeRate,
		requiredPositions,
		priceDecimals,
	); err != nil {
		logger.Fatalf("❌ %v", err)
	}
	logger.Info("✅ 持仓安全性检查通过，开始初始化交易组件...")

	// 8. 创建核心组件
	performance := telemetry.New(programStartedAt)
	exchangeExecutor := order.NewExchangeOrderExecutor(
		ex,
		cfg.Trading.Symbol,
		cfg.Timing.RateLimitRetryDelay,
		cfg.Timing.OrderRetryDelay,
	)
	exchangeExecutor.SetTelemetry(performance)
	exchangeExecutor.SetMakerGuard(
		priceMonitor.GetMarketSnapshot,
		priceTickSize,
		cfg.Execution.MakerGuardTicks,
		time.Duration(cfg.Execution.QuoteStaleMS)*time.Millisecond,
	)
	// 所有启动健康条件通过前，执行器必须保持 fail-closed。
	exchangeExecutor.StopNewOrders()
	executorAdapter := &exchangeExecutorAdapter{executor: exchangeExecutor}

	// 创建交易所适配器（匹配 position.IExchange 接口）
	exchangeAdapter := &positionExchangeAdapter{exchange: ex}
	superPositionManager := position.NewSuperPositionManager(
		cfg,
		executorAdapter,
		exchangeAdapter,
		priceDecimals,
		quantityDecimals,
		priceTickSize,
	)
	superPositionManager.SetMarketSnapshotProvider(priceMonitor.GetMarketSnapshot)
	superPositionManager.SetTelemetry(performance)
	performance.SetStateProvider(superPositionManager.TelemetryStateCounts)

	// === 新增：初始化风控监视器 ===
	riskMonitor := safety.NewRiskMonitor(cfg, ex)

	// === 创建对账器（从仓位管理器剖离） ===
	reconciler := safety.NewReconciler(cfg, exchangeAdapter, superPositionManager)
	// 将风控状态注入到对账器，用于暂停对账日志
	reconciler.SetPauseChecker(func() bool {
		return riskMonitor.IsTriggered()
	})

	// 9. 启动组件
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go performance.Run(ctx)
	if cfg.Telegram.Enabled {
		telegram := notification.NewTelegram(ctx, cfg.Telegram, ex.GetQuoteAsset())
		defer telegram.Close()
		superPositionManager.SetFilledOrderNotifier(telegram.NotifyFill)
		logger.Info("✅ Telegram 订单成交通知已启用（后台发送）")
	}

	// 🔥 关键修复：先启动订单流，再下单（避免错过成交推送）
	// 启动订单流（通过交易所接口）
	// 架构说明：
	// - 订单流与唯一市场数据流分别连接，但由同一交易门禁统一判定健康状态
	// - 订单更新通过回调函数实时推送给 SuperPositionManager
	logger.Info("🔗 启动 WebSocket 订单流...")
	if err := ex.StartOrderStream(ctx, func(update exchange.OrderUpdate) {
		posUpdate := position.OrderUpdate{
			OrderID:                update.OrderID,
			ClientOrderID:          update.ClientOrderID,
			Symbol:                 update.Symbol,
			Status:                 string(update.Status),
			Quantity:               update.Quantity,
			ExecutedQty:            update.ExecutedQty,
			Price:                  update.Price,
			AvgPrice:               update.AvgPrice,
			Side:                   string(update.Side),
			Type:                   string(update.Type),
			UpdateTime:             update.UpdateTime,
			RealizedPNL:            update.RealizedPNL,
			RealizedPNLIncremental: update.RealizedPNLIncremental,
		}
		logger.Debug("🔍 [main.go] 收到订单更新回调: ID=%d, ClientOID=%s, Price=%.2f, Status=%s",
			posUpdate.OrderID, posUpdate.ClientOrderID, posUpdate.Price, posUpdate.Status)
		superPositionManager.OnOrderUpdate(posUpdate)
	}); err != nil {
		logger.Fatalf("❌ 启动订单流失败，已拒绝启动交易: %v", err)
	}
	orderStreamReady, orderStreamState := currentOrderStreamHealth(ex)
	if !orderStreamReady {
		logger.Fatalf("❌ [%s] 订单流未就绪（状态=%s），已拒绝启动交易", ex.GetName(), orderStreamState)
	}
	logger.Info("✅ [%s] 订单流已启动（状态=%s）", ex.GetName(), orderStreamState)

	// 初始化超级仓位管理器（设置价格锚点并创建初始槽位）
	// 注意：必须在订单流启动后再初始化，避免错过买单成交推送
	if err := superPositionManager.Initialize(currentPrice, currentPriceStr); err != nil {
		logger.Fatalf("❌ 初始化超级仓位管理器失败: %v", err)
	}

	// 首次完整对账必须在放行任何新单前同步通过。
	if err := reconciler.Reconcile(); err != nil {
		logger.Fatalf("❌ 首次持仓对账失败，已拒绝启动交易: %v", err)
	}
	if !reconciler.IsHealthy() {
		logger.Fatalf("❌ 首次持仓对账未达到健康状态，已拒绝启动交易")
	}

	// 风控必须同步完成历史数据加载、实时流握手并进入 READY。
	if err := riskMonitor.Start(ctx); err != nil {
		logger.Fatalf("❌ 启动主动风控失败，已拒绝启动交易: %v", err)
	}
	if !riskMonitor.IsReady() {
		logger.Fatalf("❌ 主动风控未就绪，已拒绝启动交易")
	}

	// 从保证金守卫启动前就接管退出信号。这样启动首读已经超限、正在执行
	// 安全全撤时收到 SIGINT/SIGTERM，也不会被默认信号处理直接打断。
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 保证金守卫是交易关键依赖，独立于可关闭、可失败的只读面板。
	marginMonitor := safety.NewMarginMonitor(cfg, ex)
	if err := marginMonitor.Start(ctx); err != nil {
		logger.Fatalf("❌ 启动保证金守卫失败，已拒绝启动交易: %v", err)
	}
	if !marginMonitor.IsReady() {
		logger.Fatalf("❌ 保证金守卫未就绪，已拒绝启动交易")
	}

	orderGate := newSerializedOrderGate(exchangeExecutor)
	gateRuntime := newTradingGateRuntime(
		orderGate,
		ex,
		priceMonitor,
		riskMonitor,
		marginMonitor,
		reconciler,
		superPositionManager,
		configuredPriceStaleAfter(cfg.Timing.PriceSendInterval),
		time.Duration(cfg.Trading.ReconcileInterval)*time.Second,
	)
	marginMonitor.SetLimitHandler(gateRuntime.handleMarginLimit)
	shutdownRequested := consumePendingShutdownSignal(sigChan)
	if shutdownRequested {
		logger.Warn("⚠️ 交易门禁启动前已收到退出信号，跳过首次挂单并直接进入安全关闭")
	} else {
		// 首次评估会在所有健康条件成立后放行，并立即执行第一次 AdjustOrders。
		if err := gateRuntime.Start(ctx); err != nil {
			logger.Fatalf("❌ 启动交易门禁失败，已拒绝启动交易: %v", err)
		}
	}

	// 启动只读监控面板（失败不影响交易）。预启动信号已入队时
	// 不再创建任何运行期后台组件，保持执行器 fail-closed。
	var dash *web.Server
	if !shutdownRequested {
		// === 创建订单清理器（从仓位管理器剥离） ===
		orderCleaner := safety.NewOrderCleaner(cfg, exchangeExecutor, superPositionManager)
		orderCleaner.Start(ctx)

		if cfg.DashboardEnabled() {
			dash = web.New(web.Options{
				Performance: performance,
				Cfg:         cfg,
				Version:     Version,
				StartedAt:   programStartedAt,
				Price:       priceMonitor,
				Position:    superPositionManager,
				Risk:        riskMonitor,
				Margin:      marginMonitor,
				Exchange:    ex,
			})
			go func() {
				if err := dash.Start(); err != nil {
					logger.Error("❌ 监控面板启动失败: %v（交易继续运行）", err)
				}
			}()
		}

		go func() {
			statusInterval := time.Duration(cfg.Timing.StatusPrintInterval) * time.Minute
			ticker := time.NewTicker(statusInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					logPerformance(performance.Snapshot())
					// 风控触发时不打印状态
					if !riskMonitor.IsTriggered() {
						superPositionManager.PrintPositions()
					}
				}
			}
		}()

		// 14. 等待退出信号
		<-sigChan
	}
	signal.Stop(sigChan)

	logger.Info("🛑 收到退出信号，开始优雅关闭...")

	// 第一优先级：关闭门禁并等待串行协调器退出，杜绝撤单期间重新挂单。
	logger.Info("⏹️ 正在关闭交易门禁...")
	gateRuntime.Stop()

	// 第二优先级：普通退出按显式配置决定是否全撤；保证金硬限制已经锁存时
	// 必须完成其安全动作，不能被 cancel_on_exit=false 覆盖。
	marginLimitTriggered := stopMarginMonitorAndReadTriggered(marginMonitor)
	if shouldCancelOrdersOnShutdown(cfg.System.CancelOnExit, marginLimitTriggered) {
		if marginLimitTriggered && !cfg.System.CancelOnExit {
			logger.Warn("⚠️ 保证金限制已锁存，忽略 system.cancel_on_exit=false 并强制全撤")
		}
		logger.Info("🔄 正在撤销并确认所有订单...")
		var cancelErr error
		if marginLimitTriggered {
			cancelErr = cleanupMarginOrdersOnShutdown(context.Background(), ex, cfg.Trading.Symbol)
		} else {
			cancelCtx, cancelTimeout := context.WithTimeout(context.Background(), startupCancelTimeout)
			cancelErr = cancelAllOrdersAndConfirm(cancelCtx, ex, cfg.Trading.Symbol)
			cancelTimeout()
		}
		if cancelErr != nil {
			logger.Error("❌ 全撤确认失败: %v", cancelErr)
		} else {
			logger.Info("✅ 已确认远端挂单为空")
		}
	} else {
		logger.Warn("⚠️ 已按 system.cancel_on_exit=false 保留交易所挂单")
	}

	// 第三优先级：停止执行器的所有在途操作，再通知其余后台协程退出。
	exchangeExecutor.Shutdown()
	cancel()

	// 最后停止各条 WebSocket 流。
	logger.Info("⏹️ 正在停止价格监控...")
	priceMonitor.Stop()

	logger.Info("⏹️ 正在停止订单流...")
	if err := ex.StopOrderStream(); err != nil {
		logger.Error("❌ 停止订单流失败: %v", err)
	}

	logger.Info("⏹️ 正在停止风控监视器...")
	riskMonitor.Stop()

	if dash != nil {
		logger.Info("⏹️ 正在停止监控面板...")
		dash.Shutdown(2 * time.Second)
	}

	// 等待一小段时间，让协程完成清理（避免强制退出导致日志丢失）
	time.Sleep(500 * time.Millisecond)

	// 打印最终状态
	superPositionManager.PrintPositions()

	// 关闭文件日志
	logger.Close()

	logger.Info("✅ 系统已安全退出 www.OpenSQT.com")
}

func shouldCancelOrdersOnShutdown(cancelOnExit, marginLimitTriggered bool) bool {
	return cancelOnExit || marginLimitTriggered
}

func consumePendingShutdownSignal(signals <-chan os.Signal) bool {
	select {
	case <-signals:
		return true
	default:
		return false
	}
}

type shutdownMarginMonitor interface {
	Stop()
	IsTriggered() bool
}

// stopMarginMonitorAndReadTriggered 先等待在途账户刷新与 handler 全部退出，
// 再读取不会继续变化的锁存状态。
func stopMarginMonitorAndReadTriggered(monitor shutdownMarginMonitor) bool {
	if monitor == nil {
		return false
	}
	monitor.Stop()
	return monitor.IsTriggered()
}

type shutdownAuditClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type realShutdownAuditClock struct{}

func (realShutdownAuditClock) Now() time.Time { return time.Now() }

func (realShutdownAuditClock) Wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
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

// cleanupMarginOrdersOnShutdown 在单一、绝对的 15 秒截止时间内完成
// 初始稳定全撤和迟到订单复查，不因反复发现残单而无限延长停机。
func cleanupMarginOrdersOnShutdown(
	parent context.Context,
	ex cancelAllOrdersExchange,
	symbol string,
) error {
	if parent == nil {
		return fmt.Errorf("保证金停机清理上下文不能为空")
	}
	if ex == nil {
		return fmt.Errorf("保证金停机清理交易所不能为空")
	}

	clock := realShutdownAuditClock{}
	deadline := clock.Now().Add(marginAuditWindow)
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	return cleanupMarginOrdersOnShutdownUntil(ctx, ex, symbol, clock, deadline)
}

func cleanupMarginOrdersOnShutdownUntil(
	ctx context.Context,
	ex cancelAllOrdersExchange,
	symbol string,
	clock shutdownAuditClock,
	deadline time.Time,
) error {
	if ctx == nil {
		return fmt.Errorf("保证金停机清理上下文不能为空")
	}
	if ex == nil {
		return fmt.Errorf("保证金停机清理交易所不能为空")
	}
	if clock == nil || deadline.IsZero() {
		return fmt.Errorf("保证金停机清理时钟未初始化")
	}

	if err := cancelAllOrdersAndConfirmStable(ctx, ex, symbol, marginCancelStableReads); err != nil {
		return fmt.Errorf("保证金停机稳定全撤失败: %w", err)
	}

	nextAudit := clock.Now().Add(marginAuditInterval)
	var lastAuditErr error
	for {
		now := clock.Now()
		if !now.Before(deadline) {
			return lastAuditErr
		}

		wakeAt := nextAudit
		if wakeAt.After(deadline) {
			wakeAt = deadline
		}
		if err := clock.Wait(ctx, wakeAt.Sub(now)); err != nil {
			if !clock.Now().Before(deadline) && errors.Is(err, context.DeadlineExceeded) {
				return lastAuditErr
			}
			return errors.Join(lastAuditErr, fmt.Errorf("保证金停机复查等待失败: %w", err))
		}

		now = clock.Now()
		if !now.Before(deadline) {
			return lastAuditErr
		}
		nextAudit = now.Add(marginAuditInterval)

		openOrders, err := ex.GetOpenOrders(ctx, symbol)
		if err != nil {
			lastAuditErr = fmt.Errorf("保证金停机迟到订单复查失败: %w", err)
			logger.Warn("⚠️ %v，将在截止时间前继续重试", lastAuditErr)
			continue
		}
		if len(openOrders) == 0 {
			lastAuditErr = nil
			continue
		}

		logger.Warn("🚨 保证金停机复查发现 %d 个迟到挂单，再次执行当前交易对全撤", len(openOrders))
		if err := cancelAllOrdersAndConfirm(ctx, ex, symbol); err != nil {
			return fmt.Errorf("保证金停机迟到订单全撤失败: %w", err)
		}
		lastAuditErr = nil
	}
}

// positionExchangeAdapter 适配器，将 exchange.IExchange 转换为 position.IExchange
type positionExchangeAdapter struct {
	exchange exchange.IExchange
}

func (a *positionExchangeAdapter) GetPositions(ctx context.Context, symbol string) (interface{}, error) {
	return a.exchange.GetPositions(ctx, symbol)
}

func (a *positionExchangeAdapter) GetOpenOrders(ctx context.Context, symbol string) (interface{}, error) {
	return a.exchange.GetOpenOrders(ctx, symbol)
}

func (a *positionExchangeAdapter) GetOrder(ctx context.Context, symbol string, orderID int64) (interface{}, error) {
	return a.exchange.GetOrder(ctx, symbol, orderID)
}

func (a *positionExchangeAdapter) GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (interface{}, error) {
	lookup, ok := a.exchange.(interface {
		GetOrderByClientID(context.Context, string, string) (*exchange.Order, error)
	})
	if !ok {
		return nil, fmt.Errorf("交易所不支持按 ClientOrderID 查询")
	}
	return lookup.GetOrderByClientID(ctx, symbol, clientOrderID)
}

func (a *positionExchangeAdapter) GetBaseAsset() string {
	return a.exchange.GetBaseAsset()
}

func (a *positionExchangeAdapter) GetName() string {
	return a.exchange.GetName()
}

func (a *positionExchangeAdapter) CancelAllOrders(ctx context.Context, symbol string) error {
	return a.exchange.CancelAllOrders(ctx, symbol)
}

// exchangeExecutorAdapter 适配器，将 order.ExchangeOrderExecutor 转换为 position.OrderExecutorInterface
type exchangeExecutorAdapter struct {
	executor *order.ExchangeOrderExecutor
}

func (a *exchangeExecutorAdapter) PlaceOrder(req *position.OrderRequest) (*position.Order, error) {
	orderReq := &order.OrderRequest{
		Symbol:                 req.Symbol,
		Side:                   req.Side,
		Price:                  req.Price,
		Quantity:               req.Quantity,
		PriceDecimals:          req.PriceDecimals,
		ReduceOnly:             req.ReduceOnly,
		PostOnly:               req.PostOnly,      // 传递 PostOnly 参数
		ClientOrderID:          req.ClientOrderID, // 传递 ClientOrderID
		NearTouch:              req.NearTouch,
		AcquireSubmissionLease: req.AcquireSubmissionLease,
		OnSubmissionStarted:    req.OnSubmissionStarted,
		OnSubmissionUnknown:    req.MarkSubmissionUncertain,
		OnDefiniteRejection: func(kind order.OrderRejectionKind) {
			req.MarkDefiniteRejection(string(kind))
		},
	}
	ord, err := a.executor.PlaceOrder(orderReq)
	if err != nil {
		return nil, err
	}
	return &position.Order{
		OrderID:       ord.OrderID,
		ClientOrderID: ord.ClientOrderID, // 返回 ClientOrderID
		Symbol:        ord.Symbol,
		Side:          ord.Side,
		Type:          ord.Type,
		Price:         ord.Price,
		Quantity:      ord.Quantity,
		ExecutedQty:   ord.ExecutedQty,
		AvgPrice:      ord.AvgPrice,
		Status:        ord.Status,
		CreatedAt:     ord.CreatedAt,
		UpdateTime:    ord.UpdateTime,
	}, nil
}

func (a *exchangeExecutorAdapter) BatchPlaceOrders(orders []*position.OrderRequest) ([]*position.Order, bool, error) {
	orderReqs := make([]*order.OrderRequest, len(orders))
	for i, req := range orders {
		orderReqs[i] = &order.OrderRequest{
			Symbol:                 req.Symbol,
			Side:                   req.Side,
			Price:                  req.Price,
			Quantity:               req.Quantity,
			PriceDecimals:          req.PriceDecimals,
			ReduceOnly:             req.ReduceOnly,
			PostOnly:               req.PostOnly,      // 传递 PostOnly 参数
			ClientOrderID:          req.ClientOrderID, // 传递 ClientOrderID
			NearTouch:              req.NearTouch,
			AcquireSubmissionLease: req.AcquireSubmissionLease,
			OnSubmissionStarted:    req.OnSubmissionStarted,
			OnSubmissionUnknown:    req.MarkSubmissionUncertain,
			OnDefiniteRejection: func(kind order.OrderRejectionKind) {
				req.MarkDefiniteRejection(string(kind))
			},
		}
	}
	ords, marginError, placementErr := a.executor.BatchPlaceOrders(orderReqs)
	result := make([]*position.Order, len(ords))
	for i, ord := range ords {
		result[i] = &position.Order{
			OrderID:       ord.OrderID,
			ClientOrderID: ord.ClientOrderID, // 返回 ClientOrderID
			Symbol:        ord.Symbol,
			Side:          ord.Side,
			Type:          ord.Type,
			Price:         ord.Price,
			Quantity:      ord.Quantity,
			ExecutedQty:   ord.ExecutedQty,
			AvgPrice:      ord.AvgPrice,
			Status:        ord.Status,
			CreatedAt:     ord.CreatedAt,
			UpdateTime:    ord.UpdateTime,
		}
	}
	return result, marginError, placementErr
}

func (a *exchangeExecutorAdapter) BatchCancelOrders(orderIDs []int64) error {
	return a.executor.BatchCancelOrders(orderIDs)
}
