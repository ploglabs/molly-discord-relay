package models

import (
	"strconv"
	"time"
)

type RelayEvent struct {
	Type      string `json:"type"`
	Channel   string `json:"channel,omitempty"`
	ChannelID string `json:"channel_id,omitempty"`
	Username  string `json:"username,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	Content   string `json:"content,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Timestamp string `json:"timestamp"`
}

type User struct {
	ID       string    `json:"id"`
	Username string    `json:"username"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen"`
}

type Message struct {
	ID        string    `json:"id"`
	ChannelID string    `json:"channel_id"`
	Author    string    `json:"author"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

type Status struct {
	UserID    string    `json:"user_id"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	GuildID string `json:"guild_id,omitempty"`
}

type SendMessageRequest struct {
	Channel   string `json:"channel"`
	Username  string `json:"username"`
	AvatarURL string `json:"avatar_url,omitempty"`
	Content   string `json:"content"`
}

type SetStatusRequest struct {
	Username string `json:"username"`
	Status   string `json:"status"`
}

type APIResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

type MessageResponse struct {
	OK        bool   `json:"ok"`
	MessageID string `json:"message_id,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Error     string `json:"error,omitempty"`
}

type HistoryResponse struct {
	Messages []Message `json:"messages"`
	HasMore  bool      `json:"has_more"`
}

type PresenceResponse struct {
	Users []User `json:"users"`
}

func TimeNow() time.Time {
	return time.Now().UTC()
}

func Atoi(s string) (int, error) {
	return strconv.Atoi(s)
}