package api

import (
	"crypto/subtle"
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
	syncGuildFn      SyncGuildFunc
	apiKey           string
}

type SendMessageFunc func(channelID, username, avatarURL, content, replyToID string) (string, error)
type SendFileFunc func(channelID, username, avatarURL, content, filename string, r io.Reader) (string, error)
type ResolveChannelFunc func(nameOrID string, guildID string) (string, error)
type SyncGuildFunc func(guildID string) error

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

func (s *Server) SetSyncGuildFn(fn SyncGuildFunc) {
	s.syncGuildFn = fn
}

func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API key must always be configured — enforced at startup.
		if s.apiKey == "" {
			writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "server misconfiguration"})
			return
		}

		// Accept the key only from the X-API-Key header.
		// Query-string keys are intentionally NOT accepted: they appear in
		// access logs, proxy logs, browser history, and Referer headers.
		key := r.Header.Get("X-API-Key")
		if key == "" {
			slog.Warn("missing api key", "path", r.URL.Path, "remote", r.RemoteAddr)
			writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "unauthorized"})
			return
		}

		// Constant-time comparison prevents timing oracle attacks that could
		// allow an attacker to brute-force the key character by character.
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
			slog.Warn("invalid api key", "path", r.URL.Path, "remote", r.RemoteAddr)
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
		w.Header().Set("Access-Control-Allow-Origin", "https://molly.ploglabs.com")
		w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
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

	ch, err := s.resolveChannel(req.Channel, req.GuildID)
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

	now := models.TimeNow()
	_ = s.store.InsertMessage(models.Message{
		ID:        msgID,
		ChannelID: ch,
		Author:    req.Username,
		Content:   req.Content,
		Timestamp: now,
	})

	// Broadcast to all WebSocket clients so other terminal users see it in real-time.
	// The Discord gateway echo for this webhook message is suppressed by isOwnWebhook,
	// so without this broadcast other terminal users would never receive the message.
	evt := models.RelayEvent{
		Type:      "message_create",
		Channel:   req.Channel,
		ChannelID: ch,
		Username:  req.Username,
		Content:   req.Content,
		MessageID: msgID,
		Timestamp: now.Format(time.RFC3339),
		ReplyToID: req.ReplyToID,
	}
	if data, err := json.Marshal(evt); err == nil {
		s.hub.Broadcast(data)
	}

	writeJSON(w, http.StatusOK, models.MessageResponse{
		OK:        true,
		MessageID: msgID,
		Channel:   req.Channel,
		Timestamp: now.Format(time.RFC3339),
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
	if len(content) > 2000 {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "content exceeds 2000 characters"})
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "file is required"})
		return
	}
	defer file.Close()

	ch, err := s.resolveChannel(channel, r.FormValue("guild_id"))
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

	chID, err := s.resolveChannel(channel, r.URL.Query().Get("guild_id"))
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
	if len(req.Username) > 100 {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "username exceeds 100 characters"})
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

	parseTimestamp := func(v string) (*time.Time, bool) {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			t2, err2 := time.Parse(time.RFC3339, v)
			if err2 != nil {
				return nil, false
			}
			t = t2
		}
		return &t, true
	}

	var before *time.Time
	if v := r.URL.Query().Get("before"); v != "" {
		t, ok := parseTimestamp(v)
		if !ok {
			writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid before timestamp"})
			return
		}
		before = t
	}

	var since *time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, ok := parseTimestamp(v)
		if !ok {
			writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid since timestamp"})
			return
		}
		since = t
	}

	chID, err := s.resolveChannel(channel, r.URL.Query().Get("guild_id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, models.APIResponse{OK: false, Error: "channel not found"})
		return
	}

	chName := channel
	if ch, err2 := s.store.GetChannelByID(chID); err2 == nil && ch != nil {
		chName = ch.Name
	}

	messages, err := s.store.GetMessagesByChannelTimestamp(chID, limit, before, since)
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

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) resolveChannel(nameOrID string, guildID string) (string, error) {
	if s.resolveChannelFn != nil {
		return s.resolveChannelFn(nameOrID, guildID)
	}

	ch, err := s.store.GetChannelByID(nameOrID)
	if err == nil && ch != nil {
		return ch.ID, nil
	}

	ch, err = s.store.GetChannelByName(nameOrID)
	if err == nil && ch != nil {
		return ch.ID, nil
	}

	return "", fmt.Errorf("channel not found: %s", nameOrID)
}

func (s *Server) GetGuilds(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	q := r.URL.Query().Get("q")
	var guilds []models.Guild
	var err error
	if q != "" {
		guilds, err = s.store.SearchGuilds(q)
	} else {
		guilds, err = s.store.GetGuilds()
	}
	if err != nil {
		slog.Error("failed to get guilds", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	if guilds == nil {
		guilds = []models.Guild{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"guilds": guilds})
}

func (s *Server) GetGuildChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	guildID := chi.URLParam(r, "guild_id")
	if guildID == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "guild_id is required"})
		return
	}

	channels, err := s.store.GetChannelsByGuild(guildID)
	if err != nil {
		slog.Error("failed to get guild channels", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	if channels == nil {
		channels = []models.Channel{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"channels": channels})
}

func (s *Server) CheckBotGuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	guildID := chi.URLParam(r, "guild_id")
	if guildID == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "guild_id is required"})
		return
	}

	hasGuild, err := s.store.HasGuild(guildID)
	if err != nil {
		slog.Error("failed to check guild", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	if !hasGuild && s.syncGuildFn != nil {
		slog.Info("guild not in DB, attempting live sync", "guild_id", guildID)
		if syncErr := s.syncGuildFn(guildID); syncErr != nil {
			slog.Warn("live guild sync failed", "guild_id", guildID, "error", syncErr)
		} else {
			hasGuild, _ = s.store.HasGuild(guildID)
		}
	}

	msg := "bot is not in this guild"
	if hasGuild {
		msg = "bot is in this guild"
	}
	writeJSON(w, http.StatusOK, models.BotCheckResponse{
		OK:         true,
		BotInGuild: hasGuild,
		GuildID:    guildID,
		Message:    msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

func (s *Server) PostSetupConfig(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		DiscordID   string `json:"discord_id"`
		GuildID     string `json:"guild_id"`
		GuildName   string `json:"guild_name"`
		ChannelID   string `json:"channel_id"`
		ChannelName string `json:"channel_name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid json"})
		return
	}

	if req.DiscordID == "" || req.GuildID == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "discord_id and guild_id are required"})
		return
	}

	cfg := storage.SetupConfig{
		DiscordID:   req.DiscordID,
		GuildID:     req.GuildID,
		GuildName:   req.GuildName,
		ChannelID:   req.ChannelID,
		ChannelName: req.ChannelName,
	}
	if err := s.store.SaveSetupConfig(cfg); err != nil {
		slog.Error("failed to save setup config", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, models.APIResponse{OK: true, Message: "config saved"})
}

func (s *Server) GetSetupConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	discordID := chi.URLParam(r, "discord_id")
	if discordID == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "discord_id is required"})
		return
	}

	cfg, err := s.store.GetSetupConfig(discordID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, models.APIResponse{OK: false, Error: "no config found"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":           true,
		"discord_id":   cfg.DiscordID,
		"guild_id":     cfg.GuildID,
		"guild_name":   cfg.GuildName,
		"channel_id":   cfg.ChannelID,
		"channel_name": cfg.ChannelName,
	})
}
