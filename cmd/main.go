package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/ploglabs/molly-discord-relay/internal/api"
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

	bot, err := discord.New(cfg.DiscordToken, store, hub)
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

	resolveChannel := func(nameOrID string) (string, error) {
		ch, err := bot.Session().Channel(nameOrID)
		if err == nil && ch != nil {
			if ch.Type == discordgo.ChannelTypeGuildText || ch.Type == discordgo.ChannelTypeGuildNews {
				_ = store.UpsertChannel(models.Channel{ID: ch.ID, Name: ch.Name, GuildID: ch.GuildID, Type: "text"})
				return ch.ID, nil
			}
			return "", fmt.Errorf("channel not found: %s", nameOrID)
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

	srv := api.NewServer(store, hub, tracker, sendMsg, sendFile, resolveChannel, cfg.APIKey)

	r := chi.NewRouter()
	r.Use(chimw.Logger)
	r.Use(chimw.Recoverer)
	r.Use(srv.SecurityMiddleware)

	r.Group(func(r chi.Router) {
		r.Use(srv.AuthMiddleware)
		r.Post("/message", srv.PostMessage)
		r.Post("/file", srv.PostFile)
		r.Post("/status", srv.PostStatus)
		r.Get("/history", srv.GetHistory)
		r.Get("/presence", srv.GetPresence)
		r.Get("/api/channels", srv.GetChannels)
		r.Get("/api/channels/{channel}/messages", srv.GetChannelMessages)
		r.Get("/api/terminal/users", srv.GetTerminalUsers)
	})

	r.Get("/ws", func(w http.ResponseWriter, r *http.Request) {
		websocket.ServeWS(hub, w, r)
	})

	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%s", cfg.Port),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
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
