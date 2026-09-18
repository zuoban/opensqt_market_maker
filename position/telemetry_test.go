package position

import (
	"errors"
	"testing"
	"time"

	"opensqt/exchange"
	orderpkg "opensqt/order"
	"opensqt/telemetry"
)

func TestTelemetryFillToOppositeSubmission(t *testing.T) {
	for _, side := range []string{"BUY", "SELL"} {
		t.Run(side, func(t *testing.T) {
			qty := 0.0
			nextSide, metric := "SELL", telemetry.BuyFillToSellSubmit
			if side == "SELL" {
				qty = .1
				nextSide, metric = "BUY", telemetry.SellFillToBuySubmit
			}
			spm, slot, oid := setupOrderSlot(t, side, qty)
			r := telemetry.New(time.Now())
			spm.SetTelemetry(r)
			update := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "FILLED", Side: side, Quantity: .1, ExecutedQty: .1, AvgPrice: 101, UpdateTime: 1}
			before := time.Now()
			spm.OnOrderUpdate(update)
			if slot.oppositeFillAt.Before(before) || slot.oppositeSubmitSide != nextSide {
				t.Fatal("fill was not marked with local receipt time")
			}
			original := slot.oppositeFillAt
			pending := slot.oppositePending.Load()
			if pending == nil || pending.side != nextSide || pending.since != original {
				t.Fatal("fill did not start pending opposite-order observation")
			}
			spm.OnOrderUpdate(update)
			if slot.oppositeFillAt != original {
				t.Fatal("duplicate fill reset start time")
			}
			if slot.oppositePending.Load() != pending {
				t.Fatal("duplicate fill reset pending wait")
			}
			req := &OrderRequest{Symbol: "ETHUSDT", Side: nextSide, Price: 101, Quantity: .1, ReduceOnly: nextSide == "SELL", ClientOrderID: "retry"}
			slot.mu.Lock()
			spm.reserveOrderLocked(slot, req)
			slot.mu.Unlock()
			release, ok := req.AcquireSubmissionLease()
			if !ok {
				t.Fatal("reservation lease rejected")
			}
			// 模拟最终本地 guard 拒绝：释放 lease，但尚未真正提交。
			release()
			slot.mu.Lock()
			spm.clearReservationLocked(slot)
			if slot.oppositeFillAt != original {
				t.Fatal("local rejection lost original fill time")
			}
			spm.reserveOrderLocked(slot, req)
			slot.mu.Unlock()
			r.Refresh()
			if r.Snapshot().Latencies[metric].Count != 0 {
				t.Fatal("unsubmitted reservation produced sample")
			}
			release, ok = req.AcquireSubmissionLease()
			if !ok {
				t.Fatal("retry lease rejected")
			}
			req.OnSubmissionStarted()
			req.OnSubmissionStarted() // Remote retries must not sample this fill twice.
			release()
			r.Refresh()
			s := r.Snapshot()
			if s.Latencies[metric].Count != 1 || s.Latencies[metric].LastMS <= 0 || !slot.oppositeFillAt.IsZero() {
				t.Fatalf("first opposite submission sample: %+v", s.Latencies[metric])
			}
			if slot.oppositePending.Load() != pending {
				t.Fatal("submission attempt cleared pending wait before acceptance")
			}
			if s.Latencies[telemetry.OrderUpdateTotal].Count != 2 {
				t.Fatal("callback samples missing")
			}
		})
	}
}

func TestTelemetryPartialFillDoesNotStartOppositeTimer(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	spm.SetTelemetry(telemetry.New(time.Now()))
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "PARTIALLY_FILLED", ExecutedQty: .05, AvgPrice: 101})
	if !slot.oppositeFillAt.IsZero() {
		t.Fatal("partial fill started full-fill timer")
	}
}

func TestTelemetryZeroExecutionDoesNotStartOppositeTimer(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "SELL", 0)
	spm.SetTelemetry(telemetry.New(time.Now()))
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "FILLED", ExecutedQty: 0})
	if !slot.oppositeFillAt.IsZero() {
		t.Fatal("zero execution started a fill timer")
	}
}

func TestTelemetryGapStackPropagatesFillOrigin(t *testing.T) {
	spm, parent, oid := setupOrderSlot(t, "BUY", 0)
	spm.SetTelemetry(telemetry.New(time.Now()))
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "FILLED", ExecutedQty: .6, Quantity: .6, AvgPrice: 100})
	spm.splitGapStackedSlot(100, 2)
	child := spm.getOrCreateSlot(101)
	if child.PositionQty != .3 || child.oppositeFillAt != parent.oppositeFillAt || child.oppositeSubmitSide != "SELL" {
		t.Fatal("split inventory lost originating fill")
	}
	if child.oppositePending.Load() != parent.oppositePending.Load() || spm.TelemetryStateCounts().PendingOppositeSells != 2 {
		t.Fatal("split lost pending opposite-order observation")
	}
}

func TestTelemetryOrderUpdateIncludesSlotLockWait(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	r := telemetry.New(time.Now())
	spm.SetTelemetry(r)
	slot.mu.Lock()
	done := make(chan struct{})
	go func() { spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "NEW"}); close(done) }()
	deadline := time.Now().Add(time.Second)
	for spm.orderUpdatesActive.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	slot.mu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback did not finish")
	}
	r.Refresh()
	s := r.Snapshot()
	wait, total := s.Latencies[telemetry.OrderUpdateLockWait], s.Latencies[telemetry.OrderUpdateTotal]
	if wait.Count != 1 || wait.LastMS < 1 || total.LastMS < wait.LastMS {
		t.Fatalf("lock wait=%+v total=%+v", wait, total)
	}
	if slot.OrderStatus != OrderStatusConfirmed {
		t.Fatal("instrumented callback changed order behavior")
	}
}

func TestTelemetryStateCountsDoesNotLockSlots(t *testing.T) {
	spm, slot, _ := setupOrderSlot(t, "BUY", 0)
	spm.filledOrderKeys = map[string]struct{}{"a": {}, "b": {}}
	spm.filledOrderCount = 1
	spm.terminalOrders = map[string]terminalOrderProgress{"terminal": {}}
	spm.resolvedAbsentOrders = map[string]time.Time{"absent": time.Now()}
	slot.oppositePending.Store(&oppositeWaitState{side: "SELL", since: time.Now().Add(-time.Second)})
	slot.mu.Lock()
	defer slot.mu.Unlock()
	done := make(chan telemetry.StateCounts, 1)
	go func() { done <- spm.TelemetryStateCounts() }()
	select {
	case s := <-done:
		if s.Slots != 1 || s.FilledDedupKeys != 2 || s.FilledOrders != 1 || s.TerminalOrderKeys != 1 || s.PendingAbsenceKeys != 1 {
			t.Fatalf("counts: %+v", s)
		}
		if s.PendingOppositeSells != 1 || s.OldestOppositeSellWaitMS < 1000 || s.PendingOppositeBuys != 0 {
			t.Fatalf("pending observation missing: %+v", s)
		}
	case <-time.After(time.Second):
		t.Fatal("count collector blocked on slot lock")
	}
}

func TestTelemetryOppositeWaitSurvivesRejectionUntilRESTAcceptance(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	spm.SetTelemetry(telemetry.New(time.Now()))
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "FILLED", ExecutedQty: .3, Quantity: .3, AvgPrice: 100})
	pending := slot.oppositePending.Load()
	e := &offlineBatchExecutor{err: orderpkg.NewOrderRejectedError(orderpkg.OrderRejectionPostOnly, errors.New("post only"))}
	spm.executor = &limitedOfflineExecutor{e}
	if err := spm.AdjustOrders(100.2); err == nil {
		t.Fatal("rejection did not propagate")
	}
	if pending == nil || slot.oppositePending.Load() != pending || !slot.oppositeFillAt.IsZero() {
		t.Fatal("rejection lost pending observation or first-attempt sample")
	}
	// 测试中推进冷却期限；生产由既有定时器负责唤醒。
	slot.mu.Lock()
	slot.placementRetryNotBefore = time.Now().Add(-time.Second)
	slot.mu.Unlock()
	e.err = nil
	if err := spm.AdjustOrders(100.2); err != nil {
		t.Fatal(err)
	}
	if slot.oppositePending.Load() != nil || slot.SlotStatus != SlotStatusLocked || slot.OrderSide != "SELL" {
		t.Fatal("REST accepted sell did not clear pending wait")
	}
}

func TestTelemetryOppositeWaitUnknownIdentityAndWSAcceptance(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	spm.SetTelemetry(telemetry.New(time.Now()))
	fill := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "FILLED", ExecutedQty: .3, Quantity: .3, AvgPrice: 100}
	spm.OnOrderUpdate(fill)
	pending := slot.oppositePending.Load()
	e := &offlineBatchExecutor{err: exchange.ErrOrderPlacementUnknown}
	spm.executor = &limitedOfflineExecutor{e}
	if err := spm.AdjustOrders(100.2); !errors.Is(err, exchange.ErrOrderPlacementUnknown) {
		t.Fatal(err)
	}
	spm.OnOrderUpdate(fill) // 旧成交重放不能清掉新 reservation 的等待。
	spm.OnOrderUpdate(OrderUpdate{OrderID: 900, ClientOrderID: spm.generateClientOrderID(100, "SELL"), Status: "NEW"})
	if pending == nil || slot.oppositePending.Load() != pending || slot.SlotStatus != SlotStatusPending {
		t.Fatal("unknown result or wrong identity cleared pending wait")
	}
	spm.OnOrderUpdate(OrderUpdate{OrderID: 901, ClientOrderID: slot.ClientOID, Status: "NEW", Side: "SELL"})
	if slot.oppositePending.Load() != nil || spm.TelemetryStateCounts().PendingOppositeSells != 0 {
		t.Fatal("matching authoritative update did not clear pending wait")
	}
}

func TestTelemetryPendingOppositeBuyEndsAtRetirement(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "SELL", .1)
	spm.SetTelemetry(telemetry.New(time.Now()))
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "FILLED", ExecutedQty: .1, Quantity: .1, AvgPrice: 101})
	if spm.TelemetryStateCounts().PendingOppositeBuys != 1 {
		t.Fatal("sell fill did not start buy wait")
	}
	slot.mu.Lock()
	deleted := spm.deleteSlotIfCurrentAndRecyclable(100, slot)
	slot.mu.Unlock()
	if !deleted || slot.oppositePending.Load() != nil || spm.TelemetryStateCounts().PendingOppositeBuys != 0 {
		t.Fatal("retired slot remained in pending observation")
	}
}

func TestTelemetryOppositeWaitEndsOnInventoryCorrection(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	spm.SetTelemetry(telemetry.New(time.Now()))
	spm.OnOrderUpdate(OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: "FILLED", ExecutedQty: .3, Quantity: .3, AvgPrice: 100})
	pending := slot.oppositePending.Load()
	spm.executor = &limitedOfflineExecutor{&offlineBatchExecutor{err: exchange.ErrOrderPlacementUnknown}}
	_ = spm.AdjustOrders(100.2)
	terminal := OrderUpdate{OrderID: 456, ClientOrderID: slot.ClientOID, Status: "CANCELED", Side: "SELL", Quantity: .3, AvgPrice: 101, UpdateTime: 1}
	spm.OnOrderUpdate(terminal)
	if pending == nil || slot.oppositePending.Load() != pending {
		t.Fatal("unfilled terminal lost outstanding sell wait")
	}
	terminal.ExecutedQty, terminal.UpdateTime = .3, 2
	spm.OnOrderUpdate(terminal)
	if slot.PositionQty != 0 || slot.oppositePending.Load() != nil {
		t.Fatal("inventory correction left an impossible sell wait")
	}
	if spm.TelemetryStateCounts().PendingOppositeBuys != 0 {
		t.Fatal("terminal correction invented a full-fill timer")
	}
}

func TestTelemetryAdjustPlanning(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	r := telemetry.New(time.Now())
	spm.SetTelemetry(r)
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatal(err)
	}
	r.Refresh()
	s := r.Snapshot()
	for _, metric := range []telemetry.Metric{telemetry.Planning, telemetry.AdjustTotal, telemetry.AdjustLockWait} {
		if s.Latencies[metric].Count != 1 {
			t.Fatalf("metric %d missing", metric)
		}
	}
	if s.Latencies[telemetry.AdjustTotal].LastMS < s.Latencies[telemetry.Planning].LastMS {
		t.Fatal("planning outlasted total adjustment")
	}
	if s.Latencies[telemetry.QuoteToPlanning].Count != 0 {
		t.Fatal("invented missing quote timestamp")
	}
}
