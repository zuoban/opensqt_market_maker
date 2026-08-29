package position

import (
	"sync"
	"testing"
)

func TestGetOrCreateSlotReturnsSingleInstanceConcurrently(t *testing.T) {
	spm := NewSuperPositionManager(testConfig(), stubExecutor{}, stubEx{}, 2, 3)
	const workers = 128
	start := make(chan struct{})
	results := make(chan *InventorySlot, workers)
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			results <- spm.getOrCreateSlot(100)
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var first *InventorySlot
	for slot := range results {
		if first == nil {
			first = slot
			continue
		}
		if slot != first {
			t.Fatal("concurrent getOrCreateSlot returned different slot instances")
		}
	}
	if first == nil {
		t.Fatal("concurrent getOrCreateSlot returned no slot")
	}
}
