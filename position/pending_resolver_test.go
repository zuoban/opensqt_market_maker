package position

import (
	"context"
	"errors"
	"testing"
	"time"

	"opensqt/exchange/exchangeerr"
)

type pendingLookupTestExchange struct {
	stubEx
	raw   interface{}
	err   error
	calls int
}

func (e *pendingLookupTestExchange) GetOrderByClientID(context.Context, string, string) (interface{}, error) {
	e.calls++
	return e.raw, e.err
}

func newPendingLookupTestManager(t *testing.T, ex *pendingLookupTestExchange) (*SuperPositionManager, *InventorySlot, pendingOrderTarget) {
	t.Helper()
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, ex, 2, 3)
	slot := spm.getOrCreateSlot(100)
	slot.ClientOID = spm.generateClientOrderID(100, "BUY")
	slot.OrderSide = "BUY"
	slot.OrderStatus = OrderStatusNotPlaced
	slot.OrderPrice = 100
	slot.OrderQuantity = 0.3
	slot.OrderCreatedAt = time.Now().Add(-pendingLookupMinAge - time.Second)
	slot.SlotStatus = SlotStatusPending
	targets, remaining := spm.collectPendingOrderTargets()
	if remaining != 1 || len(targets) != 1 {
		t.Fatalf("pending targets = %d/%d", len(targets), remaining)
	}
	return spm, slot, targets[0]
}

func TestPendingResolverReleasesOnlyAfterThreeExplicitMisses(t *testing.T) {
	ex := &pendingLookupTestExchange{err: exchangeerr.ErrOrderNotFound}
	spm, slot, target := newPendingLookupTestManager(t, ex)

	for attempt := 1; attempt <= pendingLookupMissLimit; attempt++ {
		if attempt > 1 {
			slot.mu.Lock()
			slot.pendingLastLookup = time.Now().Add(-pendingLookupInterval)
			slot.mu.Unlock()
		}
		lookedUp, err := spm.resolvePendingOrderTarget(context.Background(), ex, target)
		if err != nil || !lookedUp {
			t.Fatalf("attempt %d: lookedUp=%v err=%v", attempt, lookedUp, err)
		}
		if attempt < pendingLookupMissLimit {
			slot.mu.RLock()
			if slot.SlotStatus != SlotStatusPending || slot.pendingLookupMisses != attempt {
				t.Fatalf("attempt %d released early: %+v", attempt, slot)
			}
			slot.mu.RUnlock()
		}
	}

	slot.mu.RLock()
	if slot.SlotStatus != SlotStatusFree || slot.ClientOID != "" || slot.OrderID != 0 ||
		slot.placementRetryNotBefore.Before(time.Now()) {
		t.Fatalf("confirmed absent reservation did not enter grace cooldown: %+v", slot)
	}
	slot.mu.RUnlock()
	if ex.calls != pendingLookupMissLimit {
		t.Fatalf("lookup calls = %d, want %d", ex.calls, pendingLookupMissLimit)
	}
}

func TestPendingResolverKeepsReservationOnAmbiguousLookup(t *testing.T) {
	lookupErr := errors.New("timeout")
	ex := &pendingLookupTestExchange{err: exchangeerr.ErrOrderNotFound}
	spm, slot, target := newPendingLookupTestManager(t, ex)
	if _, err := spm.resolvePendingOrderTarget(context.Background(), ex, target); err != nil {
		t.Fatal(err)
	}
	slot.mu.Lock()
	slot.pendingLastLookup = time.Now().Add(-pendingLookupInterval)
	slot.mu.Unlock()
	ex.err = lookupErr

	lookedUp, err := spm.resolvePendingOrderTarget(context.Background(), ex, target)
	if !lookedUp || !errors.Is(err, lookupErr) {
		t.Fatalf("lookedUp=%v error=%v", lookedUp, err)
	}
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.SlotStatus != SlotStatusPending || slot.ClientOID != target.clientOrderID || slot.pendingLookupMisses != 0 {
		t.Fatalf("ambiguous lookup changed reservation: %+v", slot)
	}
}

func TestPendingResolverBindsFoundOrder(t *testing.T) {
	ex := &pendingLookupTestExchange{}
	spm, slot, target := newPendingLookupTestManager(t, ex)
	ex.raw = &cancelBuyTestOrder{
		OrderID: 91, ClientOrderID: target.clientOrderID, Symbol: "ETHUSDT",
		Side: "BUY", Type: "LIMIT", Status: "NEW", Price: 100, Quantity: 0.3,
	}

	lookedUp, err := spm.resolvePendingOrderTarget(context.Background(), ex, target)
	if err != nil || !lookedUp {
		t.Fatalf("lookedUp=%v error=%v", lookedUp, err)
	}
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.OrderID != 91 || slot.OrderStatus != OrderStatusConfirmed || slot.SlotStatus != SlotStatusLocked {
		t.Fatalf("found order was not bound: %+v", slot)
	}
}

func TestLateOrderUpdateCanBindDuringAbsentGrace(t *testing.T) {
	ex := &pendingLookupTestExchange{err: exchangeerr.ErrOrderNotFound}
	spm, slot, target := newPendingLookupTestManager(t, ex)
	for attempt := 0; attempt < pendingLookupMissLimit; attempt++ {
		if attempt > 0 {
			slot.mu.Lock()
			slot.pendingLastLookup = time.Now().Add(-pendingLookupInterval)
			slot.mu.Unlock()
		}
		_, _ = spm.resolvePendingOrderTarget(context.Background(), ex, target)
	}

	spm.OnOrderUpdate(OrderUpdate{
		OrderID: 92, ClientOrderID: target.clientOrderID, Symbol: "ETHUSDT",
		Side: "BUY", Type: "LIMIT", Status: "NEW", Price: 100, Quantity: 0.3,
	})
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if slot.OrderID != 92 || slot.ClientOID != target.clientOrderID || slot.SlotStatus != SlotStatusLocked {
		t.Fatalf("late update did not reclaim slot: %+v", slot)
	}
}
