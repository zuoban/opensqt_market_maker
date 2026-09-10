package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"opensqt/logger"
)

const (
	defaultExchangeRateURL             = "https://api.frankfurter.app/latest?from=USD&to=CNY"
	defaultExchangeRateRefreshInterval = 6 * time.Hour
	defaultExchangeRateRetryInterval   = 10 * time.Minute
	defaultExchangeRateTimeout         = 4 * time.Second
	maxExchangeRateResponseBytes       = 64 << 10
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// ExchangeRateView 是面板使用的只读 USD/CNY 汇率缓存快照。
type ExchangeRateView struct {
	Ready     bool      `json:"ready"`
	CNYPerUSD float64   `json:"cnyPerUsd"`
	Source    string    `json:"source"`
	RateDate  string    `json:"rateDate"`
	UpdatedAt time.Time `json:"updatedAt"`
	Stale     bool      `json:"stale"`
	Error     string    `json:"error,omitempty"`
}

// ExchangeRateCache 异步刷新法币汇率。读取只访问内存，不阻塞面板快照或交易流程。
type ExchangeRateCache struct {
	client          httpDoer
	endpoint        string
	refreshInterval time.Duration
	retryInterval   time.Duration

	mu   sync.RWMutex
	view ExchangeRateView
}

func newExchangeRateCache(client httpDoer, endpoint string, refreshInterval, retryInterval time.Duration) *ExchangeRateCache {
	if client == nil {
		client = &http.Client{Timeout: defaultExchangeRateTimeout}
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultExchangeRateURL
	}
	if refreshInterval <= 0 {
		refreshInterval = defaultExchangeRateRefreshInterval
	}
	if retryInterval <= 0 {
		retryInterval = defaultExchangeRateRetryInterval
	}
	return &ExchangeRateCache{
		client:          client,
		endpoint:        endpoint,
		refreshInterval: refreshInterval,
		retryInterval:   retryInterval,
	}
}

func (c *ExchangeRateCache) Run(ctx context.Context) {
	if c == nil {
		return
	}
	for {
		err := c.refresh(ctx)
		delay := c.refreshInterval
		if err != nil {
			delay = c.retryInterval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (c *ExchangeRateCache) refresh(parent context.Context) error {
	if c == nil || c.client == nil {
		return fmt.Errorf("人民币汇率缓存未初始化")
	}
	ctx, cancel := context.WithTimeout(parent, defaultExchangeRateTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return c.markFailed(err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "OpenSQT-dashboard")
	resp, err := c.client.Do(req)
	if err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		return c.markFailed(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return c.markFailed(fmt.Errorf("汇率接口返回 HTTP %d", resp.StatusCode))
	}

	var payload struct {
		Base  string             `json:"base"`
		Date  string             `json:"date"`
		Rates map[string]float64 `json:"rates"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxExchangeRateResponseBytes))
	if err := decoder.Decode(&payload); err != nil {
		return c.markFailed(fmt.Errorf("解析汇率响应失败: %w", err))
	}
	rate := payload.Rates["CNY"]
	if !strings.EqualFold(payload.Base, "USD") || rate < 1 || rate > 20 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return c.markFailed(fmt.Errorf("汇率响应内容无效"))
	}

	c.mu.Lock()
	c.view = ExchangeRateView{
		Ready:     true,
		CNYPerUSD: rate,
		Source:    "Frankfurter",
		RateDate:  payload.Date,
		UpdatedAt: time.Now(),
	}
	c.mu.Unlock()
	logger.Info("💱 人民币汇率缓存已更新: 1 USD ≈ %.4f CNY（%s）", rate, payload.Date)
	return nil
}

func (c *ExchangeRateCache) markFailed(err error) error {
	c.mu.Lock()
	c.view.Stale = true
	c.view.Error = "人民币汇率暂不可用"
	c.mu.Unlock()
	logger.Warn("⚠️ 监控面板人民币汇率刷新失败，将保留最近缓存: %v", err)
	return err
}

func (c *ExchangeRateCache) View() ExchangeRateView {
	if c == nil {
		return ExchangeRateView{}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.view
}
