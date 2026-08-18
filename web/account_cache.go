package web

import (
	"context"
	"sync"
	"time"

	"opensqt/exchange"
	"opensqt/logger"
)

// AccountSource 账户缓存需要的交易所子集
type AccountSource interface {
	GetAccount(ctx context.Context) (*exchange.Account, error)
	GetQuoteAsset() string
}

// AccountView 面板用的账户读数
type AccountView struct {
	Available     float64   `json:"available"`
	Wallet        float64   `json:"wallet"`
	Margin        float64   `json:"margin"`
	Leverage      int       `json:"leverage"`
	PositionSize  float64   `json:"positionSize"`
	EntryPrice    float64   `json:"entryPrice"`
	UnrealizedPNL float64   `json:"unrealizedPnl"`
	QuoteAsset    string    `json:"quoteAsset"`
	Stale         bool      `json:"stale"`
	UpdatedAt     time.Time `json:"updatedAt"`
	Error         string    `json:"error,omitempty"`
}

// AccountCache 限频拉取账户，失败时保留上一份并标 stale。
type AccountCache struct {
	src      AccountSource
	symbol   string
	interval time.Duration

	mu   sync.RWMutex
	view AccountView
}

func newAccountCache(src AccountSource, symbol string, interval time.Duration) *AccountCache {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return &AccountCache{src: src, symbol: symbol, interval: interval}
}

func (c *AccountCache) Run(ctx context.Context) {
	if c == nil || c.src == nil {
		return
	}
	c.refresh(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

func (c *AccountCache) refresh(parent context.Context) {
	if c == nil || c.src == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()

	acc, err := c.src.GetAccount(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.view.Stale = true
		c.view.Error = err.Error()
		if c.view.UpdatedAt.IsZero() {
			logger.Warn("监控面板拉取账户失败: %v", err)
		}
		return
	}

	view := AccountView{
		QuoteAsset: c.src.GetQuoteAsset(),
		UpdatedAt:  time.Now(),
	}
	if acc != nil {
		view.Available = acc.AvailableBalance
		view.Wallet = acc.TotalWalletBalance
		view.Margin = acc.TotalMarginBalance
		view.Leverage = acc.AccountLeverage
		for _, p := range acc.Positions {
			if p != nil && (c.symbol == "" || p.Symbol == c.symbol) && p.Size != 0 {
				view.PositionSize = p.Size
				view.EntryPrice = p.EntryPrice
				view.UnrealizedPNL = p.UnrealizedPNL
				if p.Leverage > 0 {
					view.Leverage = p.Leverage
				}
				if c.symbol != "" && p.Symbol == c.symbol {
					break
				}
			}
		}
	}
	c.view = view
}

func (c *AccountCache) View() AccountView {
	if c == nil {
		return AccountView{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.view
}
