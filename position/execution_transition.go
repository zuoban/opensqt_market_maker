package position

import "math"

// Pure execution arithmetic shared by order callbacks and accepted REST results.
// Invalid values return the input unchanged with no accounting effects.
func reduceOrderExecution(before orderSlotState, update OrderUpdate, side string, slotPrice float64) (orderSlotState, executionEffect, float64, executionIssue) {
	deltaQty, deltaQuote, cumulativeQuote, issue := executionAmounts(update, before.OrderFilledQty, before.OrderFilledQuote, before.OrderPrice, slotPrice)
	if issue != executionOK {
		return before, executionEffect{}, 0, issue
	}
	next := before
	var effect executionEffect
	if deltaQty > 0 {
		next.OrderFilledQty, next.OrderFilledQuote = update.ExecutedQty, cumulativeQuote
	}
	if side == "BUY" {
		if deltaQty > 0 {
			next.PositionQty += deltaQty
			next.PositionCost += deltaQuote
			effect.BuyQty = deltaQty
		}
		return next, effect, deltaQty, executionOK
	}
	if deltaQty > 0 {
		remainingCost, releasedCost := releasePositionCost(next.Price, next.PositionQty, next.PositionCost, deltaQty)
		next.PositionCost = remainingCost
		next.OrderReleasedCost += releasedCost
		next.OrderAccumulatedGridPNL += deltaQuote - releasedCost
		next.PositionQty -= deltaQty
		if next.PositionQty < 0 {
			next.PositionQty = 0
		}
		if next.PositionQty <= fillQtyTolerance {
			next.PositionCost = 0
		}
		effect.SellQty = deltaQty
	}
	hasCumulativePNL := !update.RealizedPNLIncremental && update.RealizedPNL != 0
	if update.RealizedPNLIncremental {
		if deltaQty <= 0 {
			return next, effect, deltaQty, executionOK
		}
		effect.RealizedPNL = update.RealizedPNL
		effect.PNLFromExchange = true
	} else if hasCumulativePNL {
		effect.RealizedPNL = update.RealizedPNL - next.OrderAccumulatedPNL
		next.OrderReportedPNL = update.RealizedPNL
		effect.PNLFromExchange = true
	}
	if !update.RealizedPNLIncremental && deltaQty > 0 && !hasCumulativePNL {
		effect.RealizedPNL = fallbackRealizedPNL(update, deltaQty, next.Price)
	}
	next.OrderAccumulatedPNL += effect.RealizedPNL
	return next, effect, deltaQty, executionOK
}

// Old terminal corrections may update inventory and the old order's progress,
// but must leave every field describing the currently bound order untouched.
func reduceTerminalCorrection(before orderSlotState, update OrderUpdate, side string, slotPrice float64, progress terminalOrderProgress) (orderSlotState, terminalOrderProgress, executionEffect, executionIssue) {
	deltaQty, deltaQuote, cumulativeQuote, issue := executionAmounts(update, progress.ExecutedQty, progress.ExecutedQuote, 0, slotPrice)
	if issue != executionOK {
		return before, progress, executionEffect{}, issue
	}
	hasCumulativePNL := side == "SELL" && !update.RealizedPNLIncremental && update.RealizedPNL != 0
	pnlChanged := hasCumulativePNL && math.Abs(update.RealizedPNL-progress.ReportedPNL) > fillQtyTolerance
	acceptPNLCorrection := pnlChanged && deltaQty == 0 && progress.UpdateTime > 0 && update.UpdateTime > progress.UpdateTime
	if deltaQty == 0 && !acceptPNLCorrection {
		return before, progress, executionEffect{}, executionDuplicate
	}
	next := before
	var effect executionEffect
	if side == "BUY" {
		if deltaQty > 0 {
			next.PositionQty += deltaQty
			next.PositionCost += deltaQuote
			next.PositionStatus = PositionStatusFilled
			effect.BuyQty = deltaQty
		}
	} else {
		if deltaQty > 0 {
			next.PositionCost, _ = releasePositionCost(next.Price, next.PositionQty, next.PositionCost, deltaQty)
			next.PositionQty -= deltaQty
			if next.PositionQty < 0 {
				next.PositionQty = 0
			}
			if next.PositionQty <= fillQtyTolerance {
				next.PositionCost = 0
			}
			effect.SellQty = deltaQty
		}
		if update.RealizedPNLIncremental {
			effect.RealizedPNL = update.RealizedPNL
			effect.PNLFromExchange = true
		} else if update.RealizedPNL != 0 {
			effect.RealizedPNL = update.RealizedPNL - progress.AccountedPNL
			progress.ReportedPNL = update.RealizedPNL
			effect.PNLFromExchange = true
		}
		if !update.RealizedPNLIncremental && deltaQty > 0 && !hasCumulativePNL {
			effect.RealizedPNL = fallbackRealizedPNL(update, deltaQty, slotPrice)
		}
		progress.AccountedPNL += effect.RealizedPNL
		if hasCumulativePNL {
			progress.AccountedPNL = update.RealizedPNL
		}
		if next.PositionQty < 0.000001 {
			next.PositionStatus = PositionStatusEmpty
		} else {
			next.PositionStatus = PositionStatusFilled
		}
	}
	if update.ExecutedQty > progress.ExecutedQty {
		progress.ExecutedQty = update.ExecutedQty
		progress.ExecutedQuote = cumulativeQuote
	}
	if update.UpdateTime > progress.UpdateTime {
		progress.UpdateTime = update.UpdateTime
	}
	return next, progress, effect, executionOK
}

func executionAmounts(update OrderUpdate, previousQty, previousQuote, orderPrice, slotPrice float64) (deltaQty, deltaQuote, nextQuote float64, issue executionIssue) {
	if math.IsNaN(update.ExecutedQty) || math.IsInf(update.ExecutedQty, 0) || update.ExecutedQty < 0 {
		return 0, 0, previousQuote, executionInvalidQty
	}
	if update.ExecutedQty+fillQtyTolerance < previousQty {
		return 0, 0, previousQuote, executionStaleQty
	}
	deltaQty = update.ExecutedQty - previousQty
	if deltaQty < fillQtyTolerance {
		deltaQty = 0
	}
	nextQuote = previousQuote
	if deltaQty > 0 {
		cumulativeQuote, ok := cumulativeExecutionQuote(update, orderPrice, slotPrice)
		if !ok || cumulativeQuote+fillQtyTolerance < previousQuote {
			return 0, 0, previousQuote, executionInvalidQuote
		}
		deltaQuote = cumulativeQuote - previousQuote
		if deltaQuote <= 0 || math.IsNaN(deltaQuote) || math.IsInf(deltaQuote, 0) {
			return 0, 0, previousQuote, executionInvalidDelta
		}
		nextQuote = cumulativeQuote
	}
	return deltaQty, deltaQuote, nextQuote, executionOK
}

// cumulativeExecutionQuote 把交易所的累计成交数量与累计成交均价转换成
// 累计成交金额。orderPrice 和 slotPrice 只用于兼容缺少均价的本地测试/回读。
func cumulativeExecutionQuote(update OrderUpdate, orderPrice, slotPrice float64) (float64, bool) {
	if update.ExecutedQty <= 0 {
		return 0, true
	}
	price := update.AvgPrice
	if price <= 0 {
		price = update.Price
	}
	if price <= 0 {
		price = orderPrice
	}
	if price <= 0 {
		price = slotPrice
	}
	quote := update.ExecutedQty * price
	if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) ||
		quote <= 0 || math.IsNaN(quote) || math.IsInf(quote, 0) {
		return 0, false
	}
	return quote, true
}

// Missing historical cost falls back to logical slot price, preserving the
// existing upgrade/test compatibility rule and not changing grid PNL semantics.
func releasePositionCost(price, quantity, cost, releasedQty float64) (remainingCost, releasedCost float64) {
	if releasedQty <= 0 || quantity <= 0 {
		return cost, 0
	}
	if cost <= 0 || math.IsNaN(cost) || math.IsInf(cost, 0) {
		cost = quantity * price
	}
	releasedCost = math.Min(releasedQty, quantity) * (cost / quantity)
	remainingCost = cost - releasedCost
	if remainingCost < fillQtyTolerance {
		remainingCost = 0
	}
	return remainingCost, releasedCost
}

func fallbackRealizedPNL(update OrderUpdate, quantity, slotPrice float64) float64 {
	price := update.AvgPrice
	if price <= 0 {
		price = update.Price
	}
	if price > 0 && slotPrice > 0 {
		return quantity * (price - slotPrice)
	}
	return 0
}
