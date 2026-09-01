package web

import (
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"opensqt/logger"
)

const (
	dashboardWSWriteWait  = 2 * time.Second
	dashboardWSPongWait   = 60 * time.Second
	dashboardWSPingPeriod = 20 * time.Second
	dashboardWSReadLimit  = 1024
)

// wsConnection keeps the client lifecycle testable without weakening the
// production path, where *websocket.Conn is the only implementation.
type wsConnection interface {
	SetWriteDeadline(time.Time) error
	WriteJSON(interface{}) error
	WriteControl(int, []byte, time.Time) error
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetPongHandler(func(string) error)
	ReadMessage() (int, []byte, error)
	Close() error
}

type wsClient struct {
	hub  *hub
	conn wsConnection

	// send has one slot on purpose. A dashboard snapshot supersedes every
	// unsent snapshot, so a slow client consumes bounded memory and catches up
	// to the latest state instead of replaying an obsolete backlog.
	send    chan interface{}
	queueMu sync.Mutex
	done    chan struct{}
	close   sync.Once

	writeWait  time.Duration
	pongWait   time.Duration
	pingPeriod time.Duration
}

func newWSClient(h *hub, conn wsConnection) *wsClient {
	return &wsClient{
		hub:        h,
		conn:       conn,
		send:       make(chan interface{}, 1),
		done:       make(chan struct{}),
		writeWait:  dashboardWSWriteWait,
		pongWait:   dashboardWSPongWait,
		pingPeriod: dashboardWSPingPeriod,
	}
}

// enqueue replaces an unsent snapshot with msg. It never waits for the
// network writer, which is what keeps one slow browser from stalling the hub.
func (c *wsClient) enqueue(msg interface{}) bool {
	if c == nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
	}

	c.queueMu.Lock()
	defer c.queueMu.Unlock()
	select {
	case <-c.done:
		return false
	default:
	}

	select {
	case c.send <- msg:
		return true
	default:
	}
	select {
	case <-c.send:
	default:
	}
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

func (c *wsClient) writePump() {
	pingPeriod := c.pingPeriod
	if pingPeriod <= 0 {
		pingPeriod = dashboardWSPingPeriod
	}
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	defer c.hub.unregister(c)

	for {
		select {
		case <-c.done:
			return
		case msg := <-c.send:
			if err := c.writeJSON(msg); err != nil {
				logger.Debug("监控面板 WS 写入失败: %v", err)
				return
			}
		case <-ticker.C:
			if err := c.writePing(); err != nil {
				logger.Debug("监控面板 WS 心跳失败: %v", err)
				return
			}
		}
	}
}

func (c *wsClient) writeJSON(v interface{}) error {
	deadline := time.Now().Add(c.effectiveWriteWait())
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.conn.WriteJSON(v)
}

func (c *wsClient) writePing() error {
	deadline := time.Now().Add(c.effectiveWriteWait())
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return c.conn.WriteControl(websocket.PingMessage, nil, deadline)
}

func (c *wsClient) effectiveWriteWait() time.Duration {
	if c.writeWait > 0 {
		return c.writeWait
	}
	return dashboardWSWriteWait
}

func (c *wsClient) readPump() {
	defer c.hub.unregister(c)

	pongWait := c.pongWait
	if pongWait <= 0 {
		pongWait = dashboardWSPongWait
	}
	c.conn.SetReadLimit(dashboardWSReadLimit)
	if err := c.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		logger.Debug("监控面板 WS 设置读取超时失败: %v", err)
		return
	}
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			select {
			case <-c.done:
			default:
				logger.Debug("监控面板 WS 读取结束: %v", err)
			}
			return
		}
	}
}

func (c *wsClient) shutdown() {
	if c == nil {
		return
	}
	c.close.Do(func() {
		close(c.done)
		_ = c.conn.Close()
	})
}

type hub struct {
	mu      sync.RWMutex
	clients map[*wsClient]struct{}
	closed  bool
}

func newHub() *hub {
	return &hub{clients: make(map[*wsClient]struct{})}
}

func (h *hub) register(c *wsClient) bool {
	if h == nil || c == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.clients[c] = struct{}{}
	return true
}

func (h *hub) unregister(c *wsClient) {
	if h == nil || c == nil {
		return
	}
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.shutdown()
}

// broadcast only updates each client's in-memory latest slot. Socket writes
// happen in independent writer goroutines and therefore cannot block the hub.
func (h *hub) broadcast(msg interface{}) {
	if h == nil {
		return
	}
	h.mu.RLock()
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()
	for _, c := range clients {
		c.enqueue(msg)
	}
}

func (h *hub) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	clients := make([]*wsClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
		delete(h.clients, c)
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.shutdown()
	}
}

func (h *hub) clientCount() int {
	if h == nil {
		return 0
	}
	h.mu.RLock()
	n := len(h.clients)
	h.mu.RUnlock()
	return n
}
