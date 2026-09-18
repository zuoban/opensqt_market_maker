package logger

import (
	"log"
	"sync"
	"sync/atomic"
)

// 队列锁只保护发送与关闭，绝不覆盖输出 I/O。这样慢磁盘、慢 stderr
// 和关闭并发都不会阻塞正常入队，也不会触发 send-on-closed-channel。
type asyncLogSink struct {
	mu      sync.Mutex
	closed  bool
	queue   chan asyncLogItem
	done    chan struct{}
	dropped *atomic.Uint64
}

func newAsyncLogSink(capacity int, dropped *atomic.Uint64, write func(asyncLogItem)) *asyncLogSink {
	s := &asyncLogSink{
		queue: make(chan asyncLogItem, capacity), done: make(chan struct{}), dropped: dropped,
	}
	go func() {
		defer close(s.done)
		for item := range s.queue {
			write(item)
		}
	}()
	return s
}

func (s *asyncLogSink) enqueue(item asyncLogItem) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- item:
	default:
		s.dropped.Add(1)
	}
}

func (s *asyncLogSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()
	<-s.done
}

// 仅在 init 或持有 lifecycleMu 时调用。
func startConsoleLogger() {
	if consoleSink.Load() == nil {
		consoleSink.Store(newAsyncLogSink(4096, &consoleDropped, func(item asyncLogItem) {
			log.Printf("[%s] %s", item.levelText, item.message)
		}))
	}
}
