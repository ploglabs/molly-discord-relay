package discord

import (
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
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

var mentionPattern = regexp.MustCompile(`(^|[\s(])@([A-Za-z0-9_.-]{2,32})`)
var discordMentionRe = regexp.MustCompile(`<@!?(\d+)>`)

type Bot struct {
	session      *discordgo.Session
	store        *storage.Store
	hub          *websocket.Hub
	webhookMu    sync.RWMutex
	webhookCache map[string]webhookEntry
}

func New(token string, store *storage.Store, hub *websocket.Hub) (*Bot, error) {
	token = strings.TrimPrefix(token, "Bot ")
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
		discordgo.IntentMessageContent |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsGuildMessageReactions |
		discordgo.IntentsGuildPresences |
		discordgo.IntentsGuildMessageTyping

	b.loadWebhookCache()

	if err := b.session.Open(); err != nil {
		return err
	}

	slog.Info("discord gateway connected")
	return nil
}

func (b *Bot) loadWebhookCache() {
	webhooks, err := b.store.LoadAllWebhooks()
	if err != nil {
		slog.Warn("failed to load webhook cache from db", "error", err)
		return
	}
	b.webhookMu.Lock()
	for channelID, pair := range webhooks {
		b.webhookCache[channelID] = webhookEntry{ID: pair[0], Token: pair[1]}
	}
	b.webhookMu.Unlock()
	if len(webhooks) > 0 {
		slog.Info("loaded webhook cache from db", "count", len(webhooks))
	}
}

func (b *Bot) Disconnect() error {
	slog.Info("disconnecting from discord gateway")
	return b.session.Close()
}

func (b *Bot) Session() *discordgo.Session {
	return b.session
}

func (b *Bot) SendWebhookMessage(channelID, username, avatarURL, content, replyToID string) (string, error) {
	content = b.resolveMentions(channelID, content)

	if replyToID != "" {
		msg, err := b.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Content: replyContent(username, content),
			Reference: &discordgo.MessageReference{
				MessageID: replyToID,
				ChannelID: channelID,
			},
		})
		if err != nil {
			return "", err
		}
		return msg.ID, nil
	}

	wh, err := b.getOrCreateWebhook(channelID)
	if err != nil {
		slog.Warn("webhook unavailable, falling back to bot message", "channel_id", channelID, "error", err)
		msg, sendErr := b.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Content: botAuthoredContent(username, content),
		})
		if sendErr != nil {
			return "", sendErr
		}
		return msg.ID, nil
	}

	msg, err := b.session.WebhookExecute(wh.ID, wh.Token, true, &discordgo.WebhookParams{
		Content:   content,
		Username:  username,
		AvatarURL: avatarURL,
	})
	if err != nil {
		// Webhook may have been deleted externally — purge and retry once.
		b.invalidateWebhook(channelID)
		if wh2, err2 := b.getOrCreateWebhook(channelID); err2 == nil {
			if msg2, err3 := b.session.WebhookExecute(wh2.ID, wh2.Token, true, &discordgo.WebhookParams{
				Content:   content,
				Username:  username,
				AvatarURL: avatarURL,
			}); err3 == nil {
				return msg2.ID, nil
			}
		}
		return "", err
	}

	return msg.ID, nil
}

func (b *Bot) SendWebhookFile(channelID, username, avatarURL, content, filename string, reader io.Reader) (string, error) {
	content = b.resolveMentions(channelID, content)
	wh, err := b.getOrCreateWebhook(channelID)
	if err != nil {
		slog.Warn("webhook unavailable, falling back to bot file message", "channel_id", channelID, "error", err)
		msg, sendErr := b.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
			Content: botAuthoredContent(username, content),
			Files: []*discordgo.File{{
				Name:   filename,
				Reader: reader,
			}},
		})
		if sendErr != nil {
			return "", sendErr
		}
		return msg.ID, nil
	}
	msg, err := b.session.WebhookExecute(wh.ID, wh.Token, true, &discordgo.WebhookParams{
		Content:   content,
		Username:  username,
		AvatarURL: avatarURL,
		Files: []*discordgo.File{{
			Name:   filename,
			Reader: reader,
		}},
	})
	if err != nil {
		return "", err
	}
	return msg.ID, nil
}

func (b *Bot) invalidateWebhook(channelID string) {
	b.webhookMu.Lock()
	delete(b.webhookCache, channelID)
	b.webhookMu.Unlock()
	_ = b.store.DeleteWebhook(channelID)
}

func botAuthoredContent(username, content string) string {
	if username == "" {
		return content
	}
	if content == "" {
		return "**" + username + "**"
	}
	return "**" + username + "**: " + content
}

func replyContent(username, content string) string {
	if username == "" {
		return content
	}
	return "**" + username + "**: " + content
}

func (b *Bot) resolveMentions(channelID, content string) string {
	if !strings.Contains(content, "@") {
		return content
	}
	ch, err := b.session.Channel(channelID)
	if err != nil || ch == nil || ch.GuildID == "" {
		if stored, err := b.store.GetChannelByID(channelID); err == nil && stored != nil {
			ch = &discordgo.Channel{GuildID: stored.GuildID}
		}
	}
	if ch == nil || ch.GuildID == "" {
		return content
	}

	return mentionPattern.ReplaceAllStringFunc(content, func(match string) string {
		prefix := ""
		name := match
		if strings.HasPrefix(match, "@") {
			name = strings.TrimPrefix(match, "@")
		} else {
			prefix = match[:1]
			name = strings.TrimPrefix(match[1:], "@")
		}
		members, err := b.session.GuildMembersSearch(ch.GuildID, name, 1)
		if err != nil || len(members) == 0 || members[0].User == nil {
			return match
		}
		user := members[0].User
		if !strings.EqualFold(user.Username, name) && !strings.HasPrefix(strings.ToLower(user.Username), strings.ToLower(name)) {
			return match
		}
		return prefix + "<@" + user.ID + ">"
	})
}

func (b *Bot) resolveIncomingMentions(content string, mentions []*discordgo.User) string {
	if !strings.Contains(content, "<@") {
		return content
	}
	return discordMentionRe.ReplaceAllStringFunc(content, func(match string) string {
		id := match[2 : len(match)-1]
		if id[0] == '!' {
			id = id[1:]
		}
		for _, u := range mentions {
			if u.ID == id {
				return "@" + u.Username
			}
		}
		if u, err := b.session.User(id); err == nil && u != nil {
			return "@" + u.Username
		}
		return match
	})
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

	// Re-check after acquiring write lock (fixes double-checked locking race).
	if wh, ok := b.webhookCache[channelID]; ok {
		return wh, nil
	}

	// Check DB for a token persisted from a previous run.
	if whID, whToken, err := b.store.GetWebhook(channelID); err == nil {
		entry := webhookEntry{ID: whID, Token: whToken}
		b.webhookCache[channelID] = entry
		return entry, nil
	}

	// Create a new webhook — WebhookCreate is the only Discord API call that
	// returns a token. ChannelWebhooks intentionally omits tokens for security.
	wh, err := b.session.WebhookCreate(channelID, "molly-relay", "")
	if err != nil {
		slog.Warn("failed to create webhook", "channel_id", channelID, "error", err)
		return webhookEntry{}, err
	}

	entry := webhookEntry{ID: wh.ID, Token: wh.Token}
	b.webhookCache[channelID] = entry
	if saveErr := b.store.SaveWebhook(channelID, entry.ID, entry.Token); saveErr != nil {
		slog.Warn("failed to persist webhook token", "channel_id", channelID, "error", saveErr)
	}
	slog.Info("created webhook", "channel_id", channelID, "webhook_id", wh.ID)
	return entry, nil
}

func (b *Bot) addHandlers() {
	b.session.AddHandler(b.onReady)
	b.session.AddHandler(b.onGuildCreate)
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

func (b *Bot) isOwnWebhook(webhookID string) bool {
	b.webhookMu.RLock()
	defer b.webhookMu.RUnlock()
	for _, wh := range b.webhookCache {
		if wh.ID == webhookID {
			return true
		}
	}
	return false
}

func (b *Bot) onReady(s *discordgo.Session, r *discordgo.Ready) {
	slog.Info("discord ready", "guilds", len(r.Guilds), "user", r.User.String())

	var synced []models.Channel
	for _, g := range r.Guilds {
		channels, err := s.GuildChannels(g.ID)
		if err != nil {
			slog.Warn("failed to fetch guild channels", "guild_id", g.ID, "error", err)
			continue
		}
		for _, ch := range channels {
			if ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews {
				continue
			}
			synced = append(synced, models.Channel{
				ID:      ch.ID,
				Name:    ch.Name,
				GuildID: ch.GuildID,
				Type:    "text",
			})
		}
	}

	if err := b.store.ReplaceChannels(synced); err != nil {
		slog.Warn("failed to sync channel snapshot", "error", err)
	}
}

func (b *Bot) onRateLimit(s *discordgo.Session, rl *discordgo.RateLimit) {
	slog.Warn("discord rate limit", "retry_after", rl.RetryAfter)
}

func (b *Bot) onGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	slog.Info("bot joined new guild, syncing channels", "guild_id", g.ID, "guild_name", g.Name)

	channels, err := s.GuildChannels(g.ID)
	if err != nil {
		slog.Warn("failed to fetch channels for new guild", "guild_id", g.ID, "error", err)
		return
	}

	var synced []models.Channel
	for _, ch := range channels {
		if ch.Type != discordgo.ChannelTypeGuildText && ch.Type != discordgo.ChannelTypeGuildNews {
			continue
		}
		synced = append(synced, models.Channel{
			ID:      ch.ID,
			Name:    ch.Name,
			GuildID: ch.GuildID,
			Type:    "text",
		})
	}

	for _, ch := range synced {
		if err := b.store.UpsertChannel(ch); err != nil {
			slog.Warn("failed to upsert channel for new guild", "channel", ch.Name, "error", err)
		}
	}
	slog.Info("synced channels for new guild", "guild_id", g.ID, "count", len(synced))
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
