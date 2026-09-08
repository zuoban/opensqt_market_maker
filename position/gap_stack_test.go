package position

import (
	"testing"

	"opensqt/exchange"
)

type gapStackExecutor struct {
	orders    []*OrderRequest
	cancelIDs []int64
	nextID    int64
}

func (e *gapStackExecutor) PlaceOrder(*OrderRequest) (*Order, error) { return nil, nil }

func (e *gapStackExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	placed := make([]*Order, 0, len(requests))
	for _, req := range requests {
		release, ok := req.AcquireSubmissionLease()
		if !ok {
			continue
		}
		e.nextID++
		e.orders = append(e.orders, req)
		placed = append(placed, &Order{
			OrderID:       e.nextID,
			ClientOrderID: req.ClientOrderID,
			Symbol:        req.Symbol,
			Side:          req.Side,
			Price:         req.Price,
			Quantity:      req.Quantity,
			Status:        "NEW",
		})
		release()
	}
	return placed, false, nil
}

func (e *gapStackExecutor) BatchCancelOrders(orderIDs []int64) error {
	e.cancelIDs = append(e.cancelIDs, orderIDs...)
	return nil
}

func gapStackTestSetup(t *testing.T, executor OrderExecutorInterface) *SuperPositionManager {
	t.Helper()
	cfg := testConfig()
	cfg.Trading.PriceInterval = 0.10
	cfg.Trading.OrderQuantity = 30
	cfg.Trading.MinOrderValue = 6
	cfg.Trading.BuyWindowSize = 1
	cfg.Trading.SellWindowSize = 3
	cfg.Execution.MaxGapStackSlots = 1
	cfg.Execution.MakerGuardTicks = 2
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 103.75
	market := freshMarket(103.55, 103.54, 103.60, 1)
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })
	prepareFilledSellSlot(spm, 103.75, 0.29)
	return spm
}

func TestGapStackBuyDoublesQuantityWhenUpperGridSkipped(t *testing.T) {
	executor := &gapStackExecutor{}
	spm := gapStackTestSetup(t, executor)

	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}

	var buy *OrderRequest
	for _, req := range executor.orders {
		if req != nil && req.Side == "BUY" && sameGridPrice(req.LogicalPrice, 103.55) {
			buy = req
			break
		}
	}
	if buy == nil {
		t.Fatalf("missing 103.55 BUY, orders=%+v", executor.orders)
	}
	unit := spm.gridBuyQuantity(buy.Price)
	if buy.GapStack != 2 || buy.Quantity != roundPrice(unit*2, spm.quantityDecimals) {
		t.Fatalf("stacked buy = qty:%v gap:%d price:%v, want 2*%v", buy.Quantity, buy.GapStack, buy.Price, unit)
	}

	child := spm.getOrCreateSlot(103.65)
	child.mu.RLock()
	parent := child.stackedParentPrice
	child.mu.RUnlock()
	if parent != 103.55 {
		t.Fatalf("103.65 stackedParentPrice=%v, want 103.55", parent)
	}
}

func TestGapStackFillSplitsAndPlacesSellsAtSkippedGrids(t *testing.T) {
	executor := &gapStackExecutor{}
	spm := gapStackTestSetup(t, executor)

	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatalf("first AdjustOrders() error = %v", err)
	}
	var buy *OrderRequest
	for _, req := range executor.orders {
		if req != nil && req.Side == "BUY" && sameGridPrice(req.LogicalPrice, 103.55) {
			buy = req
			break
		}
	}
	if buy == nil {
		t.Fatal("expected stacked BUY")
	}
	parent := spm.getOrCreateSlot(103.55)
	spm.OnOrderUpdate(OrderUpdate{
		OrderID:       parent.OrderID,
		ClientOrderID: buy.ClientOrderID,
		Status:        "FILLED",
		Quantity:      buy.Quantity,
		ExecutedQty:   buy.Quantity,
		Price:         buy.Price,
		AvgPrice:      buy.Price,
		Side:          "BUY",
	})
	if parent.PositionQty != buy.Quantity || parent.gapStackCount != 2 {
		t.Fatalf("after fill qty=%v stack=%d, want qty=%v stack=2", parent.PositionQty, parent.gapStackCount, buy.Quantity)
	}

	before := len(executor.orders)
	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatalf("second AdjustOrders() error = %v", err)
	}

	child := spm.getOrCreateSlot(103.65)
	unit := buy.Quantity / 2
	if child.PositionStatus != PositionStatusFilled || child.PositionQty != unit {
		t.Fatalf("split child = status:%s qty:%v, want FILLED/%v", child.PositionStatus, child.PositionQty, unit)
	}
	if parent.PositionQty != unit {
		t.Fatalf("parent remaining qty=%v, want %v", parent.PositionQty, unit)
	}
	if child.stackedParentPrice != 0 {
		t.Fatalf("child still reserved by parent %v", child.stackedParentPrice)
	}

	sellBySlot := map[float64]float64{}
	for _, req := range executor.orders[before:] {
		if req != nil && req.Side == "SELL" {
			sellBySlot[req.LogicalPrice] = req.Price
		}
	}
	if price, ok := sellBySlot[103.55]; !ok || price < 103.65-fillQtyTolerance {
		t.Fatalf("103.55 inventory sell=%v, want >= 103.65: %+v", price, sellBySlot)
	}
	if price, ok := sellBySlot[103.65]; !ok || price < 103.75-fillQtyTolerance {
		t.Fatalf("103.65 inventory sell=%v, want >= 103.75: %+v", price, sellBySlot)
	}
}

func TestGapStackDoesNotStackWhenUpperGridAlreadyFilled(t *testing.T) {
	executor := &gapStackExecutor{}
	spm := gapStackTestSetup(t, executor)
	prepareFilledSellSlot(spm, 103.65, 0.29)

	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	var buy *OrderRequest
	for _, req := range executor.orders {
		if req != nil && req.Side == "BUY" && sameGridPrice(req.LogicalPrice, 103.55) {
			buy = req
			break
		}
	}
	if buy == nil {
		t.Fatal("expected BUY at current grid")
	}
	unit := spm.gridBuyQuantity(buy.Price)
	if buy.GapStack > 1 || buy.Quantity != unit {
		t.Fatalf("buy = qty:%v gap:%d, want single grid %v", buy.Quantity, buy.GapStack, unit)
	}
}

func TestGapStackDisabledWhenMaxSlotsZero(t *testing.T) {
	executor := &gapStackExecutor{}
	spm := gapStackTestSetup(t, executor)
	spm.config.Execution.MaxGapStackSlots = 0

	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	var buy *OrderRequest
	for _, req := range executor.orders {
		if req != nil && req.Side == "BUY" && sameGridPrice(req.LogicalPrice, 103.55) {
			buy = req
			break
		}
	}
	if buy == nil {
		t.Fatal("expected BUY at current grid")
	}
	if buy.GapStack > 1 {
		t.Fatalf("disabled gap stack still merged: %+v", buy)
	}
}

func TestGapStackCancelsUndersizedLiveBuy(t *testing.T) {
	executor := &gapStackExecutor{}
	spm := gapStackTestSetup(t, executor)
	slot := spm.getOrCreateSlot(103.55)
	slot.OrderID = 77
	slot.ClientOID = spm.generateClientOrderID(103.55, "BUY")
	slot.OrderSide = "BUY"
	slot.OrderStatus = OrderStatusConfirmed
	slot.OrderPrice = 103.55
	slot.OrderQuantity = spm.gridBuyQuantity(103.55)
	slot.SlotStatus = SlotStatusLocked

	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.cancelIDs) != 1 || executor.cancelIDs[0] != 77 {
		t.Fatalf("cancel IDs = %v, want [77]", executor.cancelIDs)
	}
	if slot.OrderStatus != OrderStatusCancelRequested {
		t.Fatalf("undersized live buy status = %s, want CANCEL_REQUESTED", slot.OrderStatus)
	}
}

func TestGapStackCancelsPreexistingCurrentBuyAfterUpperBuysLeaveWindow(t *testing.T) {
	executor := &gapStackExecutor{}
	spm := gapStackTestSetup(t, executor)
	spm.config.Execution.MaxGapStackSlots = 5
	spm.anchorPrice = 103.27
	market := freshMarket(102.67, 102.66, 102.72, 2)
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })

	type liveBuy struct {
		price    float64
		orderID  int64
		clientID string
		quantity float64
	}
	liveBuys := make([]liveBuy, 0, 6)
	for i := 0; i <= 5; i++ {
		price := roundPrice(102.67+float64(i)*0.10, spm.priceDecimals)
		slot := spm.getOrCreateSlot(price)
		slot.OrderID = int64(100 + i)
		slot.ClientOID = spm.generateClientOrderID(price, "BUY")
		slot.OrderSide = "BUY"
		slot.OrderStatus = OrderStatusConfirmed
		slot.OrderPrice = price
		slot.OrderQuantity = spm.gridBuyQuantity(price)
		slot.SlotStatus = SlotStatusLocked
		liveBuys = append(liveBuys, liveBuy{
			price:    price,
			orderID:  slot.OrderID,
			clientID: slot.ClientOID,
			quantity: slot.OrderQuantity,
		})
	}

	if err := spm.AdjustOrders(102.67); err != nil {
		t.Fatalf("first AdjustOrders() error = %v", err)
	}
	if len(executor.cancelIDs) != len(liveBuys) {
		t.Fatalf("cancel IDs = %v, want current buy and five skipped buys", executor.cancelIDs)
	}
	current := spm.getOrCreateSlot(102.67)
	if current.OrderStatus != OrderStatusCancelRequested {
		t.Fatalf("current buy status = %s, want CANCEL_REQUESTED", current.OrderStatus)
	}

	for _, buy := range liveBuys {
		spm.OnOrderUpdate(OrderUpdate{
			OrderID:       buy.orderID,
			ClientOrderID: buy.clientID,
			Status:        "CANCELED",
			Quantity:      buy.quantity,
			ExecutedQty:   0,
			Price:         buy.price,
			Side:          "BUY",
		})
	}

	if err := spm.AdjustOrders(102.67); err != nil {
		t.Fatalf("second AdjustOrders() error = %v", err)
	}
	var replacement *OrderRequest
	for _, req := range executor.orders {
		if req != nil && req.Side == "BUY" && sameGridPrice(req.LogicalPrice, 102.67) {
			replacement = req
			break
		}
	}
	if replacement == nil {
		t.Fatalf("missing replacement BUY, orders=%+v", executor.orders)
	}
	unit := spm.gridBuyQuantity(replacement.Price)
	if replacement.GapStack != 6 || replacement.Quantity != roundPrice(unit*6, spm.quantityDecimals) {
		t.Fatalf("replacement = qty:%v gap:%d, want six slots (%v each)",
			replacement.Quantity, replacement.GapStack, unit)
	}
}

func TestGapStackFailedReservationReleasesChild(t *testing.T) {
	executor := &recordingExecutor{}
	spm := gapStackTestSetup(t, executor)

	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	var buy *OrderRequest
	for _, req := range executor.orders {
		if req != nil && req.Side == "BUY" && sameGridPrice(req.LogicalPrice, 103.55) {
			buy = req
			break
		}
	}
	if buy == nil || buy.GapStack != 2 {
		t.Fatalf("expected stacked request, got %+v", buy)
	}
	child := spm.getOrCreateSlot(103.65)
	if child.stackedParentPrice != 0 {
		t.Fatalf("failed reservation left child reserved by %v", child.stackedParentPrice)
	}
	parent := spm.getOrCreateSlot(103.55)
	if parent.SlotStatus == SlotStatusPending {
		t.Fatal("failed stacked buy left parent pending")
	}
}
