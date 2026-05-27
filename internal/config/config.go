package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	DiscordToken          string
	DiscordClientID       string
	DiscordClientSecret   string
	Port                  string
	DatabasePath          string
	APIKey                string // Legacy — kept for backward compat during deprecation window
	LogLevel              string
	WebhookEncryptionKey  string // 32-byte hex (64 chars) — encrypts webhook tokens at rest
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("DISCORD_TOKEN is required")
	}

	// API_KEY is still required for backward compatibility during the deprecation window.
	// Once all clients have migrated to session-token auth, this can be removed.
	apiKey := os.Getenv("API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("API_KEY is required — set a strong random secret in your environment")
	}

	// WEBHOOK_ENCRYPTION_KEY is required to encrypt webhook tokens at rest.
	// Generate with: openssl rand -hex 32
	webhookEncKey := os.Getenv("WEBHOOK_ENCRYPTION_KEY")
	if webhookEncKey == "" {
		return nil, fmt.Errorf("WEBHOOK_ENCRYPTION_KEY is required (32-byte hex, generate with: openssl rand -hex 32)")
	}
	if len(webhookEncKey) != 64 {
		return nil, fmt.Errorf("WEBHOOK_ENCRYPTION_KEY must be 64 hex characters (32 bytes), got %d", len(webhookEncKey))
	}

	return &Config{
		DiscordToken:         token,
		DiscordClientID:      os.Getenv("DISCORD_CLIENT_ID"),
		DiscordClientSecret:  os.Getenv("DISCORD_CLIENT_SECRET"),
		Port:                 getEnv("PORT", "8080"),
		DatabasePath:         getEnv("DATABASE_PATH", "./molly.db"),
		APIKey:               apiKey,
		LogLevel:             getEnv("LOG_LEVEL", "info"),
		WebhookEncryptionKey: webhookEncKey,
	}, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}