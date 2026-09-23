package position

import (
	"math"
	"testing"
	"time"

	"opensqt/exchange"
)

func TestPriceJumpKeepsOneGridNotionalPerBuy(t *testing.T) {
	for _, price := range []float64{9.558, 9.258, 8.958, 9.858} {
		t.Run(formatPrice(price, 3), func(t *testing.T) {
			cfg := testConfig()
			cfg.Trading.PriceInterval = .1
			cfg.Trading.OrderQuantity = 1000
			cfg.Trading.BuyWindowSize, cfg.Trading.SellWindowSize = 3, 0
			executor := &recordingExecutor{}
			spm := NewSuperPositionManager(cfg, executor, stubEx{}, 3, 0, .001)
			spm.anchorPrice = 9.558
			market := freshMarket(9.558, 9.557, 9.559, 1)
			spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })
			// Observe an earlier planning cycle with no buy window, then jump.
			cfg.Trading.BuyWindowSize = 0
			if err := spm.AdjustOrders(9.558); err != nil {
				t.Fatal(err)
			}
			cfg.Trading.BuyWindowSize = 3
			market = freshMarket(price, price-.001, price+.001, 2)
			if err := spm.AdjustOrders(price); err != nil {
				t.Fatal(err)
			}
			if len(executor.orders) != 3 {
				t.Fatalf("orders=%d, want three ordinary grid orders", len(executor.orders))
			}
			for i, req := range executor.orders {
				want := roundPrice(price-float64(i+1)*.1, 3)
				if req.Side != "BUY" || req.Price != want || req.LogicalPrice != want || !req.PostOnly {
					t.Fatalf("unexpected grid request: %+v, want price %v", req, want)
				}
				if math.Abs(req.Price*req.Quantity-1000) > req.Price/2+1e-9 {
					t.Fatalf("buy notional=%v, want one 1000 USDC grid", req.Price*req.Quantity)
				}
			}
		})
	}
}

func TestCrossedBuyKeepsOriginalPriceAndQuantity(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 10
	cfg.Trading.BuyWindowSize, cfg.Trading.SellWindowSize = 1, 0
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, .01)
	spm.anchorPrice = 100
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot {
		return freshMarket(100, 84.90, 85.02, 1)
	})
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatal(err)
	}
	if len(executor.orders) != 1 {
		t.Fatalf("orders=%+v", executor.orders)
	}
	req := executor.orders[0]
	if req.Price != 90 || req.Quantity != spm.gridBuyQuantity(90) || !req.PostOnly {
		t.Fatalf("crossed buy was repriced or resized: %+v", req)
	}
}

func TestExistingInventoryIsNotSplitByOrderSize(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize, cfg.Trading.SellWindowSize = 0, 3
	executor := &recordingExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, .01)
	spm.anchorPrice = 100
	prepareFilledSellSlot(spm, 100, 3)
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatal(err)
	}
	if len(executor.orders) != 1 {
		t.Fatalf("existing slot was split into %d orders", len(executor.orders))
	}
	req := executor.orders[0]
	if req.Side != "SELL" || req.Quantity != 3 || req.Price != 101 || !req.ReduceOnly || !req.PostOnly {
		t.Fatalf("existing inventory changed: %+v", req)
	}
}

func TestPostOnlyRejectionUsesOrdinaryCooldownAndOriginalTarget(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize, cfg.Trading.SellWindowSize = 1, 0
	executor := &makerPolicyExecutor{rejectionKind: "post_only"}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, .01)
	spm.anchorPrice = 100
	for i := 0; i < 7; i++ {
		before := time.Now()
		if err := spm.AdjustOrders(100); err != nil {
			t.Fatal(err)
		}
		if len(executor.batches) != i+1 {
			t.Fatalf("batches=%d, want %d", len(executor.batches), i+1)
		}
		req := executor.batches[i][0]
		if req.Price != 99 || req.Quantity != spm.gridBuyQuantity(99) || !req.PostOnly {
			t.Fatalf("retry changed grid order: %+v", req)
		}
		slot := spm.getOrCreateSlot(99)
		if slot.placementRetryNotBefore.Before(before.Add(placementRetryCooldown)) ||
			slot.placementRetryNotBefore.After(time.Now().Add(placementRetryCooldown)) {
			t.Fatalf("retry did not use ordinary cooldown: %v", slot.placementRetryNotBefore)
		}
		if err := spm.AdjustOrders(100); err != nil {
			t.Fatal(err)
		}
		if len(executor.batches) != i+1 {
			t.Fatal("rejection immediately resubmitted")
		}
		slot.mu.Lock()
		slot.placementRetryNotBefore = time.Now().Add(-time.Second)
		slot.mu.Unlock()
	}
}

func TestSubmittedReservationRetainsIdentityWhenRetryInventoryChanges(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	slot := prepareFilledSellSlot(spm, 100, .3)
	req := &OrderRequest{Side: "SELL", Price: 101, Quantity: .3, ReduceOnly: true,
		PostOnly: true, ClientOrderID: spm.generateClientOrderID(100, "SELL")}
	slot.mu.Lock()
	spm.reserveOrderLocked(slot, req)
	slot.mu.Unlock()
	release, ok := req.AcquireSubmissionLease()
	if !ok {
		t.Fatal("initial reservation rejected")
	}
	req.OnSubmissionStarted()
	release()
	slot.mu.Lock()
	slot.PositionQty = .2 // A correction arrived during read-only confirmation.
	slot.mu.Unlock()
	if release, ok := req.AcquireSubmissionLease(); ok {
		release()
		t.Fatal("stale quantity reached a retry")
	}
	if slot.ClientOID != req.ClientOrderID || slot.SlotStatus != SlotStatusPending {
		t.Fatal("an already-submitted request lost its uncertain identity")
	}
}
