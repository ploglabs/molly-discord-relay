package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/ploglabs/molly-discord-relay/internal/api"
	"github.com/ploglabs/molly-discord-relay/internal/auth"
	"github.com/ploglabs/molly-discord-relay/internal/config"
	"github.com/ploglabs/molly-discord-relay/internal/discord"
	"github.com/ploglabs/molly-discord-relay/internal/models"
	"github.com/ploglabs/molly-discord-relay/internal/presence"
	"github.com/ploglabs/molly-discord-relay/internal/storage"
	"github.com/ploglabs/molly-discord-relay/internal/websocket"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	initLogger(cfg.LogLevel)

	store, err := storage.Open(cfg.DatabasePath)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	hub := websocket.NewHub()
	go hub.Run()

	bot, err := discord.New(cfg.DiscordToken, store, hub, cfg.WebhookEncryptionKey)
	if err != nil {
		slog.Error("failed to create discord bot", "error", err)
		os.Exit(1)
	}

	if err := bot.Connect(); err != nil {
		slog.Error("failed to connect to discord", "error", err)
		os.Exit(1)
	}

	sendMsg := func(channelID, username, avatarURL, content, replyToID string) (string, error) {
		return bot.SendWebhookMessage(channelID, username, avatarURL, content, replyToID)
	}
	sendFile := func(channelID, username, avatarURL, content, filename string, r io.Reader) (string, error) {
		return bot.SendWebhookFile(channelID, username, avatarURL, content, filename, r)
	}

	tracker := presence.NewTracker(store, hub)

	resolveChannel := func(nameOrID string, guildID string) (string, error) {
		ch, err := bot.Session().Channel(nameOrID)
		if err == nil && ch != nil {
			if ch.Type == discordgo.ChannelTypeGuildText || ch.Type == discordgo.ChannelTypeGuildNews {
				_ = store.UpsertChannel(models.Channel{ID: ch.ID, Name: ch.Name, GuildID: ch.GuildID, Type: "text"})
				return ch.ID, nil
			}
			return "", fmt.Errorf("channel not found: %s", nameOrID)
		}

		if guildID != "" {
			channels, err := bot.Session().GuildChannels(guildID)
			if err == nil {
				for _, c := range channels {
					if c.Name == nameOrID && (c.Type == discordgo.ChannelTypeGuildText || c.Type == discordgo.ChannelTypeGuildNews) {
						_ = store.UpsertChannel(models.Channel{ID: c.ID, Name: c.Name, GuildID: c.GuildID, Type: "text"})
						return c.ID, nil
					}
				}
			}
			return "", fmt.Errorf("channel not found in guild: %s", nameOrID)
		}

		for _, g := range bot.Session().State.Guilds {
			for _, c := range g.Channels {
				if c.Name == nameOrID {
					if c.Type == discordgo.ChannelTypeGuildText || c.Type == discordgo.ChannelTypeGuildNews {
						_ = store.UpsertChannel(models.Channel{ID: c.ID, Name: c.Name, GuildID: g.ID, Type: "text"})
						return c.ID, nil
					}
				}
			}
		}

		return "", fmt.Errorf("channel not found: %s", nameOrID)
	}

	srv := api.NewServer(store, hub, tracker, sendMsg, sendFile, resolveChannel,
		cfg.APIKey, cfg.WebhookEncryptionKey)

	syncGuild := func(guildID string) error {
		channels, err := bot.Session().GuildChannels(guildID)
		if err != nil {
			return err
		}
		for _, ch := range channels {
			if ch.Type == discordgo.ChannelTypeGuildText || ch.Type == discordgo.ChannelTypeGuildNews {
				_ = store.UpsertChannel(models.Channel{
					ID:      ch.ID,
					Name:    ch.Name,
					GuildID: ch.GuildID,
					Type:    "text",
				})
			}
		}
		return nil
	}
	srv.SetSyncGuildFn(syncGuild)

	// Device flow: only configure if Discord OAuth2 credentials are set.
	if cfg.DiscordClientID != "" && cfg.DiscordClientSecret != "" {
		deviceFlowFn := makeDeviceFlowFn(cfg, store)
		srv.SetDeviceFlowFn(deviceFlowFn)
		// Start background poller to finalize pending device flows.
		go pollDeviceFlows(store, cfg)
		slog.Info("discord device flow auth configured", "client_id", cfg.DiscordClientID)
	} else {
		slog.Warn("DISCORD_CLIENT_ID/SECRET not set — device flow auth disabled; only X-API-Key and manual session tokens work")
	}

	// Background: session cleanup every 10 minutes.
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			n, err := store.CleanupSessions()
			if err != nil {
				slog.Error("session cleanup error", "error", err)
			} else if n > 0 {
				slog.Info("session cleanup", "removed", n)
			}
			_ = store.CleanupDeviceFlows()
		}
	}()

	r := chi.NewRouter()
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(srv.SecurityMiddleware)

	// Public auth endpoints (no auth middleware).
	r.Post("/auth/device/authorize", srv.PostDeviceAuthorize)
	r.Post("/auth/device/token", srv.PostDeviceToken)
	r.Get("/api/bot/check/{guild_id}", srv.CheckBotGuild)

	r.Group(func(r chi.Router) {
		r.Use(srv.AuthMiddleware)
		r.Get("/auth/session", srv.GetSession)
		r.Post("/auth/session/revoke", srv.PostRevokeSession)

		r.Post("/message", srv.PostMessage)
		r.Post("/file", srv.PostFile)
		r.Post("/status", srv.PostStatus)
		r.Get("/history", srv.GetHistory)
		r.Get("/presence", srv.GetPresence)
		r.Get("/api/channels", srv.GetChannels)
		r.Get("/api/channels/{channel}/messages", srv.GetChannelMessages)
		r.Get("/api/terminal/users", srv.GetTerminalUsers)
		r.Get("/api/guilds", srv.GetGuilds)
		r.Get("/api/guilds/{guild_id}/channels", srv.GetGuildChannels)

		r.Post("/api/setup/config", srv.PostSetupConfig)
		r.Get("/api/setup/config/{discord_id}", srv.GetSetupConfig)

		r.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
			websocket.ServeWS(hub, w, r)
		})
	})

	httpServer := &http.Server{
		Addr:              fmt.Sprintf(":%s", cfg.Port),
		Handler:           r,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		slog.Info("server starting", "port", cfg.Port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	slog.Info("shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := httpServer.Shutdown(ctx); err != nil {
		slog.Error("server shutdown error", "error", err)
	}

	hub.Shutdown()

	if err := bot.Disconnect(); err != nil {
		slog.Error("discord disconnect error", "error", err)
	}

	if err := store.Close(); err != nil {
		slog.Error("storage close error", "error", err)
	}

	slog.Info("molly discord relay stopped")
}

// makeDeviceFlowFn creates the Discord OAuth2 device flow initiator function.
// Discord doesn't support true RFC 8628 device flow for user tokens from bots,
// so we implement a user-code-based redirect flow as the closest equivalent.
func makeDeviceFlowFn(cfg *config.Config, store *storage.Store) api.DeviceFlowFunc {
	return func() (*auth.DeviceFlowState, error) {
		// Generate a user-friendly 8-char code: XXXX-XXXX
		code := randomUserCode()
		deviceCode, err := auth.NewDeviceCode()
		if err != nil {
			return nil, fmt.Errorf("generate device code: %w", err)
		}

		// Construct Discord OAuth2 authorization URL.
		// The user visits this URL and authorizes the app.
		verificationURI := fmt.Sprintf(
			"https://discord.com/oauth2/authorize?client_id=%s&response_type=code&scope=identify&state=%s",
			cfg.DiscordClientID,
			deviceCode[:16], // use first 16 chars of device_code as state
		)

		flow := auth.DeviceFlowState{
			DeviceCode:      deviceCode,
			UserCode:        code,
			VerificationURI: verificationURI,
			ExpiresAt:       time.Now().Add(15 * time.Minute),
			Interval:        5,
			Status:          "pending",
		}

		if err := store.SaveDeviceFlow(flow); err != nil {
			return nil, fmt.Errorf("save device flow: %w", err)
		}

		return &flow, nil
	}
}

// pollDeviceFlows checks for OAuth2 callback completions.
// In practice, the web frontend (molly-web) handles the OAuth2 redirect and calls
// a webhook endpoint to complete the flow. This goroutine cleans up stale flows.
func pollDeviceFlows(store *storage.Store, cfg *config.Config) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		_ = store.CleanupDeviceFlows()
	}
}

// PostOAuthCallback is called by the web frontend when Discord redirects back.
// This endpoint completes the device flow: exchanges code for access_token,
// fetches user identity, creates a session, and marks the flow as authorized.
// It should be registered at POST /auth/oauth/callback and is wired in main
// via a special non-auth-protected route.
func postOAuthCallback(cfg *config.Config, store *storage.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code       string `json:"code"`
			DeviceCode string `json:"device_code"` // correlates with the flow
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" || req.DeviceCode == "" {
			http.Error(w, `{"ok":false,"error":"code and device_code required"}`, http.StatusBadRequest)
			return
		}

		// Exchange authorization code for tokens.
		discordUser, accessToken, refreshToken, err := exchangeCode(cfg, req.Code)
		if err != nil {
			slog.Error("oauth code exchange failed", "error", err)
			http.Error(w, `{"ok":false,"error":"auth failed"}`, http.StatusUnauthorized)
			return
		}

		// Create a session.
		rawToken, tokenHash, err := auth.NewSessionToken()
		if err != nil {
			http.Error(w, `{"ok":false,"error":"internal error"}`, http.StatusInternalServerError)
			return
		}

		avatarURL := auth.AvatarURL(discordUser.ID, discordUser.Avatar, discordUser.Discriminator)
		now := time.Now()

		sess := auth.Session{
			TokenHash:    tokenHash,
			DiscordID:    discordUser.ID,
			Username:     discordUser.Username,
			AvatarURL:    avatarURL,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			CreatedAt:    now,
			ExpiresAt:    now.Add(auth.SessionDuration),
			LastSeenAt:   now,
		}

		if err := store.SaveSession(sess); err != nil {
			slog.Error("save session failed", "error", err)
			http.Error(w, `{"ok":false,"error":"internal error"}`, http.StatusInternalServerError)
			return
		}

		// Mark the device flow as authorized with the session token.
		if err := store.UpdateDeviceFlow(
			req.DeviceCode, "authorized", rawToken,
			discordUser.ID, discordUser.Username, avatarURL,
		); err != nil {
			slog.Warn("update device flow failed", "error", err)
			// Non-fatal: session still created.
		}

		slog.Info("oauth callback completed", "discord_id", discordUser.ID, "username", discordUser.Username)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"username":%q,"avatar_url":%q}`,
			discordUser.Username, avatarURL)
	}
}

type discordUserInfo struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Avatar        string `json:"avatar"`
	Discriminator int    `json:"discriminator,string"`
}

func exchangeCode(cfg *config.Config, code string) (*discordUserInfo, string, string, error) {
	data := strings.NewReader(fmt.Sprintf(
		"client_id=%s&client_secret=%s&grant_type=authorization_code&code=%s",
		cfg.DiscordClientID, cfg.DiscordClientSecret, code,
	))
	resp, err := http.Post(
		"https://discord.com/api/oauth2/token",
		"application/x-www-form-urlencoded",
		data,
	)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, "", "", err
	}
	if tokenResp.AccessToken == "" {
		return nil, "", "", fmt.Errorf("no access_token in response")
	}

	// Fetch user identity.
	req, _ := http.NewRequest("GET", "https://discord.com/api/v10/users/@me", nil)
	req.Header.Set("Authorization", "Bearer "+tokenResp.AccessToken)
	userResp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer userResp.Body.Close()

	var user discordUserInfo
	if err := json.NewDecoder(userResp.Body).Decode(&user); err != nil {
		return nil, "", "", err
	}
	if user.ID == "" {
		return nil, "", "", fmt.Errorf("failed to get discord user identity")
	}

	return &user, tokenResp.AccessToken, tokenResp.RefreshToken, nil
}

func randomUserCode() string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b[:4]) + "-" + string(b[4:])
}

func initLogger(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})
	slog.SetDefault(slog.New(handler))
}
