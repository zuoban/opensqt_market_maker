package position

import (
	"errors"
	"math"
	"sort"
	"testing"
	"time"

	"opensqt/exchange"
	orderpkg "opensqt/order"
)

type marginThenAcceptExecutor struct {
	batches     [][]*OrderRequest
	nextOrderID int64
	cancelCalls int
}

func (e *marginThenAcceptExecutor) PlaceOrder(*OrderRequest) (*Order, error) {
	return nil, nil
}

func (e *marginThenAcceptExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	e.batches = append(e.batches, append([]*OrderRequest(nil), requests...))
	if len(e.batches) == 1 {
		for _, req := range requests {
			release, ok := req.AcquireSubmissionLease()
			if ok {
				release()
			}
		}
		return nil, true, nil
	}

	placed := make([]*Order, 0, len(requests))
	for _, req := range requests {
		release, ok := req.AcquireSubmissionLease()
		if !ok {
			continue
		}
		release()
		e.nextOrderID++
		placed = append(placed, &Order{
			OrderID:       e.nextOrderID,
			ClientOrderID: req.ClientOrderID,
			Symbol:        req.Symbol,
			Side:          req.Side,
			Type:          "LIMIT",
			Price:         req.Price,
			Quantity:      req.Quantity,
			Status:        "NEW",
		})
	}
	return placed, false, nil
}

func (e *marginThenAcceptExecutor) BatchCancelOrders([]int64) error {
	e.cancelCalls++
	return nil
}

type reduceOnlySellMarginExecutor struct {
	requests []*OrderRequest
	err      error
}

func (e *reduceOnlySellMarginExecutor) PlaceOrder(*OrderRequest) (*Order, error) {
	return nil, e.err
}

func (e *reduceOnlySellMarginExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	e.requests = append(e.requests, requests...)
	for _, req := range requests {
		release, ok := req.AcquireSubmissionLease()
		if ok {
			release()
		}
	}
	return nil, true, e.err
}

func (e *reduceOnlySellMarginExecutor) BatchCancelOrders([]int64) error { return nil }

func TestReduceOnlySellMarginRejectionPropagatesAndPreservesInventory(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 0
	cfg.Trading.SellWindowSize = 1

	marginRejected := orderpkg.NewOrderRejectedError(
		orderpkg.OrderRejectionMargin,
		errors.New("insufficient margin"),
	)
	criticalErr := errors.Join(orderpkg.ErrReduceOnlySellMarginRejected, marginRejected)
	executor := &reduceOnlySellMarginExecutor{err: criticalErr}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	slot := spm.getOrCreateSlot(100)
	slot.PositionStatus = PositionStatusFilled
	slot.PositionQty = 0.1

	type notification struct {
		delay           time.Duration
		managerUnlocked bool
	}
	notified := make(chan notification, 4)
	spm.SetAdjustmentNotifier(func(delay time.Duration) {
		unlocked := spm.mu.TryLock()
		if unlocked {
			spm.mu.Unlock()
		}
		notified <- notification{delay: delay, managerUnlocked: unlocked}
	})

	err := spm.AdjustOrders(100)
	if !errors.Is(err, orderpkg.ErrReduceOnlySellMarginRejected) {
		t.Fatalf("AdjustOrders() error = %v, want ErrReduceOnlySellMarginRejected", err)
	}
	if orderpkg.IsDefiniteOrderRejection(err) {
		t.Fatalf("AdjustOrders() error = %v, want non-definite critical failure", err)
	}
	var rejected *orderpkg.OrderRejectedError
	if !errors.As(err, &rejected) || rejected.Kind != orderpkg.OrderRejectionMargin {
		t.Fatalf("AdjustOrders() rejection = %#v, want margin kind", rejected)
	}
	if len(executor.requests) != 1 || executor.requests[0].Side != "SELL" ||
		!executor.requests[0].ReduceOnly {
		t.Fatalf("submitted requests = %+v, want one ReduceOnly SELL", executor.requests)
	}

	slot.mu.RLock()
	positionStatus := slot.PositionStatus
	positionQty := slot.PositionQty
	slotStatus := slot.SlotStatus
	orderID := slot.OrderID
	clientOID := slot.ClientOID
	retryAt := slot.placementRetryNotBefore
	slot.mu.RUnlock()
	if positionStatus != PositionStatusFilled || positionQty != 0.1 {
		t.Fatalf("inventory after rejection = status:%s qty:%v, want FILLED/0.1", positionStatus, positionQty)
	}
	if slotStatus != SlotStatusFree || orderID != 0 || clientOID != "" {
		t.Fatalf("reservation after rejection = slot:%s order:%d client:%q, want FREE/0/empty",
			slotStatus, orderID, clientOID)
	}
	if retryAt.IsZero() || !retryAt.After(time.Now()) {
		t.Fatalf("retry deadline = %s, want future cooldown", retryAt)
	}
	if !spm.insufficientMargin {
		t.Fatal("critical SELL margin rejection did not activate the BUY margin lock")
	}

	var notifications []notification
	for {
		select {
		case got := <-notified:
			notifications = append(notifications, got)
		default:
			goto notificationsDrained
		}
	}

notificationsDrained:
	shortRetrySeen := false
	marginUnlockSeen := false
	for _, got := range notifications {
		if !got.managerUnlocked {
			t.Fatal("retry notifier ran while manager lock was held")
		}
		if got.delay > 0 && got.delay <= placementRetryCooldown {
			shortRetrySeen = true
		}
		if got.delay > placementRetryCooldown && got.delay <= spm.marginLockDuration {
			marginUnlockSeen = true
		}
	}
	if !shortRetrySeen {
		t.Fatalf("notifications = %+v, want slot retry within %s", notifications, placementRetryCooldown)
	}
	if !marginUnlockSeen {
		t.Fatalf("notifications = %+v, want margin unlock within %s", notifications, spm.marginLockDuration)
	}
}

func TestMarginLockKeepsExistingBuysAndStillPlacesReduceOnlySells(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 3
	cfg.Trading.SellWindowSize = 0
	cfg.Trading.MarginLockDurationSec = 30

	executor := &marginThenAcceptExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	existingBuy := spm.getOrCreateSlot(99)
	existingBuy.OrderID = 77
	existingBuy.ClientOID = spm.generateClientOrderID(99, "BUY")
	existingBuy.OrderSide = "BUY"
	existingBuy.OrderStatus = OrderStatusPlaced
	existingBuy.OrderPrice = 99
	existingBuy.OrderQuantity = 0.3
	existingBuy.SlotStatus = SlotStatusLocked

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("first AdjustOrders() error = %v", err)
	}
	if !spm.insufficientMargin {
		t.Fatal("margin error did not activate the buy placement lock")
	}
	if executor.cancelCalls != 0 {
		t.Fatalf("margin error canceled existing buys: cancel calls = %d", executor.cancelCalls)
	}
	if existingBuy.OrderID != 77 || existingBuy.OrderStatus != OrderStatusPlaced ||
		existingBuy.SlotStatus != SlotStatusLocked {
		t.Fatalf("existing buy was mutated after margin error: %+v", existingBuy)
	}

	// 跨过下沿滞回带，确保锁定期内本来有新 BUY 候选；同时增加一个
	// 已持仓槽位。第二批应只有 ReduceOnly SELL。
	cfg.Trading.SellWindowSize = 2
	filled := spm.getOrCreateSlot(100)
	filled.PositionStatus = PositionStatusFilled
	filled.PositionQty = 0.1

	if err := spm.AdjustOrders(103); err != nil {
		t.Fatalf("second AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 2 {
		t.Fatalf("placement batches = %d, want 2", len(executor.batches))
	}
	if len(executor.batches[1]) == 0 {
		t.Fatal("margin lock suppressed the reduce-only sell batch")
	}
	for _, req := range executor.batches[1] {
		if req.Side != "SELL" || !req.ReduceOnly || !req.PostOnly {
			t.Fatalf("order submitted during margin lock = %+v, want PostOnly ReduceOnly SELL", req)
		}
	}
	if existingBuy.OrderID != 77 || existingBuy.OrderStatus != OrderStatusCancelRequested {
		t.Fatalf("buy that left the window was not marked for cancel: %+v", existingBuy)
	}
	if executor.cancelCalls == 0 {
		t.Fatal("grid move during margin lock did not cancel the out-of-window buy")
	}
}

func TestAdjustOrdersRepricesCrossedSellAndSubmitsReduceOnlyFirst(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1

	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	filled := spm.getOrCreateSlot(100)
	filled.PositionStatus = PositionStatusFilled
	filled.PositionQty = 0.1

	if err := spm.AdjustOrders(110); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.orders) < 2 {
		t.Fatalf("submitted orders = %d, want both SELL and BUY", len(executor.orders))
	}
	first := executor.orders[0]
	if first.Side != "SELL" || !first.ReduceOnly || !first.PostOnly {
		t.Fatalf("first submitted order = %+v, want PostOnly ReduceOnly SELL", first)
	}
	if first.Price != 110.1 {
		t.Fatalf("crossed sell price = %.2f, want nearest fallback Maker price 110.10", first.Price)
	}
	if executor.orders[1].Side != "BUY" {
		t.Fatalf("second submitted side = %s, want BUY after reduce-only sells", executor.orders[1].Side)
	}
}

type failThenSucceedExecutor struct {
	fail        bool
	calls       int
	nextOrderID int64
	wantErr     error
}

func (e *failThenSucceedExecutor) PlaceOrder(*OrderRequest) (*Order, error) {
	return nil, nil
}

func (e *failThenSucceedExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	e.calls++
	placed := make([]*Order, 0, len(requests))
	for _, req := range requests {
		release, ok := req.AcquireSubmissionLease()
		if !ok {
			continue
		}
		release()
		if e.fail {
			continue
		}
		e.nextOrderID++
		placed = append(placed, &Order{
			OrderID:       e.nextOrderID,
			ClientOrderID: req.ClientOrderID,
			Symbol:        req.Symbol,
			Side:          req.Side,
			Price:         req.Price,
			Quantity:      req.Quantity,
			Status:        "NEW",
		})
	}
	if e.fail {
		return nil, false, e.wantErr
	}
	return placed, false, nil
}

func (e *failThenSucceedExecutor) BatchCancelOrders([]int64) error { return nil }

func TestDefinitePlacementFailureUsesPerSlotRetryCooldown(t *testing.T) {
	wantErr := errors.New("definite reject")
	executor := &failThenSucceedExecutor{fail: true, wantErr: wantErr}
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 0
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	err := spm.AdjustOrders(100)
	if !errors.Is(err, wantErr) {
		t.Fatalf("first AdjustOrders() error = %v, want definite rejection", err)
	}
	slot := spm.getOrCreateSlot(99)
	if slot.SlotStatus != SlotStatusFree || slot.ClientOID != "" ||
		!slot.placementRetryNotBefore.After(time.Now()) {
		t.Fatalf("failed reservation did not enter retry cooldown: %+v", slot)
	}

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("second AdjustOrders() error = %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("immediate price tick retried failed slot: executor calls = %d", executor.calls)
	}

	// 无需在测试中真实等待一秒：模拟冷却已到期，下一个 reservation
	// 应能正常创建，成功后不应残留失败冷却。
	slot.mu.Lock()
	slot.placementRetryNotBefore = time.Now().Add(-time.Millisecond)
	slot.mu.Unlock()
	executor.fail = false

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("third AdjustOrders() error = %v", err)
	}
	if executor.calls != 2 {
		t.Fatalf("expired cooldown did not retry: executor calls = %d", executor.calls)
	}
	if slot.OrderID == 0 || slot.SlotStatus != SlotStatusLocked ||
		!slot.placementRetryNotBefore.IsZero() {
		t.Fatalf("successful retry retained stale cooldown or was not bound: %+v", slot)
	}
}

func TestSellCandidateScanDoesNotClearExpiredCooldownUnderRLock(t *testing.T) {
	expired := time.Now().Add(-time.Millisecond)
	slot := &InventorySlot{
		PositionStatus:          PositionStatusFilled,
		PositionQty:             0.1,
		SlotStatus:              SlotStatusFree,
		placementRetryNotBefore: expired,
	}
	slot.mu.RLock()
	if !sellCandidateEligibleLocked(slot, time.Now()) {
		slot.mu.RUnlock()
		t.Fatal("expired cooldown should still be sell-eligible")
	}
	slot.mu.RUnlock()
	if !slot.placementRetryNotBefore.Equal(expired) {
		t.Fatal("planning scan cleared placementRetryNotBefore under RLock")
	}

	slot.mu.Lock()
	if !placementRetryReadyLocked(slot, time.Now()) {
		slot.mu.Unlock()
		t.Fatal("write path should accept expired cooldown")
	}
	slot.mu.Unlock()
	if !slot.placementRetryNotBefore.IsZero() {
		t.Fatal("write path did not clear expired cooldown")
	}
}

func TestOrderThresholdPrioritizesValidReduceOnlySellOverBuy(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.OrderCleanupThreshold = 1
	cfg.Trading.MinOrderValue = 6

	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	buySlot := spm.getOrCreateSlot(99)
	filledSlot := spm.getOrCreateSlot(100)
	filledSlot.PositionStatus = PositionStatusFilled
	filledSlot.PositionQty = 0.1

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.orders) != 1 {
		t.Fatalf("submitted orders = %d, want exactly one SELL", len(executor.orders))
	}
	order := executor.orders[0]
	if order.Side != "SELL" || !order.PostOnly || !order.ReduceOnly {
		t.Fatalf("submitted order = %+v, want PostOnly ReduceOnly SELL", order)
	}
	if buySlot.SlotStatus != SlotStatusFree || buySlot.OrderID != 0 || buySlot.ClientOID != "" {
		t.Fatalf("BUY slot consumed despite SELL priority: %+v", buySlot)
	}
}

func TestInvalidSellCandidateLeavesThresholdCapacityForBuy(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.OrderCleanupThreshold = 1
	cfg.Trading.MinOrderValue = 20

	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	filledSlot := spm.getOrCreateSlot(100)
	filledSlot.PositionStatus = PositionStatusFilled
	filledSlot.PositionQty = 0.1 // 卖单名义价值约 10.1，低于 20。

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.orders) != 1 {
		t.Fatalf("submitted orders = %d, want exactly one BUY", len(executor.orders))
	}
	order := executor.orders[0]
	if order.Side != "BUY" || !order.PostOnly || order.ReduceOnly {
		t.Fatalf("submitted order = %+v, want PostOnly BUY", order)
	}
}

func prepareFilledSellSlot(spm *SuperPositionManager, price, quantity float64) *InventorySlot {
	slot := spm.getOrCreateSlot(price)
	slot.PositionStatus = PositionStatusFilled
	slot.PositionQty = quantity
	slot.SlotStatus = SlotStatusFree
	return slot
}

func recordedSellOrders(orders []*OrderRequest) []*OrderRequest {
	sells := make([]*OrderRequest, 0, len(orders))
	for _, order := range orders {
		if order != nil && order.Side == "SELL" {
			sells = append(sells, order)
		}
	}
	return sells
}

func assertSellRequestInvariants(
	t *testing.T,
	spm *SuperPositionManager,
	order *OrderRequest,
	currentPrice, priceInterval, tickSize float64,
) (float64, int64) {
	t.Helper()
	if order == nil {
		t.Fatal("recorded SELL request is nil")
	}
	if !order.PostOnly || !order.ReduceOnly {
		t.Fatalf("SELL request = %+v, want PostOnly ReduceOnly", order)
	}
	if order.Price <= currentPrice {
		t.Fatalf("SELL price = %.12f, want above current %.12f", order.Price, currentPrice)
	}
	slotPrice, side, valid := spm.parseClientOrderID(order.ClientOrderID)
	if !valid || side != "SELL" {
		t.Fatalf("ClientOrderID = %q, parsed price=%v side=%q valid=%v",
			order.ClientOrderID, slotPrice, side, valid)
	}
	targetPrice := slotPrice + priceInterval
	if order.Price+fillQtyTolerance < targetPrice {
		t.Fatalf("SELL price = %.12f below slot %.12f target %.12f",
			order.Price, slotPrice, targetPrice)
	}
	priceTick, ok := priceTickIndexUp(order.Price, tickSize)
	if !ok {
		t.Fatalf("SELL price %.12f has no valid tick index for tick %.12f", order.Price, tickSize)
	}
	if got := roundPrice(float64(priceTick)*tickSize, spm.priceDecimals); got != order.Price {
		t.Fatalf("SELL price %.12f is not aligned to tick %.12f (quantized %.12f)",
			order.Price, tickSize, got)
	}
	return slotPrice, priceTick
}

func TestValidateGridPriceInterval(t *testing.T) {
	tests := []struct {
		name          string
		priceInterval float64
		tickSize      float64
		wantErr       bool
	}{
		{name: "SOL grid", priceInterval: 0.02, tickSize: 0.01},
		{name: "non power of ten tick", priceInterval: 1, tickSize: 0.25},
		{name: "floating point integer ratio", priceInterval: 0.3, tickSize: 0.1},
		{name: "fractional tick ratio", priceInterval: 0.015, tickSize: 0.01, wantErr: true},
		{name: "interval below tick", priceInterval: 0.005, tickSize: 0.01, wantErr: true},
		{name: "zero interval", tickSize: 0.01, wantErr: true},
		{name: "invalid tick", priceInterval: 0.02, tickSize: math.NaN(), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGridPriceInterval(tt.priceInterval, tt.tickSize)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateGridPriceInterval(%v, %v) error = %v, wantErr %v",
					tt.priceInterval, tt.tickSize, err, tt.wantErr)
			}
		})
	}
}

func TestAllocateMakerSafeSellPriceRejectsUnrepresentableGrid(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 4, 2, 0.01)
	spm.anchorPrice = 100
	price, priceTick, ok := spm.allocateMakerSafeSellPrice(100.03, 100.02, 0.015, nil)
	if ok || price != 0 || priceTick != 0 {
		t.Fatalf("allocateMakerSafeSellPrice() = (%v, %v, %v), want fail-closed zero result",
			price, priceTick, ok)
	}
}

func TestCrossedSellUsesNearestLiveMakerTick(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 0
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.PriceInterval = 0.1
	cfg.Trading.MinOrderValue = 1
	cfg.Execution.MakerGuardTicks = 2
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 4, 2, 0.01)
	spm.anchorPrice = 104.7
	prepareFilledSellSlot(spm, 104.7, 1.91)

	market := freshMarket(104.94, 104.93, 104.94, 1)
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })

	if err := spm.AdjustOrders(104.94); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	sells := recordedSellOrders(executor.orders)
	if len(sells) != 1 {
		t.Fatalf("recorded SELL requests = %d, want 1: %+v", len(sells), sells)
	}
	if sells[0].Price != 104.95 {
		t.Fatalf("crossed SELL price = %.4f, want nearest Maker tick 104.9500", sells[0].Price)
	}
	if !sells[0].PostOnly || !sells[0].ReduceOnly {
		t.Fatalf("crossed SELL lost safety flags: %+v", sells[0])
	}
}

func TestInitializeSnapsMarketPriceToExchangeTick(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 0.02
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 4, 2, 0.01)

	if err := spm.Initialize(104.423, "104.423"); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if spm.anchorPrice != 104.42 {
		t.Fatalf("anchor price = %.12f, want nearest valid tick 104.42", spm.anchorPrice)
	}
	if got := spm.lastMarketPrice.Load().(float64); got != 104.423 {
		t.Fatalf("last market price = %.12f, want raw market price 104.423", got)
	}
}

func TestHighTickIndexGridPriceDoesNotSkipLevel(t *testing.T) {
	const (
		anchorPrice   = 35.41357750
		priceInterval = 0.0001
		tickSize      = 0.00000001
	)
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 8, 4, tickSize)
	spm.anchorPrice = anchorPrice
	targetPrice := anchorPrice + priceInterval

	price, priceTick, ok := spm.allocateMakerSafeSellPrice(
		targetPrice,
		anchorPrice,
		priceInterval,
		nil,
	)
	if !ok {
		t.Fatal("allocateMakerSafeSellPrice() rejected a representable high tick-index grid")
	}
	anchorTick, _ := priceTickIndexNearest(anchorPrice, tickSize)
	wantTick := anchorTick + 10000
	if priceTick != wantTick || price != roundPrice(targetPrice, 8) {
		t.Fatalf("allocated = price %.12f tick %d, want target %.12f tick %d",
			price, priceTick, roundPrice(targetPrice, 8), wantTick)
	}
}

func TestAdjustOrdersAllocatesNearestDistinctMakerTicksForCrossedSellSlots(t *testing.T) {
	tests := []struct {
		name          string
		anchorPrice   float64
		currentPrice  float64
		priceInterval float64
		tickSize      float64
		priceDecimals int
		slotPrices    []float64
		quantity      float64
		wantTicks     []int64
	}{
		{
			name:          "wide interval uses nearest ticks",
			anchorPrice:   100,
			currentPrice:  103.81,
			priceInterval: 0.5,
			tickSize:      0.01,
			priceDecimals: 4,
			slotPrices:    []float64{102, 102.5, 103},
			quantity:      0.48,
			wantTicks:     []int64{10386, 10387, 10388},
		},
		{
			name:          "non power of ten tick",
			anchorPrice:   100,
			currentPrice:  100,
			priceInterval: 1,
			tickSize:      0.25,
			priceDecimals: 2,
			slotPrices:    []float64{97, 98, 99},
			quantity:      0.1,
			wantTicks:     []int64{401, 402, 403},
		},
		{
			name:          "SOL crossed exits use nearest ticks",
			anchorPrice:   104.38,
			currentPrice:  104.42,
			priceInterval: 0.02,
			tickSize:      0.01,
			priceDecimals: 4,
			slotPrices:    []float64{104.38, 104.40},
			quantity:      0.10,
			wantTicks:     []int64{10443, 10444},
		},
		{
			name:          "non-zero grid phase does not delay crossed exit",
			anchorPrice:   100.03,
			currentPrice:  100.06,
			priceInterval: 0.02,
			tickSize:      0.01,
			priceDecimals: 4,
			slotPrices:    []float64{100.03, 100.05},
			quantity:      0.10,
			wantTicks:     []int64{10007, 10008},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Trading.BuyWindowSize = 0
			cfg.Trading.SellWindowSize = len(tt.slotPrices)
			cfg.Trading.PriceInterval = tt.priceInterval
			cfg.Trading.MinOrderValue = 1
			executor := &recordingExecutor{}
			spm := NewSuperPositionManager(
				cfg, executor, stubEx{}, tt.priceDecimals, 3, tt.tickSize,
			)
			spm.anchorPrice = tt.anchorPrice

			wantSlots := make(map[string]struct{}, len(tt.slotPrices))
			for _, slotPrice := range tt.slotPrices {
				prepareFilledSellSlot(spm, slotPrice, tt.quantity)
				wantSlots[formatPrice(slotPrice, tt.priceDecimals)] = struct{}{}
			}

			if err := spm.AdjustOrders(tt.currentPrice); err != nil {
				t.Fatalf("AdjustOrders() error = %v", err)
			}
			sells := recordedSellOrders(executor.orders)
			if len(sells) != len(tt.slotPrices) {
				t.Fatalf("recorded SELL requests = %d, want %d: %+v",
					len(sells), len(tt.slotPrices), sells)
			}

			seenClientOIDs := make(map[string]struct{}, len(sells))
			seenSlots := make(map[string]struct{}, len(sells))
			seenTicks := make(map[int64]struct{}, len(sells))
			gotTicks := make([]int64, 0, len(sells))
			for _, order := range sells {
				slotPrice, priceTick := assertSellRequestInvariants(
					t, spm, order, tt.currentPrice, tt.priceInterval, tt.tickSize,
				)
				if _, exists := seenClientOIDs[order.ClientOrderID]; exists {
					t.Fatalf("duplicate ClientOrderID %q", order.ClientOrderID)
				}
				seenClientOIDs[order.ClientOrderID] = struct{}{}
				slotKey := formatPrice(slotPrice, tt.priceDecimals)
				if _, expected := wantSlots[slotKey]; !expected {
					t.Fatalf("ClientOrderID %q maps to unexpected slot %s", order.ClientOrderID, slotKey)
				}
				seenSlots[slotKey] = struct{}{}
				if _, exists := seenTicks[priceTick]; exists {
					t.Fatalf("multiple SELL requests collapse to tick %d", priceTick)
				}
				seenTicks[priceTick] = struct{}{}
				gotTicks = append(gotTicks, priceTick)
			}
			if len(seenSlots) != len(wantSlots) {
				t.Fatalf("covered slots = %v, want %v", seenSlots, wantSlots)
			}
			sort.Slice(gotTicks, func(i, j int) bool { return gotTicks[i] < gotTicks[j] })
			if len(gotTicks) != len(tt.wantTicks) {
				t.Fatalf("ticks = %v, want %v", gotTicks, tt.wantTicks)
			}
			for i := range gotTicks {
				if gotTicks[i] != tt.wantTicks[i] {
					t.Fatalf("ticks = %v, want %v", gotTicks, tt.wantTicks)
				}
			}
		})
	}
}

func TestAdjustOrdersDoesNotReusePlacedLockedSellTick(t *testing.T) {
	const (
		currentPrice  = 103.81
		priceInterval = 0.5
		tickSize      = 0.01
	)
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 0
	cfg.Trading.SellWindowSize = 2
	cfg.Trading.PriceInterval = priceInterval
	cfg.Trading.MinOrderValue = 1
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 4, 3, tickSize)
	spm.anchorPrice = 100

	occupied := prepareFilledSellSlot(spm, 103.5, 0.48)
	occupied.OrderID = 77
	occupied.ClientOID = spm.generateClientOrderID(occupied.Price, "SELL")
	occupied.OrderSide = "SELL"
	occupied.OrderStatus = OrderStatusPlaced
	occupied.OrderPrice = 103.86
	occupied.OrderQuantity = occupied.PositionQty
	occupied.SlotStatus = SlotStatusLocked
	occupiedClientOID := occupied.ClientOID
	prepareFilledSellSlot(spm, 102.5, 0.48)
	prepareFilledSellSlot(spm, 103, 0.48)

	if err := spm.AdjustOrders(currentPrice); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	sells := recordedSellOrders(executor.orders)
	if len(sells) != 2 {
		t.Fatalf("recorded SELL requests = %d, want 2: %+v", len(sells), sells)
	}
	seenTicks := map[int64]struct{}{10386: {}}
	for _, order := range sells {
		_, priceTick := assertSellRequestInvariants(
			t, spm, order, currentPrice, priceInterval, tickSize,
		)
		if _, exists := seenTicks[priceTick]; exists {
			t.Fatalf("new SELL reused occupied or batch tick %d", priceTick)
		}
		seenTicks[priceTick] = struct{}{}
	}
	for _, wantTick := range []int64{10387, 10388} {
		if _, exists := seenTicks[wantTick]; !exists {
			t.Fatalf("allocated ticks = %v, missing %d", seenTicks, wantTick)
		}
	}
	if occupied.OrderID != 77 || occupied.ClientOID != occupiedClientOID ||
		occupied.OrderStatus != OrderStatusPlaced || occupied.SlotStatus != SlotStatusLocked ||
		occupied.OrderPrice != 103.86 {
		t.Fatalf("occupied SELL was mutated: %+v", occupied)
	}
}

func TestAdjustOrdersRefreshesSellPriceOccupancyAfterCandidateScan(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 0
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.PriceInterval = 1
	cfg.Trading.MinOrderValue = 1
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 100

	remote := prepareFilledSellSlot(spm, 99, 0.1)
	prepareFilledSellSlot(spm, 100, 0.1)
	remoteClientOID := spm.generateClientOrderID(remote.Price, "SELL")

	// Deterministically bind a remote SELL after the initial occupancy scan but
	// while AdjustOrders is still planning. The new candidate at slot 100 must
	// observe that late update before reserving its own sell price.
	spm.afterSellCandidateScan = func() {
		done := make(chan struct{})
		go func() {
			spm.OnOrderUpdate(OrderUpdate{
				OrderID:       92,
				ClientOrderID: remoteClientOID,
				Status:        "NEW",
				Price:         101,
				Quantity:      0.1,
			})
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent order update did not complete")
		}
	}

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	sells := recordedSellOrders(executor.orders)
	if len(sells) != 1 {
		t.Fatalf("recorded SELL requests = %d, want 1: %+v", len(sells), sells)
	}
	if sells[0].Price != 101.01 {
		t.Fatalf("new SELL price = %v, want 101.01 above concurrently occupied 101", sells[0].Price)
	}
	if remote.OrderID != 92 || remote.OrderPrice != 101 ||
		remote.OrderStatus != OrderStatusConfirmed || remote.SlotStatus != SlotStatusLocked {
		t.Fatalf("concurrent remote SELL was not preserved: %+v", remote)
	}
}

func TestAdjustOrdersTreatsPendingAndCancelRequestedSellPricesAsOccupied(t *testing.T) {
	const (
		currentPrice  = 103.81
		priceInterval = 0.5
		tickSize      = 0.01
	)
	tests := []struct {
		name        string
		orderID     int64
		orderStatus string
		slotStatus  string
	}{
		{
			name:        "pending reservation",
			orderStatus: OrderStatusNotPlaced,
			slotStatus:  SlotStatusPending,
		},
		{
			name:        "cancel requested remote order",
			orderID:     88,
			orderStatus: OrderStatusCancelRequested,
			slotStatus:  SlotStatusLocked,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Trading.BuyWindowSize = 0
			cfg.Trading.SellWindowSize = 1
			cfg.Trading.PriceInterval = priceInterval
			cfg.Trading.MinOrderValue = 1
			executor := &recordingExecutor{}
			spm := NewSuperPositionManager(cfg, executor, stubEx{}, 4, 3, tickSize)
			spm.anchorPrice = 100

			occupied := prepareFilledSellSlot(spm, 103.5, 0.48)
			occupied.OrderID = tt.orderID
			occupied.ClientOID = spm.generateClientOrderID(occupied.Price, "SELL")
			occupied.OrderSide = "SELL"
			occupied.OrderStatus = tt.orderStatus
			occupied.OrderPrice = 103.86
			occupied.OrderQuantity = occupied.PositionQty
			occupied.SlotStatus = tt.slotStatus
			occupiedClientOID := occupied.ClientOID
			prepareFilledSellSlot(spm, 103, 0.48)

			if err := spm.AdjustOrders(currentPrice); err != nil {
				t.Fatalf("AdjustOrders() error = %v", err)
			}
			sells := recordedSellOrders(executor.orders)
			if len(sells) != 1 {
				t.Fatalf("recorded SELL requests = %d, want 1: %+v", len(sells), sells)
			}
			_, priceTick := assertSellRequestInvariants(
				t, spm, sells[0], currentPrice, priceInterval, tickSize,
			)
			if priceTick != 10387 {
				t.Fatalf("new SELL tick = %d, want 10387 above legacy off-grid price 10386", priceTick)
			}
			if occupied.OrderID != tt.orderID || occupied.ClientOID != occupiedClientOID ||
				occupied.OrderStatus != tt.orderStatus || occupied.SlotStatus != tt.slotStatus ||
				occupied.OrderPrice != 103.86 {
				t.Fatalf("occupied SELL reservation was mutated: %+v", occupied)
			}
		})
	}
}

func TestOrderThresholdCountsUncertainAndCancelRequestedReservations(t *testing.T) {
	tests := []struct {
		name        string
		orderID     int64
		orderStatus string
		slotStatus  string
	}{
		{
			name:        "unknown pending reservation",
			orderStatus: OrderStatusNotPlaced,
			slotStatus:  SlotStatusPending,
		},
		{
			name:        "cancel requested remote order",
			orderID:     91,
			orderStatus: OrderStatusCancelRequested,
			slotStatus:  SlotStatusLocked,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Trading.BuyWindowSize = 0
			cfg.Trading.SellWindowSize = 1
			cfg.Trading.OrderCleanupThreshold = 1
			cfg.Trading.MinOrderValue = 1
			executor := &recordingExecutor{}
			spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
			spm.anchorPrice = 100

			occupied := prepareFilledSellSlot(spm, 99, 0.1)
			occupied.OrderID = tt.orderID
			occupied.ClientOID = spm.generateClientOrderID(occupied.Price, "SELL")
			occupied.OrderSide = "SELL"
			occupied.OrderStatus = tt.orderStatus
			occupied.OrderPrice = 101
			occupied.OrderQuantity = occupied.PositionQty
			occupied.SlotStatus = tt.slotStatus
			prepareFilledSellSlot(spm, 100, 0.1)

			if err := spm.AdjustOrders(100); err != nil {
				t.Fatalf("AdjustOrders() error = %v", err)
			}
			if len(executor.orders) != 0 {
				t.Fatalf("submitted orders = %+v, want threshold occupied by existing reservation", executor.orders)
			}
		})
	}
}

func TestUnboundNewSellUpdateLocksSlotAndPreventsReplacement(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 0
	cfg.Trading.SellWindowSize = 1
	cfg.Trading.MinOrderValue = 1
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 100

	slot := prepareFilledSellSlot(spm, 100, 0.1)
	clientOID := spm.generateClientOrderID(slot.Price, "SELL")
	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       92,
		ClientOrderID: clientOID,
		Status:        "NEW",
		Price:         101.25,
		Quantity:      0.1,
	})

	if slot.OrderID != 92 || slot.ClientOID != clientOID || slot.OrderSide != "SELL" ||
		slot.OrderStatus != OrderStatusConfirmed || slot.OrderPrice != 101.25 ||
		slot.OrderQuantity != 0.1 || slot.SlotStatus != SlotStatusLocked {
		t.Fatalf("unbound remote SELL was not bound fail-closed: %+v", slot)
	}
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.orders) != 0 {
		t.Fatalf("remote SELL was overwritten by new reservation: %+v", executor.orders)
	}
}

func TestGridBuyQuantityRoundsUpToMeetMinNotional(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.OrderQuantity = 20
	cfg.Trading.MinOrderValue = 20
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 4, 2)

	quantity := spm.gridBuyQuantity(103.79)
	if quantity != 0.2 {
		t.Fatalf("gridBuyQuantity(103.79) = %.12f, want 0.20 (round-half 20/103.79 is 0.19)", quantity)
	}
	if quantity*103.79 < 20 {
		t.Fatalf("buy notional = %.12f, want >= 20", quantity*103.79)
	}
	if got := spm.gridBuyQuantity(100); got != 0.2 {
		t.Fatalf("gridBuyQuantity(100) = %.12f, want 0.20", got)
	}
}

func TestReduceOnlyNotionalAllowsQuantityRoundingShortfall(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.MinOrderValue = 20
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 4, 2)

	if spm.reduceOnlyNotionalTooSmall(103.89, 0.19) {
		t.Fatal("0.19 inventory at 103.89 is only one qty step short of min and must still sell")
	}
	if !spm.reduceOnlyNotionalTooSmall(103.89, 0.10) {
		t.Fatal("0.10 inventory at 103.89 is true dust relative to min 20 and should skip")
	}
}

func TestAdjustOrdersPlacesSellAfterRoundedMinNotionalBuyFill(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 0
	cfg.Trading.SellWindowSize = 3
	cfg.Trading.PriceInterval = 0.05
	cfg.Trading.OrderQuantity = 20
	cfg.Trading.MinOrderValue = 20
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 4, 2, 0.01)
	spm.anchorPrice = 103.79

	for _, slotPrice := range []float64{103.74, 103.79, 103.84} {
		prepareFilledSellSlot(spm, slotPrice, 0.19)
	}

	if err := spm.AdjustOrders(103.79); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	sells := recordedSellOrders(executor.orders)
	if len(sells) != 3 {
		t.Fatalf("recorded SELL requests = %d, want 3: %+v", len(sells), sells)
	}
	for _, order := range sells {
		if order.Quantity != 0.19 {
			t.Fatalf("SELL quantity = %.12f, want filled inventory 0.19", order.Quantity)
		}
		if order.Price*order.Quantity >= 20 {
			continue
		}
		if order.Price*(order.Quantity+0.01) < 20 {
			t.Fatalf("SELL %+v is below min even after one qty step", order)
		}
	}
}

func TestAdjustOrdersBuyQuantityMeetsMinNotionalAfterRounding(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 0
	cfg.Trading.PriceInterval = 0.05
	cfg.Trading.OrderQuantity = 20
	cfg.Trading.MinOrderValue = 20
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 4, 2, 0.01)
	spm.anchorPrice = 103.79

	if err := spm.AdjustOrders(103.79); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.orders) == 0 {
		t.Fatal("expected BUY orders below current price")
	}
	for _, order := range executor.orders {
		if order.Side != "BUY" {
			t.Fatalf("order = %+v, want BUY", order)
		}
		if order.Quantity != 0.2 {
			t.Fatalf("BUY quantity = %.12f at %.4f, want 0.20 so notional stays >= 20",
				order.Quantity, order.Price)
		}
		if order.Quantity*order.Price < 20 {
			t.Fatalf("BUY notional = %.12f at %.4f, want >= 20",
				order.Quantity*order.Price, order.Price)
		}
	}
}
