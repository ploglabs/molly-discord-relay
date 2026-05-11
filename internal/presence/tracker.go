package presence

import (
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/ploglabs/molly-discord-relay/internal/models"
	"github.com/ploglabs/molly-discord-relay/internal/storage"
	"github.com/ploglabs/molly-discord-relay/internal/websocket"
)

type Tracker struct {
	store *storage.Store
	hub   *websocket.Hub
	mu    sync.RWMutex
	last  map[string]time.Time
}

func NewTracker(store *storage.Store, hub *websocket.Hub) *Tracker {
	return &Tracker{
		store: store,
		hub:   hub,
		last:  make(map[string]time.Time),
	}
}

func (t *Tracker) SetOnline(userID, username string) error {
	if err := t.store.SetUserOnline(userID, username); err != nil {
		return err
	}

	evt := models.RelayEvent{
		Type:      "presence_online",
		UserID:    userID,
		Username:  username,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	t.broadcastEvent(evt)
	return nil
}

func (t *Tracker) SetOffline(userID string) error {
	if err := t.store.SetUserOffline(userID); err != nil {
		return err
	}

	evt := models.RelayEvent{
		Type:      "presence_offline",
		UserID:    userID,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	t.broadcastEvent(evt)
	return nil
}

func (t *Tracker) SetStatus(userID, username, status string) error {
	if t.shouldDebounce(userID, status) {
		slog.Debug("status debounced", "user_id", userID)
		return nil
	}

	t.mu.Lock()
	t.last[userID+"::"+status] = time.Now()
	t.mu.Unlock()

	if err := t.store.SetStatus(userID, status); err != nil {
		return err
	}

	evt := models.RelayEvent{
		Type:      "status_update",
		UserID:    userID,
		Username:  username,
		Content:   status,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	t.broadcastEvent(evt)
	return nil
}

func (t *Tracker) GetOnlineUsers() ([]models.User, error) {
	return t.store.ListOnlineUsers()
}

func (t *Tracker) GetStatus(userID string) (*models.Status, error) {
	return t.store.GetStatus(userID)
}

func (t *Tracker) GetAllStatuses() ([]models.Status, error) {
	return t.store.GetAllStatuses()
}

func (t *Tracker) broadcastEvent(evt models.RelayEvent) {
	data, err := json.Marshal(evt)
	if err != nil {
		slog.Error("failed to marshal presence event", "error", err)
		return
	}
	t.hub.Broadcast(data)
}

func (t *Tracker) shouldDebounce(userID, status string) bool {
	key := userID + "::" + status
	t.mu.RLock()
	defer t.mu.RUnlock()

	if last, ok := t.last[key]; ok {
		return time.Since(last) < 5*time.Second
	}
	return false
}