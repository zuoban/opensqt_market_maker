package logger

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAsyncSinkSlowOutputDoesNotBlockEnqueueAndDrainsOnClose(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var dropped atomic.Uint64
	var written []string
	sink := newAsyncLogSink(2, &dropped, func(item asyncLogItem) {
		if len(written) == 0 {
			close(started)
			<-release
		}
		written = append(written, item.message)
	})
	defer sink.close()
	defer close(release)
	sink.enqueue(asyncLogItem{message: "first"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("writer did not start")
	}
	queued := make(chan struct{})
	go func() {
		for _, message := range []string{"second", "third", "dropped"} {
			sink.enqueue(asyncLogItem{message: message})
		}
		close(queued)
	}()
	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("enqueue waited for slow output")
	}
	if dropped.Load() != 1 {
		t.Fatalf("dropped=%d, want 1", dropped.Load())
	}
	// 排空和顺序在另一个无阻塞 sink 上验证，避免依赖调度时序。
	var completed []string
	drain := newAsyncLogSink(3, &dropped, func(item asyncLogItem) { completed = append(completed, item.message) })
	for _, message := range []string{"a", "b", "c"} {
		drain.enqueue(asyncLogItem{message: message})
	}
	drain.close()
	if len(completed) != 3 || completed[0] != "a" || completed[2] != "c" {
		t.Fatalf("close did not drain in order: %v", completed)
	}
}

func TestAsyncSinkConcurrentSendAndClose(t *testing.T) {
	var dropped atomic.Uint64
	sink := newAsyncLogSink(16, &dropped, func(asyncLogItem) {})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				sink.enqueue(asyncLogItem{})
			}
		}()
	}
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); sink.close() }()
	}
	wg.Wait()
}

func TestFileQueueDoesNotAcquireFileIOLock(t *testing.T) {
	var dropped atomic.Uint64
	sink := newAsyncLogSink(1, &dropped, func(asyncLogItem) {})
	previous := fileSink.Swap(sink)
	defer func() { fileSink.Store(previous); sink.close() }()
	fileMu.Lock()
	defer fileMu.Unlock()
	done := make(chan struct{})
	go func() { enqueueFileLog(asyncLogItem{}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("file enqueue waited for file I/O lock")
	}
}
