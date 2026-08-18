package web

import (
	"sync"

	"github.com/gorilla/websocket"
	"opensqt/logger"
)

type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *wsClient) writeJSON(v interface{}) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteJSON(v)
}

type hub struct {
	mu         sync.Mutex
	clients    map[*wsClient]struct{}
	register   chan *wsClient
	unregister chan *wsClient
	broadcast  chan interface{}
}

func newHub() *hub {
	return &hub{
		clients:    make(map[*wsClient]struct{}),
		register:   make(chan *wsClient, 8),
		unregister: make(chan *wsClient, 8),
		broadcast:  make(chan interface{}, 4),
	}
}

func (h *hub) run() {
	for {
		select {
		case c := <-h.register:
			h.mu.Lock()
			h.clients[c] = struct{}{}
			h.mu.Unlock()
		case c := <-h.unregister:
			h.remove(c)
		case msg := <-h.broadcast:
			h.mu.Lock()
			list := make([]*wsClient, 0, len(h.clients))
			for c := range h.clients {
				list = append(list, c)
			}
			h.mu.Unlock()
			for _, c := range list {
				if err := c.writeJSON(msg); err != nil {
					logger.Debug("监控面板 WS 写入失败: %v", err)
					h.remove(c)
				}
			}
		}
	}
}

func (h *hub) remove(c *wsClient) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		_ = c.conn.Close()
	}
	h.mu.Unlock()
}

func (h *hub) clientCount() int {
	h.mu.Lock()
	n := len(h.clients)
	h.mu.Unlock()
	return n
}
