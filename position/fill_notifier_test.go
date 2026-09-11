package position

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opensqt/utils"
)

func TestFilledOrderNotifierUsesAcceptedRecordAfterUnlock(t *testing.T) {
	for _, side := range []string{"BUY", "SELL"} {
		t.Run(side, func(t *testing.T) {
			qty := 0.0
			if side == "SELL" {
				qty = 0.03
			}
			spm, slot, oid := setupOrderSlot(t, side, qty)
			slot.PositionCost = qty * 100
			var adjusted bool
			spm.SetAdjustmentNotifier(func(time.Duration) { adjusted = true })
			var records []FilledOrderRecord
			spm.SetFilledOrderNotifier(func(record FilledOrderRecord) {
				if !slot.mu.TryLock() {
					t.Fatal("fill notification ran with slot locked")
				}
				slot.mu.Unlock()
				if !adjusted {
					t.Fatal("fill notification ran before adjustment was requested")
				}
				if snapshot := spm.Snapshot(); snapshot.FilledOrderCount != 1 || snapshot.FilledOrders[0] != record {
					t.Fatal("notification did not match published fill history")
				}
				records = append(records, record)
			})
			update := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: OrderStatusPartiallyFilled,
				ExecutedQty: 0.01, AvgPrice: 101, RealizedPNL: 0.02, RealizedPNLIncremental: true}
			spm.OnOrderUpdate(update)
			if len(records) != 0 {
				t.Fatal("partial fill triggered notification")
			}
			update.Status, update.ExecutedQty, update.AvgPrice = OrderStatusFilled, 0.03, 101.5
			update.UpdateTime = time.Now().UnixMilli()
			spm.OnOrderUpdate(update)
			spm.OnOrderUpdate(update)
			if len(records) != 1 || records[0].Side != side || records[0].Price != 101.5 || records[0].Quantity != 0.03 {
				t.Fatalf("unexpected notifications: %+v", records)
			}
			if side == "SELL" {
				assertClose(t, "notified realized PNL", records[0].RealizedPNL, 0.04)
			}
		})
	}
}

func TestFilledOrderNotifierConcurrentReplay(t *testing.T) {
	spm, _, oid := setupOrderSlot(t, "BUY", 0)
	var count atomic.Int64
	spm.SetFilledOrderNotifier(func(FilledOrderRecord) { count.Add(1) })
	update := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: OrderStatusFilled, ExecutedQty: 0.03, AvgPrice: 101}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() { spm.OnOrderUpdate(update) })
	}
	wg.Wait()
	if count.Load() != 1 || spm.Snapshot().FilledOrderCount != 1 {
		t.Fatalf("concurrent replay sent %d notifications", count.Load())
	}
}

func TestFilledOrderNotifierIgnoresUnacceptedUpdates(t *testing.T) {
	for _, status := range []string{"NEW", "CANCELED", "EXPIRED", "REJECTED", "invalid fill", "wrong identity"} {
		t.Run(status, func(t *testing.T) {
			spm, _, oid := setupOrderSlot(t, "BUY", 0)
			spm.SetFilledOrderNotifier(func(FilledOrderRecord) { t.Error("unexpected notification") })
			update := OrderUpdate{OrderID: 123, ClientOrderID: oid, Status: status, AvgPrice: 101}
			if status == "invalid fill" {
				update.Status, update.ExecutedQty = OrderStatusFilled, -1
			}
			if status == "wrong identity" {
				update.Status, update.ExecutedQty = OrderStatusFilled, 0.03
				update.ClientOrderID = utils.GenerateOrderID(100, "BUY", 2)
			}
			spm.OnOrderUpdate(update)
		})
	}
}
