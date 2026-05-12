package discord

import (
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/ploglabs/molly-discord-relay/internal/models"
)

func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.Bot && m.WebhookID == "" {
		return
	}

	chName := b.channelName(m.ChannelID)
	evt := models.RelayEvent{
		Type:      "message_create",
		Channel:   chName,
		ChannelID: m.ChannelID,
		Username:  m.Author.Username,
		UserID:    m.Author.ID,
		Content:   m.Content,
		MessageID: m.ID,
		Timestamp: m.Timestamp.Format(time.RFC3339),
	}

	b.broadcast(evt)
	_ = b.store.InsertMessage(models.Message{
		ID:        m.ID,
		ChannelID: m.ChannelID,
		Author:    m.Author.Username,
		Content:   m.Content,
		Timestamp: m.Timestamp,
	})
}

func (b *Bot) onMessageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if m.Author != nil && m.Author.Bot {
		return
	}

	chName := b.channelName(m.ChannelID)
	evt := models.RelayEvent{
		Type:      "message_update",
		Channel:   chName,
		ChannelID: m.ChannelID,
		Content:   m.Content,
		MessageID: m.ID,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	if m.Author != nil {
		evt.Username = m.Author.Username
		evt.UserID = m.Author.ID
	}

	b.broadcast(evt)

	if m.Author != nil {
		_ = b.store.UpdateMessage(models.Message{
			ID:      m.ID,
			Content: m.Content,
			Author:  m.Author.Username,
		})
	}
}

func (b *Bot) onMessageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	chName := b.channelName(m.ChannelID)
	evt := models.RelayEvent{
		Type:      "message_delete",
		Channel:   chName,
		ChannelID: m.ChannelID,
		MessageID: m.ID,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	b.broadcast(evt)
	_ = b.store.DeleteMessage(m.ID)
}

func (b *Bot) onTypingStart(s *discordgo.Session, t *discordgo.TypingStart) {
	chName := b.channelName(t.ChannelID)

	username := b.usernameForTyping(s, t.GuildID, t.UserID)

	evt := models.RelayEvent{
		Type:      "typing_start",
		Channel:   chName,
		ChannelID: t.ChannelID,
		Username:  username,
		UserID:    t.UserID,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	b.broadcast(evt)
}

func (b *Bot) usernameForTyping(s *discordgo.Session, guildID, userID string) string {
	if guildID != "" {
		if member, err := s.State.Member(guildID, userID); err == nil && member != nil && member.User != nil {
			return member.User.Username
		}
		if member, err := s.GuildMember(guildID, userID); err == nil && member != nil && member.User != nil {
			return member.User.Username
		}
	}
	if user, err := s.User(userID); err == nil && user != nil {
		return user.Username
	}
	return userID
}

func (b *Bot) onGuildMemberAdd(s *discordgo.Session, m *discordgo.GuildMemberAdd) {
	evt := models.RelayEvent{
		Type:      "user_join",
		Username:  m.User.Username,
		UserID:    m.User.ID,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	b.broadcast(evt)
	_ = b.store.SetUserOnline(m.User.ID, m.User.Username)
}

func (b *Bot) onGuildMemberRemove(s *discordgo.Session, m *discordgo.GuildMemberRemove) {
	evt := models.RelayEvent{
		Type:      "user_leave",
		Username:  m.User.Username,
		UserID:    m.User.ID,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	b.broadcast(evt)
	_ = b.store.SetUserOffline(m.User.ID)
}

func (b *Bot) onMessageReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	evt := models.RelayEvent{
		Type:      "reaction_add",
		ChannelID: r.ChannelID,
		MessageID: r.MessageID,
		UserID:    r.UserID,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	evt.Content = r.Emoji.Name

	b.broadcast(evt)
}

func (b *Bot) onMessageReactionRemove(s *discordgo.Session, r *discordgo.MessageReactionRemove) {
	evt := models.RelayEvent{
		Type:      "reaction_remove",
		ChannelID: r.ChannelID,
		MessageID: r.MessageID,
		UserID:    r.UserID,
		Timestamp: time.Now().Format(time.RFC3339),
	}
	evt.Content = r.Emoji.Name

	b.broadcast(evt)
}

func (b *Bot) channelName(channelID string) string {
	ch, err := b.session.Channel(channelID)
	if err == nil && ch != nil {
		return ch.Name
	}

	stored, err := b.store.GetChannelByID(channelID)
	if err == nil && stored != nil {
		return stored.Name
	}

	return channelID
}
