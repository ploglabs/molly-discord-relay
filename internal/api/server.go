package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/ploglabs/molly-discord-relay/internal/audit"
	"github.com/ploglabs/molly-discord-relay/internal/auth"
	"github.com/ploglabs/molly-discord-relay/internal/models"
	"github.com/ploglabs/molly-discord-relay/internal/presence"
	"github.com/ploglabs/molly-discord-relay/internal/ratelimit"
	"github.com/ploglabs/molly-discord-relay/internal/storage"
	"github.com/ploglabs/molly-discord-relay/internal/websocket"
)

// contextKey is an unexported type for context keys to avoid collisions.
type contextKey int

const sessionContextKey contextKey = 0

// Server handles all HTTP API requests.
type Server struct {
	store            *storage.Store
	hub              *websocket.Hub
	tracker          *presence.Tracker
	sendMsg          SendMessageFunc
	sendFile         SendFileFunc
	resolveChannelFn ResolveChannelFunc
	syncGuildFn      SyncGuildFunc
	apiKey           string  // legacy, kept for backward-compat deprecation window
	encryptionKey    string  // WEBHOOK_ENCRYPTION_KEY hex
	limiter          *ratelimit.Limiter
	deviceFlowFn     DeviceFlowFunc // nil if Discord OAuth not configured
}

type SendMessageFunc func(channelID, username, avatarURL, content, replyToID string) (string, error)
type SendFileFunc func(channelID, username, avatarURL, content, filename string, r io.Reader) (string, error)
type ResolveChannelFunc func(nameOrID string, guildID string) (string, error)
type SyncGuildFunc func(guildID string) error
// DeviceFlowFunc initiates a Discord OAuth2 device flow and returns flow state.
type DeviceFlowFunc func() (*auth.DeviceFlowState, error)

func NewServer(
	store *storage.Store,
	hub *websocket.Hub,
	tracker *presence.Tracker,
	sendMsg SendMessageFunc,
	sendFile SendFileFunc,
	resolveCh ResolveChannelFunc,
	apiKey string,
	encryptionKey string,
) *Server {
	l := ratelimit.New()
	l.StartCleanup()
	return &Server{
		store:         store,
		hub:           hub,
		tracker:       tracker,
		sendMsg:       sendMsg,
		sendFile:      sendFile,
		resolveChannelFn: resolveCh,
		apiKey:        apiKey,
		encryptionKey: encryptionKey,
		limiter:       l,
	}
}

func (s *Server) SetSyncGuildFn(fn SyncGuildFunc)   { s.syncGuildFn = fn }
func (s *Server) SetDeviceFlowFn(fn DeviceFlowFunc) { s.deviceFlowFn = fn }

// EncryptionKey returns the webhook encryption key (for use by discord package).
func (s *Server) EncryptionKey() string { return s.encryptionKey }

// SessionFromContext retrieves the verified session from a request context.
// Returns nil if the request was authenticated via legacy X-API-Key.
func SessionFromContext(ctx context.Context) *auth.Session {
	s, _ := ctx.Value(sessionContextKey).(*auth.Session)
	return s
}

// AuthMiddleware supports two auth mechanisms (during migration period):
//
//  1. Authorization: Bearer <session_token>  — new per-user sessions
//  2. X-API-Key: <key>                       — legacy shared key (deprecated)
//
// Legacy key auth still works but returns X-Molly-Deprecation header.
func (s *Server) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// --- Try Bearer token first ---
		if authHeader := r.Header.Get("Authorization"); strings.HasPrefix(authHeader, "Bearer ") {
			rawToken := strings.TrimPrefix(authHeader, "Bearer ")
			if rawToken == "" {
				audit.LogAuthFailed(r.RemoteAddr, "empty bearer token")
				writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "unauthorized"})
				return
			}

			tokenHash := auth.HashToken(rawToken)
			sess, err := s.store.GetSessionByHash(tokenHash)
			if err != nil {
				audit.LogAuthFailed(r.RemoteAddr, "session not found")
				writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "unauthorized"})
				return
			}

			if time.Now().After(sess.ExpiresAt) {
				audit.Log(audit.AuditEvent{
					Event:      audit.EventSessionExpired,
					DiscordID:  sess.DiscordID,
					Username:   sess.Username,
					SessionID:  sess.TokenHash,
					RemoteAddr: r.RemoteAddr,
				})
				writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "session expired"})
				return
			}

			ctx := context.WithValue(r.Context(), sessionContextKey, sess)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// --- Fall back to legacy X-API-Key ---
		if s.apiKey == "" {
			writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "server misconfiguration"})
			return
		}

		key := r.Header.Get("X-API-Key")
		if key == "" {
			slog.Warn("missing auth credential", "path", r.URL.Path, "remote", r.RemoteAddr)
			audit.LogAuthFailed(r.RemoteAddr, "no credentials provided")
			writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "unauthorized"})
			return
		}

		if subtle.ConstantTimeCompare([]byte(key), []byte(s.apiKey)) != 1 {
			slog.Warn("invalid api key", "path", r.URL.Path, "remote", r.RemoteAddr)
			audit.LogAuthFailed(r.RemoteAddr, "invalid api key")
			writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "unauthorized"})
			return
		}

		// Legacy auth accepted — warn caller to upgrade.
		w.Header().Set("X-Molly-Deprecation",
			"X-API-Key auth is deprecated. Upgrade molly to use session-based auth.")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) SecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Access-Control-Allow-Origin", "https://molly.ploglabs.com")
		w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Authorization, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
		next.ServeHTTP(w, r)
	})
}

// --- Auth endpoints ---

// PostDeviceAuthorize starts an OAuth2 device flow.
// POST /auth/device/authorize
func (s *Server) PostDeviceAuthorize(w http.ResponseWriter, r *http.Request) {
	if s.deviceFlowFn == nil {
		writeJSON(w, http.StatusServiceUnavailable, models.APIResponse{
			OK:    false,
			Error: "device auth not configured (set DISCORD_CLIENT_ID and DISCORD_CLIENT_SECRET)",
		})
		return
	}

	flow, err := s.deviceFlowFn()
	if err != nil {
		slog.Error("device flow init failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "failed to start device flow"})
		return
	}

	audit.Log(audit.AuditEvent{
		Event:      audit.EventAuthDeviceStart,
		RemoteAddr: r.RemoteAddr,
		Details:    flow.UserCode,
	})

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":               true,
		"device_code":      flow.DeviceCode,
		"user_code":        flow.UserCode,
		"verification_uri": flow.VerificationURI,
		"expires_in":       int(time.Until(flow.ExpiresAt).Seconds()),
		"interval":         flow.Interval,
	})
}

// PostDeviceToken polls whether a device flow has completed.
// POST /auth/device/token
func (s *Server) PostDeviceToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.DeviceCode == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "device_code is required"})
		return
	}

	flow, err := s.store.GetDeviceFlow(req.DeviceCode)
	if err != nil {
		writeJSON(w, http.StatusNotFound, models.APIResponse{OK: false, Error: "device flow not found or expired"})
		return
	}

	if time.Now().After(flow.ExpiresAt) {
		writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "device code expired"})
		return
	}

	switch flow.Status {
	case "pending":
		writeJSON(w, http.StatusAccepted, map[string]interface{}{
			"ok":     false,
			"status": "pending",
			"error":  "authorization_pending",
		})
	case "authorized":
		// Return session token once, then clear it.
		token := flow.SessionToken
		if token == "" {
			writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "session token missing"})
			return
		}
		audit.Log(audit.AuditEvent{
			Event:      audit.EventAuthDeviceComplete,
			DiscordID:  flow.DiscordID,
			Username:   flow.Username,
			RemoteAddr: r.RemoteAddr,
		})
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":            true,
			"session_token": token,
			"username":      flow.Username,
			"avatar_url":    flow.AvatarURL,
			"discord_id":    flow.DiscordID,
		})
	case "expired":
		writeJSON(w, http.StatusUnauthorized, models.APIResponse{OK: false, Error: "authorization expired"})
	default:
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "unknown flow status"})
	}
}

// GetSession returns info about the current session.
// GET /auth/session
func (s *Server) GetSession(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		// Legacy X-API-Key path — no per-user identity available.
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":        true,
			"auth_type": "api_key",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":         true,
		"auth_type":  "session",
		"discord_id": sess.DiscordID,
		"username":   sess.Username,
		"avatar_url": sess.AvatarURL,
		"expires_at": sess.ExpiresAt.Format(time.RFC3339),
	})
}

// PostRevokeSession logs out the current session.
// POST /auth/session/revoke
func (s *Server) PostRevokeSession(w http.ResponseWriter, r *http.Request) {
	sess := SessionFromContext(r.Context())
	if sess == nil {
		// Legacy key — nothing to revoke.
		writeJSON(w, http.StatusOK, models.APIResponse{OK: true, Message: "ok"})
		return
	}
	if err := s.store.DeleteSession(sess.TokenHash); err != nil {
		slog.Error("failed to revoke session", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "internal error"})
		return
	}
	audit.LogSessionRevoked(sess.DiscordID, sess.Username, sess.TokenHash)
	writeJSON(w, http.StatusOK, models.APIResponse{OK: true, Message: "session revoked"})
}

// --- Message endpoints ---

func (s *Server) PostMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, models.APIResponse{OK: false, Error: "method not allowed"})
		return
	}

	// Rate limiting: apply per session, or per remote addr for legacy key.
	sess := SessionFromContext(r.Context())
	limitKey := r.RemoteAddr
	if sess != nil {
		limitKey = sess.TokenHash
	}
	if !s.limiter.AllowMessage(limitKey) {
		if sess != nil {
			audit.LogRateLimited(sess.DiscordID, sess.Username, sess.TokenHash, "POST /message", r.RemoteAddr)
		}
		writeJSON(w, http.StatusTooManyRequests, models.APIResponse{OK: false, Error: "rate limit exceeded"})
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

	// Phase 3: Server-side identity verification.
	// If authenticated via session, ignore client-supplied username/avatar.
	username := strings.TrimSpace(req.Username)
	avatarURL := strings.TrimSpace(req.AvatarURL)
	if sess != nil {
		username = sess.Username
		avatarURL = sess.AvatarURL
	}

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

	msgID, err := s.sendMsg(ch, username, avatarURL, req.Content, req.ReplyToID)
	if err != nil {
		slog.Error("failed to send message to discord", "error", err)
		writeJSON(w, http.StatusInternalServerError, models.APIResponse{OK: false, Error: "failed to send message"})
		return
	}

	now := models.TimeNow()
	_ = s.store.InsertMessage(models.Message{
		ID:        msgID,
		ChannelID: ch,
		Author:    username,
		Content:   req.Content,
		Timestamp: now,
	})

	if sess != nil {
		audit.LogMessageSent(sess.DiscordID, sess.Username, sess.TokenHash, req.Channel, msgID, r.RemoteAddr)
	}

	evt := models.RelayEvent{
		Type:      "message_create",
		Channel:   req.Channel,
		ChannelID: ch,
		Username:  username,
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

	sess := SessionFromContext(r.Context())
	limitKey := r.RemoteAddr
	if sess != nil {
		limitKey = sess.TokenHash
	}
	if !s.limiter.AllowFile(limitKey) {
		if sess != nil {
			audit.LogRateLimited(sess.DiscordID, sess.Username, sess.TokenHash, "POST /file", r.RemoteAddr)
		}
		writeJSON(w, http.StatusTooManyRequests, models.APIResponse{OK: false, Error: "rate limit exceeded"})
		return
	}

	if err := r.ParseMultipartForm(25 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "invalid multipart form"})
		return
	}

	channel := strings.TrimSpace(r.FormValue("channel"))
	content := strings.TrimSpace(strings.ReplaceAll(r.FormValue("content"), "\x00", ""))

	// Phase 3: server-side identity for file uploads too.
	username := strings.TrimSpace(r.FormValue("username"))
	avatarURL := strings.TrimSpace(r.FormValue("avatar_url"))
	if sess != nil {
		username = sess.Username
		avatarURL = sess.AvatarURL
	}

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

	if sess != nil {
		audit.Log(audit.AuditEvent{
			Event:      audit.EventFileSent,
			DiscordID:  sess.DiscordID,
			Username:   sess.Username,
			SessionID:  sess.TokenHash,
			Channel:    channel,
			MessageID:  msgID,
			RemoteAddr: r.RemoteAddr,
		})
	}

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

	sess := SessionFromContext(r.Context())
	limitKey := r.RemoteAddr
	if sess != nil {
		limitKey = sess.TokenHash
	}
	if !s.limiter.AllowHistory(limitKey) {
		writeJSON(w, http.StatusTooManyRequests, models.APIResponse{OK: false, Error: "rate limit exceeded"})
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

	sess := SessionFromContext(r.Context())
	limitKey := r.RemoteAddr
	if sess != nil {
		limitKey = sess.TokenHash
	}
	if !s.limiter.AllowStatus(limitKey) {
		writeJSON(w, http.StatusTooManyRequests, models.APIResponse{OK: false, Error: "rate limit exceeded"})
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

	// Phase 3: use verified identity for status updates.
	username := strings.TrimSpace(req.Username)
	if sess != nil {
		username = sess.Username
	}

	req.Status = strings.ReplaceAll(req.Status, "\x00", "")
	req.Status = strings.TrimSpace(req.Status)

	if username == "" {
		writeJSON(w, http.StatusBadRequest, models.APIResponse{OK: false, Error: "username is required"})
		return
	}
	if len(username) > 100 {
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

	if err := s.tracker.SetStatus(username, username, req.Status); err != nil {
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
