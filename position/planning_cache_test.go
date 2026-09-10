package position

import (
	"testing"
	"time"

	"opensqt/exchange"
)

func TestUnchangedQuotePricesSkipOnlyWithoutPendingMakerRetry(t *testing.T) {
	for _, rejection := range []string{"", "maker_quote_moved", "post_only", "market_data_stale"} {
		t.Run("rejection="+rejection, func(t *testing.T) {
			cfg := testConfig()
			cfg.Trading.BuyWindowSize = 1
			executor := &makerPolicyExecutor{rejectionKind: rejection}
			spm := NewSuperPositionManager(cfg, executor, stubEx{}, 2, 3)
			spm.anchorPrice = 100
			market := freshMarket(100, 99.90, 100.02, 10)
			spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })
			if err := spm.AdjustOrders(100); err != nil {
				t.Fatal(err)
			}
			if len(executor.batches) != 1 {
				t.Fatalf("initial batches = %d, want 1", len(executor.batches))
			}
			market.QuoteVersion++ // Only sizes/sequence changed; bid/ask remain equal.
			if got, want := spm.ShouldSkipUnchangedGrid(100), rejection == ""; got != want {
				t.Fatalf("ShouldSkipUnchangedGrid = %v, want %v", got, want)
			}
		})
	}
}

func TestAdjustFingerprintUsesPlannedQuoteInsteadOfLaterQuote(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.BuyWindowSize = 0
	spm := NewSuperPositionManager(cfg, stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	market := freshMarket(100, 99.90, 100.02, 10)
	spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })
	spm.afterSellCandidateScan = func() {
		market.BestBid = 99.95
		market.QuoteVersion++
	}
	if err := spm.AdjustOrders(100); err != nil {
		t.Fatal(err)
	}
	if spm.ShouldSkipUnchangedGrid(100) {
		t.Fatal("quote arriving during planning must trigger another adjustment")
	}
}

func TestSamePriceQuoteStillReplansOnBookAndEpochChanges(t *testing.T) {
	tests := []struct {
		name   string
		change func(*exchange.MarketSnapshot)
	}{
		{"bid", func(m *exchange.MarketSnapshot) { m.BestBid += 0.01 }},
		{"ask", func(m *exchange.MarketSnapshot) { m.BestAsk += 0.01 }},
		{"epoch", func(m *exchange.MarketSnapshot) { m.StreamEpoch++ }},
		{"reset", func(m *exchange.MarketSnapshot) { m.Ready = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
			spm.anchorPrice = 100
			market := freshMarket(100, 99.90, 100.02, 10)
			spm.SetMarketSnapshotProvider(func() exchange.MarketSnapshot { return market })
			spm.storeAdjustFingerprint(100, 100)
			tt.change(&market)
			if spm.ShouldSkipUnchangedGrid(100) {
				t.Fatal("changed quote boundary/connection state was skipped")
			}
		})
	}
}

func TestSellOccupancyRefreshIncludesActiveOrderUpdate(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.anchorPrice = 100
	slot := prepareFilledSellSlot(spm, 100, 0.1)
	clientOID := spm.generateClientOrderID(100, "SELL")
	slot.mu.Lock()
	locked := true
	defer func() {
		if locked {
			slot.mu.Unlock()
		}
	}()
	updated := make(chan struct{})
	go func() {
		spm.OnOrderUpdate(OrderUpdate{
			OrderID: 20, ClientOrderID: clientOID, Status: "NEW", Price: 101, Quantity: 0.1,
		})
		close(updated)
	}()
	deadline := time.Now().Add(time.Second)
	for spm.orderUpdatesActive.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("order update did not start")
		}
		time.Sleep(time.Millisecond)
	}

	// No update has finished yet: the generation is unchanged. An active writer
	// must nevertheless prevent reuse of an earlier occupancy snapshot.
	version := spm.orderUpdateVersion.Load()
	occupied := make(map[int64]struct{})
	refreshed := make(chan struct{})
	go func() {
		spm.refreshOccupiedSellPriceTicks(occupied, &version)
		close(refreshed)
	}()
	select {
	case <-refreshed:
		t.Fatal("refresh skipped an in-flight update")
	case <-time.After(10 * time.Millisecond):
	}
	slot.mu.Unlock()
	locked = false
	<-updated
	<-refreshed
	// If the scan acquired the slot first, the completed update changes the
	// generation and the next candidate must re-read it.
	spm.refreshOccupiedSellPriceTicks(occupied, &version)
	tick, _ := priceTickIndexUp(101, spm.priceTickSize)
	if _, ok := occupied[tick]; !ok {
		t.Fatal("concurrent remote sell price was not reserved")
	}
}
