package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// Session represents a verified, per-user session.
// The raw token is never stored — only SHA256(token) lives in the DB.
type Session struct {
	TokenHash    string    // SHA256(session_token), hex-encoded
	DiscordID    string
	Username     string
	AvatarURL    string
	AccessToken  string    // AES-256-GCM encrypted, stored in DB
	RefreshToken string    // AES-256-GCM encrypted, stored in DB
	CreatedAt    time.Time
	ExpiresAt    time.Time
	LastSeenAt   time.Time
}

// DeviceFlowState tracks an ongoing device authorization flow.
type DeviceFlowState struct {
	DeviceCode      string    // opaque code sent to Discord
	UserCode        string    // shown to user (e.g. "ABCD-EFGH")
	VerificationURI string
	ExpiresAt       time.Time
	Interval        int       // poll interval seconds
	Status          string    // "pending", "authorized", "expired"
	SessionToken    string    // set once authorized (raw token, returned once)
	DiscordID       string
	Username        string
	AvatarURL       string
}

const (
	SessionDuration = 24 * time.Hour
	tokenBytes      = 32
)

// NewSessionToken generates a cryptographically random 32-byte base64url token
// and returns (rawToken, tokenHash). Store only the hash.
func NewSessionToken() (rawToken, tokenHash string, err error) {
	b := make([]byte, tokenBytes)
	if _, err = rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate session token: %w", err)
	}
	rawToken = base64.RawURLEncoding.EncodeToString(b)
	tokenHash = HashToken(rawToken)
	return rawToken, tokenHash, nil
}

// HashToken returns SHA256(rawToken) as a hex string.
// This is what gets stored in the sessions table.
func HashToken(rawToken string) string {
	h := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(h[:])
}

// NewDeviceCode generates a random opaque device code (for Discord).
func NewDeviceCode() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate device code: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// AvatarURL constructs the Discord CDN avatar URL from user ID and avatar hash.
// Falls back to default avatar if hash is empty.
func AvatarURL(userID, avatarHash string, discriminator int) string {
	if avatarHash == "" {
		idx := discriminator % 5
		return fmt.Sprintf("https://cdn.discordapp.com/embed/avatars/%d.png", idx)
	}
	ext := "png"
	if len(avatarHash) > 2 && avatarHash[:2] == "a_" {
		ext = "gif"
	}
	return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.%s?size=128", userID, avatarHash, ext)
}
