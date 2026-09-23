package exchange

import (
	"testing"
	"time"
)

func TestLastEventAtUsesLatestTradeOrBookEvent(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	for _, tc := range []struct {
		name     string
		snapshot MarketSnapshot
		want     time.Time
	}{
		{"empty", MarketSnapshot{}, time.Time{}},
		{"live trade with static book", MarketSnapshot{QuoteReceivedAt: now.Add(-7 * time.Second), ReceivedAt: now, TradeTime: now.Add(-time.Second)}, now},
		{"new book", MarketSnapshot{QuoteReceivedAt: now, ReceivedAt: now.Add(-time.Second)}, now},
		{"trade time", MarketSnapshot{TradeTime: now, ReceivedAt: now.Add(-time.Second)}, now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.snapshot.LastEventAt(); got != tc.want {
				t.Fatalf("last event=%v, want %v", got, tc.want)
			}
		})
	}
}
