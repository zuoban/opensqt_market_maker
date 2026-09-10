package position

import (
	"math"
	"time"
)

// orderSlotState is a value-only projection of fields read/written by order
// updates. It contains no locks, manager pointers, caches or external callbacks.
// It is not a complete persistence schema for InventorySlot.
type orderSlotState struct {
	Price                   float64
	PositionStatus          string
	PositionQty             float64
	PositionCost            float64
	OrderID                 int64
	ClientOID               string
	OrderSide               string
	OrderStatus             string
	OrderPrice              float64
	OrderQuantity           float64
	OrderFilledQty          float64
	SlotStatus              string
	PostOnlyFailCount       int
	PlacementRetryNotBefore time.Time
	PendingLookupMisses     int
	PendingLastLookup       time.Time
	OrderReportedPNL        float64
	OrderAccumulatedPNL     float64
	OrderFilledQuote        float64
	OrderReleasedCost       float64
	OrderAccumulatedGridPNL float64
	GapStackCount           int
	OppositeFillAt          time.Time
	OppositeSubmitSide      string
}

// All environmental facts, including clocks and canonical identity, are supplied
// by the locked caller. Computing a transition cannot read or change live state.
type orderUpdateInput struct {
	Update                 OrderUpdate
	Side                   string
	SlotPrice              float64
	CanonicalSlotClientOID string
	AlreadyFilled          bool
	TerminalSeen           bool
	Terminal               terminalOrderProgress
	Now                    time.Time
	ReceivedAt             time.Time
	MeasureFillLatency     bool
	Symbol                 string
	PriceInterval          float64
	PriceDecimals          int
}

type executionEffect struct {
	BuyQty          float64
	SellQty         float64
	RealizedPNL     float64
	PNLFromExchange bool
}

type executionIssue string

const (
	executionOK           executionIssue = ""
	executionInvalidQty   executionIssue = "非法累计成交量"
	executionStaleQty     executionIssue = "累计成交量回退"
	executionInvalidQuote executionIssue = "非法累计成交金额"
	executionInvalidDelta executionIssue = "非法成交金额增量"
	executionDuplicate    executionIssue = "重复或无法证明为更新版本的终态"
)

type transitionDisposition string

const (
	transitionApplied        transitionDisposition = "applied"
	transitionFilledReplay   transitionDisposition = "filled_replay"
	transitionTerminalReplay transitionDisposition = "terminal_replay"
	transitionWrongIdentity  transitionDisposition = "wrong_identity"
	transitionCorrection     transitionDisposition = "terminal_correction"
)

// orderTransition describes both the next slot and every accounting/notification
// intent. The caller publishes it once under the same slot lock used for reading.
type orderTransition struct {
	Slot                orderSlotState
	Effect              executionEffect
	Disposition         transitionDisposition
	Issue               executionIssue
	StoreTerminal       bool
	Terminal            terminalOrderProgress
	RecordFill          bool
	Fill                FilledOrderRecord
	Adjust              bool
	AdjustmentNotBefore time.Time
	ReleaseStackCount   int
}

func reduceOrderUpdate(before orderSlotState, in orderUpdateInput) orderTransition {
	next := orderTransition{Slot: before, Disposition: transitionApplied}
	update, side := in.Update, in.Side
	if in.AlreadyFilled {
		next.Disposition = transitionFilledReplay
		return next
	}
	if in.TerminalSeen {
		if !isTerminalOrderStatus(update.Status) {
			next.Disposition = transitionTerminalReplay
			return next
		}
		next.Slot, next.Terminal, next.Effect, next.Issue = reduceTerminalCorrection(before, update, side, in.SlotPrice, in.Terminal)
		next.Disposition = transitionCorrection
		if next.Issue == executionOK {
			next.StoreTerminal, next.Adjust = true, true
			if side == "BUY" && next.Slot.PositionQty > 0 || side == "SELL" && next.Slot.PositionQty <= 0 {
				next.Slot.PlacementRetryNotBefore = time.Time{}
			}
		}
		return next
	}
	if before.ClientOID != "" && in.CanonicalSlotClientOID != update.ClientOrderID {
		next.Disposition = transitionWrongIdentity
		return next
	}
	slot := &next.Slot
	slot.PendingLookupMisses, slot.PendingLastLookup = 0, time.Time{}
	canonicalClientOID := in.CanonicalSlotClientOID
	if slot.OrderID == 0 {
		slot.OrderID, slot.ClientOID, slot.OrderSide = update.OrderID, update.ClientOrderID, side
		canonicalClientOID = update.ClientOrderID
	} else if slot.OrderID != update.OrderID {
		slot.OrderID = update.OrderID
	}
	if slot.ClientOID == "" {
		slot.ClientOID = update.ClientOrderID
		canonicalClientOID = update.ClientOrderID
	}
	if slot.OrderSide == "" {
		slot.OrderSide = side
	}
	if update.Price > 0 && !math.IsNaN(update.Price) && !math.IsInf(update.Price, 0) {
		slot.OrderPrice = update.Price
	}
	if update.Quantity > 0 && !math.IsNaN(update.Quantity) && !math.IsInf(update.Quantity, 0) {
		slot.OrderQuantity = update.Quantity
	}
	if canonicalClientOID == update.ClientOrderID && slot.OrderSide == side && !isTerminalOrderStatus(update.Status) && update.Status != OrderStatusFilled {
		slot.SlotStatus = SlotStatusLocked
	}

	switch update.Status {
	case "NEW":
		if slot.OrderStatus != OrderStatusCancelRequested {
			slot.OrderStatus = OrderStatusConfirmed
		}
	case OrderStatusPartiallyFilled, OrderStatusFilled:
		*slot, next.Effect, _, next.Issue = reduceOrderExecution(*slot, update, side, in.SlotPrice)
		if next.Issue != executionOK {
			// Preserve the old boundary: authoritative identity/price binding is
			// accepted before invalid/stale execution is ignored.
			return next
		}
		if update.Status == OrderStatusPartiallyFilled {
			slot.OrderStatus = OrderStatusPartiallyFilled
			return next
		}
		realizedPNL, gridPNL, entryPrice := 0.0, 0.0, 0.0
		if side == "SELL" {
			realizedPNL, gridPNL = slot.OrderAccumulatedPNL, slot.OrderAccumulatedGridPNL
			if update.ExecutedQty > fillQtyTolerance {
				entryPrice = slot.OrderReleasedCost / update.ExecutedQty
			}
		}
		next.RecordFill = true
		next.Fill = makeFilledOrderRecord(in, slot.OrderPrice, entryPrice, gridPNL, realizedPNL)
		slot.OrderStatus, slot.OrderID, slot.ClientOID, slot.OrderSide = OrderStatusNotPlaced, 0, "", ""
		slot.OrderQuantity, slot.OrderFilledQty = 0, 0
		if side == "BUY" {
			slot.PositionStatus = PositionStatusFilled
		} else if slot.PositionQty < 0.000001 {
			slot.PositionStatus = PositionStatusEmpty
		}
		slot.SlotStatus, slot.PostOnlyFailCount = SlotStatusFree, 0
		if in.MeasureFillLatency && update.ExecutedQty > fillQtyTolerance {
			slot.OppositeFillAt, slot.OppositeSubmitSide = time.Time{}, ""
			if side == "BUY" && slot.PositionQty > fillQtyTolerance {
				slot.OppositeFillAt, slot.OppositeSubmitSide = in.ReceivedAt, "SELL"
			} else if side == "SELL" && slot.PositionQty <= fillQtyTolerance {
				slot.OppositeFillAt, slot.OppositeSubmitSide = in.ReceivedAt, "BUY"
			}
		}
		next.Adjust = true
		slot.clearExecutionAccounting()
	case "CANCELED", "EXPIRED", "REJECTED":
		*slot, next.Effect, _, next.Issue = reduceOrderExecution(*slot, update, side, in.SlotPrice)
		// A terminal status still finalizes a binding when its execution values
		// regress; retain the already-accounted high-water mark, as before.
		if side == "BUY" {
			if slot.PositionQty > 0 || slot.OrderFilledQty > 0 {
				slot.PositionStatus = PositionStatusFilled
			} else {
				slot.PositionStatus = PositionStatusEmpty
				if slot.GapStackCount > 1 {
					next.ReleaseStackCount, slot.GapStackCount = slot.GapStackCount, 0
				}
			}
		} else if side == "SELL" {
			if slot.PositionQty > 0 {
				slot.PostOnlyFailCount++
				slot.PositionStatus = PositionStatusFilled
			} else {
				slot.PositionStatus = PositionStatusEmpty
			}
		}
		slot.SlotStatus, next.Adjust = SlotStatusFree, true
		if update.Status == "REJECTED" {
			retrySameSide := side == "BUY" && slot.PositionQty <= 0 && slot.OrderFilledQty <= 0 || side == "SELL" && slot.PositionQty > 0
			if retrySameSide {
				next.AdjustmentNotBefore = in.Now.Add(placementRetryCooldown)
				slot.PlacementRetryNotBefore = next.AdjustmentNotBefore
			} else {
				slot.PlacementRetryNotBefore = time.Time{}
			}
		}
		next.StoreTerminal = true
		next.Terminal = terminalOrderProgress{ExecutedQty: slot.OrderFilledQty, ExecutedQuote: slot.OrderFilledQuote,
			ReportedPNL: slot.OrderReportedPNL, AccountedPNL: slot.OrderAccumulatedPNL, UpdateTime: update.UpdateTime}
		slot.OrderStatus, slot.OrderID, slot.ClientOID = OrderStatusCanceled, 0, ""
		slot.OrderQuantity, slot.OrderFilledQty = 0, 0
		slot.clearExecutionAccounting() // Keep OrderSide, OrderPrice and creation time.
	}
	return next
}

func (slot *orderSlotState) clearExecutionAccounting() {
	slot.OrderReportedPNL, slot.OrderAccumulatedPNL, slot.OrderFilledQuote = 0, 0, 0
	slot.OrderReleasedCost, slot.OrderAccumulatedGridPNL = 0, 0
}

func makeFilledOrderRecord(in orderUpdateInput, orderPrice, entryPrice, gridPNL, realizedPNL float64) FilledOrderRecord {
	update := in.Update
	price := update.AvgPrice
	if price <= 0 {
		price = update.Price
	}
	if price <= 0 {
		price = orderPrice
	}
	if price <= 0 {
		price = in.SlotPrice
	}
	if in.Side == "BUY" && entryPrice <= 0 {
		entryPrice = price
	}
	targetPrice := 0.0
	if in.SlotPrice > 0 && in.PriceInterval > 0 {
		targetPrice = roundPrice(in.SlotPrice+in.PriceInterval, in.PriceDecimals)
	}
	symbol := update.Symbol
	if symbol == "" {
		symbol = in.Symbol
	}
	filledAt := in.Now
	if update.UpdateTime > 0 {
		filledAt = timestampToTime(update.UpdateTime)
	}
	return FilledOrderRecord{OrderID: update.OrderID, ClientOrderID: update.ClientOrderID, Symbol: symbol,
		Side: in.Side, Price: price, Quantity: update.ExecutedQty, SlotPrice: in.SlotPrice, TargetPrice: targetPrice,
		EntryPrice: entryPrice, GridPNL: gridPNL, RealizedPNL: realizedPNL, FilledAt: filledAt}
}
