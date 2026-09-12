package position

import (
	"math"
	"testing"
	"time"

	"opensqt/exchange"
)

type makerPolicyExecutor struct {
	batches       [][]*OrderRequest
	rejectionKind string
	cancelIDs     []int64
}

func (e *makerPolicyExecutor) PlaceOrder(*OrderRequest) (*Order, error) { return nil, nil }

func (e *makerPolicyExecutor) BatchPlaceOrders(requests []*OrderRequest) ([]*Order, bool, error) {
	batch := append([]*OrderRequest(nil), requests...)
	e.batches = append(e.batches, batch)
	if e.rejectionKind != "" {
		for _, req := range requests {
			req.MarkDefiniteRejection(e.rejectionKind)
		}
	}
	return nil, false, nil
}

func (e *makerPolicyExecutor) BatchCancelOrders(orderIDs []int64) error {
	e.cancelIDs = append(e.cancelIDs, orderIDs...)
	return nil
}

func freshMarket(last, bid, ask float64, quoteVersion uint64) exchange.MarketSnapshot {
	return exchange.MarketSnapshot{
		Symbol: "ETHUSDT", LastPrice: last, BestBid: bid, BestAsk: ask,
		QuoteReceivedAt: time.Now(), ReceivedAt: time.Now(),
		QuoteVersion: quoteVersion, StreamEpoch: 1, Ready: true,
	}
}

func TestAdjustOrdersPlacesWhenBookLooksLikeQuantityNotPrice(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.Symbol = "BNBUSDC"
	cfg.Trading.PriceInterval = 1
	cfg.Trading.OrderQuantity = 100
	cfg.Trading.MinOrderValue = 20
	cfg.Trading.BuyWindowSize = 10
	cfg.Trading.SellWindowSize = 0
	cfg.Execution.CatchUpMode = "passive"
	cfg.Execution.MaxCatchUpDistanceRatio = 0.5

	executor := &makerPolicyExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 3, 2, 0.01)
	spm.anchorPrice = 736.58
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot {
		return freshMarket(736.61, 10.50, 12.30, 3)
	})

	if err := spm.AdjustOrders(736.61); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 1 || len(executor.batches[0]) < 8 {
		t.Fatalf("quantity-like bookTicker blocked the buy window: %+v", executor.batches)
	}
}

func TestAdjustOrdersPlacesBNBWindowBelowLiveAsk(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.Symbol = "BNBUSDC"
	cfg.Trading.PriceInterval = 1
	cfg.Trading.OrderQuantity = 100
	cfg.Trading.MinOrderValue = 20
	cfg.Trading.BuyWindowSize = 10
	cfg.Trading.SellWindowSize = 0
	cfg.Execution.CatchUpMode = "passive"

	executor := &makerPolicyExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 3, 2, 0.01)
	spm.anchorPrice = 736.58
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot {
		return freshMarket(736.47, 736.46, 736.48, 4)
	})

	if err := spm.AdjustOrders(736.47); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 1 || len(executor.batches[0]) < 8 {
		t.Fatalf("live BNB book should fill the buy window: %+v", executor.batches)
	}
}

func TestMakerCapReachableByBuyWindow(t *testing.T) {
	if !makerCapReachableByBuyWindow(727.58, 736.46, 1, 10) {
		t.Fatal("normal window below cap must be reachable")
	}
	if makerCapReachableByBuyWindow(727.58, 11.98, 1, 10) {
		t.Fatal("quantity-like cap far below the window must be unreachable")
	}
	if !makerCapReachableByBuyWindow(100, 94.99, 10, 1) {
		t.Fatal("catch-up just beyond half a grid must still be treated as the same market")
	}
}

func TestAdjustOrdersPlacesWhenBookTickerIsQuietButTradesAreLive(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 1
	cfg.Trading.BuyWindowSize = 3
	cfg.Trading.SellWindowSize = 0
	cfg.Execution.QuoteStaleMS = 1500
	cfg.Execution.CatchUpMode = "exact_wait"

	executor := &makerPolicyExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 3, 2, 0.01)
	spm.anchorPrice = 736.53
	now := time.Now()
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot {
		return exchange.MarketSnapshot{
			Symbol: "BNBUSDC", LastPrice: 736.61, BestBid: 736.60, BestAsk: 736.62,
			QuoteReceivedAt: now.Add(-7 * time.Second), ReceivedAt: now,
			QuoteVersion: 4, StreamEpoch: 1, Ready: true,
		}
	})

	if err := spm.AdjustOrders(736.61); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 1 || len(executor.batches[0]) == 0 {
		t.Fatalf("quiet bookTicker blocked live-stream buys: %+v", executor.batches)
	}
}

func TestPassiveCatchUpUsesMakerCapAndActualPriceForFixedNotional(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 10
	cfg.Trading.BuyWindowSize = 1
	cfg.Execution.MakerGuardTicks = 2
	cfg.Execution.CatchUpMode = "passive"
	cfg.Execution.MaxActiveCatchUpSlots = 1
	cfg.Execution.MaxCatchUpSlotsPerAdjust = 1
	cfg.Execution.MaxCatchUpDistanceRatio = 0.5

	executor := &makerPolicyExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 100
	market := freshMarket(100, 94.90, 95.02, 1)
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 1 || len(executor.batches[0]) != 1 {
		t.Fatalf("batches = %+v, want one catch-up request", executor.batches)
	}
	req := executor.batches[0][0]
	if !req.CatchUp || req.LogicalPrice != 100 || req.Price != 95 {
		t.Fatalf("catch-up request = %+v", req)
	}
	wantQty := spm.gridBuyQuantity(req.Price)
	if req.Quantity != wantQty || req.Quantity == spm.gridBuyQuantity(req.LogicalPrice) {
		t.Fatalf("quantity=%v want(actual price)=%v logical-price quantity=%v",
			req.Quantity, wantQty, spm.gridBuyQuantity(req.LogicalPrice))
	}
	if math.Abs(req.Price*req.Quantity-cfg.Trading.OrderQuantity) > req.Price*spm.quantityStep()/2+1e-9 {
		t.Fatalf("catch-up notional=%v is not quantity-step close to %v",
			req.Price*req.Quantity, cfg.Trading.OrderQuantity)
	}
}

func TestPassiveCatchUpHonorsDistanceAndActiveLimits(t *testing.T) {
	t.Run("distance", func(t *testing.T) {
		cfg := testConfig()
		cfg.Trading.PriceInterval = 10
		cfg.Trading.BuyWindowSize = 1
		cfg.Execution.MakerGuardTicks = 2
		cfg.Execution.CatchUpMode = "passive"
		cfg.Execution.MaxCatchUpDistanceRatio = 0.5
		executor := &makerPolicyExecutor{}
		spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
		spm.anchorPrice = 100
		market := freshMarket(100, 94.89, 95.01, 1) // cap=94.99，距离 5.01 > 半格
		spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })

		if err := spm.AdjustOrders(100); err != nil {
			t.Fatalf("AdjustOrders() error = %v", err)
		}
		if len(executor.batches) != 0 || spm.catchUpAbandoned.Load() != 1 {
			t.Fatalf("batches=%d abandoned=%d", len(executor.batches), spm.catchUpAbandoned.Load())
		}
	})

	t.Run("active slots", func(t *testing.T) {
		cfg := testConfig()
		cfg.Trading.PriceInterval = 10
		cfg.Trading.BuyWindowSize = 1
		cfg.Execution.MakerGuardTicks = 2
		cfg.Execution.CatchUpMode = "passive"
		cfg.Execution.MaxActiveCatchUpSlots = 1
		cfg.Execution.MaxCatchUpDistanceRatio = 0.5
		executor := &makerPolicyExecutor{}
		spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
		spm.anchorPrice = 100

		existing := spm.getOrCreateSlot(80)
		existing.OrderID = 88
		existing.ClientOID = spm.generateClientOrderID(80, "BUY")
		existing.OrderSide = "BUY"
		existing.OrderStatus = OrderStatusPlaced
		existing.OrderPrice = 79
		existing.OrderQuantity = 0.1
		existing.SlotStatus = SlotStatusLocked

		market := freshMarket(100, 94.90, 95.02, 1)
		spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })
		if err := spm.AdjustOrders(100); err != nil {
			t.Fatalf("AdjustOrders() error = %v", err)
		}
		if len(executor.batches) != 0 {
			t.Fatalf("new catch-up request bypassed active limit: %+v", executor.batches)
		}
		if len(executor.cancelIDs) != 1 || executor.cancelIDs[0] != 88 {
			t.Fatalf("out-of-window existing catch-up cancel IDs = %v", executor.cancelIDs)
		}
	})
}

func TestMakerMovedRetryWaitsForNewQuoteVersion(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 1
	cfg.Execution.MakerGuardTicks = 2
	cfg.Execution.CatchUpMode = "passive"
	cfg.Execution.MaxCatchUpDistanceRatio = 1
	executor := &makerPolicyExecutor{rejectionKind: "maker_quote_moved"}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 100
	market := freshMarket(100, 99.90, 100.02, 10)
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("first AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 1 {
		t.Fatalf("first batch count = %d", len(executor.batches))
	}
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("same-quote AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 1 {
		t.Fatalf("same QuoteVersion retried: batch count=%d", len(executor.batches))
	}

	market = freshMarket(100, 99.89, 100.01, 11)
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatalf("new-quote AdjustOrders() error = %v", err)
	}
	if len(executor.batches) != 2 {
		t.Fatalf("new QuoteVersion did not restore retry: batch count=%d", len(executor.batches))
	}
}

func TestPostOnlyRetryBackoffIsBoundedAndBurstsPause(t *testing.T) {
	cfg := testConfig()
	cfg.Execution.PostOnlyRetryMinMS = 50
	cfg.Execution.PostOnlyRetryMaxMS = 500
	cfg.Execution.PostOnlyRetryBurst = 5
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)

	wants := []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		500 * time.Millisecond,
		1 * time.Second,
	}
	for i, want := range wants {
		if got := spm.postOnlyRetryDelay(i + 1); got != want {
			t.Fatalf("consecutive=%d delay=%s want=%s", i+1, got, want)
		}
	}
}
