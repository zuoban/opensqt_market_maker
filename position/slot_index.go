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
	// 读取远多于增删。写入时复制，保证 snapshot 返回的旧视图在解锁后
	// 仍然不可变；热路径遍历因此无需每次复制整个价格索引。
	next := make([]float64, len(idx.prices)+1)
	copy(next, idx.prices[:i])
	next[i] = price
	copy(next[i+1:], idx.prices[i:])
	idx.prices = next
}

func (idx *slotPriceIndex) removeLocked(price float64) {
	i := sort.Search(len(idx.prices), func(i int) bool { return idx.prices[i] >= price })
	if i == len(idx.prices) || idx.prices[i] != price {
		return
	}
	next := make([]float64, len(idx.prices)-1)
	copy(next, idx.prices[:i])
	copy(next[i:], idx.prices[i+1:])
	idx.prices = next
}

// snapshot 返回只读的不可变价格视图。调用方不得修改返回的切片；后续索引
// 写入会发布新的底层数组，因此当前视图可在解锁后安全遍历且不产生分配。
func (idx *slotPriceIndex) snapshot() []float64 {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	prices := idx.prices
	idx.mu.RUnlock()
	return prices
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
