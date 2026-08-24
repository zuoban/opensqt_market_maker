package order

import (
	"context"
	"fmt"
	"opensqt/exchange"
	"opensqt/logger"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

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
}

// Order 订单信息
type Order struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          string
	Price         float64
	Quantity      float64
	Status        string
	CreatedAt     time.Time
}

// ExchangeOrderExecutor 基于 exchange.IExchange 的订单执行器
type ExchangeOrderExecutor struct {
	exchange    exchange.IExchange
	symbol      string
	rateLimiter *rate.Limiter

	// 时间配置
	rateLimitRetryDelay time.Duration
	orderRetryDelay     time.Duration
}

// NewExchangeOrderExecutor 创建基于交易所接口的订单执行器
func NewExchangeOrderExecutor(ex exchange.IExchange, symbol string, rateLimitRetryDelay, orderRetryDelay int) *ExchangeOrderExecutor {
	return &ExchangeOrderExecutor{
		exchange:            ex,
		symbol:              symbol,
		rateLimiter:         rate.NewLimiter(rate.Limit(25), 30), // 25单/秒，突发30
		rateLimitRetryDelay: time.Duration(rateLimitRetryDelay) * time.Second,
		orderRetryDelay:     time.Duration(orderRetryDelay) * time.Millisecond,
	}
}

// isPostOnlyError 检查是否为PostOnly错误
func isPostOnlyError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Binance: code=-5022, Bitget: Post Only order will be rejected, Gate.io: ORDER_POC_IMMEDIATE
	return strings.Contains(errStr, "-5022") ||
		strings.Contains(errStr, "Post Only") ||
		strings.Contains(errStr, "post_only") ||
		strings.Contains(errStr, "would immediately match") ||
		strings.Contains(errStr, "ORDER_POC_IMMEDIATE")
}

// PlaceOrder 下单（严格 PostOnly，带重试）
func (oe *ExchangeOrderExecutor) PlaceOrder(req *OrderRequest) (*Order, error) {
	// 限流
	if err := oe.rateLimiter.Wait(context.Background()); err != nil {
		return nil, fmt.Errorf("速率限制等待失败: %v", err)
	}

	const maxRetries = 5 // 首次尝试失败后最多再重试5次
	var lastErr error
	postOnlyFailCount := 0

	for i := 0; i <= maxRetries; i++ {
		// 转换为通用订单请求
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
			ClientOrderID: req.ClientOrderID, // 传递自定义订单ID
		}

		// 调用交易所接口
		exchangeOrder, err := oe.exchange.PlaceOrder(context.Background(), exchangeReq)
		if err == nil {
			// 转换回 Order 格式
			order := &Order{
				OrderID:       exchangeOrder.OrderID,
				ClientOrderID: exchangeOrder.ClientOrderID,
				Symbol:        req.Symbol,
				Side:          req.Side,
				Price:         req.Price,
				Quantity:      req.Quantity,
				Status:        string(exchangeOrder.Status),
				CreatedAt:     time.Now(),
			}

			logger.Info("✅ [%s] 下单成功(PostOnly): %s %.*f 数量: %.4f 订单ID: %d",
				oe.exchange.GetName(), req.Side, req.PriceDecimals, req.Price, req.Quantity, exchangeOrder.OrderID)
			return order, nil
		}

		lastErr = err

		// 判断错误类型
		errStr := err.Error()
		if strings.Contains(errStr, "-4061") {
			// 持仓模式不匹配：双向持仓 vs 单向持仓
			logger.Fatalf("❌ 下单失败，请在交易所将双向持仓改为单向持仓。错误码: -4061")
			return nil, fmt.Errorf("持仓模式不匹配: %w", err)
		} else if strings.Contains(errStr, "-1003") || strings.Contains(errStr, "rate limit") {
			// 速率限制，等待后重试
			logger.Warn("⚠️ 触发速率限制，等待后重试...")
			time.Sleep(oe.rateLimitRetryDelay)
			continue
		} else if isPostOnlyError(err) {
			// PostOnly 被拒表示该价格会立即成交。严格 Maker 模式下只重试，绝不降级为普通单。
			postOnlyFailCount++
			logger.Warn("⚠️ [%s] PostOnly被拒(%d/%d): %s %.2f，严格Maker模式不会降级",
				oe.exchange.GetName(), postOnlyFailCount, maxRetries+1, req.Side, req.Price)

			if i < maxRetries {
				time.Sleep(oe.orderRetryDelay)
			}
			continue
		} else if strings.Contains(errStr, "-4061") {
			// 持仓模式不匹配（已在前面处理，这里保留以防万一）
			return nil, err
		} else if strings.Contains(errStr, "-2019") || strings.Contains(errStr, "保证金不足") || strings.Contains(errStr, "insufficient") {
			// 保证金不足，不重试
			return nil, err
		} else if strings.Contains(errStr, "-1021") {
			// 时间戳不同步，不重试
			return nil, err
		}

		// 其他错误，短暂等待后重试
		if i < maxRetries {
			time.Sleep(oe.orderRetryDelay)
		}
	}

	return nil, fmt.Errorf("下单失败（重试%d次）: %w", maxRetries, lastErr)
}

// BatchPlaceOrders 批量下单
// 返回：成功下单的订单列表，以及是否出现保证金不足错误
func (oe *ExchangeOrderExecutor) BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool) {
	placedOrders := make([]*Order, 0, len(orders))
	hasMarginError := false

	for _, orderReq := range orders {
		order, err := oe.PlaceOrder(orderReq)
		if err != nil {
			logger.Warn("⚠️ [%s] 下单失败 %.2f %s: %v",
				oe.exchange.GetName(), orderReq.Price, orderReq.Side, err)

			// 检查是否是保证金不足错误
			errStr := err.Error()
			if strings.Contains(errStr, "保证金不足") || strings.Contains(errStr, "-2019") || strings.Contains(errStr, "insufficient") {
				hasMarginError = true
				logger.Error("❌ [保证金不足] 订单 %.2f %s 因保证金不足失败", orderReq.Price, orderReq.Side)
			}
			continue
		}
		placedOrders = append(placedOrders, order)
	}

	return placedOrders, hasMarginError
}

// CancelOrder 取消订单
func (oe *ExchangeOrderExecutor) CancelOrder(orderID int64) error {
	// 限流
	if err := oe.rateLimiter.Wait(context.Background()); err != nil {
		return fmt.Errorf("速率限制等待失败: %v", err)
	}

	err := oe.exchange.CancelOrder(context.Background(), oe.symbol, orderID)
	if err != nil {
		// 如果是"Unknown order"错误，说明订单已经不存在（可能已成交或已取消），不算错误
		errStr := err.Error()
		if strings.Contains(errStr, "-2011") || strings.Contains(errStr, "Unknown order") || strings.Contains(errStr, "does not exist") {
			logger.Info("ℹ️ [%s] 订单 %d 已不存在（可能已成交或已取消），跳过取消", oe.exchange.GetName(), orderID)
			return nil
		}
		return fmt.Errorf("取消订单失败: %v", err)
	}

	logger.Info("✅ [%s] 取消订单成功: %d", oe.exchange.GetName(), orderID)
	return nil
}

// BatchCancelOrders 批量撤单
func (oe *ExchangeOrderExecutor) BatchCancelOrders(orderIDs []int64) error {
	if len(orderIDs) == 0 {
		return nil
	}

	// 使用交易所的批量撤单接口
	err := oe.exchange.BatchCancelOrders(context.Background(), oe.symbol, orderIDs)
	if err != nil {
		logger.Warn("⚠️ [%s] 批量撤单失败: %v，尝试单个撤单", oe.exchange.GetName(), err)
		// 如果批量撤单失败，尝试单个撤单
		for _, orderID := range orderIDs {
			if err := oe.CancelOrder(orderID); err != nil {
				logger.Warn("⚠️ [%s] 取消订单 %d 失败: %v", oe.exchange.GetName(), orderID, err)
			}
		}
	}

	return nil
}

// CheckOrderStatus 检查订单状态
func (oe *ExchangeOrderExecutor) CheckOrderStatus(orderID int64) (string, float64, error) {
	order, err := oe.exchange.GetOrder(context.Background(), oe.symbol, orderID)
	if err != nil {
		return "", 0, err
	}

	return string(order.Status), order.ExecutedQty, nil
}

// GetOpenOrders 获取未完成订单
func (oe *ExchangeOrderExecutor) GetOpenOrders() ([]interface{}, error) {
	orders, err := oe.exchange.GetOpenOrders(context.Background(), oe.symbol)
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
