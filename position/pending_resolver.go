package position

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"opensqt/exchange/exchangeerr"
	"opensqt/logger"
)

// clientOrderLookup 是结果不确定订单的可选恢复能力。只有明确返回
// exchangeerr.ErrOrderNotFound 的查询才会计入“不存在”证据。
type clientOrderLookup interface {
	GetOrderByClientID(ctx context.Context, symbol, clientOrderID string) (interface{}, error)
}

type pendingOrderTarget struct {
	price         float64
	clientOrderID string
	side          string
	orderPrice    float64
	quantity      float64
	executedQty   float64
	createdAt     time.Time
}

func (spm *SuperPositionManager) collectPendingOrderTargets() ([]pendingOrderTarget, int) {
	targets := make([]pendingOrderTarget, 0)
	remaining := 0
	spm.forEachSlot(func(price float64, slot *InventorySlot) bool {
		slot.mu.RLock()
		if slot.SlotStatus == SlotStatusPending {
			remaining++
			if slot.OrderID == 0 && slot.ClientOID != "" {
				targets = append(targets, pendingOrderTarget{
					price:         price,
					clientOrderID: slot.ClientOID,
					side:          slot.OrderSide,
					orderPrice:    slot.OrderPrice,
					quantity:      slot.OrderQuantity,
					executedQty:   slot.OrderFilledQty,
					createdAt:     slot.OrderCreatedAt,
				})
			}
		}
		slot.mu.RUnlock()
		return true
	})
	return targets, remaining
}

func (spm *SuperPositionManager) pendingTargetMatchesLocked(slot *InventorySlot, target pendingOrderTarget) bool {
	return slot != nil && slot.SlotStatus == SlotStatusPending && slot.OrderID == 0 &&
		spm.canonicalClientOrderID(slot.ClientOID) == spm.canonicalClientOrderID(target.clientOrderID) &&
		slot.OrderSide == target.side && sameOrderQuantity(slot.OrderQuantity, target.quantity) &&
		slot.OrderCreatedAt.Equal(target.createdAt)
}

// ResolvePendingOrders 尝试收敛无 OrderID 的 PENDING reservation。
//
// 安全规则：reservation 至少存在 5 秒，且同一身份收到 3 次、间隔至少
// 500ms 的明确“不存在”后才释放。超时、传输错误、无效响应和不支持查询
// 都不会释放槽位。单次调用最多等待约 2 秒，避免拖住对账主循环。
func (spm *SuperPositionManager) ResolvePendingOrders(ctx context.Context) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("恢复待确认订单的上下文不能为空")
	}
	lookup, supported := spm.exchange.(clientOrderLookup)
	if !supported {
		_, remaining := spm.collectPendingOrderTargets()
		return remaining, nil
	}
	started := time.Now()
	for {
		targets, remaining := spm.collectPendingOrderTargets()
		if remaining == 0 {
			return 0, nil
		}

		lookedUp := false
		var lookupErrs []error
		for _, target := range targets {
			didLookup, err := spm.resolvePendingOrderTarget(ctx, lookup, target)
			lookedUp = lookedUp || didLookup
			if err != nil {
				lookupErrs = append(lookupErrs, err)
			}
		}
		_, remaining = spm.collectPendingOrderTargets()
		if remaining == 0 {
			return 0, errors.Join(lookupErrs...)
		}
		if len(lookupErrs) > 0 {
			return remaining, errors.Join(lookupErrs...)
		}
		if !lookedUp || time.Since(started) >= pendingLookupPassBudget {
			return remaining, nil
		}

		wait, eligible := spm.nextPendingLookupWait(time.Now())
		if !eligible || wait > pendingLookupInterval || time.Since(started)+wait > pendingLookupPassBudget {
			return remaining, nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return remaining, ctx.Err()
		case <-timer.C:
		}
	}
}

func (spm *SuperPositionManager) nextPendingLookupWait(now time.Time) (time.Duration, bool) {
	var shortest time.Duration
	eligible := false
	spm.forEachSlot(func(_ float64, slot *InventorySlot) bool {
		slot.mu.RLock()
		if slot.SlotStatus != SlotStatusPending || slot.OrderID != 0 || slot.ClientOID == "" ||
			slot.OrderCreatedAt.IsZero() || now.Sub(slot.OrderCreatedAt) < pendingLookupMinAge {
			slot.mu.RUnlock()
			return true
		}
		wait := time.Duration(0)
		if !slot.pendingLastLookup.IsZero() {
			wait = pendingLookupInterval - now.Sub(slot.pendingLastLookup)
			if wait < 0 {
				wait = 0
			}
		}
		slot.mu.RUnlock()
		if !eligible || wait < shortest {
			shortest = wait
		}
		eligible = true
		return true
	})
	return shortest, eligible
}

func (spm *SuperPositionManager) resolvePendingOrderTarget(
	ctx context.Context,
	lookup clientOrderLookup,
	target pendingOrderTarget,
) (bool, error) {
	value, ok := spm.slots.Load(target.price)
	if !ok {
		return false, nil
	}
	slot, _ := value.(*InventorySlot)
	if slot == nil {
		return false, nil
	}

	now := time.Now()
	slot.mu.Lock()
	if !spm.pendingTargetMatchesLocked(slot, target) || target.createdAt.IsZero() ||
		now.Sub(target.createdAt) < pendingLookupMinAge ||
		(!slot.pendingLastLookup.IsZero() && now.Sub(slot.pendingLastLookup) < pendingLookupInterval) {
		slot.mu.Unlock()
		return false, nil
	}
	// 先记录查询时间，防止撤单恢复和定期对账并发查询同一 identity。
	slot.pendingLastLookup = now
	slot.mu.Unlock()

	raw, err := lookup.GetOrderByClientID(ctx, spm.config.Trading.Symbol, target.clientOrderID)
	if err != nil {
		if !errors.Is(err, exchangeerr.ErrOrderNotFound) {
			spm.resetPendingLookupMisses(target)
			return true, fmt.Errorf("ClientOID=%s 查询失败: %w", target.clientOrderID, err)
		}
		return true, spm.recordPendingOrderAbsent(target)
	}

	update, quantity, err := spm.extractCanceledBuyUpdate(raw, buyCancelTarget{})
	if err != nil {
		spm.resetPendingLookupEvidence(target)
		return true, fmt.Errorf("ClientOID=%s 查询响应无效: %w", target.clientOrderID, err)
	}
	if err := spm.validatePendingOrderUpdate(target, update, quantity); err != nil {
		spm.resetPendingLookupEvidence(target)
		return true, fmt.Errorf("ClientOID=%s 查询结果不匹配: %w", target.clientOrderID, err)
	}
	spm.resetPendingLookupEvidence(target)
	update.ClientOrderID = target.clientOrderID
	spm.OnOrderUpdate(update)
	logger.Info("✅ [待确认订单恢复] ClientOID=%s 已按交易所状态 %s 收敛", target.clientOrderID, update.Status)
	return true, nil
}

func (spm *SuperPositionManager) resetPendingLookupMisses(target pendingOrderTarget) {
	value, ok := spm.slots.Load(target.price)
	if !ok {
		return
	}
	slot, _ := value.(*InventorySlot)
	if slot == nil {
		return
	}
	slot.mu.Lock()
	if spm.pendingTargetMatchesLocked(slot, target) {
		slot.pendingLookupMisses = 0
	}
	slot.mu.Unlock()
}

func (spm *SuperPositionManager) recordPendingOrderAbsent(target pendingOrderTarget) error {
	value, ok := spm.slots.Load(target.price)
	if !ok {
		return nil
	}
	slot, _ := value.(*InventorySlot)
	if slot == nil {
		return nil
	}
	slot.mu.Lock()
	if !spm.pendingTargetMatchesLocked(slot, target) {
		slot.mu.Unlock()
		return nil
	}
	slot.pendingLookupMisses++
	misses := slot.pendingLookupMisses
	if misses < pendingLookupMissLimit {
		slot.mu.Unlock()
		logger.Debug("⏳ [待确认订单恢复] ClientOID=%s 明确不存在 (%d/%d)",
			target.clientOrderID, misses, pendingLookupMissLimit)
		return nil
	}
	spm.clearReservationLocked(slot)
	retryAt := time.Now().Add(pendingLateUpdateGrace)
	slot.placementRetryNotBefore = retryAt
	slot.mu.Unlock()
	spm.rememberResolvedAbsentOrder(target.clientOrderID, retryAt)
	logger.Warn("🔓 [待确认订单恢复] ClientOID=%s 连续 %d 次明确不存在，释放槽位并保留迟到事件缓冲",
		target.clientOrderID, pendingLookupMissLimit)
	spm.notifyAdjustment(time.Until(retryAt))
	return nil
}

func (spm *SuperPositionManager) rememberResolvedAbsentOrder(clientOrderID string, until time.Time) {
	clientOrderID = spm.canonicalClientOrderID(clientOrderID)
	if clientOrderID == "" || until.IsZero() {
		return
	}
	now := time.Now()
	spm.resolvedAbsentOrdersMu.Lock()
	for key, deadline := range spm.resolvedAbsentOrders {
		if now.Sub(deadline) > time.Minute {
			delete(spm.resolvedAbsentOrders, key)
		}
	}
	spm.resolvedAbsentOrders[clientOrderID] = until
	spm.resolvedAbsentOrdersMu.Unlock()
}

func (spm *SuperPositionManager) forgetResolvedAbsentOrder(clientOrderID string) {
	clientOrderID = spm.canonicalClientOrderID(clientOrderID)
	if clientOrderID == "" {
		return
	}
	spm.resolvedAbsentOrdersMu.Lock()
	delete(spm.resolvedAbsentOrders, clientOrderID)
	spm.resolvedAbsentOrdersMu.Unlock()
}

func (spm *SuperPositionManager) resolvedAbsentOrderConverged(clientOrderID string, now time.Time) (bool, bool) {
	clientOrderID = spm.canonicalClientOrderID(clientOrderID)
	if clientOrderID == "" {
		return false, false
	}
	spm.resolvedAbsentOrdersMu.Lock()
	until, exists := spm.resolvedAbsentOrders[clientOrderID]
	if exists && !now.Before(until) {
		delete(spm.resolvedAbsentOrders, clientOrderID)
	}
	spm.resolvedAbsentOrdersMu.Unlock()
	return exists && !now.Before(until), exists
}

func (spm *SuperPositionManager) resetPendingLookupEvidence(target pendingOrderTarget) {
	value, ok := spm.slots.Load(target.price)
	if !ok {
		return
	}
	slot, _ := value.(*InventorySlot)
	if slot == nil {
		return
	}
	slot.mu.Lock()
	if spm.pendingTargetMatchesLocked(slot, target) {
		slot.pendingLookupMisses = 0
		slot.pendingLastLookup = time.Time{}
	}
	slot.mu.Unlock()
}

func (spm *SuperPositionManager) validatePendingOrderUpdate(target pendingOrderTarget, update OrderUpdate, quantity float64) error {
	if update.OrderID <= 0 {
		return fmt.Errorf("OrderID=%d 非法", update.OrderID)
	}
	if spm.canonicalClientOrderID(update.ClientOrderID) != spm.canonicalClientOrderID(target.clientOrderID) {
		return fmt.Errorf("ClientOrderID=%q，本地=%q", update.ClientOrderID, target.clientOrderID)
	}
	if strings.ToUpper(update.Side) != strings.ToUpper(target.side) {
		return fmt.Errorf("Side=%s，本地=%s", update.Side, target.side)
	}
	if update.Symbol != "" && update.Symbol != spm.config.Trading.Symbol {
		return fmt.Errorf("Symbol=%s，期望 %s", update.Symbol, spm.config.Trading.Symbol)
	}
	if quantity <= 0 || math.IsNaN(quantity) || math.IsInf(quantity, 0) {
		return fmt.Errorf("Quantity=%v 非法", quantity)
	}
	tolerance := math.Max(fillQtyTolerance, math.Max(math.Abs(quantity), math.Abs(target.quantity))*1e-8)
	if target.quantity > 0 && math.Abs(quantity-target.quantity) > tolerance {
		return fmt.Errorf("Quantity=%.12f，本地=%.12f", quantity, target.quantity)
	}
	if update.ExecutedQty < 0 || math.IsNaN(update.ExecutedQty) || math.IsInf(update.ExecutedQty, 0) ||
		update.ExecutedQty+fillQtyTolerance < target.executedQty || update.ExecutedQty > quantity+tolerance {
		return fmt.Errorf("ExecutedQty=%.12f 超出 [%.12f, %.12f]", update.ExecutedQty, target.executedQty, quantity)
	}
	if target.orderPrice > 0 && update.Price > 0 {
		priceTolerance := math.Max(fillQtyTolerance, math.Max(math.Abs(target.orderPrice), math.Abs(update.Price))*1e-8)
		if math.Abs(target.orderPrice-update.Price) > priceTolerance {
			return fmt.Errorf("Price=%.12f，本地=%.12f", update.Price, target.orderPrice)
		}
	}
	switch update.Status {
	case "NEW":
		if update.ExecutedQty > tolerance {
			return fmt.Errorf("Status=NEW 但 ExecutedQty=%.12f", update.ExecutedQty)
		}
	case OrderStatusPartiallyFilled:
		if update.ExecutedQty <= tolerance || update.ExecutedQty >= quantity-tolerance {
			return fmt.Errorf("PARTIALLY_FILLED 与 %.12f/%.12f 不一致", update.ExecutedQty, quantity)
		}
	case OrderStatusFilled:
		if math.Abs(update.ExecutedQty-quantity) > tolerance {
			return fmt.Errorf("FILLED 与 %.12f/%.12f 不一致", update.ExecutedQty, quantity)
		}
	case OrderStatusCanceled, "EXPIRED", "REJECTED":
	default:
		return fmt.Errorf("Status=%s 无法收敛", update.Status)
	}
	return nil
}
