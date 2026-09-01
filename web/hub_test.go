package web

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var errFakeWSClosed = errors.New("fake websocket closed")

type fakeWSConnection struct {
	mu sync.Mutex

	writes        chan interface{}
	controls      chan int
	closed        chan struct{}
	closeOnce     sync.Once
	writeJSONFn   func(interface{}) error
	writeDeadline time.Time
	readDeadline  time.Time
	readLimit     int64
	pongHandler   func(string) error
}

func newFakeWSConnection() *fakeWSConnection {
	return &fakeWSConnection{
		writes:   make(chan interface{}, 32),
		controls: make(chan int, 8),
		closed:   make(chan struct{}),
	}
}

func (c *fakeWSConnection) SetWriteDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.writeDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *fakeWSConnection) WriteJSON(v interface{}) error {
	if c.writeJSONFn != nil {
		return c.writeJSONFn(v)
	}
	select {
	case <-c.closed:
		return errFakeWSClosed
	case c.writes <- v:
		return nil
	}
}

func (c *fakeWSConnection) WriteControl(messageType int, _ []byte, _ time.Time) error {
	select {
	case <-c.closed:
		return errFakeWSClosed
	case c.controls <- messageType:
		return nil
	}
}

func (c *fakeWSConnection) SetReadLimit(limit int64) {
	c.mu.Lock()
	c.readLimit = limit
	c.mu.Unlock()
}

func (c *fakeWSConnection) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	c.readDeadline = deadline
	c.mu.Unlock()
	return nil
}

func (c *fakeWSConnection) SetPongHandler(handler func(string) error) {
	c.mu.Lock()
	c.pongHandler = handler
	c.mu.Unlock()
}

func (c *fakeWSConnection) ReadMessage() (int, []byte, error) {
	<-c.closed
	return websocket.CloseMessage, nil, errFakeWSClosed
}

func (c *fakeWSConnection) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeWSConnection) deadlines() (time.Time, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeDeadline, c.readDeadline
}

func (c *fakeWSConnection) currentPongHandler() func(string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pongHandler
}

func TestWSClientQueueKeepsLatestSnapshot(t *testing.T) {
	h := newHub()
	client := newWSClient(h, newFakeWSConnection())
	if !client.enqueue("old") || !client.enqueue("latest") {
		t.Fatal("enqueue should accept snapshots while the client is open")
	}
	if got := <-client.send; got != "latest" {
		t.Fatalf("queued snapshot = %v, want latest", got)
	}
	if cap(client.send) != 1 {
		t.Fatalf("queue capacity = %d, want 1", cap(client.send))
	}

	client.shutdown()
	if client.enqueue("after-close") {
		t.Fatal("closed client accepted another snapshot")
	}
}

func TestHubBroadcastDoesNotWaitForSlowClient(t *testing.T) {
	h := newHub()
	defer h.close()

	slowConn := newFakeWSConnection()
	slowStarted := make(chan struct{}, 1)
	slowConn.writeJSONFn = func(interface{}) error {
		select {
		case slowStarted <- struct{}{}:
		default:
		}
		<-slowConn.closed
		return errFakeWSClosed
	}
	slowClient := newWSClient(h, slowConn)
	slowClient.pingPeriod = time.Hour
	fastConn := newFakeWSConnection()
	fastClient := newWSClient(h, fastConn)
	fastClient.pingPeriod = time.Hour
	if !h.register(slowClient) || !h.register(fastClient) {
		t.Fatal("failed to register test clients")
	}
	go slowClient.writePump()
	go fastClient.writePump()

	h.broadcast("first")
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow writer did not start")
	}

	started := time.Now()
	h.broadcast("second")
	h.broadcast("latest")
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("broadcast waited for slow client: %v", elapsed)
	}

	deadline := time.After(time.Second)
	for {
		select {
		case got := <-fastConn.writes:
			if got == "latest" {
				return
			}
		case <-deadline:
			t.Fatal("fast client did not receive the latest snapshot")
		}
	}
}

func TestWSClientWriterUsesDeadlineAndPing(t *testing.T) {
	h := newHub()
	defer h.close()
	conn := newFakeWSConnection()
	client := newWSClient(h, conn)
	client.pingPeriod = 10 * time.Millisecond
	if !h.register(client) {
		t.Fatal("failed to register client")
	}
	go client.writePump()

	select {
	case messageType := <-conn.controls:
		if messageType != websocket.PingMessage {
			t.Fatalf("control message = %d, want ping", messageType)
		}
	case <-time.After(time.Second):
		t.Fatal("writer did not send a ping")
	}
	writeDeadline, _ := conn.deadlines()
	remaining := time.Until(writeDeadline)
	if remaining < 1500*time.Millisecond || remaining > 2500*time.Millisecond {
		t.Fatalf("write deadline remaining = %v, want about 2s", remaining)
	}
}

func TestWSClientReadDeadlineRenewsOnPong(t *testing.T) {
	h := newHub()
	defer h.close()
	conn := newFakeWSConnection()
	client := newWSClient(h, conn)
	client.pongWait = 100 * time.Millisecond
	if !h.register(client) {
		t.Fatal("failed to register client")
	}
	done := make(chan struct{})
	go func() {
		client.readPump()
		close(done)
	}()

	var handler func(string) error
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		handler = conn.currentPongHandler()
		if handler != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if handler == nil {
		t.Fatal("pong handler was not installed")
	}
	_, before := conn.deadlines()
	time.Sleep(5 * time.Millisecond)
	if err := handler("pong"); err != nil {
		t.Fatalf("pong handler: %v", err)
	}
	_, after := conn.deadlines()
	if !after.After(before) {
		t.Fatalf("read deadline was not renewed: before=%v after=%v", before, after)
	}

	h.unregister(client)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read pump did not stop after unregister")
	}
}
