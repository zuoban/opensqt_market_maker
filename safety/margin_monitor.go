package safety

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/logger"
)

const (
	defaultMarginRefreshInterval = 2 * time.Second
	defaultMarginStaleAfter      = 10 * time.Second
	marginRequestTimeout         = 3 * time.Second
	marginErrorLogInterval       = 30 * time.Second
)

// MarginSnapshot 是保证金守卫对交易门禁和只读面板提供的不可变读数。
// UsedMargin 使用交易所账户口径：保证金余额减去可用余额，因此同时包含
// 持仓初始保证金和挂单冻结保证金。
type MarginSnapshot struct {
	Ready            bool      `json:"ready"`
	Triggered        bool      `json:"triggered"`
	UsagePercent     float64   `json:"usagePercent"`
	LimitPercent     float64   `json:"limitPercent"`
	UsedMargin       float64   `json:"usedMargin"`
	MarginBalance    float64   `json:"marginBalance"`
	AvailableBalance float64   `json:"availableBalance"`
	WalletBalance    float64   `json:"walletBalance"`
	QuoteAsset       string    `json:"quoteAsset"`
	UpdatedAt        time.Time `json:"updatedAt"`
	TriggeredAt      time.Time `json:"triggeredAt,omitempty"`
	Stale            bool      `json:"stale"`
	Error            string    `json:"error,omitempty"`
}

// MarginAccountSource 是保证金守卫所需的最小交易所接口。
type MarginAccountSource interface {
	GetAccount(ctx context.Context) (*exchange.Account, error)
	GetQuoteAsset() string
}

// MarginMonitor 定期读取账户保证金。占用比例一旦超过限制便锁存到本次运行结束，
// 防止全撤释放挂单保证金后立刻重新挂单造成反复震荡。
type MarginMonitor struct {
	source          MarginAccountSource
	limitPercent    float64
	refreshInterval time.Duration
	staleAfter      time.Duration

	mu          sync.RWMutex
	snapshot    MarginSnapshot
	lastSuccess time.Time
	handler     func(MarginSnapshot)
	lastLogAt   time.Time

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewMarginMonitor 创建运行时保证金守卫。
func NewMarginMonitor(cfg *config.Config, source MarginAccountSource) *MarginMonitor {
	limit := 100.0
	if cfg != nil && cfg.Trading.MaxMarginUsagePercent > 0 {
		limit = cfg.Trading.MaxMarginUsagePercent
	}
	quoteAsset := ""
	if source != nil {
		quoteAsset = source.GetQuoteAsset()
	}
	return &MarginMonitor{
		source:          source,
		limitPercent:    limit,
		refreshInterval: defaultMarginRefreshInterval,
		staleAfter:      defaultMarginStaleAfter,
		snapshot: MarginSnapshot{
			LimitPercent: limit,
			QuoteAsset:   quoteAsset,
			Stale:        true,
		},
	}
}

// SetLimitHandler 设置首次触发限制时的非阻塞处理函数。若设置前已经触发，
// 会立即补发当前快照，确保交易门禁不会错过启动窗口。
func (m *MarginMonitor) SetLimitHandler(handler func(MarginSnapshot)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.handler = handler
	snap := m.snapshot
	m.mu.Unlock()
	if handler != nil && snap.Triggered {
		handler(m.Snapshot())
	}
}

// Start 同步完成首轮账户读取后启动后台刷新。首轮失败时 fail-closed。
func (m *MarginMonitor) Start(parent context.Context) error {
	if m == nil {
		return fmt.Errorf("保证金守卫未初始化")
	}
	if parent == nil {
		return fmt.Errorf("保证金守卫上下文不能为空")
	}

	m.lifecycleMu.Lock()
	if m.done != nil {
		m.lifecycleMu.Unlock()
		return fmt.Errorf("保证金守卫已启动")
	}
	m.lifecycleMu.Unlock()

	if err := m.refresh(parent); err != nil {
		return fmt.Errorf("保证金守卫首次读取失败: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	m.lifecycleMu.Lock()
	if m.done != nil {
		m.lifecycleMu.Unlock()
		cancel()
		return fmt.Errorf("保证金守卫已启动")
	}
	m.cancel = cancel
	m.done = done
	m.lifecycleMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(m.refreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = m.refresh(ctx)
			}
		}
	}()
	return nil
}

// Stop 停止后台账户刷新。
func (m *MarginMonitor) Stop() {
	if m == nil {
		return
	}
	m.lifecycleMu.Lock()
	cancel := m.cancel
	done := m.done
	m.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// IsReady 表示最近一次成功账户读数仍在新鲜度窗口内。
func (m *MarginMonitor) IsReady() bool {
	return m.Snapshot().Ready
}

// IsTriggered 表示保证金限制已在本次运行中锁存。
func (m *MarginMonitor) IsTriggered() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	triggered := m.snapshot.Triggered
	m.mu.RUnlock()
	return triggered
}

// Snapshot 返回保证金守卫只读快照。
func (m *MarginMonitor) Snapshot() MarginSnapshot {
	if m == nil {
		return MarginSnapshot{Stale: true}
	}
	m.mu.RLock()
	snap := m.snapshot
	lastSuccess := m.lastSuccess
	staleAfter := m.staleAfter
	m.mu.RUnlock()

	snap.Ready = !lastSuccess.IsZero() && time.Since(lastSuccess) <= staleAfter
	if !snap.Ready {
		snap.Stale = true
	}
	return snap
}

func (m *MarginMonitor) refresh(parent context.Context) error {
	if m == nil || m.source == nil {
		return fmt.Errorf("保证金账户数据源未初始化")
	}
	ctx, cancel := context.WithTimeout(parent, marginRequestTimeout)
	defer cancel()

	account, err := m.source.GetAccount(ctx)
	if err != nil {
		m.recordRefreshError(err)
		return err
	}
	used, usage, err := calculateMarginUsage(account)
	if err != nil {
		m.recordRefreshError(err)
		return err
	}

	now := time.Now()
	m.mu.Lock()
	wasStale := m.snapshot.Stale
	wasTriggered := m.snapshot.Triggered
	triggeredAt := m.snapshot.TriggeredAt
	triggered := wasTriggered || usage > m.limitPercent
	if triggered && triggeredAt.IsZero() {
		triggeredAt = now
	}
	m.snapshot = MarginSnapshot{
		Ready:            true,
		Triggered:        triggered,
		UsagePercent:     usage,
		LimitPercent:     m.limitPercent,
		UsedMargin:       used,
		MarginBalance:    account.TotalMarginBalance,
		AvailableBalance: account.AvailableBalance,
		WalletBalance:    account.TotalWalletBalance,
		QuoteAsset:       m.source.GetQuoteAsset(),
		UpdatedAt:        now,
		TriggeredAt:      triggeredAt,
	}
	m.lastSuccess = now
	m.lastLogAt = time.Time{}
	handler := m.handler
	snap := m.snapshot
	m.mu.Unlock()

	if wasStale {
		logger.Info("✅ 保证金守卫已就绪: 占用 %.2f%% / 上限 %.2f%%", usage, m.limitPercent)
	}
	if !wasTriggered && triggered {
		logger.Warn("🚨 保证金占用超过限制: %.2f%% > %.2f%%，已锁存停单并请求全撤", usage, m.limitPercent)
		if handler != nil {
			handler(snap)
		}
	}
	return nil
}

func (m *MarginMonitor) recordRefreshError(err error) {
	if m == nil || err == nil {
		return
	}
	now := time.Now()
	m.mu.Lock()
	m.snapshot.Stale = true
	m.snapshot.Error = err.Error()
	shouldLog := m.lastLogAt.IsZero() || now.Sub(m.lastLogAt) >= marginErrorLogInterval
	if shouldLog {
		m.lastLogAt = now
	}
	m.mu.Unlock()
	if shouldLog {
		logger.Warn("⚠️ 保证金守卫读取账户失败，保留上一读数: %v", err)
	}
}

func calculateMarginUsage(account *exchange.Account) (used, usagePercent float64, err error) {
	if account == nil {
		return 0, 0, fmt.Errorf("交易所返回空账户")
	}
	values := map[string]float64{
		"walletBalance":    account.TotalWalletBalance,
		"marginBalance":    account.TotalMarginBalance,
		"availableBalance": account.AvailableBalance,
	}
	for name, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, 0, fmt.Errorf("账户字段 %s 不是有限数值", name)
		}
	}

	margin := account.TotalMarginBalance
	available := account.AvailableBalance
	if margin <= 0 {
		return 0, 0, fmt.Errorf("账户保证金余额必须大于0，当前为 %g", margin)
	}

	used = margin - available
	if used < 0 {
		used = 0
	}
	usagePercent = used / margin * 100
	return used, usagePercent, nil
}
