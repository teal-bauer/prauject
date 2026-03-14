package server

import (
	"sync"

	"github.com/gorilla/websocket"
)

type Hub struct {
	mu    sync.RWMutex
	conns map[string]map[*wsConn]bool
}

type wsConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (c *wsConn) writeMessage(msgType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(msgType, data)
}

func newHub() *Hub {
	return &Hub{
		conns: make(map[string]map[*wsConn]bool),
	}
}

func (h *Hub) register(id string, conn *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[id] == nil {
		h.conns[id] = make(map[*wsConn]bool)
	}
	h.conns[id][conn] = true
}

func (h *Hub) unregister(id string, conn *wsConn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conns[id] != nil {
		delete(h.conns[id], conn)
		if len(h.conns[id]) == 0 {
			delete(h.conns, id)
		}
	}
}
