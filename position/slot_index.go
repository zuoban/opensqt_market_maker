package position

import (
	"sort"
	"sync"
)

// slotPriceIndex 保存槽位价格的有序副本，供窗口扫描替代 sync.Map.Range。
type slotPriceIndex struct {
	mu     sync.RWMutex
	prices []float64
}

func (idx *slotPriceIndex) insert(price float64) {
	if idx == nil {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.insertLocked(price)
}

func (idx *slotPriceIndex) remove(price float64) {
	if idx == nil {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.removeLocked(price)
}

func (idx *slotPriceIndex) insertLocked(price float64) {
	i := sort.Search(len(idx.prices), func(i int) bool { return idx.prices[i] >= price })
	if i < len(idx.prices) && idx.prices[i] == price {
		return
	}
	idx.prices = append(idx.prices, 0)
	copy(idx.prices[i+1:], idx.prices[i:])
	idx.prices[i] = price
}

func (idx *slotPriceIndex) removeLocked(price float64) {
	i := sort.Search(len(idx.prices), func(i int) bool { return idx.prices[i] >= price })
	if i == len(idx.prices) || idx.prices[i] != price {
		return
	}
	idx.prices = append(idx.prices[:i], idx.prices[i+1:]...)
}

func (idx *slotPriceIndex) snapshot() []float64 {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]float64, len(idx.prices))
	copy(out, idx.prices)
	return out
}

// hasPriceInOpenClosedRange reports whether the index contains a price in
// (lowerExclusive, upperInclusive]. Sell-window eligibility uses price <= max,
// so this range identifies exactly the slots whose eligibility changes when
// that upper bound moves in either direction.
func (idx *slotPriceIndex) hasPriceInOpenClosedRange(lowerExclusive, upperInclusive float64) bool {
	if idx == nil || lowerExclusive >= upperInclusive {
		return false
	}
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	i := sort.Search(len(idx.prices), func(i int) bool { return idx.prices[i] > lowerExclusive })
	return i < len(idx.prices) && idx.prices[i] <= upperInclusive
}
