package position

import (
	"errors"
	"testing"
	"time"
)

func requireAdjustmentNotification(t *testing.T, notified <-chan time.Duration) time.Duration {
	t.Helper()
	select {
	case delay := <-notified:
		return delay
	default:
		t.Fatal("expected adjustment notification")
		return 0
	}
}

func TestTerminalOrderUpdateNotifiesAfterSlotUnlock(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	type notification struct {
		delay        time.Duration
		slotUnlocked bool
	}
	notified := make(chan notification, 1)
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		unlocked := slot.mu.TryLock()
		if unlocked {
			slot.mu.Unlock()
		}
		notified <- notification{delay: delay, slotUnlocked: unlocked}
	})

	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       123,
		ClientOrderID: oid,
		Status:        OrderStatusFilled,
		ExecutedQty:   0.03,
		AvgPrice:      99,
	})

	select {
	case got := <-notified:
		if got.delay > 0 {
			t.Fatalf("terminal adjustment delay = %s, want immediate", got.delay)
		}
		if !got.slotUnlocked {
			t.Fatal("adjustment notifier ran while slot lock was still held")
		}
	default:
		t.Fatal("terminal order update did not request an adjustment")
	}
}

func TestNonTerminalOrderUpdateDoesNotRequestAdjustment(t *testing.T) {
	spm, _, oid := setupOrderSlot(t, "BUY", 0)
	notified := make(chan struct{}, 1)
	spm.SetAdjustmentNotifier(func(time.Duration) {
		notified <- struct{}{}
	})

	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       123,
		ClientOrderID: oid,
		Status:        "NEW",
		Price:         99,
		Quantity:      0.03,
	})

	select {
	case <-notified:
		t.Fatal("non-terminal order update requested a redundant adjustment")
	default:
	}
}

func TestDefinitePlacementFailureSchedulesCooldownAdjustment(t *testing.T) {
	wantErr := errors.New("definite reject")
	executor := &failThenSucceedExecutor{fail: true, wantErr: wantErr}
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 0
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	type notification struct {
		delay       time.Duration
		spmUnlocked bool
	}
	notified := make(chan notification, 1)
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		unlocked := spm.mu.TryLock()
		if unlocked {
			spm.mu.Unlock()
		}
		select {
		case notified <- notification{delay: delay, spmUnlocked: unlocked}:
		default:
		}
	})

	err := spm.AdjustOrders(100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("AdjustOrders() error = %v, want definite rejection", err)
	}

	select {
	case got := <-notified:
		if got.delay <= 0 || got.delay > placementRetryCooldown {
			t.Fatalf("retry adjustment delay = %s, want (0, %s]", got.delay, placementRetryCooldown)
		}
		if !got.spmUnlocked {
			t.Fatal("retry adjustment notifier ran while manager lock was still held")
		}
	default:
		t.Fatal("definite placement failure did not schedule a cooldown adjustment")
	}
}

func TestMarginErrorSchedulesSlotAndLockExpiryAdjustments(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 0
	cfg.Trading.MarginLockDurationSec = 5
	executor := &marginThenAcceptExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	type notification struct {
		delay       time.Duration
		spmUnlocked bool
	}
	notified := make(chan notification, 4)
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		unlocked := spm.mu.TryLock()
		if unlocked {
			spm.mu.Unlock()
		}
		notified <- notification{delay: delay, spmUnlocked: unlocked}
	})

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("first AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 1 {
		t.Fatalf("placement batches = %d, want first rejected batch", len(executor.batches))
	}

	notifications := make([]notification, 0, 2)
	for len(notifications) < 2 {
		select {
		case got := <-notified:
			notifications = append(notifications, got)
		default:
			t.Fatalf("margin error notifications = %d, want slot and lock deadlines", len(notifications))
		}
	}
	first, second := notifications[0], notifications[1]
	if !first.spmUnlocked || !second.spmUnlocked {
		t.Fatalf("margin notifications ran under manager lock: first=%+v second=%+v", first, second)
	}
	shortDelay, lockDelay := first.delay, second.delay
	if shortDelay > lockDelay {
		shortDelay, lockDelay = lockDelay, shortDelay
	}
	if shortDelay <= 0 || shortDelay > placementRetryCooldown {
		t.Fatalf("slot retry delay = %s, want (0, %s]", shortDelay, placementRetryCooldown)
	}
	if lockDelay <= placementRetryCooldown || lockDelay > spm.marginLockDuration {
		t.Fatalf("margin lock expiry delay = %s, want (%s, %s]",
			lockDelay, placementRetryCooldown, spm.marginLockDuration)
	}
	select {
	case extra := <-notified:
		t.Fatalf("unexpected extra margin adjustment notification: %+v", extra)
	default:
	}

	// 模拟 1 秒槽位 deadline 已到期；此时只剩保证金锁，短 deadline
	// 触发的普通唤醒仍不能提前恢复 BUY。
	buySlot := spm.getOrCreateSlot(99)
	buySlot.mu.Lock()
	buySlot.placementRetryNotBefore = time.Now().Add(-time.Millisecond)
	buySlot.mu.Unlock()
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("locked AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 1 {
		t.Fatalf("margin lock allowed an early BUY retry: batches=%d", len(executor.batches))
	}

	// 模拟协调器在保证金锁 deadline 到期后唤醒。届时槽位的 1 秒冷却也
	// 必然已经到期；无需价格变化，使用相同价格即可恢复 BUY。
	spm.mu.Lock()
	spm.marginLockTime = time.Now().Add(-spm.marginLockDuration - time.Millisecond)
	spm.mu.Unlock()

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("expired margin lock AdjustOrders() error = %v", err)
	}
	if spm.insufficientMargin {
		t.Fatal("expired margin lock was not cleared")
	}
	if len(executor.batches) != 2 || len(executor.batches[1]) != 1 {
		t.Fatalf("BUY did not resume at margin lock expiry: batches=%+v", executor.batches)
	}
	if req := executor.batches[1][0]; req.Side != "BUY" || req.Price != 99 {
		t.Fatalf("resumed order = %+v, want BUY at unchanged grid price 99", req)
	}
}

func TestIgnoredTerminalOrderCorrectionsDoNotRequestAdjustment(t *testing.T) {
	spm, _, oid := setupOrderSlot(t, "BUY", 0)
	notified := make(chan time.Duration, 2)
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		notified <- delay
	})

	terminal := OrderUpdate{
		OrderID:       123,
		ClientOrderID: oid,
		Status:        "CANCELED",
		ExecutedQty:   0.03,
	}
	spm.OnOrderUpdate(terminal)
	requireAdjustmentNotification(t, notified)

	updates := []struct {
		name   string
		update OrderUpdate
	}{
		{name: "duplicate", update: terminal},
		{name: "regressed", update: func() OrderUpdate {
			update := terminal
			update.ExecutedQty = 0.02
			return update
		}()},
		{name: "invalid", update: func() OrderUpdate {
			update := terminal
			update.ExecutedQty = -1
			return update
		}()},
	}
	for _, tt := range updates {
		t.Run(tt.name, func(t *testing.T) {
			spm.OnOrderUpdate(tt.update)
			select {
			case delay := <-notified:
				t.Fatalf("ignored terminal correction requested adjustment with delay %s", delay)
			default:
			}
		})
	}
}

func TestAcceptedTerminalCorrectionRequestsImmediateAdjustment(t *testing.T) {
	t.Run("quantity", func(t *testing.T) {
		spm, slot, oid := setupOrderSlot(t, "BUY", 0)
		notified := make(chan time.Duration, 2)
		spm.SetAdjustmentNotifier(func(delay time.Duration) {
			notified <- delay
		})

		terminal := OrderUpdate{
			OrderID:       123,
			ClientOrderID: oid,
			Status:        "CANCELED",
			UpdateTime:    100,
		}
		spm.OnOrderUpdate(terminal)
		requireAdjustmentNotification(t, notified)

		corrected := terminal
		corrected.ExecutedQty = 0.03
		corrected.AvgPrice = 99
		corrected.UpdateTime = 101
		spm.OnOrderUpdate(corrected)

		select {
		case delay := <-notified:
			if delay > 0 {
				t.Fatalf("quantity correction adjustment delay = %s, want immediate", delay)
			}
		default:
			t.Fatal("accepted terminal quantity correction did not request an adjustment")
		}
		assertClose(t, "corrected buy position", slot.PositionQty, 0.03)
	})

	t.Run("authoritative pnl", func(t *testing.T) {
		spm, _, oid := setupOrderSlot(t, "SELL", 0.10)
		notified := make(chan time.Duration, 2)
		spm.SetAdjustmentNotifier(func(delay time.Duration) {
			notified <- delay
		})

		terminal := OrderUpdate{
			OrderID:       123,
			ClientOrderID: oid,
			Status:        "CANCELED",
			ExecutedQty:   0.02,
			AvgPrice:      101,
			RealizedPNL:   0.10,
			UpdateTime:    100,
		}
		spm.OnOrderUpdate(terminal)
		requireAdjustmentNotification(t, notified)

		corrected := terminal
		corrected.RealizedPNL = 0.12
		corrected.UpdateTime = 101
		spm.OnOrderUpdate(corrected)

		select {
		case delay := <-notified:
			if delay > 0 {
				t.Fatalf("authoritative PNL correction adjustment delay = %s, want immediate", delay)
			}
		default:
			t.Fatal("accepted authoritative PNL correction did not request an adjustment")
		}
		assertClose(t, "corrected realized PNL", spm.GetRealizedPNL(), 0.12)
	})
}

func TestEmptyBuyRejectedSchedulesCooldownAdjustment(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	notified := make(chan time.Duration, 1)
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		notified <- delay
	})

	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       123,
		ClientOrderID: oid,
		Status:        "REJECTED",
	})

	select {
	case delay := <-notified:
		if delay <= 0 || delay > placementRetryCooldown {
			t.Fatalf("empty BUY rejection adjustment delay = %s, want (0, %s]", delay, placementRetryCooldown)
		}
	default:
		t.Fatal("empty BUY rejection did not schedule a cooldown adjustment")
	}

	slot.mu.RLock()
	retryAt := slot.placementRetryNotBefore
	positionStatus := slot.PositionStatus
	slot.mu.RUnlock()
	if retryAt.IsZero() || !retryAt.After(time.Now()) {
		t.Fatalf("empty BUY rejection retry deadline = %s, want future deadline", retryAt)
	}
	if positionStatus != PositionStatusEmpty {
		t.Fatalf("empty BUY rejection position status = %s, want %s", positionStatus, PositionStatusEmpty)
	}
}

func TestFilledSellRejectedSchedulesCooldownAdjustment(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "SELL", 0.10)
	notified := make(chan time.Duration, 1)
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		notified <- delay
	})

	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       123,
		ClientOrderID: oid,
		Status:        "REJECTED",
	})

	delay := requireAdjustmentNotification(t, notified)
	if delay <= 0 || delay > placementRetryCooldown {
		t.Fatalf("filled SELL rejection adjustment delay = %s, want (0, %s]", delay, placementRetryCooldown)
	}
	slot.mu.RLock()
	retryAt := slot.placementRetryNotBefore
	positionStatus := slot.PositionStatus
	slot.mu.RUnlock()
	if retryAt.IsZero() || !retryAt.After(time.Now()) {
		t.Fatalf("filled SELL rejection retry deadline = %s, want future deadline", retryAt)
	}
	if positionStatus != PositionStatusFilled {
		t.Fatalf("filled SELL rejection position status = %s, want %s", positionStatus, PositionStatusFilled)
	}
}

func TestPartiallyFilledBuyRejectedRequestsImmediateAdjustment(t *testing.T) {
	spm, slot, oid := setupOrderSlot(t, "BUY", 0)
	notified := make(chan time.Duration, 1)
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		notified <- delay
	})

	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       123,
		ClientOrderID: oid,
		Status:        "REJECTED",
		ExecutedQty:   0.03,
		AvgPrice:      99,
	})

	select {
	case delay := <-notified:
		if delay > 0 {
			t.Fatalf("partially filled BUY rejection adjustment delay = %s, want immediate", delay)
		}
	default:
		t.Fatal("partially filled BUY rejection did not request an immediate adjustment")
	}

	slot.mu.RLock()
	retryAt := slot.placementRetryNotBefore
	positionStatus := slot.PositionStatus
	positionQty := slot.PositionQty
	slot.mu.RUnlock()
	if !retryAt.IsZero() {
		t.Fatalf("partially filled BUY rejection retry deadline = %s, want none", retryAt)
	}
	if positionStatus != PositionStatusFilled || positionQty != 0.03 {
		t.Fatalf("partially filled BUY rejection position = %s/%.12f, want FILLED/0.03",
			positionStatus, positionQty)
	}
}
