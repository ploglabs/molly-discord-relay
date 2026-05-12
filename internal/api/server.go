package api

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ploglabs/molly-discord-relay/internal/models"
	"github.com/ploglabs/molly-discord-relay/internal/presence"
	"github.com/ploglabs/molly-discord-relay/internal/storage"
	"github.com/ploglabs/molly-discord-relay/internal/websocket"
)

type Server struct {
	store            *storage.Store
	hub              *websocket.Hub
	tracker          *presence.Tracker
	sendMsg          SendMessageFunc
	sendFile         SendFileFunc
	resolveChannelFn ResolveChannelFunc
	apiKey           string
}

type SendMessageFunc func(channelID, username, avatarURL, content, replyToID string) (string, error)
type SendFileFunc func(channelID, username, avatarURL, content, filename string, r io.Reader) (string, error)
type ResolveChannelFunc func(nameOrID string) (string, error)

func NewServer(store *storage.Store, hub *websocket.Hub, tracker *presence.Tracker, sendMsg SendMessageFunc, sendFile SendFileFunc, resolveCh ResolveChannelFunc, apiKey string) *Server {
	return &Server{
		store:            store,
		hub:              hub,
		tracker:          tracker,
		sendMsg:          sendMsg,
		sendFile:         sendFile,
		resolveChannelFn: resolveCh,
		apiKey:           apiKey,
	}
}

func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Header.Get("X-API-Key")
		if key != s.apiKey {
			slog.Warn("unauthorized request", "path", r.URL.Path, "remote", r.RemoteAddr)
			writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) SecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) PostMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "failed to read body"})
		return
	}
	defer r.Body.Close()

	var req models.SendMessageRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid json"})
		return
	}

	req.Channel = strings.TrimSpace(req.Channel)
	req.Content = strings.ReplaceAll(req.Content, "\x00", "")
	req.Content = strings.TrimSpace(req.Content)
	req.Username = strings.TrimSpace(req.Username)

	if req.Channel == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "channel is required"})
		return
	}
	if req.Content == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "content is required"})
		return
	}
	if len(req.Content) > 2000 {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "content exceeds 2000 characters"})
		return
	}

	ch, err := s.resolveChannel(req.Channel)
	if err != nil {
		writeJSON(w, http.StatusNotFound, models.APIResponse{OK: false, Error: "channel not found"})
		return
	}

	msgID, err := s.sendMsg(ch, req.Username, req.AvatarURL, req.Content, req.ReplyToID)
	if err != nil {
		slog.Error("failed to send message to discord", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "failed to send message"})
		return
	}

	_ = s.store.InsertMessage(models.Message{
		ID:        msgID,
		ChannelID: ch,
		Author:    req.Username,
		Content:   req.Content,
		Timestamp: models.TimeNow(),
	})

	writeJSON(w, http.StatusOK, models.MessageResponse{
		OK:        true,
		MessageID: msgID,
		Channel:   req.Channel,
		Timestamp: models.TimeNow().Format("2006-01-02T15:04:05Z"),
	})
}

func (s *Server) PostFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}
	if s.sendFile == nil {
		writeJSON(w, http.StatusServiceUnavailable, models.APIResponse{OK: false, Error: "file sending is not configured"})
		return
	}
	if err := r.ParseMultipartForm(25 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid multipart form"})
		return
	}

	channel := strings.TrimSpace(r.FormValue("channel"))
	username := strings.TrimSpace(r.FormValue("username"))
	avatarURL := strings.TrimSpace(r.FormValue("avatar_url"))
	content := strings.TrimSpace(strings.ReplaceAll(r.FormValue("content"), "\x00", ""))
	if channel == "" || username == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "channel and username are required"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "file is required"})
		return
	}
	defer file.Close()

	ch, err := s.resolveChannel(channel)
	if err != nil {
		writeJSON(w, http.StatusNotFound, models.APIResponse{OK: false, Error: "channel not found"})
		return
	}

	msgID, err := s.sendFile(ch, username, avatarURL, content, header.Filename, file)
	if err != nil {
		slog.Error("failed to send file to discord", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "failed to send file"})
		return
	}

	storedContent := content
	if storedContent == "" {
		storedContent = header.Filename
	}
	_ = s.store.InsertMessage(models.Message{
		ID:        msgID,
		ChannelID: ch,
		Author:    username,
		Content:   storedContent,
		Timestamp: models.TimeNow(),
	})

	writeJSON(w, http.StatusOK, models.MessageResponse{
		OK:        true,
		MessageID: msgID,
		Channel:   channel,
		Timestamp: models.TimeNow().Format(time.RFC3339),
	})
}

func (s *Server) GetHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	channel := r.URL.Query().Get("channel")
	if channel == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "channel is required"})
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		parsed, err := models.Atoi(v)
		if err != nil || parsed <= 0 || parsed > 1000 {
			writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid limit"})
			return
		}
		limit = parsed
	}

	before := r.URL.Query().Get("before")
	after := r.URL.Query().Get("after")

	chID, err := s.resolveChannel(channel)
	if err != nil {
		writeJSON(w, http.StatusNotFound, models.APIResponse{OK: false, Error: "channel not found"})
		return
	}

	messages, err := s.store.GetMessages(chID, limit, before, after)
	if err != nil {
		slog.Error("failed to fetch history", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	hasMore := len(messages) == limit
	writeJSON(w, http.StatusOK, models.HistoryResponse{
		Messages: messages,
		HasMore:  hasMore,
	})
}

func (s *Server) PostStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "failed to read body"})
		return
	}
	defer r.Body.Close()

	var req models.SetStatusRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid json"})
		return
	}

	req.Username = strings.TrimSpace(req.Username)
	req.Status = strings.ReplaceAll(req.Status, "\x00", "")
	req.Status = strings.TrimSpace(req.Status)

	if req.Username == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "username is required"})
		return
	}
	if req.Status == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "status is required"})
		return
	}
	if len(req.Status) > 128 {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "status exceeds 128 characters"})
		return
	}

	if err := s.tracker.SetStatus(req.Username, req.Username, req.Status); err != nil {
		slog.Error("failed to set status", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{OK: true, Message: "status updated"})
}

func (s *Server) GetPresence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	users, err := s.tracker.GetOnlineUsers()
	if err != nil {
		slog.Error("failed to get online users", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, models.PresenceResponse{Users: users})
}

func (s *Server) GetTerminalUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}
	users := s.hub.GetTerminalUsers()
	if users == nil {
		users = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"users": users})
}

func (s *Server) GetChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	channels, err := s.store.GetChannels()
	if err != nil {
		slog.Error("failed to get channels", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, channels)
}

type terminalMessage struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Content   string    `json:"content"`
	Channel   string    `json:"channel"`
	Timestamp time.Time `json:"timestamp"`
}

// GetChannelMessages serves GET /api/channels/{channel}/messages for molly-terminal.
// Returns messages in the format expected by the terminal client.
func (s *Server) GetChannelMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	channel := chi.URLParam(r, "channel")
	if channel == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "channel is required"})
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		parsed, err := models.Atoi(v)
		if err != nil || parsed <= 0 || parsed > 1000 {
			writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid limit"})
			return
		}
		limit = parsed
	}

	var before *time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			t2, err2 := time.Parse(time.RFC3339, v)
			if err2 != nil {
				writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid before timestamp"})
				return
			}
			t = t2
		}
		before = &t
	}

	chID, err := s.resolveChannel(channel)
	if err != nil {
		writeJSON(w, http.StatusNotFound, models.APIResponse{OK: false, Error: "channel not found"})
		return
	}

	chName := channel
	if ch, err2 := s.store.GetChannelByID(chID); err2 == nil && ch != nil {
		chName = ch.Name
	}

	messages, err := s.store.GetMessagesByChannelTimestamp(chID, limit, before)
	if err != nil {
		slog.Error("failed to fetch channel messages", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	result := make([]terminalMessage, 0, len(messages))
	for _, m := range messages {
		result = append(result, terminalMessage{
			ID:        m.ID,
			Username:  m.Author,
			Content:   m.Content,
			Channel:   chName,
			Timestamp: m.Timestamp,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) resolveChannel(nameOrID string) (string, error) {
	ch, err := s.store.GetChannelByName(nameOrID)
	if err == nil && ch != nil {
		return ch.ID, nil
	}

	ch, err = s.store.GetChannelByID(nameOrID)
	if err == nil && ch != nil {
		return ch.ID, nil
	}

	if s.resolveChannelFn != nil {
		return s.resolveChannelFn(nameOrID)
	}

	return "", fmt.Errorf("channel not found: %s", nameOrID)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
