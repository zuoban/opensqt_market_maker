package position

import (
	"opensqt/telemetry"
	"time"
)

type oppositeWaitState struct {
	since time.Time
	side  string
}

// 调用方持有槽位锁；只发布不可变观测值，不改变交易状态或重试规则。
func (slot *InventorySlot) confirmOppositeOrderLocked(side string) {
	if pending := slot.oppositePending.Load(); pending != nil && pending.side == side {
		slot.oppositePending.Store(nil)
	}
}

func (slot *InventorySlot) updateOppositeWaitLocked(in orderUpdateInput, next orderTransition) {
	if next.Disposition == transitionApplied && next.Issue == executionOK {
		switch in.Update.Status {
		case "NEW", OrderStatusPartiallyFilled, OrderStatusFilled:
			slot.confirmOppositeOrderLocked(in.Side)
		}
		if next.RecordFill && in.MeasureFillLatency && in.Update.ExecutedQty > fillQtyTolerance &&
			!slot.oppositeFillAt.IsZero() && slot.oppositeSubmitSide != "" {
			slot.oppositePending.Store(&oppositeWaitState{since: slot.oppositeFillAt, side: slot.oppositeSubmitSide})
		}
	}
	// 迟到修正可能使原来的对向单不再需要；不要留下永远无法完成的读数。
	if next.Disposition == transitionApplied || next.Disposition == transitionCorrection {
		if pending := slot.oppositePending.Load(); pending != nil &&
			(pending.side == "SELL" && slot.PositionQty <= fillQtyTolerance ||
				pending.side == "BUY" && slot.PositionQty > fillQtyTolerance) {
			slot.oppositePending.Store(nil)
		}
	}
}

func (spm *SuperPositionManager) SetTelemetry(recorder *telemetry.Recorder) {
	spm.performance.Store(recorder)
}

// TelemetryStateCounts reads container sizes and immutable wait markers without locking slots.
// Counts are individually consistent; they are not a transactional trading view.
func (spm *SuperPositionManager) TelemetryStateCounts() telemetry.StateCounts {
	slots := spm.slotIndex.snapshot()
	s := telemetry.StateCounts{Slots: len(slots)}
	spm.filledOrdersMu.RLock()
	s.FilledDedupKeys = len(spm.filledOrderKeys)
	s.FilledOrders = spm.filledOrderCount
	spm.filledOrdersMu.RUnlock()
	spm.terminalOrdersMu.RLock()
	s.TerminalOrderKeys = len(spm.terminalOrders)
	spm.terminalOrdersMu.RUnlock()
	spm.resolvedAbsentOrdersMu.Lock()
	s.PendingAbsenceKeys = len(spm.resolvedAbsentOrders)
	spm.resolvedAbsentOrdersMu.Unlock()
	now := time.Now()
	for _, slot := range slots {
		pending := slot.oppositePending.Load()
		if pending == nil || slot.retired.Load() {
			continue
		}
		age := max(0, float64(now.Sub(pending.since))/float64(time.Millisecond))
		if pending.side == "BUY" {
			s.PendingOppositeBuys++
			s.OldestOppositeBuyWaitMS = max(s.OldestOppositeBuyWaitMS, age)
		} else if pending.side == "SELL" {
			s.PendingOppositeSells++
			s.OldestOppositeSellWaitMS = max(s.OldestOppositeSellWaitMS, age)
		}
	}
	return s
}
