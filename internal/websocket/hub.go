package websocket

import (
	"encoding/json"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"
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
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 256),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		usernames:  make(map[*Client]string),
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
				close(client.send)
			}
			h.mu.Unlock()
			h.clientCount.Add(-1)
			slog.Info("client disconnected", "remote", client.conn.RemoteAddr().String(), "total", h.clientCount.Load())
			h.RemoveUsername(client)

		case message := <-h.broadcast:
			h.mu.RLock()
			for client := range h.clients {
				select {
				case client.send <- message:
				default:
					h.mu.RUnlock()
					h.mu.Lock()
					close(client.send)
					delete(h.clients, client)
					h.mu.Unlock()
					h.mu.RLock()
				}
			}
			h.mu.RUnlock()
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

	select {
	case <-time.After(time.Second * 5):
		slog.Warn("hub shutdown timeout")
	default:
	}
}
