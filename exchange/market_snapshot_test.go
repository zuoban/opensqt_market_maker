package exchange

import (
	"testing"
	"time"
)

func TestMakerBookUsableTreatsLiveTradesAsFreshBook(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	snapshot := MarketSnapshot{
		BestBid:         736.60,
		BestAsk:         736.62,
		QuoteReceivedAt: now.Add(-7 * time.Second),
		ReceivedAt:      now.Add(-200 * time.Millisecond),
		TradeTime:       now.Add(-200 * time.Millisecond),
		Ready:           true,
	}
	if !snapshot.MakerBookUsable(now, 1500*time.Millisecond) {
		t.Fatal("unchanged bookTicker with a live trade was treated as stale")
	}
	if snapshot.MakerBookUsable(now.Add(2*time.Second), 1500*time.Millisecond) {
		t.Fatal("silent stream still usable after stale window")
	}
}

func TestBookAgreesWithLastRejectsQuantityScale(t *testing.T) {
	ok := MarketSnapshot{LastPrice: 736.52, BestBid: 736.50, BestAsk: 736.54}
	if !ok.BookAgreesWithLast() {
		t.Fatal("live BNB book should agree with last")
	}
	bad := MarketSnapshot{LastPrice: 736.52, BestBid: 0.72, BestAsk: 1.04}
	if bad.BookAgreesWithLast() {
		t.Fatal("quantity-scale bookTicker should not agree with last")
	}
}

func TestMakerBookUsableRequiresReadyBook(t *testing.T) {
	now := time.Now()
	snapshot := MarketSnapshot{
		LastPrice: 100, ReceivedAt: now, Ready: false,
	}
	if snapshot.MakerBookUsable(now, time.Second) {
		t.Fatal("unready snapshot was usable")
	}
	snapshot.Ready = true
	snapshot.BestBid, snapshot.BestAsk = 99, 101
	if snapshot.MakerBookUsable(now, time.Second) {
		t.Fatal("missing QuoteReceivedAt was usable")
	}
}
