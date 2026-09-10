package position

import (
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/utils"
)

func transitionInput(side, status string, quantity, average float64) orderUpdateInput {
	return orderUpdateInput{
		Update: OrderUpdate{OrderID: 123, ClientOrderID: transitionClientOID(side), Status: status, Side: side, Quantity: .1, ExecutedQty: quantity, AvgPrice: average},
		Side:   side, SlotPrice: 100, CanonicalSlotClientOID: transitionClientOID(side),
		Now: time.Unix(1700000000, 0), ReceivedAt: time.Unix(1700000000, 0).Add(-time.Second),
		MeasureFillLatency: true, Symbol: "ETHUSDT", PriceInterval: 1, PriceDecimals: 2,
	}
}

func transitionClientOID(side string) string {
	if side == "SELL" {
		return "10000_S_1700000000001"
	}
	return "10000_B_1700000000001"
}

func transitionSlot(side string, quantity, cost float64) orderSlotState {
	status := PositionStatusEmpty
	if quantity > 0 {
		status = PositionStatusFilled
	}
	return orderSlotState{Price: 100, PositionStatus: status, PositionQty: quantity, PositionCost: cost,
		OrderID: 123, ClientOID: transitionClientOID(side), OrderSide: side, OrderStatus: OrderStatusPlaced,
		OrderPrice: 101, OrderQuantity: .1, SlotStatus: SlotStatusLocked}
}

func TestOrderTransitionDoesNotPublishWhileCalculating(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "SELL", .1)
	slot.PositionCost = 10
	var notifications atomic.Int64
	spm.SetAdjustmentNotifier(func(time.Duration) { notifications.Add(1) })
	update := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: OrderStatusPartiallyFilled, ExecutedQty: .04, AvgPrice: 102, RealizedPNL: .13, RealizedPNLIncremental: true}
	slot.mu.Lock()
	before := readOrderSlotState(slot)
	in := spm.orderUpdateFacts(update, "SELL", 100, time.Unix(1700000000, 0))
	in.CanonicalSlotClientOID = oid
	inputCopy := in
	next := reduceOrderUpdate(before, in)
	for i := 0; i < 10; i++ {
		if got := reduceOrderUpdate(before, in); got != next {
			t.Fatal("identical inputs produced different transition")
		}
	}
	if in != inputCopy || readOrderSlotState(slot) != before {
		t.Fatal("calculation changed input/live slot")
	}
	if spm.GetTotalSellQty() != 0 || spm.GetRealizedPNL() != 0 || len(spm.filledOrderKeys) != 0 || len(spm.terminalOrders) != 0 || notifications.Load() != 0 {
		t.Fatal("calculation published accounting or notification")
	}
	slot.mu.Unlock()
	assertClose(t, "projected quantity", next.Slot.PositionQty, .06)
	assertClose(t, "projected cost", next.Slot.PositionCost, 6)
	assertClose(t, "projected grid pnl", next.Slot.OrderAccumulatedGridPNL, .08)
	assertClose(t, "pnl delta", next.Effect.RealizedPNL, .13)
	// The real callback must publish the same calculated result, once.
	spm.OnOrderUpdate(update)
	if got := readOrderSlotState(slot); got != next.Slot {
		t.Fatalf("publication diverged: got=%+v want=%+v", got, next.Slot)
	}
	assertClose(t, "published sell quantity", spm.GetTotalSellQty(), .04)
	assertClose(t, "published pnl", spm.GetRealizedPNL(), .13)
}

func TestPureBuyPartialAndFullFill(t *testing.T) {
	before := transitionSlot("BUY", 0, 0)
	partial := reduceOrderUpdate(before, transitionInput("BUY", OrderStatusPartiallyFilled, .04, 99))
	assertClose(t, "partial quantity", partial.Slot.PositionQty, .04)
	assertClose(t, "partial cost", partial.Slot.PositionCost, 3.96)
	if partial.RecordFill || partial.Adjust || partial.Slot.PositionStatus != PositionStatusEmpty {
		t.Fatal("partial fill finalized early")
	}
	in := transitionInput("BUY", OrderStatusFilled, .1, 100)
	next := reduceOrderUpdate(partial.Slot, in)
	assertClose(t, "buy delta", next.Effect.BuyQty, .06)
	assertClose(t, "full cost", next.Slot.PositionCost, 10)
	if !next.RecordFill || !next.Adjust || next.StoreTerminal || next.Slot.ClientOID != "" || next.Slot.OrderID != 0 || next.Slot.SlotStatus != SlotStatusFree {
		t.Fatalf("full fill effects: %+v", next)
	}
	if next.Fill.EntryPrice != 100 || next.Fill.TargetPrice != 101 || next.Fill.Symbol != "ETHUSDT" || next.Fill.FilledAt != in.Now {
		t.Fatalf("fill record=%+v", next.Fill)
	}
	if next.Slot.OppositeFillAt != in.ReceivedAt || next.Slot.OppositeSubmitSide != "SELL" {
		t.Fatal("fill timer lost original receipt time")
	}
	in.AlreadyFilled = true
	replay := reduceOrderUpdate(next.Slot, in)
	if replay.Slot != next.Slot || replay.Effect != (executionEffect{}) || replay.RecordFill || replay.Adjust {
		t.Fatal("full-fill replay changed state")
	}
}

func TestPureSellSeparatesGridCostAndAuthoritativePNL(t *testing.T) {
	before := transitionSlot("SELL", .1, 9.5)
	partial := reduceOrderUpdate(before, transitionInput("SELL", OrderStatusPartiallyFilled, .04, 102))
	assertClose(t, "released cost", partial.Slot.OrderReleasedCost, 3.8)
	assertClose(t, "partial grid pnl", partial.Slot.OrderAccumulatedGridPNL, .28)
	assertClose(t, "fallback pnl", partial.Effect.RealizedPNL, .08)
	in := transitionInput("SELL", OrderStatusFilled, .1, 103)
	in.Update.RealizedPNL = .25
	in.Update.UpdateTime = 1700000001123
	next := reduceOrderUpdate(partial.Slot, in)
	assertClose(t, "sell delta", next.Effect.SellQty, .06)
	assertClose(t, "pnl correction", next.Effect.RealizedPNL, .17)
	assertClose(t, "total grid pnl", next.Fill.GridPNL, .8)
	assertClose(t, "entry cost", next.Fill.EntryPrice, 95)
	if next.Fill.RealizedPNL != .25 || next.Slot.PositionQty != 0 || next.Slot.PositionCost != 0 || next.Slot.OrderAccumulatedPNL != 0 || next.Slot.OrderReleasedCost != 0 {
		t.Fatalf("sell completion=%+v", next)
	}
	if !next.Fill.FilledAt.Equal(time.UnixMilli(in.Update.UpdateTime)) || next.Slot.OppositeSubmitSide != "BUY" {
		t.Fatal("fill timestamp/opposite direction changed")
	}
}

func TestPureTerminalCorrectionPreservesCurrentBinding(t *testing.T) {
	before := transitionSlot("SELL", .05, 4.75)
	before.OrderID, before.ClientOID, before.SlotStatus = 456, "new-order", SlotStatusPending
	before.OrderFilledQty, before.OrderAccumulatedPNL = .01, .7
	before.PendingLookupMisses = 2
	in := transitionInput("SELL", "CANCELED", .06, 103)
	in.TerminalSeen = true
	in.Terminal = terminalOrderProgress{ExecutedQty: .05, ExecutedQuote: 5.1, ReportedPNL: .11, AccountedPNL: .11, UpdateTime: 100}
	in.Update.RealizedPNL, in.Update.UpdateTime = .13, 200
	next := reduceOrderUpdate(before, in)
	want := before
	want.PositionQty, want.PositionCost = next.Slot.PositionQty, next.Slot.PositionCost
	if next.Slot != want {
		t.Fatalf("old correction changed current binding: got=%+v want=%+v", next.Slot, want)
	}
	assertClose(t, "corrected inventory", next.Slot.PositionQty, .04)
	assertClose(t, "corrected cost", next.Slot.PositionCost, 3.8)
	assertClose(t, "pnl delta", next.Effect.RealizedPNL, .02)
	assertClose(t, "cumulative quote", next.Terminal.ExecutedQuote, 6.18)
	if !next.StoreTerminal || !next.Adjust || next.RecordFill || next.Terminal.UpdateTime != 200 {
		t.Fatalf("correction effects=%+v", next)
	}
	// A same-quantity PNL correction needs a strictly newer event watermark.
	in.Terminal = next.Terminal
	in.Update.RealizedPNL = .15
	in.Update.UpdateTime = 150
	stale := reduceOrderUpdate(next.Slot, in)
	if stale.Slot != next.Slot || stale.Effect != (executionEffect{}) || stale.StoreTerminal || stale.Adjust {
		t.Fatal("stale terminal PNL changed state")
	}
	in.Update.UpdateTime = 300
	fresh := reduceOrderUpdate(next.Slot, in)
	assertClose(t, "pnl-only correction", fresh.Effect.RealizedPNL, .02)
	if fresh.Effect.SellQty != 0 || fresh.Slot != next.Slot {
		t.Fatal("PNL-only correction moved inventory")
	}
}

func TestPureInvalidExecutionKeepsAccountingButFinalizesTerminal(t *testing.T) {
	for _, status := range []string{OrderStatusPartiallyFilled, OrderStatusFilled, "CANCELED", "EXPIRED", "REJECTED"} {
		for _, quantity := range []float64{-.1, .03, math.NaN(), math.Inf(1)} {
			before := transitionSlot("BUY", .04, 3.96)
			before.OrderFilledQty, before.OrderFilledQuote = .04, 3.96
			in := transitionInput("BUY", status, quantity, 99)
			next := reduceOrderUpdate(before, in)
			if next.Issue == executionOK || next.Effect != (executionEffect{}) || next.Slot.PositionQty != before.PositionQty || next.Slot.PositionCost != before.PositionCost {
				t.Fatalf("%s invalid execution applied: %+v", status, next)
			}
			if isTerminalOrderStatus(status) {
				if !next.StoreTerminal || !next.Adjust || next.Terminal.ExecutedQty != .04 || next.Terminal.ExecutedQuote != 3.96 || next.Slot.OrderID != 0 {
					t.Fatalf("%s failed to finalize high-water mark", status)
				}
			} else if next.RecordFill || next.StoreTerminal || next.Adjust || next.Slot.OrderID != before.OrderID {
				t.Fatalf("%s invalid execution finalized", status)
			}
		}
	}
}

func TestPureInvalidQuoteCannotPublishAccounting(t *testing.T) {
	for _, side := range []string{"BUY", "SELL"} {
		for _, tc := range []struct {
			name    string
			average float64
			issue   executionIssue
		}{
			{"regressed", 79, executionInvalidQuote},
			{"unchanged", 80, executionInvalidDelta},
			{"nan", math.NaN(), executionInvalidQuote},
			{"infinite", math.Inf(1), executionInvalidQuote},
		} {
			t.Run(side+"/"+tc.name, func(t *testing.T) {
				before := transitionSlot(side, .06, 6)
				before.OrderFilledQty, before.OrderFilledQuote = .04, 4
				in := transitionInput(side, OrderStatusPartiallyFilled, .05, tc.average)
				in.Update.RealizedPNL = .2
				for _, correction := range []bool{false, true} {
					in.TerminalSeen = correction
					if correction {
						in.Update.Status = "CANCELED"
						in.Terminal = terminalOrderProgress{ExecutedQty: .04, ExecutedQuote: 4, AccountedPNL: .1}
					}
					next := reduceOrderUpdate(before, in)
					if next.Issue != tc.issue || next.Slot != before || next.Effect != (executionEffect{}) || next.Adjust || next.StoreTerminal || next.RecordFill {
						t.Fatalf("invalid quote changed state (correction=%v): %+v", correction, next)
					}
				}
			})
		}
	}
}

func TestPureIgnoredIdentityAndReplayPreserveEntireSlot(t *testing.T) {
	before := transitionSlot("BUY", .04, 4)
	before.PendingLookupMisses = 2
	before.PendingLastLookup = time.Unix(123, 0)
	for _, disposition := range []transitionDisposition{transitionFilledReplay, transitionTerminalReplay, transitionWrongIdentity} {
		t.Run(string(disposition), func(t *testing.T) {
			in := transitionInput("BUY", OrderStatusFilled, .1, 100)
			switch disposition {
			case transitionFilledReplay:
				in.AlreadyFilled = true
			case transitionTerminalReplay:
				in.TerminalSeen = true
				in.Update.Status = "NEW"
			case transitionWrongIdentity:
				in.Update.ClientOrderID = "another-order"
			}
			next := reduceOrderUpdate(before, in)
			if next != (orderTransition{Slot: before, Disposition: disposition}) {
				t.Fatalf("ignored update has effects: %+v", next)
			}
		})
	}
}

func TestPureRejectComputesCooldownAndGapRelease(t *testing.T) {
	before := transitionSlot("BUY", 0, 0)
	before.GapStackCount = 3
	in := transitionInput("BUY", "REJECTED", 0, 0)
	next := reduceOrderUpdate(before, in)
	if next.ReleaseStackCount != 3 || next.Slot.GapStackCount != 0 || next.AdjustmentNotBefore != in.Now.Add(placementRetryCooldown) || next.Slot.PlacementRetryNotBefore != next.AdjustmentNotBefore {
		t.Fatalf("rejection intent=%+v", next)
	}
	in.TerminalSeen, in.Terminal = true, next.Terminal
	replay := reduceOrderUpdate(next.Slot, in)
	if replay.Adjust || replay.ReleaseStackCount != 0 || replay.Slot != next.Slot {
		t.Fatal("duplicate rejection repeated side effects")
	}
	// A late BUY fill reverses direction and clears the earlier same-side delay.
	in.Update.ExecutedQty, in.Update.AvgPrice = .03, 99
	correction := reduceOrderUpdate(next.Slot, in)
	if !correction.Adjust || !correction.Slot.PlacementRetryNotBefore.IsZero() {
		t.Fatal("late buy fill retained buy retry delay")
	}
}

func TestOrderTransitionPreservesUnrelatedSlotFields(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	slot.OrderCreatedAt = time.Unix(123, 0)
	slot.MakerGuardSkipCount, slot.TotalPostOnlyRejects, slot.ConsecutivePostOnlyRejects = 3, 4, 5
	slot.LastTriedQuoteVersion, slot.makerRetryQuoteVersion, slot.stackedParentPrice = 6, 7, 99
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: OrderStatusFilled, ExecutedQty: .1, AvgPrice: 100})
	if slot.OrderCreatedAt != time.Unix(123, 0) || slot.MakerGuardSkipCount != 3 || slot.TotalPostOnlyRejects != 4 || slot.ConsecutivePostOnlyRejects != 5 || slot.LastTriedQuoteVersion != 6 || slot.makerRetryQuoteVersion != 7 || slot.stackedParentPrice != 99 {
		t.Fatal("publication changed unrelated maker/binding metadata")
	}
}

func TestConcurrentSlotsPublishEveryAccountingDelta(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	const slots, steps = 64, 8
	updates := make([]OrderUpdate, slots)
	for i := 0; i < slots; i++ {
		price, side := float64(100+i), "BUY"
		if i%2 != 0 {
			side = "SELL"
		}
		oid := utils.GenerateOrderID(price, side, 2)
		slot := spm.getOrCreateSlot(price)
		slot.ClientOID, slot.OrderID, slot.OrderSide, slot.OrderStatus = oid, int64(i+1), side, OrderStatusPlaced
		slot.OrderPrice, slot.OrderQuantity, slot.SlotStatus = price+1, steps, SlotStatusLocked
		if side == "SELL" {
			slot.PositionQty, slot.PositionCost, slot.PositionStatus = steps, steps*price, PositionStatusFilled
		}
		updates[i] = OrderUpdate{OrderID: int64(i + 1), ClientOrderID: oid, Side: side, AvgPrice: price + 1, RealizedPNL: .25, RealizedPNLIncremental: true}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, update := range updates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for step := 1; step <= steps; step++ {
				update.Status, update.ExecutedQty = OrderStatusPartiallyFilled, float64(step)
				if step == steps {
					update.Status = OrderStatusFilled
				}
				spm.OnOrderUpdate(update)
				spm.OnOrderUpdate(update)
			}
		}()
	}
	close(start)
	wg.Wait()
	assertClose(t, "concurrent total buys", spm.GetTotalBuyQty(), slots/2*steps)
	assertClose(t, "concurrent total sells", spm.GetTotalSellQty(), slots/2*steps)
	assertClose(t, "concurrent pnl", spm.GetRealizedPNL(), slots/2*steps*.25)
	if spm.filledOrderCount != slots {
		t.Fatalf("completed orders=%d want %d", spm.filledOrderCount, slots)
	}
}

func TestPureTransitionCanRunConcurrently(t *testing.T) {
	before, in := transitionSlot("SELL", .1, 10), transitionInput("SELL", OrderStatusFilled, .1, 101)
	want := reduceOrderUpdate(before, in)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if got := reduceOrderUpdate(before, in); !reflect.DeepEqual(got, want) {
					t.Error("concurrent calculation changed result")
					return
				}
			}
		}()
	}
	wg.Wait()
}

var benchmarkOrderTransition orderTransition

func BenchmarkReduceOrderUpdate(b *testing.B) {
	before, in := transitionSlot("SELL", .1, 10), transitionInput("SELL", OrderStatusFilled, .1, 101)
	b.ReportAllocs()
	for b.Loop() {
		benchmarkOrderTransition = reduceOrderUpdate(before, in)
	}
}
