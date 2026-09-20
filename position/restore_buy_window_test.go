package position

import (
	"testing"

	"opensqt/exchange"
)

func TestBuyWindowStartsOneGridBelowCurrent(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 25
	cfg.Trading.BuyWindowSize = 5
	cfg.Trading.MinOrderValue = 6
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 4000

	got := spm.buyWindowPrices(4000, 5)
	want := []float64{3975, 3950, 3925, 3900, 3875}
	if len(got) != len(want) {
		t.Fatalf("buy window = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buy window = %v, want %v", got, want)
		}
	}
}

func TestEmptyStartDoesNotBuyCurrentGrid(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 1
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 0
	cfg.Trading.MinOrderValue = 6
	executor := &makerPolicyExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 100
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot {
		return freshMarket(100, 99.90, 100.02, 1)
	})

	if err := spm.AdjustOrders(100); err != nil {
		t.Fatal(err)
	}
	if len(executor.batches) != 1 {
		t.Fatalf("batches = %+v", executor.batches)
	}
	for _, req := range executor.batches[0] {
		if req.Side == "BUY" && sameGridPrice(req.LogicalPrice, 100) {
			t.Fatalf("empty start bought current grid: %+v", req)
		}
	}
	if !hasBuyAt(executor.batches[0], 99) {
		t.Fatalf("empty start missing next-grid BUY: %+v", executor.batches[0])
	}
}

func TestSamePriceRestoreOccupiesCurrentGridAndDoesNotRebuyIt(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 1
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 2
	cfg.Trading.MinOrderValue = 6
	executor := &makerPolicyExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 100
	spm.lastMarketPrice.Store(100.0)

	if err := spm.initializeSellSlotsFromPosition(0.30, 100); err != nil {
		t.Fatal(err)
	}
	slot := spm.getOrCreateSlot(100)
	if slot.PositionStatus != PositionStatusFilled || slot.PositionQty != 0.3 {
		t.Fatalf("restored slot 100 = status:%s qty:%v", slot.PositionStatus, slot.PositionQty)
	}
	if spm.getOrCreateSlot(101).PositionQty != 0 {
		t.Fatal("same-price restore still occupied a slot above the market")
	}

	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot {
		return freshMarket(100, 99.90, 100.02, 1)
	})
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatal(err)
	}
	for _, batch := range executor.batches {
		for _, req := range batch {
			if req.Side == "BUY" && sameGridPrice(req.LogicalPrice, 100) {
				t.Fatalf("restart rebought current grid: %+v", req)
			}
		}
	}
}

func TestRalliedRestoreKeepsEntryGridAndAllowsLowerBuys(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 1
	cfg.Trading.BuyWindowSize = 2
	cfg.Trading.SellWindowSize = 2
	cfg.Trading.MinOrderValue = 6
	executor := &makerPolicyExecutor{}
	spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3, 0.01)
	spm.anchorPrice = 100
	spm.lastMarketPrice.Store(110.0)

	if err := spm.initializeSellSlotsFromPosition(0.30, 100); err != nil {
		t.Fatal(err)
	}
	if spm.getOrCreateSlot(100).PositionStatus != PositionStatusFilled {
		t.Fatal("rallied restore should occupy the entry grid")
	}
	if spm.getOrCreateSlot(110).PositionQty != 0 {
		t.Fatal("rallied restore occupied the current grid as if it had already been bought")
	}

	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot {
		return freshMarket(110, 109.90, 110.02, 1)
	})
	if err := spm.AdjustOrders(110); err != nil {
		t.Fatal(err)
	}
	var buys []*OrderRequest
	for _, batch := range executor.batches {
		for _, req := range batch {
			if req.Side == "BUY" {
				buys = append(buys, req)
				if sameGridPrice(req.LogicalPrice, 110) {
					t.Fatalf("rallied restart bought current grid: %+v", req)
				}
			}
		}
	}
	if !hasBuyAt(buys, 109) {
		t.Fatalf("rallied restart missing next-grid BUY: %+v", buys)
	}
}

func hasBuyAt(orders []*OrderRequest, price float64) bool {
	for _, req := range orders {
		if req != nil && req.Side == "BUY" && sameGridPrice(req.LogicalPrice, price) {
			return true
		}
	}
	return false
}
