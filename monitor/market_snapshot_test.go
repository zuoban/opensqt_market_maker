package monitor

import (
	"testing"
	"time"

	"opensqt/exchange"
)

func marketUpdateTimes() (time.Time, time.Time) {
	receivedAt := time.Unix(1_700_000_100, 0)
	return receivedAt.Add(-time.Millisecond), receivedAt
}

func TestMarketSnapshotRequiresTradeAndBookFromCurrentEpoch(t *testing.T) {
	pm := NewPriceMonitor(nil, "ETHUSDT", 5)
	eventTime, receivedAt := marketUpdateTimes()

	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", LastPrice: 2010.25, EventTime: eventTime,
		ReceivedAt: receivedAt, StreamEpoch: 1,
	})
	if got := pm.GetMarketSnapshot(); got.Ready {
		t.Fatalf("trade-only snapshot unexpectedly ready: %+v", got)
	}

	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", BestBid: 2010.20, BestAsk: 2010.30,
		EventTime: eventTime, ReceivedAt: receivedAt, QuoteVersion: 1, StreamEpoch: 1,
	})
	got := pm.GetMarketSnapshot()
	if !got.Ready || got.LastPrice != 2010.25 || got.BestBid != 2010.20 || got.BestAsk != 2010.30 ||
		got.QuoteVersion != 1 || got.StreamEpoch != 1 || !got.QuoteReceivedAt.Equal(receivedAt) {
		t.Fatalf("merged snapshot = %+v", got)
	}
}

func TestMarketResetInvalidatesOldEpochWithoutInheritance(t *testing.T) {
	pm := NewPriceMonitor(nil, "ETHUSDT", 5)
	_, receivedAt := marketUpdateTimes()
	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", LastPrice: 100, ReceivedAt: receivedAt, StreamEpoch: 1,
	})
	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", BestBid: 99, BestAsk: 101, ReceivedAt: receivedAt,
		QuoteVersion: 10, StreamEpoch: 1,
	})

	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", ReceivedAt: receivedAt.Add(time.Second), StreamEpoch: 2, Reset: true,
	})
	reset := pm.GetMarketSnapshot()
	if reset.Ready || reset.LastPrice != 0 || reset.BestBid != 0 || reset.BestAsk != 0 ||
		reset.QuoteVersion != 0 || reset.StreamEpoch != 2 {
		t.Fatalf("reset snapshot retained old market data: %+v", reset)
	}

	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", BestBid: 109, BestAsk: 111,
		ReceivedAt: receivedAt.Add(2 * time.Second), QuoteVersion: 11, StreamEpoch: 2,
	})
	if got := pm.GetMarketSnapshot(); got.Ready || got.LastPrice != 0 {
		t.Fatalf("new epoch inherited old trade: %+v", got)
	}
	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", LastPrice: 110,
		ReceivedAt: receivedAt.Add(3 * time.Second), StreamEpoch: 2,
	})
	if got := pm.GetMarketSnapshot(); !got.Ready || got.LastPrice != 110 || got.BestBid != 109 {
		t.Fatalf("new epoch did not become ready from its own events: %+v", got)
	}

	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", LastPrice: 90, BestBid: 89, BestAsk: 91,
		ReceivedAt: receivedAt.Add(4 * time.Second), QuoteVersion: 12, StreamEpoch: 1,
	})
	if got := pm.GetMarketSnapshot(); got.LastPrice != 110 || got.BestBid != 109 || got.StreamEpoch != 2 {
		t.Fatalf("late old-epoch event polluted current snapshot: %+v", got)
	}
}

func TestQuoteOnlyUpdateWakesSubscribers(t *testing.T) {
	pm := NewPriceMonitor(nil, "ETHUSDT", 1)
	updates := pm.Subscribe()
	go pm.periodicPriceSender()
	defer pm.Stop()

	now := time.Now()
	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", LastPrice: 100, ReceivedAt: now, StreamEpoch: 1,
	})
	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", BestBid: 99, BestAsk: 101, ReceivedAt: now,
		QuoteVersion: 1, StreamEpoch: 1,
	})
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("initial market update did not reach subscriber")
	}

	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", BestBid: 100, BestAsk: 102, ReceivedAt: now.Add(time.Millisecond),
		QuoteVersion: 2, StreamEpoch: 1,
	})
	select {
	case change := <-updates:
		if !change.QuoteChanged || change.NewPrice != 100 || change.Market.QuoteVersion != 2 {
			t.Fatalf("quote-only wake = %+v", change)
		}
	case <-time.After(time.Second):
		t.Fatal("quote-only update did not wake subscriber")
	}
}

func TestMarketResetWakesSubscribersImmediately(t *testing.T) {
	pm := NewPriceMonitor(nil, "ETHUSDT", 1)
	updates := pm.Subscribe()
	t.Cleanup(pm.Stop)

	receivedAt := time.Now()
	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", LastPrice: 100,
		EventTime: receivedAt, ReceivedAt: receivedAt, StreamEpoch: 1,
	})
	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", BestBid: 99.9, BestAsk: 100.1,
		EventTime: receivedAt, ReceivedAt: receivedAt, QuoteVersion: 1, StreamEpoch: 1,
	})
	pm.updateMarket(exchange.MarketUpdate{
		Symbol: "ETHUSDT", ReceivedAt: receivedAt.Add(time.Millisecond),
		StreamEpoch: 1, Reset: true,
	})

	go pm.periodicPriceSender()
	select {
	case change := <-updates:
		if !change.Reset || !change.QuoteChanged || change.NewPrice != 0 || change.Market.Ready {
			t.Fatalf("reset change = %+v", change)
		}
	case <-time.After(time.Second):
		t.Fatal("market reset did not wake subscriber")
	}
}
