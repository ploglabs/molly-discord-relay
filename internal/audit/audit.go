package audit

import (
	"encoding/json"
	"log/slog"
	"time"
)

// Event types for security-relevant actions.
const (
	EventSessionCreated = "session.created"
	EventSessionRevoked = "session.revoked"
	EventSessionExpired = "session.expired"
	EventAuthFailed     = "auth.failed"
	EventAuthDeviceStart = "auth.device.start"
	EventAuthDeviceComplete = "auth.device.complete"
	EventMessageSent    = "message.sent"
	EventFileSent       = "file.sent"
	EventRateLimited    = "rate.limited"
)

// AuditEvent is a structured log entry for security-relevant actions.
// NEVER include: tokens, message content, encryption keys, or raw passwords.
type AuditEvent struct {
	Timestamp  time.Time `json:"ts"`
	Event      string    `json:"event"`
	DiscordID  string    `json:"discord_id,omitempty"`
	Username   string    `json:"username,omitempty"`
	SessionID  string    `json:"session_id,omitempty"` // only first 8 chars of token hash
	Channel    string    `json:"channel,omitempty"`
	MessageID  string    `json:"message_id,omitempty"`
	RemoteAddr string    `json:"remote_addr,omitempty"`
	Details    string    `json:"details,omitempty"`
}

// Log writes a structured audit event to the default slog logger.
// Uses the "audit" group so log aggregators can filter/route these specifically.
func Log(evt AuditEvent) {
	evt.Timestamp = time.Now().UTC()

	// Truncate session_id to first 8 chars for log correlation without full exposure.
	if len(evt.SessionID) > 8 {
		evt.SessionID = evt.SessionID[:8] + "..."
	}

	b, _ := json.Marshal(evt)
	slog.Info("audit", "event", evt.Event, "data", string(b))
}

// LogAuthFailed logs a failed authentication attempt.
func LogAuthFailed(remoteAddr, details string) {
	Log(AuditEvent{
		Event:      EventAuthFailed,
		RemoteAddr: remoteAddr,
		Details:    details,
	})
}

// LogSessionCreated logs when a new session is established.
func LogSessionCreated(discordID, username, sessionHash, remoteAddr string) {
	Log(AuditEvent{
		Event:      EventSessionCreated,
		DiscordID:  discordID,
		Username:   username,
		SessionID:  sessionHash,
		RemoteAddr: remoteAddr,
	})
}

// LogSessionRevoked logs when a session is revoked.
func LogSessionRevoked(discordID, username, sessionHash string) {
	Log(AuditEvent{
		Event:     EventSessionRevoked,
		DiscordID: discordID,
		Username:  username,
		SessionID: sessionHash,
	})
}

// LogMessageSent logs metadata about a sent message (no content).
func LogMessageSent(discordID, username, sessionHash, channel, messageID, remoteAddr string) {
	Log(AuditEvent{
		Event:      EventMessageSent,
		DiscordID:  discordID,
		Username:   username,
		SessionID:  sessionHash,
		Channel:    channel,
		MessageID:  messageID,
		RemoteAddr: remoteAddr,
	})
}

// LogRateLimited logs when a session is rate-limited.
func LogRateLimited(discordID, username, sessionHash, endpoint, remoteAddr string) {
	Log(AuditEvent{
		Event:      EventRateLimited,
		DiscordID:  discordID,
		Username:   username,
		SessionID:  sessionHash,
		RemoteAddr: remoteAddr,
		Details:    endpoint,
	})
}
