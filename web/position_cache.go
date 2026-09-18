package web

import (
	"context"
	"reflect"
	"sync/atomic"
	"time"

	"opensqt/position"
)

const defaultPositionRefreshInterval = 400 * time.Millisecond
const positionIdleAfter = 10 * time.Second

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
	lastRead atomic.Int64
	wake     chan struct{}
}

func newPositionCache(src PositionSnapshotSource, interval time.Duration) *PositionCache {
	if interval <= 0 {
		interval = defaultPositionRefreshInterval
	}
	if isNilPositionSnapshotSource(src) {
		src = nil
	}
	return &PositionCache{src: src, interval: interval, wake: make(chan struct{}, 1)}
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
	timer := time.NewTimer(c.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.wake:
		}
		if ctx.Err() != nil {
			return
		}
		c.refreshIfActive(time.Now())
		// 从上次完成时刻安排下一次刷新，避免 ticker 与请求唤醒撞在一起
		// 导致重复扫描，或因几微秒偏差跳过整轮。空闲时只低频检查。
		now := time.Now()
		delay := positionIdleAfter
		if c.active(now) {
			delay = c.interval
			if entry := c.latest.Load(); entry != nil {
				delay = max(0, entry.updatedAt.Add(c.interval).Sub(now))
			}
		}
		timer.Stop()
		timer.Reset(delay)
	}
}

func (c *PositionCache) active(now time.Time) bool {
	lastRead := c.lastRead.Load()
	return lastRead != 0 && now.Sub(time.Unix(0, lastRead)) < positionIdleAfter
}

func (c *PositionCache) refreshIfActive(now time.Time) {
	if !c.active(now) {
		return
	}
	if entry := c.latest.Load(); entry != nil && now.Sub(entry.updatedAt) < c.interval {
		return
	}
	c.refresh()
}

func (c *PositionCache) refresh() {
	if c == nil || c.src == nil {
		return
	}
	snapshot := c.src.Snapshot()
	if _, owned := c.src.(*position.SuperPositionManager); !owned {
		// SuperPositionManager.Snapshot 已经返回独占切片；其它来源可能复用
		// 自己的缓冲区，仍复制以维持缓存的不可变约定。
		snapshot = clonePositionSnapshot(snapshot)
	}
	c.latest.Store(&positionCacheEntry{
		snapshot:  snapshot,
		updatedAt: time.Now(),
	})
}

// View 返回不会与缓存共享可变切片的仓位快照副本。
func (c *PositionCache) View() (position.PositionSnapshot, time.Time, bool) {
	snapshot, updatedAt, ready := c.viewReadOnly()
	return clonePositionSnapshot(snapshot), updatedAt, ready
}

// viewReadOnly 仅供本包的快照组装/序列化读取。缓存每次刷新发布新切片，
// 因此旧视图在异步写出期间仍然有效；调用方不得修改任何切片元素。
func (c *PositionCache) viewReadOnly() (position.PositionSnapshot, time.Time, bool) {
	if c == nil {
		return position.PositionSnapshot{}, time.Time{}, false
	}
	now := time.Now()
	c.lastRead.Store(now.UnixNano())
	entry := c.latest.Load()
	if entry == nil || now.Sub(entry.updatedAt) >= c.interval {
		// 首次访问/空闲后恢复只唤醒唯一刷新协程，HTTP/WS 不等待槽位锁。
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
	if entry == nil {
		return position.PositionSnapshot{}, time.Time{}, false
	}
	return entry.snapshot, entry.updatedAt, true
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
