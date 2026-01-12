package safety

import (
	"context"
	"fmt"
	"opensqt/config"
	"opensqt/exchange"
	"opensqt/logger"
	"sync"
	"sync/atomic"
	"time"
)

type TakeProfitMonitor struct {
	cfg            *config.Config
	exchange       exchange.IExchange
	initialBalance atomic.Value
	lastBalance    atomic.Value
	triggered      atomic.Bool
	isBalanceSet   atomic.Bool
	mu             sync.RWMutex
}

func NewTakeProfitMonitor(cfg *config.Config, ex exchange.IExchange) *TakeProfitMonitor {
	return &TakeProfitMonitor{
		cfg:      cfg,
		exchange: ex,
	}
}

func (t *TakeProfitMonitor) SetInitialBalance(ctx context.Context) error {
	account, err := t.exchange.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("获取初始余额失败: %w", err)
	}

	balance := t.getEffectiveBalance(account)
	if balance <= 0 {
		return fmt.Errorf("账户余额无效: %.2f", balance)
	}

	t.initialBalance.Store(balance)
	t.lastBalance.Store(balance)
	t.isBalanceSet.Store(true)

	logger.Info("💰 [止盈监控] 初始余额已记录: %.2f USDT", balance)
	return nil
}

func (t *TakeProfitMonitor) Start(ctx context.Context, onTrigger func()) {
	if !t.cfg.Trading.TakeProfit.Enabled {
		logger.Info("⚠️ 自动止盈未启用")
		return
	}

	checkInterval := t.cfg.Trading.TakeProfit.CheckInterval
	if checkInterval <= 0 {
		checkInterval = 30
	}

	logger.Info("🎯 [止盈监控] 启动 (目标: %.2f USDT, 间隔: %d秒)",
		t.cfg.Trading.TakeProfit.TargetProfit, checkInterval)

	ticker := time.NewTicker(time.Duration(checkInterval) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logger.Info("⏹️ [止盈监控] 监控已停止")
			return

		case <-ticker.C:
			if !t.isBalanceSet.Load() {
				continue
			}

			if t.checkProfitAndTrigger() {
				onTrigger()
				return
			}
		}
	}
}

func (t *TakeProfitMonitor) checkProfitAndTrigger() bool {
	if t.triggered.Load() {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	account, err := t.exchange.GetAccount(ctx)
	if err != nil {
		logger.Error("❌ [止盈检查] 获取账户余额失败: %v", err)
		return false
	}

	currentBalance := t.getEffectiveBalance(account)
	t.lastBalance.Store(currentBalance)

	initialBalance := t.initialBalance.Load().(float64)
	totalProfit := currentBalance - initialBalance

	logger.Debug("📊 [止盈检查] 初始余额: %.2f USDT, 当前余额: %.2f USDT, 盈利: %.2f USDT, 目标: %.2f USDT",
		initialBalance, currentBalance, totalProfit, t.cfg.Trading.TakeProfit.TargetProfit)

	if totalProfit >= t.cfg.Trading.TakeProfit.TargetProfit {
		t.triggered.Store(true)

		logger.Info("🎯 [止盈触发] ===")
		logger.Info("🎯 [止盈触发] 初始余额: %.2f USDT", initialBalance)
		logger.Info("🎯 [止盈触发] 当前余额: %.2f USDT", currentBalance)
		logger.Info("🎯 [止盈触发] 总盈利: %.2f USDT", totalProfit)
		logger.Info("🎯 [止盈触发] 目标盈利: %.2f USDT", t.cfg.Trading.TakeProfit.TargetProfit)
		if initialBalance > 0 {
			logger.Info("🎯 [止盈触发] 盈利率: %.2f%%", (totalProfit/initialBalance)*100)
		}
		logger.Info("🎯 [止盈触发] ===")

		return true
	}

	return false
}

func (t *TakeProfitMonitor) IsTriggered() bool {
	return t.triggered.Load()
}

func (t *TakeProfitMonitor) GetCurrentProfit() (float64, float64, float64) {
	initialBalance := t.initialBalance.Load().(float64)
	currentBalance := t.lastBalance.Load().(float64)
	profit := currentBalance - initialBalance
	return initialBalance, currentBalance, profit
}

func (t *TakeProfitMonitor) getEffectiveBalance(account *exchange.Account) float64 {
	balance := account.TotalMarginBalance
	if balance > 0 {
		return balance
	}

	if account.TotalWalletBalance > 0 {
		return account.TotalWalletBalance
	}

	return account.AvailableBalance
}
