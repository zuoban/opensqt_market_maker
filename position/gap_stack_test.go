package position

import (
	"testing"
	"time"

	"opensqt/exchange"
)

type gapStackExecutor struct {
	orders      []*OrderRequest
	cancelIDs   []int64
	nextID      int64
	beforeLease func(*OrderRequest)
}

func (e *gapStackExecutor) PlaceOrder(*OrderRequest) (*Order, error) { return nil, nil }

func (e *gapStackExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	placed := make([]*Order, 0, len(requests))
	for _, req := range requests {
		if e.beforeLease != nil {
			e.beforeLease(req)
		}
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
	observeGapStackBaseline(t, spm, 103.75)
	market := freshMarket(103.55, 103.54, 103.60, 1)
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })
	prepareFilledSellSlot(spm, 103.75, 0.29)
	return spm
}

// 通过真实规划入口建立观测起点，不预先下单或伪造跨格状态。
func observeGapStackBaseline(t *testing.T, spm *SuperPositionManager, price float64) {
	t.Helper()
	buyWindow, sellWindow := spm.config.Trading.BuyWindowSize, spm.config.Trading.SellWindowSize
	spm.config.Trading.BuyWindowSize, spm.config.Trading.SellWindowSize = 0, 0
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot {
		return freshMarket(price, price-0.01, price+0.05, 1)
	})
	if err := spm.AdjustOrders(price); err != nil {
		t.Fatalf("baseline AdjustOrders(%v): %v", price, err)
	}
	spm.config.Trading.BuyWindowSize, spm.config.Trading.SellWindowSize = buyWindow, sellWindow
}

func TestGapStackRequiresObservedDownwardJump(t *testing.T) {
	for _, tc := range []struct {
		name      string
		previous  []float64
		max       int
		epoch     uint64
		wantCount int
		wantQty   float64
	}{
		{"startup with empty upper grids", nil, 2, 1, 1, 0.14},
		{"unchanged grid", []float64{736.78}, 2, 1, 1, 0.14},
		{"one grid drop", []float64{737.78}, 2, 1, 1, 0.14},
		{"successive one grid drops", []float64{739.78, 738.78, 737.78}, 2, 1, 1, 0.14},
		{"upward move", []float64{734.78}, 2, 1, 1, 0.14},
		{"two grid drop", []float64{738.78}, 2, 1, 2, 0.28},
		{"three grid drop", []float64{739.78}, 2, 1, 3, 0.42},
		{"configured extra grid cap", []float64{741.78}, 1, 1, 2, 0.28},
		{"disabled", []float64{739.78}, 0, 1, 1, 0.14},
		{"reconnected stream", []float64{739.78}, 2, 2, 1, 0.14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Trading.Symbol = "BNBUSDC"
			cfg.Trading.PriceInterval = 1
			cfg.Trading.OrderQuantity = 100
			cfg.Trading.MinOrderValue = 6
			cfg.Trading.BuyWindowSize, cfg.Trading.SellWindowSize = 1, 0
			cfg.Execution.MaxGapStackSlots = tc.max
			executor := &gapStackExecutor{}
			spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 2, 0.01)
			spm.anchorPrice = 739.78
			for _, price := range tc.previous {
				observeGapStackBaseline(t, spm, price)
			}
			market := freshMarket(736.78, 736.77, 736.83, 2)
			market.StreamEpoch = tc.epoch
			spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })
			if err := spm.AdjustOrders(736.78); err != nil {
				t.Fatal(err)
			}
			if len(executor.orders) != 1 {
				t.Fatalf("orders=%+v, want one BUY", executor.orders)
			}
			buy := executor.orders[0]
			if buy.Side != "BUY" || buy.GapStack != tc.wantCount || buy.Quantity != tc.wantQty {
				t.Fatalf("buy side=%s qty=%v count=%d, want BUY/%v/%d",
					buy.Side, buy.Quantity, buy.GapStack, tc.wantQty, tc.wantCount)
			}
			if err := spm.AdjustOrders(736.78); err != nil {
				t.Fatal(err)
			}
			if len(executor.orders) != 1 || len(executor.cancelIDs) != 0 {
				t.Fatal("same-grid adjustment resubmitted or resized the existing BUY")
			}
		})
	}
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
	wantCost := unit * buy.Price
	if mathAbs(child.PositionCost-wantCost) > 1e-12 || mathAbs(parent.PositionCost-wantCost) > 1e-12 {
		t.Fatalf("split costs = child:%v parent:%v, want %v each",
			child.PositionCost, parent.PositionCost, wantCost)
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
	// 同一格内拆出的仓位全部卖完，也只能恢复普通一格买单，不能复用旧跌幅。
	for _, req := range executor.orders[before:] {
		if req.Side != "SELL" {
			continue
		}
		slot := spm.getOrCreateSlot(req.LogicalPrice)
		spm.OnOrderUpdate(OrderUpdate{
			OrderID: slot.OrderID, ClientOrderID: req.ClientOrderID, Status: "FILLED",
			Quantity: req.Quantity, ExecutedQty: req.Quantity, Price: req.Price, AvgPrice: req.Price, Side: "SELL",
		})
	}
	before = len(executor.orders)
	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatal(err)
	}
	var rebought bool
	for _, req := range executor.orders[before:] {
		if req.Side == "BUY" && req.LogicalPrice == 103.55 {
			rebought = true
			if req.GapStack != 1 || req.Quantity != unit {
				t.Fatalf("sold inventory caused repeated stacking: %+v", req)
			}
		}
	}
	if !rebought {
		t.Fatal("missing ordinary BUY after sells")
	}
}

func TestGapStackRechecksChildBeforeSubmission(t *testing.T) {
	executor := &gapStackExecutor{}
	spm := gapStackTestSetup(t, executor)
	oldID := spm.generateClientOrderID(103.65, "BUY")
	executor.beforeLease = func(req *OrderRequest) {
		if req.Side == "BUY" && req.GapStack > 1 {
			// 规划后、真实提交前收到旧买单迟到成交，不能仍提交双倍数量。
			spm.OnOrderUpdate(OrderUpdate{
				OrderID: 700, ClientOrderID: oldID, Status: "FILLED", Side: "BUY",
				Quantity: 0.29, ExecutedQty: 0.29, Price: 103.65, AvgPrice: 103.65,
			})
		}
	}
	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatal(err)
	}
	for _, req := range executor.orders {
		if req.Side == "BUY" {
			t.Fatal("submitted stacked BUY after its child filled")
		}
	}
	parent := spm.getOrCreateSlot(103.55)
	if parent.ClientOID != "" || parent.SlotStatus != SlotStatusFree {
		t.Fatal("invalidated parent reservation was not released")
	}
	child := spm.getOrCreateSlot(103.65)
	if child.PositionQty != 0.29 {
		t.Fatalf("lost late child fill: %v", child.PositionQty)
	}
	if child.stackedParentPrice != 0 {
		t.Fatal("invalidated parent left the filled child reserved")
	}
	executor.beforeLease = nil
	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatal(err)
	}
	for _, req := range executor.orders {
		if req.Side == "BUY" && req.GapStack > 1 {
			t.Fatal("late child fill was counted as a missing grid on retry")
		}
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
	observeGapStackBaseline(t, spm, 103.27)
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
	// 先确认当前格撤销。上方旧单仍有成交可能，不能提前创建合并买单。
	first := liveBuys[0]
	spm.OnOrderUpdate(OrderUpdate{
		OrderID: first.orderID, ClientOrderID: first.clientID, Status: "CANCELED",
		Quantity: first.quantity, Price: first.price, Side: "BUY",
	})
	spm.clearAdjustFingerprint()
	if err := spm.AdjustOrders(102.67); err != nil {
		t.Fatal(err)
	}
	for _, req := range executor.orders {
		if req.Side == "BUY" {
			t.Fatal("submitted replacement before upper BUY cancellations were confirmed")
		}
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
	// 明确未提交的重试保留真实跨格证据，清除规划指纹不会制造或丢掉格数。
	parent.placementRetryNotBefore = time.Time{}
	spm.clearAdjustFingerprint()
	before := len(executor.orders)
	if err := spm.AdjustOrders(103.55); err != nil {
		t.Fatal(err)
	}
	var retried bool
	for _, req := range executor.orders[before:] {
		if req.Side == "BUY" && req.LogicalPrice == 103.55 {
			retried = true
			if req.GapStack != 2 || req.Quantity != buy.Quantity {
				t.Fatalf("retry changed proven gap quantity: %+v", req)
			}
		}
	}
	if !retried {
		t.Fatal("missing BUY retry")
	}
}
