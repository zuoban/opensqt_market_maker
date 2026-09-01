package web

import (
	"context"
	"reflect"
	"sync/atomic"
	"time"

	"opensqt/position"
)

const defaultPositionRefreshInterval = 400 * time.Millisecond

// PositionSnapshotSource 是仓位缓存依赖的最小只读接口。
type PositionSnapshotSource interface {
	Snapshot() position.PositionSnapshot
}

type positionCacheEntry struct {
	snapshot  position.PositionSnapshot
	updatedAt time.Time
}

// PositionCache 在单个后台循环中刷新仓位快照。读取方只复制最近一次完整结果，
// 不会等待正在进行的刷新；首次刷新完成前返回未就绪的零值。
type PositionCache struct {
	src      PositionSnapshotSource
	interval time.Duration
	latest   atomic.Pointer[positionCacheEntry]
	running  atomic.Bool
}

func newPositionCache(src PositionSnapshotSource, interval time.Duration) *PositionCache {
	if interval <= 0 {
		interval = defaultPositionRefreshInterval
	}
	if isNilPositionSnapshotSource(src) {
		src = nil
	}
	return &PositionCache{src: src, interval: interval}
}

func isNilPositionSnapshotSource(src PositionSnapshotSource) bool {
	if src == nil {
		return true
	}
	value := reflect.ValueOf(src)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// Run 阻塞运行唯一的刷新循环；同一缓存已有循环运行时，重复调用会立即返回。
func (c *PositionCache) Run(ctx context.Context) {
	if c == nil || c.src == nil || ctx == nil || !c.running.CompareAndSwap(false, true) {
		return
	}
	defer c.running.Store(false)
	select {
	case <-ctx.Done():
		return
	default:
	}

	c.refresh()
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh()
		}
	}
}

func (c *PositionCache) refresh() {
	if c == nil || c.src == nil {
		return
	}
	snapshot := clonePositionSnapshot(c.src.Snapshot())
	c.latest.Store(&positionCacheEntry{
		snapshot:  snapshot,
		updatedAt: time.Now(),
	})
}

// View 返回不会与缓存共享可变切片的仓位快照副本。
func (c *PositionCache) View() (position.PositionSnapshot, time.Time, bool) {
	if c == nil {
		return position.PositionSnapshot{}, time.Time{}, false
	}
	entry := c.latest.Load()
	if entry == nil {
		return position.PositionSnapshot{}, time.Time{}, false
	}
	return clonePositionSnapshot(entry.snapshot), entry.updatedAt, true
}

func clonePositionSnapshot(src position.PositionSnapshot) position.PositionSnapshot {
	dst := src
	dst.BuyWindowPrices = clonePositionSlice(src.BuyWindowPrices)
	dst.SellWindowPrices = clonePositionSlice(src.SellWindowPrices)
	dst.Slots = clonePositionSlice(src.Slots)
	dst.FilledOrders = clonePositionSlice(src.FilledOrders)
	dst.FilledHourly = clonePositionSlice(src.FilledHourly)
	return dst
}

func clonePositionSlice[T any](src []T) []T {
	if src == nil {
		return nil
	}
	dst := make([]T, len(src))
	copy(dst, src)
	return dst
}
