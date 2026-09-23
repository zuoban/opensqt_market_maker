package position

import (
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

func TestAdjustOrdersPlacesWhenBookTickerIsQuietButTradesAreLive(t *testing.T) {
	cfg := testConfig()
	cfg.Trading.PriceInterval = 1
	cfg.Trading.BuyWindowSize = 3
	cfg.Trading.SellWindowSize = 0

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
