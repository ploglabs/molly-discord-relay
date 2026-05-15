package storage

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ploglabs/molly-discord-relay/internal/models"
)

type Store struct {
	db *sql.DB
	mu sync.RWMutex
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("set busy timeout: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	slog.Info("storage opened", "path", path)
	return s, nil
}

func (s *Store) Close() error {
	slog.Info("storage closing")
	return s.db.Close()
}

func (s *Store) migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id TEXT PRIMARY KEY,
		username TEXT NOT NULL,
		online INTEGER DEFAULT 0,
		last_seen DATETIME
	);
	CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		channel_id TEXT NOT NULL,
		author TEXT NOT NULL,
		content TEXT NOT NULL,
		timestamp DATETIME NOT NULL
	);
	CREATE TABLE IF NOT EXISTS statuses (
		user_id TEXT PRIMARY KEY,
		status TEXT NOT NULL,
		updated_at DATETIME NOT NULL
	);
	CREATE TABLE IF NOT EXISTS channels (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		guild_id TEXT,
		type TEXT NOT NULL DEFAULT 'text'
	);
	CREATE TABLE IF NOT EXISTS webhooks (
		channel_id   TEXT PRIMARY KEY,
		webhook_id   TEXT NOT NULL,
		webhook_token TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_messages_channel ON messages(channel_id, timestamp);
	CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages(timestamp);
	`

	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	_, _ = s.db.Exec("ALTER TABLE channels ADD COLUMN type TEXT NOT NULL DEFAULT 'text'")
	return nil
}

func (s *Store) UpsertUser(u models.User) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO users (id, username, online, last_seen) VALUES (?, ?, ?, ?)",
		u.ID, u.Username, u.Online, u.LastSeen,
	)
	return err
}

func (s *Store) GetUser(id string) (*models.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var u models.User
	var online int
	err := s.db.QueryRow("SELECT id, username, online, last_seen FROM users WHERE id = ?", id).
		Scan(&u.ID, &u.Username, &online, &u.LastSeen)
	if err != nil {
		return nil, err
	}
	u.Online = online == 1
	return &u, nil
}

func (s *Store) SetUserOnline(id, username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO users (id, username, online, last_seen) VALUES (?, ?, 1, ?)",
		id, username, time.Now(),
	)
	return err
}

func (s *Store) SetUserOffline(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"UPDATE users SET online = 0, last_seen = ? WHERE id = ?",
		time.Now(), id,
	)
	return err
}

func (s *Store) ListOnlineUsers() ([]models.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT id, username, online, last_seen FROM users WHERE online = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []models.User
	for rows.Next() {
		var u models.User
		var online int
		if err := rows.Scan(&u.ID, &u.Username, &online, &u.LastSeen); err != nil {
			return nil, err
		}
		u.Online = online == 1
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) InsertMessage(m models.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO messages (id, channel_id, author, content, timestamp) VALUES (?, ?, ?, ?, ?)",
		m.ID, m.ChannelID, m.Author, m.Content, m.Timestamp,
	)
	return err
}

func (s *Store) GetMessages(channelID string, limit int, before, after string) ([]models.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	var query string
	var args []interface{}

	args = append(args, channelID, limit)

	switch {
	case before != "" && after != "":
		query = "SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp > (SELECT timestamp FROM messages WHERE id = ?) AND timestamp < (SELECT timestamp FROM messages WHERE id = ?) ORDER BY timestamp DESC LIMIT ?"
		args = append(args, after, before)
	case before != "":
		query = "SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp < (SELECT timestamp FROM messages WHERE id = ?) ORDER BY timestamp DESC LIMIT ?"
		args = append(args, before)
	case after != "":
		query = "SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp > (SELECT timestamp FROM messages WHERE id = ?) ORDER BY timestamp ASC LIMIT ?"
		args = append(args, after)
	default:
		query = "SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? ORDER BY timestamp DESC LIMIT ?"
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []models.Message
	for rows.Next() {
		var m models.Message
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.Author, &m.Content, &m.Timestamp); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (s *Store) GetMessagesByChannelTimestamp(channelID string, limit int, before *time.Time) ([]models.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	var rows *sql.Rows
	var err error
	if before != nil {
		rows, err = s.db.Query(
			"SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp < ? ORDER BY timestamp DESC LIMIT ?",
			channelID, before.UTC(), limit,
		)
	} else {
		rows, err = s.db.Query(
			"SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? ORDER BY timestamp DESC LIMIT ?",
			channelID, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []models.Message
	for rows.Next() {
		var m models.Message
		if err := rows.Scan(&m.ID, &m.ChannelID, &m.Author, &m.Content, &m.Timestamp); err != nil {
			return nil, err
		}
		messages = append(messages, m)
	}
	return messages, rows.Err()
}

func (s *Store) GetMessageByID(id string) (*models.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var m models.Message
	err := s.db.QueryRow(
		"SELECT id, channel_id, author, content, timestamp FROM messages WHERE id = ?", id,
	).Scan(&m.ID, &m.ChannelID, &m.Author, &m.Content, &m.Timestamp)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) DeleteMessage(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM messages WHERE id = ?", id)
	return err
}

func (s *Store) UpdateMessage(m models.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"UPDATE messages SET content = ?, author = ? WHERE id = ?",
		m.Content, m.Author, m.ID,
	)
	return err
}

func (s *Store) SetStatus(userID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO statuses (user_id, status, updated_at) VALUES (?, ?, ?)",
		userID, status, time.Now(),
	)
	return err
}

func (s *Store) GetStatus(userID string) (*models.Status, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var st models.Status
	err := s.db.QueryRow("SELECT user_id, status, updated_at FROM statuses WHERE user_id = ?", userID).
		Scan(&st.UserID, &st.Status, &st.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *Store) GetAllStatuses() ([]models.Status, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT user_id, status, updated_at FROM statuses")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []models.Status
	for rows.Next() {
		var st models.Status
		if err := rows.Scan(&st.UserID, &st.Status, &st.UpdatedAt); err != nil {
			return nil, err
		}
		statuses = append(statuses, st)
	}
	return statuses, rows.Err()
}

func (s *Store) UpsertChannel(c models.Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if c.Type == "" {
		c.Type = "text"
	}
	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO channels (id, name, guild_id, type) VALUES (?, ?, ?, ?)",
		c.ID, c.Name, c.GuildID, c.Type,
	)
	return err
}

func (s *Store) GetChannels() ([]models.Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT id, name, guild_id, type FROM channels WHERE type = 'text' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []models.Channel
	for rows.Next() {
		var c models.Channel
		if err := rows.Scan(&c.ID, &c.Name, &c.GuildID, &c.Type); err != nil {
			return nil, err
		}
		channels = append(channels, c)
	}
	return channels, rows.Err()
}

func (s *Store) GetChannelByName(name string) (*models.Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var c models.Channel
	err := s.db.QueryRow("SELECT id, name, guild_id, type FROM channels WHERE name = ? AND type = 'text'", name).
		Scan(&c.ID, &c.Name, &c.GuildID, &c.Type)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) GetChannelByID(id string) (*models.Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var c models.Channel
	err := s.db.QueryRow("SELECT id, name, guild_id, type FROM channels WHERE id = ? AND type = 'text'", id).
		Scan(&c.ID, &c.Name, &c.GuildID, &c.Type)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SaveWebhook persists a webhook token so it survives restarts.
func (s *Store) SaveWebhook(channelID, webhookID, webhookToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO webhooks (channel_id, webhook_id, webhook_token) VALUES (?, ?, ?)",
		channelID, webhookID, webhookToken,
	)
	return err
}

// GetWebhook returns the stored webhook entry for a channel, if any.
func (s *Store) GetWebhook(channelID string) (id, token string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	err = s.db.QueryRow(
		"SELECT webhook_id, webhook_token FROM webhooks WHERE channel_id = ?", channelID,
	).Scan(&id, &token)
	return
}

// DeleteWebhook removes a stored webhook (called when Discord reports it no longer exists).
func (s *Store) DeleteWebhook(channelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM webhooks WHERE channel_id = ?", channelID)
	return err
}

// LoadAllWebhooks returns all persisted channel→(id,token) pairs for cache preload.
func (s *Store) LoadAllWebhooks() (map[string][2]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query("SELECT channel_id, webhook_id, webhook_token FROM webhooks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][2]string)
	for rows.Next() {
		var channelID, webhookID, webhookToken string
		if err := rows.Scan(&channelID, &webhookID, &webhookToken); err != nil {
			return nil, err
		}
		out[channelID] = [2]string{webhookID, webhookToken}
	}
	return out, rows.Err()
}
