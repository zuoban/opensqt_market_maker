package web

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"opensqt/position"
)

type countingPositionSnapshotSource struct {
	calls int
}

func (s *countingPositionSnapshotSource) Snapshot() position.PositionSnapshot {
	s.calls++
	return position.PositionSnapshot{}
}

func TestPositionCacheDoesNotRefreshAfterPreCanceledStart(t *testing.T) {
	source := &countingPositionSnapshotSource{}
	cache := newPositionCache(source, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cache.Run(ctx)
	if source.calls != 0 {
		t.Fatalf("position cache refreshed %d times after cancellation", source.calls)
	}
}

type staticPositionSnapshotSource struct {
	snapshot position.PositionSnapshot
}

func (s *staticPositionSnapshotSource) Snapshot() position.PositionSnapshot {
	return s.snapshot
}

func TestPositionCacheViewIsImmutable(t *testing.T) {
	source := &staticPositionSnapshotSource{snapshot: position.PositionSnapshot{
		Initialized:      true,
		Symbol:           "SOLUSDC",
		BuyWindowPrices:  []float64{101},
		SellWindowPrices: []float64{102},
		Slots:            []position.SlotSnapshot{{Price: 101}},
		FilledOrders:     []position.FilledOrderRecord{{OrderID: 7}},
		FilledHourly:     []position.HourlyFillBucket{{Buy: 1}},
	}}
	cache := newPositionCache(source, time.Second)
	cache.refresh()

	first, updatedAt, ready := cache.View()
	if !ready || updatedAt.IsZero() {
		t.Fatalf("cache readiness = %v, updatedAt = %v", ready, updatedAt)
	}
	first.BuyWindowPrices[0] = 999
	first.SellWindowPrices[0] = 999
	first.Slots[0].Price = 999
	first.FilledOrders[0].OrderID = 999
	first.FilledHourly[0].Buy = 999
	source.snapshot.BuyWindowPrices[0] = 888
	source.snapshot.SellWindowPrices[0] = 888
	source.snapshot.Slots[0].Price = 888
	source.snapshot.FilledOrders[0].OrderID = 888
	source.snapshot.FilledHourly[0].Buy = 888

	second, secondUpdatedAt, secondReady := cache.View()
	if !secondReady || !secondUpdatedAt.Equal(updatedAt) {
		t.Fatalf("second view readiness = %v, updatedAt = %v", secondReady, secondUpdatedAt)
	}
	if second.BuyWindowPrices[0] != 101 || second.SellWindowPrices[0] != 102 || second.Slots[0].Price != 101 ||
		second.FilledOrders[0].OrderID != 7 || second.FilledHourly[0].Buy != 1 {
		t.Fatalf("cached snapshot was mutated: %+v", second)
	}
}

func TestClonePositionSnapshotDoesNotShareSlices(t *testing.T) {
	src := position.PositionSnapshot{
		BuyWindowPrices:  []float64{101},
		SellWindowPrices: []float64{102},
		Slots:            []position.SlotSnapshot{{Price: 103}},
		FilledOrders:     []position.FilledOrderRecord{{OrderID: 104}},
		FilledHourly:     []position.HourlyFillBucket{{Buy: 105}},
	}

	cloned := clonePositionSnapshot(src)
	cloned.BuyWindowPrices[0] = 201
	cloned.SellWindowPrices[0] = 202
	cloned.Slots[0].Price = 203
	cloned.FilledOrders[0].OrderID = 204
	cloned.FilledHourly[0].Buy = 205

	if src.BuyWindowPrices[0] != 101 || src.SellWindowPrices[0] != 102 || src.Slots[0].Price != 103 ||
		src.FilledOrders[0].OrderID != 104 || src.FilledHourly[0].Buy != 105 {
		t.Fatalf("clone shares slice storage with source: %+v", src)
	}
}

func TestClonePositionSnapshotPreservesSliceSemantics(t *testing.T) {
	src := position.PositionSnapshot{
		BuyWindowPrices:  []float64{},
		SellWindowPrices: nil,
		Slots:            []position.SlotSnapshot{},
		FilledOrders:     []position.FilledOrderRecord{},
		FilledHourly:     []position.HourlyFillBucket{},
	}

	cloned := clonePositionSnapshot(src)
	if cloned.BuyWindowPrices == nil {
		t.Fatal("non-nil empty buyWindowPrices became nil")
	}
	if cloned.SellWindowPrices != nil {
		t.Fatal("nil sellWindowPrices became non-nil")
	}
	if cloned.Slots == nil {
		t.Fatal("non-nil empty slots became nil")
	}
	if cloned.FilledOrders == nil {
		t.Fatal("non-nil empty filledOrders became nil")
	}
	if cloned.FilledHourly == nil {
		t.Fatal("non-nil empty filledHourly became nil")
	}
}

func TestPositionCacheViewSerializesEmptyCollectionsAsArrays(t *testing.T) {
	cache := newPositionCache(&staticPositionSnapshotSource{snapshot: position.PositionSnapshot{
		BuyWindowPrices:  []float64{},
		SellWindowPrices: []float64{},
		Slots:            []position.SlotSnapshot{},
		FilledOrders:     []position.FilledOrderRecord{},
		FilledHourly:     []position.HourlyFillBucket{},
	}}, time.Second)
	cache.refresh()

	snapshot, _, ready := cache.View()
	if !ready {
		t.Fatal("position cache is not ready")
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal position snapshot: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("unmarshal position snapshot: %v", err)
	}
	for _, field := range []string{"buyWindowPrices", "sellWindowPrices", "slots", "filledOrders", "filledHourly"} {
		if got := string(fields[field]); got != "[]" {
			t.Errorf("%s serialized as %s, want []", field, got)
		}
	}
}

func TestAssemblerReportsCachedPositionAge(t *testing.T) {
	updatedAt := time.Now().Add(-2 * time.Second)
	cache := newPositionCache(nil, time.Second)
	cache.latest.Store(&positionCacheEntry{
		snapshot:  position.PositionSnapshot{Initialized: true, Symbol: "SOLUSDC"},
		updatedAt: updatedAt,
	})

	snapshot := (&assembler{position: cache}).Build()
	if !snapshot.PositionReady || snapshot.Position.Symbol != "SOLUSDC" {
		t.Fatalf("position snapshot = ready:%v value:%+v", snapshot.PositionReady, snapshot.Position)
	}
	if !snapshot.PositionUpdatedAt.Equal(updatedAt) {
		t.Fatalf("positionUpdatedAt = %v, want %v", snapshot.PositionUpdatedAt, updatedAt)
	}
	if snapshot.PositionAgeMs < 1900 || snapshot.PositionAgeMs > 2500 {
		t.Fatalf("positionAgeMs = %d", snapshot.PositionAgeMs)
	}
}

func TestPositionCacheTreatsTypedNilSourceAsNotReady(t *testing.T) {
	var source *position.SuperPositionManager
	cache := newPositionCache(source, time.Second)
	if cache.src != nil {
		t.Fatalf("typed nil source was retained: %T", cache.src)
	}
	value, updatedAt, ready := cache.View()
	if ready || !updatedAt.IsZero() || value.Initialized {
		t.Fatalf("typed nil view = ready:%v updated:%v value:%+v", ready, updatedAt, value)
	}
}
