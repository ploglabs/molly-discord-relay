package websocket

import (
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
)

type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
	clientCount atomic.Int64

	usernames map[*Client]string
	userMu    sync.RWMutex

	// closedSend tracks clients whose send channel has already been closed
	// by the slow-client eviction path, to prevent a double-close panic in
	// the unregister handler.
	closedSend map[*Client]bool
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		usernames:  make(map[*Client]string),
		closedSend: make(map[*Client]bool),
	}
}

func (h *Hub) Run() {
	slog.Info("websocket hub started")
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
			h.clientCount.Add(1)
			slog.Info("client connected", "remote", client.conn.RemoteAddr().String(), "total", h.clientCount.Load())

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				// Only close if the slow-eviction path hasn't already closed it.
				if !h.closedSend[client] {
					close(client.send)
				}
				delete(h.closedSend, client)
			}
			h.mu.Unlock()
			h.clientCount.Add(-1)
			slog.Info("client disconnected", "remote", client.conn.RemoteAddr().String(), "total", h.clientCount.Load())
			h.RemoveUsername(client)

		case message := <-h.broadcast:
			h.mu.RLock()
			var slow []*Client
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					slow = append(slow, client)
				}
			}
			h.mu.RUnlock()
			if len(slow) > 0 {
				h.mu.Lock()
				for _, client := range slow {
					if _, ok := h.clients[client]; ok {
						h.closedSend[client] = true
						close(client.send)
						delete(h.clients, client)
						h.clientCount.Add(-1)
						h.RemoveUsername(client)
					}
				}
				h.mu.Unlock()
			}
		}
	}
}

func (h *Hub) Broadcast(data []byte) {
	select {
	case h.broadcast <- data:
	default:
		slog.Warn("broadcast channel full, dropping message")
	}
}

func (h *Hub) Register(client *Client) {
	h.register <- client
}

func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
}

func (h *Hub) ClientCount() int64 {
	return h.clientCount.Load()
}

func (h *Hub) SetUsername(client *Client, username string) {
	h.userMu.Lock()
	h.usernames[client] = username
	h.userMu.Unlock()
	h.broadcastTerminalUsers()
}

func (h *Hub) RemoveUsername(client *Client) {
	h.userMu.Lock()
	had := h.usernames[client] != ""
	delete(h.usernames, client)
	h.userMu.Unlock()
	if had {
		h.broadcastTerminalUsers()
	}
}

func (h *Hub) GetTerminalUsers() []string {
	h.userMu.RLock()
	defer h.userMu.RUnlock()
	seen := make(map[string]struct{})
	var users []string
	for _, u := range h.usernames {
		if u == "" {
			continue
		}
		if _, ok := seen[u]; !ok {
			seen[u] = struct{}{}
			users = append(users, u)
		}
	}
	sort.Strings(users)
	return users
}

func (h *Hub) broadcastTerminalUsers() {
	users := h.GetTerminalUsers()
	if users == nil {
		users = []string{}
	}
	evt := struct {
		Type  string   `json:"type"`
		Users []string `json:"users"`
	}{Type: "terminal_online", Users: users}
	data, err := json.Marshal(evt)
	if err != nil {
		return
	}
	h.Broadcast(data)
}

func (h *Hub) Shutdown() {
	h.mu.Lock()
	defer h.mu.Unlock()

	slog.Info("hub shutting down", "clients", len(h.clients))
	for client := range h.clients {
		close(client.send)
		delete(h.clients, client)
	}
}
