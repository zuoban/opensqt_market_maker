package position

import (
	"errors"
	"sort"
	"testing"
	"time"
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

func TestMarginLockKeepsExistingBuysAndStillPlacesReduceOnlySells(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 3
	cfg.Trading.SellWindowSize = 0
	cfg.Trading.MarginLockDurationSec = 30

	executor := &marginThenAcceptExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
	spm.anchorPrice = 100

	existingBuy := spm.getOrCreateSlot(97)
	existingBuy.OrderID = 77
	existingBuy.ClientOID = spm.generateClientOrderID(97, "BUY")
	existingBuy.OrderSide = "BUY"
	existingBuy.OrderStatus = OrderStatusPlaced
	existingBuy.OrderPrice = 97
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

	// 换一个行情网格，确保锁定期内本来有新 BUY 候选；同时增加一个
	// 已持仓槽位。第二批应只有 ReduceOnly SELL。
	cfg.Trading.SellWindowSize = 2
	filled := spm.getOrCreateSlot(100)
	filled.PositionStatus = PositionStatusFilled
	filled.PositionQty = 0.1

	if err := spm.AdjustOrders(102); err != nil {
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
	if existingBuy.OrderID != 77 || existingBuy.OrderStatus != OrderStatusPlaced ||
		existingBuy.SlotStatus != SlotStatusLocked {
		t.Fatalf("existing buy changed during margin lock: %+v", existingBuy)
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
	if first.Price <= 110 {
		t.Fatalf("crossed sell price = %.2f, want above current price 110", first.Price)
	}
	if first.Price < 101 {
		t.Fatalf("repriced sell %.2f fell below grid target 101", first.Price)
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

func TestAdjustOrdersAllocatesDistinctTicksForCrossedSellSlots(t *testing.T) {
	tests := []struct {
		name          string
		currentPrice  float64
		priceInterval float64
		tickSize      float64
		priceDecimals int
		slotPrices    []float64
		quantity      float64
		wantTicks     []int64
	}{
		{
			name:          "screenshot SOL parameters",
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
			currentPrice:  100,
			priceInterval: 1,
			tickSize:      0.25,
			priceDecimals: 2,
			slotPrices:    []float64{97, 98, 99},
			quantity:      0.1,
			wantTicks:     []int64{401, 402, 403},
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
			spm.anchorPrice = tt.currentPrice

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
	spm.anchorPrice = currentPrice

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
			spm.anchorPrice = currentPrice

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
				t.Fatalf("new SELL tick = %d, want 10387 after occupied 10386", priceTick)
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
