package position

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"opensqt/logger"
)

func TestOrderHistoryLookupPreservesIdentity(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	longID := strings.Repeat("long-identity-", 12)
	identities := []OrderUpdate{
		{ClientOrderID: "10000_B_1700000000001", OrderID: 42},
		{ClientOrderID: "123", OrderID: 43},
		{OrderID: 123},
		{ClientOrderID: "order:123"},
		{ClientOrderID: strings.Repeat("x", 36)},
		{ClientOrderID: strings.Repeat("x", 57)},
		{ClientOrderID: strings.Repeat("x", 58)},
		{ClientOrderID: "订单\x00identity"},
		{ClientOrderID: longID},
		{OrderID: math.MaxInt64},
		{OrderID: math.MinInt64},
	}
	now := time.Now()
	for i, identity := range identities {
		spm.appendFilledOrder(FilledOrderRecord{OrderID: identity.OrderID, ClientOrderID: identity.ClientOrderID}, now)
		spm.storeTerminalOrderProgress(identity, terminalOrderProgress{ExecutedQty: float64(i + 1)})
	}
	for i, identity := range identities {
		if !spm.wasFilledOrderRecorded(identity) {
			t.Fatalf("missing fill for %+v", identity)
		}
		progress, found := spm.getTerminalOrderProgress(identity)
		if !found || progress.ExecutedQty != float64(i+1) {
			t.Fatalf("wrong terminal history for %+v: %+v, found=%v", identity, progress, found)
		}
	}
	// A ClientOrderID remains authoritative even if the numeric ID changes.
	changed := identities[0]
	changed.OrderID = 999
	if !spm.wasFilledOrderRecorded(changed) {
		t.Fatal("numeric ID change lost ClientOrderID history")
	}
	if progress, found := spm.getTerminalOrderProgress(changed); !found || progress.ExecutedQty != 1 {
		t.Fatal("numeric ID change lost ClientOrderID terminal history")
	}
	for _, missing := range []OrderUpdate{
		{}, {OrderID: 42}, {ClientOrderID: "missing", OrderID: 123},
		{ClientOrderID: longID + "different"}, {OrderID: 124},
	} {
		if spm.wasFilledOrderRecorded(missing) {
			t.Fatalf("false fill match for %+v", missing)
		}
		if _, found := spm.getTerminalOrderProgress(missing); found {
			t.Fatalf("false terminal match for %+v", missing)
		}
	}
}

func TestOrderLogFilteringPreservesAccountingAndVisibleLogs(t *testing.T) {
	previous := logger.GetLevel()
	t.Cleanup(func() { logger.SetLevel(previous) })
	for _, level := range []logger.LogLevel{logger.INFO, logger.WARN, logger.ERROR} {
		t.Run(level.String(), func(t *testing.T) {
			logger.SetLevel(level)
			spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
			expectLog := func(logLevel logger.LogLevel, message string, emit func()) {
				t.Helper()
				before := logger.RecentLogs(1)
				emit()
				after := logger.RecentLogs(1)
				if logLevel < level {
					if !slices.Equal(before, after) {
						t.Fatal("filtered diagnostic reached recent logs")
					}
				} else if len(after) != 1 || after[0].Level != logLevel.String() || !strings.Contains(after[0].Message, message) {
					t.Fatalf("visible diagnostic missing: %+v", after)
				}
			}
			expectLog(logger.INFO, "已实现盈亏", func() {
				spm.publishExecutionEffect(executionEffect{BuyQty: .25, SellQty: .125, RealizedPNL: .0625}, 100, false)
			})
			if spm.GetTotalBuyQty() != .25 || spm.GetTotalSellQty() != .125 || spm.GetRealizedPNL() != .0625 {
				t.Fatal("log filtering suppressed accounting")
			}
			in := orderUpdateInput{Update: OrderUpdate{OrderID: 123, ClientOrderID: "log-level-test", Status: "FILLED"}, Side: "BUY", SlotPrice: 100}
			expectLog(logger.INFO, "买单成交", func() {
				spm.logOrderTransition(in, orderTransition{Disposition: transitionApplied, RecordFill: true})
			})
			expectLog(logger.WARN, "忽略成交进度", func() {
				spm.logOrderTransition(in, orderTransition{Disposition: transitionCorrection, Issue: executionInvalidQty})
			})
			expectLog(logger.DEBUG, "已完成订单更新被忽略", func() {
				spm.logOrderTransition(in, orderTransition{Disposition: transitionFilledReplay})
			})
		})
	}
}

func BenchmarkOrderHistoryLookup(b *testing.B) {
	for _, tc := range []struct {
		name   string
		update OrderUpdate
	}{
		{"client", OrderUpdate{ClientOrderID: "10000_B_1700000000001", OrderID: 123}},
		{"client36", OrderUpdate{ClientOrderID: strings.Repeat("x", 36)}},
		{"long_client", OrderUpdate{ClientOrderID: strings.Repeat("x", 128)}},
		{"order_id", OrderUpdate{OrderID: math.MaxInt64}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
			spm.appendFilledOrder(FilledOrderRecord{OrderID: tc.update.OrderID, ClientOrderID: tc.update.ClientOrderID}, time.Now())
			spm.storeTerminalOrderProgress(tc.update, terminalOrderProgress{ExecutedQty: 1})
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if !spm.wasFilledOrderRecorded(tc.update) {
					b.Fatal("lost fill")
				}
				if progress, found := spm.getTerminalOrderProgress(tc.update); !found || progress.ExecutedQty != 1 {
					b.Fatal("lost terminal progress")
				}
			}
		})
	}
}

// No log line is emitted: DEBUG replay diagnostics are disabled under both
// production's default INFO level and the stress fixture's ERROR level.
func BenchmarkSuppressedOrderReplayLog(b *testing.B) {
	for _, level := range []logger.LogLevel{logger.INFO, logger.ERROR} {
		b.Run(level.String(), func(b *testing.B) {
			oldLevel := logger.GetLevel()
			logger.SetLevel(level)
			b.Cleanup(func() { logger.SetLevel(oldLevel) })
			spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
			in := orderUpdateInput{Update: OrderUpdate{OrderID: 12345, ClientOrderID: "10000_B_1700000000001", Status: "FILLED"}}
			next := orderTransition{Disposition: transitionFilledReplay}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				spm.logOrderTransition(in, next)
			}
		})
	}
}
