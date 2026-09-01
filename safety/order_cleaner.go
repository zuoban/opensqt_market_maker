package safety

import (
	"context"
	"opensqt/config"
	"opensqt/logger"
	"sort"
	"time"
)

// OrderCleanerSlotInfo 订单清理所需的槽位信息
type OrderCleanerSlotInfo struct {
	Price       float64
	OrderID     int64
	ClientOID   string
	OrderSide   string
	OrderStatus string
}

// IOrderExecutor 订单执行器接口（用于批量撤单）
type IOrderExecutor interface {
	BatchCancelOrders(orderIDs []int64) error
}

// IOrderCleanerPositionManager 订单清理所需的仓位管理器接口
type IOrderCleanerPositionManager interface {
	IterateSlots(fn func(price float64, slot SlotInfo) bool)
	// 仅当槽位仍属于被撤订单且状态未变化时，更新槽位状态。
	CompareAndSwapSlotOrderStatus(price float64, orderID int64, clientOID, expectedStatus, newStatus string) bool
}

// OrderCleaner 订单清理器
type OrderCleaner struct {
	cfg      *config.Config
	executor IOrderExecutor
	pm       IOrderCleanerPositionManager
}

// NewOrderCleaner 创建订单清理器
func NewOrderCleaner(cfg *config.Config, executor IOrderExecutor, pm IOrderCleanerPositionManager) *OrderCleaner {
	return &OrderCleaner{
		cfg:      cfg,
		executor: executor,
		pm:       pm,
	}
}

// Start 启动订单清理协程
func (oc *OrderCleaner) Start(ctx context.Context) {
	go func() {
		cleanupInterval := time.Duration(oc.cfg.Timing.OrderCleanupInterval) * time.Second
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logger.Info("⏹️ 订单清理协程已停止")
				return
			case <-ticker.C:
				oc.CleanupOrders()
			}
		}
	}()
	logger.Info("✅ 订单清理协程已启动")
}

// CleanupOrders 清理订单
func (oc *OrderCleaner) CleanupOrders() {
	// 订单状态常量
	const (
		OrderStatusPlaced          = "PLACED"
		OrderStatusConfirmed       = "CONFIRMED"
		OrderStatusCancelRequested = "CANCEL_REQUESTED"
	)

	// 统计当前订单数
	totalOrders := 0
	var buyOrders []OrderCleanerSlotInfo
	var sellOrders []OrderCleanerSlotInfo

	oc.pm.IterateSlots(func(price float64, slot SlotInfo) bool {
		orderID := slot.OrderID
		clientOID := slot.ClientOID
		orderSide := slot.OrderSide
		orderStatus := slot.OrderStatus

		// 🔥 修复：排除部分成交的订单（PARTIALLY_FILLED不能撤销，会造成资金悬空）
		if orderStatus == OrderStatusPlaced || orderStatus == OrderStatusConfirmed {
			totalOrders++
			target := OrderCleanerSlotInfo{
				Price:       price,
				OrderID:     orderID,
				ClientOID:   clientOID,
				OrderSide:   orderSide,
				OrderStatus: orderStatus,
			}
			if orderSide == "BUY" {
				buyOrders = append(buyOrders, target)
			} else if orderSide == "SELL" {
				sellOrders = append(sellOrders, target)
			}
		}
		return true
	})

	threshold := oc.cfg.Trading.OrderCleanupThreshold
	if threshold <= 0 {
		threshold = 100
	}

	batchSize := oc.cfg.Trading.CleanupBatchSize
	if batchSize <= 0 {
		batchSize = 10
	}

	// 🔥 核心策略：达到阈值才清理，不提前
	// 清理时优先清理数量多的一方（买单或卖单）
	if totalOrders >= threshold {
		canceledCount := 0

		logger.Info("🧹 [订单清理] 当前订单数: %d (买单: %d, 卖单: %d), 阈值: %d, 批次大小: %d",
			totalOrders, len(buyOrders), len(sellOrders), threshold, batchSize)

		// 🔥 新策略：优先清理数量多的一方
		// 如果买单多，就清理买单；如果卖单多，就清理卖单
		buyOrdersToCancel := 0
		sellOrdersToCancel := 0

		if len(buyOrders) > len(sellOrders) {
			// 买单多，清理买单
			buyOrdersToCancel = batchSize
			logger.Info("📊 [清理策略] 买单数量多于卖单，清理 %d 个买单", buyOrdersToCancel)
		} else if len(sellOrders) > len(buyOrders) {
			// 卖单多，清理卖单
			sellOrdersToCancel = batchSize
			logger.Info("📊 [清理策略] 卖单数量多于买单，清理 %d 个卖单", sellOrdersToCancel)
		} else {
			// 数量相等，平均清理
			buyOrdersToCancel = batchSize / 2
			sellOrdersToCancel = batchSize - buyOrdersToCancel
			logger.Info("📊 [清理策略] 买卖单数量相等，平均清理 (买单: %d, 卖单: %d)", buyOrdersToCancel, sellOrdersToCancel)
		}

		// 清理买单：清理价格最低的（离当前价格最远的）
		if len(buyOrders) > 0 && buyOrdersToCancel > 0 {
			// 按价格从低到高排序，清理最低的
			sort.Slice(buyOrders, func(i, j int) bool {
				return buyOrders[i].Price < buyOrders[j].Price
			})

			cancelCount := buyOrdersToCancel
			if cancelCount > len(buyOrders) {
				cancelCount = len(buyOrders)
			}

			if cancelCount > 0 {
				orderIDs := make([]int64, 0, cancelCount)
				for i := 0; i < cancelCount; i++ {
					orderIDs = append(orderIDs, buyOrders[i].OrderID)
				}

				logger.Info("🧹 [订单清理-买单] 买单数: %d, 取消价格最低的 %d 个 (%.2f ~ %.2f)",
					len(buyOrders), cancelCount, buyOrders[0].Price, buyOrders[cancelCount-1].Price)

				if err := oc.executor.BatchCancelOrders(orderIDs); err != nil {
					logger.Error("❌ [订单清理-买单] 批量撤单失败: %v", err)
				} else {
					// WebSocket 可能在 REST 撤单返回前已清槽或复用了同价槽位；
					// 只有订单身份和快照状态仍一致时才能写入 CANCEL_REQUESTED。
					for _, target := range buyOrders[:cancelCount] {
						oc.pm.CompareAndSwapSlotOrderStatus(
							target.Price, target.OrderID, target.ClientOID,
							target.OrderStatus, OrderStatusCancelRequested,
						)
					}
					canceledCount += cancelCount
				}
			}
		}

		// 清理卖单：清理价格最高的（离当前价格最远的）
		if len(sellOrders) > 0 && sellOrdersToCancel > 0 {
			// 按价格从高到低排序，清理最高的
			sort.Slice(sellOrders, func(i, j int) bool {
				return sellOrders[i].Price > sellOrders[j].Price
			})

			cancelCount := sellOrdersToCancel
			if cancelCount > len(sellOrders) {
				cancelCount = len(sellOrders)
			}

			if cancelCount > 0 {
				orderIDs := make([]int64, 0, cancelCount)
				for i := 0; i < cancelCount; i++ {
					orderIDs = append(orderIDs, sellOrders[i].OrderID)
				}

				logger.Info("🧹 [订单清理-卖单] 卖单数: %d, 取消价格最高的 %d 个 (%.2f ~ %.2f)",
					len(sellOrders), cancelCount, sellOrders[0].Price, sellOrders[cancelCount-1].Price)

				if err := oc.executor.BatchCancelOrders(orderIDs); err != nil {
					logger.Error("❌ [订单清理-卖单] 批量撤单失败: %v", err)
				} else {
					// 与买单相同，按订单身份和快照状态做条件更新。
					for _, target := range sellOrders[:cancelCount] {
						oc.pm.CompareAndSwapSlotOrderStatus(
							target.Price, target.OrderID, target.ClientOID,
							target.OrderStatus, OrderStatusCancelRequested,
						)
					}
					canceledCount += cancelCount
				}
			}
		}

		logger.Info("✅ [订单清理完成] 清理了 %d 个订单，剩余: %d", canceledCount, totalOrders-canceledCount)
	} else {
		logger.Debug("ℹ️ [订单清理] 总订单数: %d (阈值: %d，无需清理)", totalOrders, threshold)
	}
}
