package position

import (
	"sort"
	"sync"
)

// slotPriceIndex 保存按不可变 Price 排序的槽位指针。扫描直接访问槽位，
// 不再对每个价格重复查询 sync.Map；成员增删以写时复制发布。
type slotPriceIndex struct {
	mu    sync.RWMutex
	slots []*InventorySlot
}

func (idx *slotPriceIndex) insert(price float64) {
	if idx == nil {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.insertLocked(newEmptySlot(price))
}

func (idx *slotPriceIndex) remove(price float64) {
	if idx == nil {
		return
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.removeLocked(price)
}

func (idx *slotPriceIndex) insertLocked(slot *InventorySlot) {
	i := sort.Search(len(idx.slots), func(i int) bool { return idx.slots[i].Price >= slot.Price })
	if i < len(idx.slots) && idx.slots[i].Price == slot.Price {
		return
	}
	// 读取远多于增删。写入时复制，保证 snapshot 返回的旧视图在解锁后
	// 仍然不可变；热路径遍历因此无需每次复制整个价格索引。
	next := make([]*InventorySlot, len(idx.slots)+1)
	copy(next, idx.slots[:i])
	next[i] = slot
	copy(next[i+1:], idx.slots[i:])
	idx.slots = next
}

func (idx *slotPriceIndex) removeLocked(price float64) {
	i := sort.Search(len(idx.slots), func(i int) bool { return idx.slots[i].Price >= price })
	if i == len(idx.slots) || idx.slots[i].Price != price {
		return
	}
	next := make([]*InventorySlot, len(idx.slots)-1)
	copy(next, idx.slots[:i])
	copy(next[i:], idx.slots[i+1:])
	idx.slots = next
}

// snapshot 返回成员不可变的视图，槽位内容仍须持有各自的锁才能访问。
// 后续索引写入不会修改已发布的切片；回收的旧槽由 retired 标记跳过。
func (idx *slotPriceIndex) snapshot() []*InventorySlot {
	if idx == nil {
		return nil
	}
	idx.mu.RLock()
	slots := idx.slots
	idx.mu.RUnlock()
	return slots
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
	i := sort.Search(len(idx.slots), func(i int) bool { return idx.slots[i].Price > lowerExclusive })
	return i < len(idx.slots) && idx.slots[i].Price <= upperInclusive
}
