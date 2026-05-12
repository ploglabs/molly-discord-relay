package discord

import (
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/bwmarrin/discordgo"
	"github.com/ploglabs/molly-discord-relay/internal/models"
	"github.com/ploglabs/molly-discord-relay/internal/storage"
	"github.com/ploglabs/molly-discord-relay/internal/websocket"
)

type webhookEntry struct {
	ID    string
	Token string
}

type Bot struct {
	session      *discordgo.Session
	store        *storage.Store
	hub          *websocket.Hub
	webhookMu    sync.RWMutex
	webhookCache map[string]webhookEntry
}

func New(token string, store *storage.Store, hub *websocket.Hub) (*Bot, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}

	b := &Bot{
		session:      s,
		store:        store,
		hub:          hub,
		webhookCache: make(map[string]webhookEntry),
	}

	b.addHandlers()

	return b, nil
}

func (b *Bot) Connect() error {
	slog.Info("connecting to discord gateway")

	b.session.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsGuildMessageReactions |
		discordgo.IntentsGuildPresences |
		discordgo.IntentsGuildMessageTyping

	if err := b.session.Open(); err != nil {
		return err
	}

	slog.Info("discord gateway connected")
	return nil
}

func (b *Bot) Disconnect() error {
	slog.Info("disconnecting from discord gateway")
	return b.session.Close()
}

func (b *Bot) Session() *discordgo.Session {
	return b.session
}

func (b *Bot) SendWebhookMessage(channelID, username, avatarURL, content string) (string, error) {
	wh, err := b.getOrCreateWebhook(channelID)
	if err != nil {
		slog.Error("failed to get webhook", "channel_id", channelID, "error", err)
		return "", err
	}

	msg, err := b.session.WebhookExecute(wh.ID, wh.Token, false, &discordgo.WebhookParams{
		Content:   content,
		Username:  username,
		AvatarURL: avatarURL,
	})
	if err != nil {
		return "", err
	}

	return msg.ID, nil
}

func (b *Bot) getOrCreateWebhook(channelID string) (webhookEntry, error) {
	b.webhookMu.RLock()
	if wh, ok := b.webhookCache[channelID]; ok {
		b.webhookMu.RUnlock()
		return wh, nil
	}
	b.webhookMu.RUnlock()

	b.webhookMu.Lock()
	defer b.webhookMu.Unlock()

	webhooks, err := b.session.ChannelWebhooks(channelID)
	if err != nil {
		return webhookEntry{}, err
	}

	for _, wh := range webhooks {
		if wh.User != nil && wh.User.ID == b.session.State.User.ID {
			entry := webhookEntry{ID: wh.ID, Token: wh.Token}
			b.webhookCache[channelID] = entry
			return entry, nil
		}
	}

	wh, err := b.session.WebhookCreate(channelID, "molly-relay", "")
	if err != nil {
		return webhookEntry{}, err
	}

	entry := webhookEntry{ID: wh.ID, Token: wh.Token}
	b.webhookCache[channelID] = entry
	slog.Info("created webhook", "channel_id", channelID, "webhook_id", wh.ID)
	return entry, nil
}

func (b *Bot) addHandlers() {
	b.session.AddHandler(b.onReady)
	b.session.AddHandler(b.onMessageCreate)
	b.session.AddHandler(b.onMessageUpdate)
	b.session.AddHandler(b.onMessageDelete)
	b.session.AddHandler(b.onTypingStart)
	b.session.AddHandler(b.onGuildMemberAdd)
	b.session.AddHandler(b.onGuildMemberRemove)
	b.session.AddHandler(b.onMessageReactionAdd)
	b.session.AddHandler(b.onMessageReactionRemove)
	b.session.AddHandler(b.onRateLimit)
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	slog.Info("discord ready", "guilds", len(r.Guilds), "user", r.User.String())

	for _, g := range r.Guilds {
		channels, err := s.GuildChannels(g.ID)
		if err != nil {
			slog.Warn("failed to fetch guild channels", "guild_id", g.ID, "error", err)
			continue
		}
		for _, ch := range channels {
			_ = b.store.UpsertChannel(models.Channel{
				ID:      ch.ID,
				Name:    ch.Name,
				GuildID: ch.GuildID,
			})
		}
	}
}

func (b *Bot) onRateLimit(s *discordgo.Session, rl *discordgo.RateLimit) {
	slog.Warn("discord rate limit", "retry_after", rl.RetryAfter)
}

func (b *Bot) broadcast(evt models.RelayEvent) {
	data, err := json.Marshal(evt)
	if err != nil {
		slog.Error("failed to marshal relay event", "error", err)
		return
	}
	b.hub.Broadcast(data)
	slog.Debug("broadcast event", "type", evt.Type, "channel", evt.Channel)
}