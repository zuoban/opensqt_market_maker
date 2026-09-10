package position

import (
	"time"

	"opensqt/logger"
)

// These projections require slot.mu. Copy individual values, never InventorySlot
// itself: copying it would also copy a live RWMutex.
func readOrderSlotState(slot *InventorySlot) orderSlotState {
	return orderSlotState{
		Price: slot.Price, PositionStatus: slot.PositionStatus, PositionQty: slot.PositionQty, PositionCost: slot.PositionCost,
		OrderID: slot.OrderID, ClientOID: slot.ClientOID, OrderSide: slot.OrderSide, OrderStatus: slot.OrderStatus,
		OrderPrice: slot.OrderPrice, OrderQuantity: slot.OrderQuantity, OrderFilledQty: slot.OrderFilledQty,
		SlotStatus: slot.SlotStatus, PostOnlyFailCount: slot.PostOnlyFailCount,
		PlacementRetryNotBefore: slot.placementRetryNotBefore, PendingLookupMisses: slot.pendingLookupMisses, PendingLastLookup: slot.pendingLastLookup,
		OrderReportedPNL: slot.orderReportedPNL, OrderAccumulatedPNL: slot.orderAccumulatedPNL,
		OrderFilledQuote: slot.orderFilledQuote, OrderReleasedCost: slot.orderReleasedCost, OrderAccumulatedGridPNL: slot.orderAccumulatedGridPNL,
		GapStackCount: slot.gapStackCount, OppositeFillAt: slot.oppositeFillAt, OppositeSubmitSide: slot.oppositeSubmitSide,
	}
}

func applyOrderSlotState(slot *InventorySlot, next orderSlotState) {
	// Price is the map identity and is never written by an order update.
	slot.PositionStatus, slot.PositionQty, slot.PositionCost = next.PositionStatus, next.PositionQty, next.PositionCost
	slot.OrderID, slot.ClientOID, slot.OrderSide, slot.OrderStatus = next.OrderID, next.ClientOID, next.OrderSide, next.OrderStatus
	slot.OrderPrice, slot.OrderQuantity, slot.OrderFilledQty = next.OrderPrice, next.OrderQuantity, next.OrderFilledQty
	slot.SlotStatus, slot.PostOnlyFailCount = next.SlotStatus, next.PostOnlyFailCount
	slot.placementRetryNotBefore, slot.pendingLookupMisses, slot.pendingLastLookup = next.PlacementRetryNotBefore, next.PendingLookupMisses, next.PendingLastLookup
	slot.orderReportedPNL, slot.orderAccumulatedPNL = next.OrderReportedPNL, next.OrderAccumulatedPNL
	slot.orderFilledQuote, slot.orderReleasedCost, slot.orderAccumulatedGridPNL = next.OrderFilledQuote, next.OrderReleasedCost, next.OrderAccumulatedGridPNL
	slot.gapStackCount, slot.oppositeFillAt, slot.oppositeSubmitSide = next.GapStackCount, next.OppositeFillAt, next.OppositeSubmitSide
}

func (spm *SuperPositionManager) orderUpdateFacts(update OrderUpdate, side string, price float64, now time.Time) orderUpdateInput {
	in := orderUpdateInput{Update: update, Side: side, SlotPrice: price, Now: now, PriceDecimals: spm.priceDecimals}
	if spm.config != nil {
		in.Symbol, in.PriceInterval = spm.config.Trading.Symbol, spm.config.Trading.PriceInterval
	}
	return in
}

// publishOrderTransitionLocked is the only live-state publication boundary for
// the callback. It is still in-memory; no durable commit or rollback is claimed.
func (spm *SuperPositionManager) publishOrderTransitionLocked(slot *InventorySlot, in orderUpdateInput, next orderTransition) {
	applyOrderSlotState(slot, next.Slot)
	spm.publishExecutionEffect(next.Effect, in.SlotPrice, next.Disposition == transitionCorrection)
	if next.StoreTerminal {
		spm.storeTerminalOrderProgress(in.Update, next.Terminal)
	}
	if next.RecordFill {
		spm.appendFilledOrder(next.Fill, in.Now)
	}
	spm.logOrderTransition(in, next)
}

func (spm *SuperPositionManager) publishExecutionEffect(effect executionEffect, price float64, correction bool) {
	if effect.BuyQty == 0 && effect.SellQty == 0 && effect.RealizedPNL == 0 {
		return
	}
	// Different slots can publish concurrently. Add deltas to current totals
	// under one mutex instead of racing atomic Load+Store pairs across slots.
	spm.pnlMu.Lock()
	if effect.BuyQty != 0 {
		spm.totalBuyQty.Store(spm.totalBuyQty.Load().(float64) + effect.BuyQty)
	}
	if effect.SellQty != 0 {
		spm.totalSellQty.Store(spm.totalSellQty.Load().(float64) + effect.SellQty)
	}
	total := spm.realizedPNL.Load().(float64)
	if effect.RealizedPNL != 0 {
		total += effect.RealizedPNL
		spm.realizedPNL.Store(total)
	}
	spm.pnlMu.Unlock()
	if effect.RealizedPNL != 0 {
		label, source := "已实现盈亏", "成交价差"
		if correction {
			label = "终态盈亏修正"
		}
		if effect.PNLFromExchange {
			source = "成交推送"
		}
		logger.Info("💵 [%s] 价格: %s, 本笔: %.6f, 累计: %.6f (%s)", label, formatPrice(price, spm.priceDecimals), effect.RealizedPNL, total, source)
	}
}

// Compatibility boundary for the accepted-REST-order cancellation path. The
// arithmetic is identical to callback execution, but binding policy stays there.
func (spm *SuperPositionManager) applyOrderExecutionDelta(slot *InventorySlot, update OrderUpdate, side string, slotPrice float64) (float64, bool) {
	next, effect, deltaQty, issue := reduceOrderExecution(readOrderSlotState(slot), update, side, slotPrice)
	if issue != executionOK {
		logger.Warn("⚠️ [忽略成交进度] 价格: %s, 状态: %s, 原因: %s", formatPrice(slotPrice, spm.priceDecimals), update.Status, issue)
		return 0, true
	}
	applyOrderSlotState(slot, next)
	spm.publishExecutionEffect(effect, slotPrice, false)
	return deltaQty, false
}

func (spm *SuperPositionManager) logOrderTransition(in orderUpdateInput, next orderTransition) {
	u := in.Update
	if next.Issue != executionOK {
		if next.Issue == executionDuplicate {
			logger.Debug("⏭️ [重复终态被忽略] ID=%d, ClientOID=%s, Status=%s", u.OrderID, u.ClientOrderID, u.Status)
		} else {
			logger.Warn("⚠️ [忽略成交进度] 价格: %s, 状态: %s, 原因: %s", formatPrice(in.SlotPrice, spm.priceDecimals), u.Status, next.Issue)
		}
	}
	switch next.Disposition {
	case transitionFilledReplay:
		logger.Debug("⏭️ [已完成订单更新被忽略] ID=%d, ClientOID=%s, Status=%s", u.OrderID, u.ClientOrderID, u.Status)
	case transitionTerminalReplay:
		logger.Debug("⏭️ [忽略终态后的乱序推送] ID=%d, ClientOID=%s, Status=%s", u.OrderID, u.ClientOrderID, u.Status)
	case transitionWrongIdentity:
		logger.Info("⚠️ [订单更新被忽略] 槽位 %.2f: ClientOID不匹配 (槽位: %s, 推送: %s, OrderID: %d)", in.SlotPrice, next.Slot.ClientOID, u.ClientOrderID, u.OrderID)
	case transitionApplied:
		if next.RecordFill {
			side := "买"
			if in.Side == "SELL" {
				side = "卖"
			}
			logger.Info("✅ [%s单成交] 价格: %s, 持仓: %.4f, 槽位状态: %s, 订单状态: %s, SlotStatus: FREE",
				side, formatPrice(in.SlotPrice, spm.priceDecimals), next.Slot.PositionQty, next.Slot.PositionStatus, next.Slot.OrderStatus)
		} else if next.StoreTerminal {
			logger.Info("⚠️ [订单%s] 价格: %s, 方向: %s, 已成交: %.4f, 剩余持仓: %.4f", u.Status, formatPrice(in.SlotPrice, spm.priceDecimals), in.Side, next.Terminal.ExecutedQty, next.Slot.PositionQty)
		}
	}
}
