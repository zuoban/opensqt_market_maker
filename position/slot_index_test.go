package position

import (
	"sync"
	"testing"
)

func TestSlotPriceIndexInsertRemoveAndSnapshot(t *testing.T) {
	var idx slotPriceIndex
	idx.insert(101)
	idx.insert(99)
	idx.insert(100)
	idx.insert(100)
	got := idx.snapshot()
	want := []float64{99, 100, 101}
	if len(got) != len(want) {
		t.Fatalf("snapshot = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("snapshot = %v, want %v", got, want)
		}
	}
	idx.remove(100)
	idx.remove(50)
	got = idx.snapshot()
	if len(got) != 2 || got[0] != 99 || got[1] != 101 {
		t.Fatalf("after remove snapshot = %v", got)
	}
}

func TestSlotPriceIndexSnapshotRemainsImmutableAfterWrite(t *testing.T) {
	var idx slotPriceIndex
	idx.insert(99)
	idx.insert(100)
	old := idx.snapshot()

	idx.insert(101)
	idx.remove(99)
	if len(old) != 2 || old[0] != 99 || old[1] != 100 {
		t.Fatalf("旧快照被后续写入修改: %v", old)
	}
	current := idx.snapshot()
	if len(current) != 2 || current[0] != 100 || current[1] != 101 {
		t.Fatalf("当前快照 = %v, want [100 101]", current)
	}
}

func BenchmarkSlotPriceIndexSnapshot(b *testing.B) {
	var idx slotPriceIndex
	for i := 0; i < 500; i++ {
		idx.insert(float64(i))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if len(idx.snapshot()) != 500 {
			b.Fatal("价格索引长度异常")
		}
	}
}

func TestSlotPriceIndexHasPriceInOpenClosedRange(t *testing.T) {
	var idx slotPriceIndex
	idx.insert(99)
	idx.insert(100)
	idx.insert(101)

	for _, tt := range []struct {
		name  string
		lower float64
		upper float64
		want  bool
	}{
		{name: "inside", lower: 100.95, upper: 101.05, want: true},
		{name: "lower endpoint excluded", lower: 101, upper: 102, want: false},
		{name: "upper endpoint included", lower: 100, upper: 101, want: true},
		{name: "empty range", lower: 101, upper: 101, want: false},
		{name: "outside", lower: 101.05, upper: 102, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := idx.hasPriceInOpenClosedRange(tt.lower, tt.upper); got != tt.want {
				t.Fatalf("hasPriceInOpenClosedRange(%v, %v) = %v, want %v",
					tt.lower, tt.upper, got, tt.want)
			}
		})
	}
}

func TestGetOrCreateSlotUpdatesIndex(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	spm.getOrCreateSlot(102)
	spm.getOrCreateSlot(98)
	got := spm.slotIndex.snapshot()
	if len(got) != 2 || got[0] != 98 || got[1] != 102 {
		t.Fatalf("index = %v, want [98 102]", got)
	}
}

func requireSlotIndexMatchesMap(t *testing.T, spm *SuperPositionManager) {
	t.Helper()
	inMap := make(map[float64]struct{})
	spm.slots.Range(func(key, _ any) bool {
		inMap[key.(float64)] = struct{}{}
		return true
	})
	for _, price := range spm.slotIndex.snapshot() {
		if _, ok := inMap[price]; !ok {
			t.Fatalf("index has %v missing from map", price)
		}
		delete(inMap, price)
	}
	if len(inMap) != 0 {
		t.Fatalf("map has prices missing from index: %v", inMap)
	}
}

func TestDeleteSlotRemovesIndexWhenCurrent(t *testing.T) {
	const price = float64(90)
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	slot := spm.getOrCreateSlot(price)
	slot.mu.Lock()
	if !spm.deleteSlotIfCurrentAndRecyclable(price, slot) {
		t.Fatal("recyclable current slot was not deleted")
	}
	slot.mu.Unlock()
	if _, ok := spm.slots.Load(price); ok {
		t.Fatal("deleted slot still in map")
	}
	if len(spm.slotIndex.snapshot()) != 0 {
		t.Fatal("index still has deleted price")
	}
}

func TestDeleteSlotKeepsIndexWhenPointerReplaced(t *testing.T) {
	const price = float64(90)
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	old := spm.getOrCreateSlot(price)
	replacement := newEmptySlot(price)
	replacement.OrderStatus = OrderStatusConfirmed
	replacement.OrderID = 8
	replacement.OrderSide = "BUY"
	replacement.ClientOID = "live-90"
	replacement.SlotStatus = SlotStatusLocked
	spm.slots.Store(price, replacement)

	old.mu.Lock()
	deleted := spm.deleteSlotIfCurrentAndRecyclable(price, old)
	old.mu.Unlock()
	if deleted {
		t.Fatal("CompareAndDelete succeeded on a replaced slot")
	}
	got, ok := spm.slots.Load(price)
	if !ok || got != replacement {
		t.Fatal("replacement slot was removed from map")
	}
	idx := spm.slotIndex.snapshot()
	if len(idx) != 1 || idx[0] != price {
		t.Fatalf("index = %v, want [%v]", idx, price)
	}
}

func TestLockMappedSlotReturnsNewSlotAfterRecycle(t *testing.T) {
	const price = float64(90)
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	stale := spm.getOrCreateSlot(price)
	stale.mu.Lock()
	if !spm.deleteSlotIfCurrentAndRecyclable(price, stale) {
		t.Fatal("recyclable slot was not deleted")
	}
	stale.mu.Unlock()

	locked := spm.lockMappedSlot(price)
	defer locked.mu.Unlock()
	if locked == stale {
		t.Fatal("lockMappedSlot returned the recycled slot")
	}
	current, ok := spm.slots.Load(price)
	if !ok || current != locked {
		t.Fatal("lockMappedSlot returned a detached slot")
	}
	requireSlotIndexMatchesMap(t, spm)
}

func TestSlotIndexSurvivesConcurrentRecycleAndGetOrCreate(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	const workers = 32
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(workers * 2)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for n := 0; n < iters; n++ {
				slot := spm.lockMappedSlot(90)
				slot.mu.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			for n := 0; n < iters; n++ {
				spm.recycleIdleSlots([]float64{100})
				spm.getOrCreateSlot(90)
			}
		}()
	}
	wg.Wait()
	requireSlotIndexMatchesMap(t, spm)
}
