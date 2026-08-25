package safety

import (
	"context"
	"fmt"
	"opensqt/config"
	"opensqt/exchange"
	"opensqt/logger"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SymbolData 单个币种的K线数据缓存
type SymbolData struct {
	candles []*exchange.Candle
	mu      sync.RWMutex
}

// RiskMonitor 主动安全风控监视器
type RiskMonitor struct {
	cfg           *config.Config
	exchange      exchange.IExchange
	symbolDataMap map[string]*SymbolData
	mu            sync.RWMutex
	triggered     bool
	ready         bool
	lastMsg       string
	liveReadyCh   chan struct{}
	liveReadyOnce sync.Once
	seenLive      map[string]bool
	lastEventAt   map[string]time.Time
}

// NewRiskMonitor 创建风控监视器
func NewRiskMonitor(cfg *config.Config, ex exchange.IExchange) *RiskMonitor {
	symbolDataMap := make(map[string]*SymbolData)
	for _, symbol := range cfg.RiskControl.MonitorSymbols {
		symbolDataMap[symbol] = &SymbolData{
			candles: make([]*exchange.Candle, 0, cfg.RiskControl.AverageWindow+1),
		}
	}

	return &RiskMonitor{
		cfg:           cfg,
		exchange:      ex,
		symbolDataMap: symbolDataMap,
		liveReadyCh:   make(chan struct{}),
		seenLive:      make(map[string]bool, len(symbolDataMap)),
		lastEventAt:   make(map[string]time.Time, len(symbolDataMap)),
	}
}

// Start 启动监控
func (r *RiskMonitor) Start(ctx context.Context) error {
	if !r.cfg.RiskControl.Enabled {
		logger.Info("⚠️ 主动安全风控未启用")
		r.setReady(true, "风控已禁用")
		return nil
	}
	r.setReady(false, "风控初始化中")

	logger.Info("🛡️ 启动主动安全风控监控 (周期: %s, 倍数: %.1f, 窗口: %d)",
		r.cfg.RiskControl.Interval, r.cfg.RiskControl.VolumeMultiplier, r.cfg.RiskControl.AverageWindow)
	logger.Info("🛡️ 监控币种: %v (恢复阈值: %d/%d)", r.cfg.RiskControl.MonitorSymbols,
		r.cfg.RiskControl.RecoveryThreshold, len(r.cfg.RiskControl.MonitorSymbols))

	// 预加载历史K线数据
	logger.Info("📊 正在加载历史K线数据...")
	for _, symbol := range r.cfg.RiskControl.MonitorSymbols {
		candles, err := r.exchange.GetHistoricalKlines(ctx, symbol, r.cfg.RiskControl.Interval, r.cfg.RiskControl.AverageWindow+1)
		if err != nil {
			err = fmt.Errorf("加载 %s 历史K线失败: %w", symbol, err)
			r.setReady(false, err.Error())
			return err
		}

		closedCount := 0
		for _, candle := range candles {
			if candle != nil && candle.IsClosed {
				closedCount++
			}
		}
		if closedCount < r.cfg.RiskControl.AverageWindow {
			err := fmt.Errorf("%s 历史完结K线不足: %d < %d", symbol, closedCount, r.cfg.RiskControl.AverageWindow)
			r.setReady(false, err.Error())
			return err
		}

		r.mu.RLock()
		symbolData, exists := r.symbolDataMap[symbol]
		r.mu.RUnlock()
		if !exists {
			err := fmt.Errorf("风控币种未初始化: %s", symbol)
			r.setReady(false, err.Error())
			return err
		}
		symbolData.mu.Lock()
		for _, candle := range candles {
			mergeRiskCandle(&symbolData.candles, candle, r.cfg.RiskControl.AverageWindow+2)
		}
		loaded := len(symbolData.candles)
		symbolData.mu.Unlock()
		logger.Info("✅ %s: 已加载 %d 根历史K线", symbol, loaded)
	}
	logger.Info("✅ 历史K线数据加载完成，等待实时K线流...")

	// 启动K线流
	if err := r.exchange.StartKlineStream(ctx, r.cfg.RiskControl.MonitorSymbols, r.cfg.RiskControl.Interval, r.onCandleUpdate); err != nil {
		err = fmt.Errorf("启动K线流失败: %w", err)
		r.setReady(false, err.Error())
		return err
	}

	readyTimer := time.NewTimer(15 * time.Second)
	defer readyTimer.Stop()
	select {
	case <-r.liveReadyCh:
		r.setReady(true, "风控数据已就绪")
	case <-readyTimer.C:
		err := fmt.Errorf("等待所有实时 K 线就绪超时")
		r.setReady(false, err.Error())
		_ = r.exchange.StopKlineStream()
		return err
	case <-ctx.Done():
		r.setReady(false, "风控启动已取消")
		return ctx.Err()
	}

	// 启动定期报告协程（每60秒）
	go r.reportLoop(ctx)
	go r.staleLoop(ctx)
	return nil
}

// onCandleUpdate K线更新回调（实时检测）
func (r *RiskMonitor) onCandleUpdate(candle *exchange.Candle) {
	if candle == nil {
		logger.Warn("⚠️ 收到空K线数据")
		return
	}
	c := candle

	// 更新缓存
	r.mu.Lock()
	symbolData, exists := r.symbolDataMap[c.Symbol]
	if exists {
		r.lastEventAt[c.Symbol] = time.Now()
		r.seenLive[c.Symbol] = true
	}
	allSeen := exists && len(r.seenLive) == len(r.symbolDataMap)
	if allSeen {
		for symbol := range r.symbolDataMap {
			if !r.seenLive[symbol] {
				allSeen = false
				break
			}
		}
	}
	r.mu.Unlock()

	if !exists {
		logger.Warn("⚠️ 收到未监控的币种K线: %s", c.Symbol)
		return
	}

	symbolData.mu.Lock()
	mergeRiskCandle(&symbolData.candles, c, r.cfg.RiskControl.AverageWindow+2)
	currentCount := len(symbolData.candles)
	symbolData.mu.Unlock()

	if allSeen {
		r.liveReadyOnce.Do(func() { close(r.liveReadyCh) })
	}

	// 只在完结K线时打印日志，避免日志过多
	if c.IsClosed {
		logger.Debug("📈 [K线收集] %s: 价格=%.4f, 成交量=%.0f, 完结=%v, 已缓存%d根",
			c.Symbol, c.Close, c.Volume, c.IsClosed, currentCount)
	}

	// 实时检测（使用最新数据，包括未完结的K线）
	r.checkMarket()
}

// checkMarket 执行市场检查（实时，无日志）
func (r *RiskMonitor) checkMarket() {
	// 先检查当前状态（不持有锁）
	r.mu.RLock()
	triggered := r.triggered
	r.mu.RUnlock()

	if triggered {
		// 已触发状态：检查是否可以解除
		canRecover, details := r.checkRecovery()

		r.mu.Lock()
		if canRecover {
			// 统计恢复的币种数量
			recoveredCount := 0
			for _, detail := range details {
				if !strings.Contains(detail, "未恢复") {
					recoveredCount++
				}
			}
			logger.Info("✅ 市场风险信号消失，解除风控限制。(%d/%d 币种已恢复正常，达到恢复阈值 %d)",
				recoveredCount, len(r.cfg.RiskControl.MonitorSymbols), r.cfg.RiskControl.RecoveryThreshold)
			logger.Info("详情: %s", strings.Join(details, ", "))
			r.triggered = false
			r.lastMsg = "已恢复正常"
		} else {
			r.lastMsg = fmt.Sprintf("风控中，等待恢复: %s", strings.Join(details, ","))
		}
		r.mu.Unlock()
	} else {
		// 未触发状态：检查是否需要触发
		panicCount := 0
		details := []string{}

		for _, symbol := range r.cfg.RiskControl.MonitorSymbols {
			isPanic, reason := r.checkSymbol(symbol)
			if isPanic {
				panicCount++
				details = append(details, fmt.Sprintf("%s(%s)", symbol, reason))
			}
		}

		// 全部币种都出现异常时才触发
		r.mu.Lock()
		if panicCount > 0 && panicCount >= len(r.cfg.RiskControl.MonitorSymbols) {
			logger.Warn("🚨🚨🚨 触发主动安全风控！市场出现集体异动！🚨🚨🚨")
			logger.Warn("详情: %s", strings.Join(details, ", "))
			r.triggered = true
			r.lastMsg = fmt.Sprintf("触发风控: %d/%d 币种异常 (%s)", panicCount, len(r.cfg.RiskControl.MonitorSymbols), strings.Join(details, ","))
		} else {
			r.lastMsg = "监控正常"
		}
		r.mu.Unlock()
	}
}

// checkRecovery 检查是否可以解除风控（价格回到均线上方 + 成交量恢复正常）
func (r *RiskMonitor) checkRecovery() (bool, []string) {
	recoveredCount := 0
	details := []string{}

	for _, symbol := range r.cfg.RiskControl.MonitorSymbols {
		isRecovered, reason := r.checkSymbolRecovery(symbol)
		if isRecovered {
			recoveredCount++
			details = append(details, fmt.Sprintf("%s(%s)", symbol, reason))
		} else {
			details = append(details, fmt.Sprintf("%s(未恢复:%s)", symbol, reason))
		}
	}

	// 达到恢复阈值即可解除风控
	threshold := r.cfg.RiskControl.RecoveryThreshold
	return recoveredCount >= threshold, details
}

// checkSymbolRecovery 检查单个币种是否恢复（价格>均价 且 成交量<均值×倍数）
// 解除风控必须使用完结的K线数据
func (r *RiskMonitor) checkSymbolRecovery(symbol string) (bool, string) {
	symbolData, exists := r.symbolDataMap[symbol]
	if !exists {
		return false, "无数据"
	}

	symbolData.mu.RLock()
	candles := cloneCandles(symbolData.candles)
	candleCount := len(candles)
	symbolData.mu.RUnlock()

	if candleCount < r.cfg.RiskControl.AverageWindow+1 {
		return false, "数据不足"
	}

	// 找到最新的完结K线用于判断（如果最后一根是未完结的，使用倒数第二根）
	var currentCandle *exchange.Candle
	var currentPrice float64

	for i := candleCount - 1; i >= 0; i-- {
		if candles[i].IsClosed {
			currentCandle = candles[i]
			currentPrice = currentCandle.Close
			break
		}
	}

	if currentCandle == nil {
		return false, "无完结K线"
	}

	// 计算移动平均价格和移动平均成交量（只使用完结的K线，排除当前用于判断的这根）
	var totalPrice float64
	var totalVol float64
	var validCount int
	window := r.cfg.RiskControl.AverageWindow

	for i := candleCount - 1; i >= 0 && validCount < window; i-- {
		if candles[i].IsClosed && candles[i] != currentCandle {
			totalPrice += candles[i].Close
			totalVol += candles[i].Volume
			validCount++
		}
	}

	if validCount < window {
		return false, fmt.Sprintf("完结K线不足(%d<%d)", validCount, window)
	}

	avgPrice := totalPrice / float64(validCount)
	avgVol := totalVol / float64(validCount)

	// 恢复条件：价格 > 均价 且 成交量 < 均值×倍数（与触发条件对应）
	priceAboveMA := currentPrice > avgPrice
	volNormal := currentCandle.Volume < avgVol*r.cfg.RiskControl.VolumeMultiplier

	if priceAboveMA && volNormal {
		return true, "价格回归均线/量正常"
	}

	// 返回未恢复原因
	if !priceAboveMA {
		return false, fmt.Sprintf("价格%.2f<均价%.2f", currentPrice, avgPrice)
	}
	return false, fmt.Sprintf("量%.0f>均量×%.1f", currentCandle.Volume, r.cfg.RiskControl.VolumeMultiplier)
}

// checkSymbol 检查单个币种（基于移动平均线）
// 触发风控可以使用最新K线数据（包括未完结的K线），以便及时检测到异常
func (r *RiskMonitor) checkSymbol(symbol string) (bool, string) {
	r.mu.RLock()
	symbolData, exists := r.symbolDataMap[symbol]
	r.mu.RUnlock()

	if !exists {
		return false, ""
	}

	symbolData.mu.RLock()
	candles := cloneCandles(symbolData.candles)
	candleCount := len(candles)
	symbolData.mu.RUnlock()

	if candleCount < r.cfg.RiskControl.AverageWindow+1 {
		return false, ""
	}

	// 最新K线（可以是未完结的，用于实时检测）
	currentCandle := candles[candleCount-1]
	currentPrice := currentCandle.Close

	// 计算移动平均价格和移动平均成交量（使用历史完结的K线）
	var totalPrice float64
	var totalVol float64
	var validCount int
	window := r.cfg.RiskControl.AverageWindow

	// 从倒数第二根K线开始往前计算（排除当前可能未完结的K线）
	for i := candleCount - 2; i >= 0 && validCount < window; i-- {
		if candles[i].IsClosed {
			totalPrice += candles[i].Close
			totalVol += candles[i].Volume
			validCount++
		}
	}

	if validCount < window {
		return false, ""
	}

	avgPrice := totalPrice / float64(validCount)
	avgVol := totalVol / float64(validCount)

	// 计算当前价格偏离均线的百分比
	priceDeviation := (currentPrice - avgPrice) / avgPrice * 100
	volRatio := currentCandle.Volume / avgVol

	// 触发条件：当前价格 < 均价 且 成交量放大（使用最新数据，包括未完结K线）
	if currentPrice < avgPrice && currentCandle.Volume > avgVol*r.cfg.RiskControl.VolumeMultiplier {
		return true, fmt.Sprintf("价格%.2f%%低于均线/量×%.1f", priceDeviation, volRatio)
	}

	return false, ""
}

// IsTriggered 返回是否触发风控
func (r *RiskMonitor) IsTriggered() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.triggered || (r.cfg.RiskControl.Enabled && !r.ready)
}

// IsReady 表示历史数据和实时 K 线均健康。
func (r *RiskMonitor) IsReady() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.cfg.RiskControl.Enabled || r.ready
}

// reportLoop 定期报告状态（每60秒）
func (r *RiskMonitor) reportLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.reportStatus()
		}
	}
}

// reportStatus 报告状态
func (r *RiskMonitor) reportStatus() {
	triggered := r.IsTriggered()

	if triggered {
		logger.Warn("⚠️ [风控监测] 当前市场交易出现异动,触发主动安全风控,停止交易!")
	} else {
		logger.Info("🛡️ [风控监测] 市场环境正常。")
	}

	// 打印各币种的移动平均线数值
	r.printMovingAverages(triggered)
}

// printMovingAverages 打印各币种的移动平均线数值
func (r *RiskMonitor) printMovingAverages(inRiskControl bool) {
	logger.Info("📊 [移动平均线监测] 当前各币种数据:")

	hasStaleData := false
	for _, symbol := range r.cfg.RiskControl.MonitorSymbols {
		m := r.metricsForSymbol(symbol, inRiskControl)
		if !m.Ready {
			logger.Info("  %s: %s", symbol, m.SkipReason)
			continue
		}

		var statusMsg string
		if inRiskControl {
			if m.PriceAboveMA && m.VolumeNormal {
				statusMsg = fmt.Sprintf("正常[%s|%s]: 当前价=%.4f, 均价=%.4f (偏离%.2f%%), 现价在均价上方已恢复, 当前量=%.0f, 均量=%.0f (倍数×%.2f) 成交量已恢复",
					m.KlineStatus, m.KlineAgeStr, m.CurrentPrice, m.AvgPrice, m.PriceDeviation, m.CurrentVolume, m.AvgVolume, m.VolumeRatio)
			} else {
				priceStatus := "现价在均价下方未恢复"
				if m.PriceAboveMA {
					priceStatus = "现价在均价上方已恢复"
				}
				volStatus := "成交量未恢复"
				if m.VolumeNormal {
					volStatus = "成交量已恢复"
				}
				statusMsg = fmt.Sprintf("异常[%s|%s]: 当前价=%.4f, 均价=%.4f (偏离%.2f%%), %s, 当前量=%.0f, 均量=%.0f (倍数×%.2f) %s",
					m.KlineStatus, m.KlineAgeStr, m.CurrentPrice, m.AvgPrice, m.PriceDeviation, priceStatus, m.CurrentVolume, m.AvgVolume, m.VolumeRatio, volStatus)
			}
		} else if m.Abnormal {
			statusMsg = fmt.Sprintf("🚨异常[%s|%s]: 当前价=%.4f, 均价=%.4f (偏离%.2f%%), 当前量=%.0f, 均量=%.0f (倍数×%.2f)",
				m.KlineStatus, m.KlineAgeStr, m.CurrentPrice, m.AvgPrice, m.PriceDeviation, m.CurrentVolume, m.AvgVolume, m.VolumeRatio)
		} else {
			statusMsg = fmt.Sprintf("✅正常[%s|%s]: 当前价=%.4f, 均价=%.4f (偏离%.2f%%), 当前量=%.0f, 均量=%.0f (倍数×%.2f)",
				m.KlineStatus, m.KlineAgeStr, m.CurrentPrice, m.AvgPrice, m.PriceDeviation, m.CurrentVolume, m.AvgVolume, m.VolumeRatio)
		}

		logger.Info("  %s %s", symbol, statusMsg)
		if m.CandleAge > 2*time.Minute {
			hasStaleData = true
		}
	}

	if hasStaleData {
		logger.Warn("⚠️ [K线数据] 部分币种的K线数据超过2分钟未更新，可能K线流断开或重连中")
	}
}

// Stop 停止监控
func (r *RiskMonitor) Stop() {
	r.setReady(false, "风控已停止")
	if r.exchange != nil {
		r.exchange.StopKlineStream()
	}
}

func (r *RiskMonitor) setReady(ready bool, msg string) {
	r.mu.Lock()
	changed := r.ready != ready
	r.ready = ready
	if msg != "" && (!ready || !r.triggered) {
		r.lastMsg = msg
	}
	r.mu.Unlock()
	if changed {
		if ready {
			logger.Info("✅ [风控数据] %s", msg)
		} else {
			logger.Warn("⚠️ [风控数据] %s", msg)
		}
	}
}

func (r *RiskMonitor) staleLoop(ctx context.Context) {
	interval := parseRiskInterval(r.cfg.RiskControl.Interval)
	if interval <= 0 {
		interval = time.Minute
	}
	checkEvery := interval / 2
	if checkEvery < 5*time.Second {
		checkEvery = 5 * time.Second
	}
	if checkEvery > 30*time.Second {
		checkEvery = 30 * time.Second
	}
	staleAfter := 2 * interval
	if staleAfter < 30*time.Second {
		staleAfter = 30 * time.Second
	}
	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			var stale []string
			r.mu.RLock()
			for _, symbol := range r.cfg.RiskControl.MonitorSymbols {
				last := r.lastEventAt[symbol]
				if last.IsZero() || now.Sub(last) > staleAfter {
					stale = append(stale, symbol)
				}
			}
			r.mu.RUnlock()
			if len(stale) > 0 {
				r.setReady(false, fmt.Sprintf("K线数据陈旧: %s", strings.Join(stale, ",")))
			} else {
				r.setReady(true, "K线数据已恢复")
			}
		}
	}
}

func parseRiskInterval(value string) time.Duration {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return 0
	}
	n, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || n <= 0 {
		return 0
	}
	switch value[len(value)-1] {
	case 'm':
		return time.Duration(n) * time.Minute
	case 'h':
		return time.Duration(n) * time.Hour
	case 'd':
		return time.Duration(n) * 24 * time.Hour
	default:
		return 0
	}
}

func mergeRiskCandle(dst *[]*exchange.Candle, candle *exchange.Candle, limit int) {
	if candle == nil {
		return
	}
	copyOfCandle := *candle
	items := *dst
	replaced := false
	for i, existing := range items {
		if existing == nil || existing.Timestamp != candle.Timestamp {
			continue
		}
		// 已收到完结事件后，不允许乱序的未完结快照覆盖它。
		if existing.IsClosed && !candle.IsClosed {
			return
		}
		items[i] = &copyOfCandle
		replaced = true
		break
	}
	if !replaced {
		items = append(items, &copyOfCandle)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i] == nil {
			return true
		}
		if items[j] == nil {
			return false
		}
		return items[i].Timestamp < items[j].Timestamp
	})
	if limit > 0 && len(items) > limit {
		items = append([]*exchange.Candle(nil), items[len(items)-limit:]...)
	}
	*dst = items
}
