package storage

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ploglabs/molly-discord-relay/internal/auth"
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

const currentSchemaVersion = 6

func (s *Store) migrate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS _meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create _meta table: %w", err)
	}

	var version int
	var versionStr string
	if err := s.db.QueryRow("SELECT value FROM _meta WHERE key = 'schema_version'").Scan(&versionStr); err == nil {
		version, _ = strconv.Atoi(versionStr)
	}

	if version < 1 {
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
			return fmt.Errorf("run v1 migration: %w", err)
		}
		version = 1
	}

	if version < 2 {
		if _, err := s.db.Exec("SELECT type FROM channels LIMIT 1"); err != nil {
			if _, err := s.db.Exec("ALTER TABLE channels ADD COLUMN type TEXT NOT NULL DEFAULT 'text'"); err != nil {
				return fmt.Errorf("add channels.type column: %w", err)
			}
			slog.Info("added type column to channels table")
		}
		version = 2
	}

	if version < 3 {
		if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS setup_configs (
			discord_id    TEXT PRIMARY KEY,
			guild_id      TEXT NOT NULL,
			guild_name    TEXT NOT NULL,
			channel_id    TEXT NOT NULL,
			channel_name  TEXT NOT NULL,
			created_at    DATETIME NOT NULL DEFAULT (datetime('now'))
		)`); err != nil {
			return fmt.Errorf("create setup_configs table: %w", err)
		}
		version = 3
	}

	if version < 4 {
		if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS guilds (
			id   TEXT PRIMARY KEY,
			name TEXT NOT NULL
		)`); err != nil {
			return fmt.Errorf("create guilds table: %w", err)
		}
		version = 4
	}

	if version < 5 {
		// Per-user session tokens: store only SHA256(token), never the raw token.
		if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS sessions (
			token_hash     TEXT PRIMARY KEY,
			discord_id     TEXT NOT NULL,
			username       TEXT NOT NULL,
			avatar_url     TEXT NOT NULL DEFAULT '',
			access_token   TEXT NOT NULL DEFAULT '',
			refresh_token  TEXT NOT NULL DEFAULT '',
			created_at     DATETIME NOT NULL,
			expires_at     DATETIME NOT NULL,
			last_seen_at   DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_discord_id ON sessions(discord_id);
		CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

		CREATE TABLE IF NOT EXISTS device_flows (
			device_code      TEXT PRIMARY KEY,
			user_code        TEXT NOT NULL,
			verification_uri TEXT NOT NULL DEFAULT 'https://discord.com/activate',
			expires_at       DATETIME NOT NULL,
			poll_interval    INTEGER NOT NULL DEFAULT 5,
			status           TEXT NOT NULL DEFAULT 'pending',
			session_token    TEXT NOT NULL DEFAULT '',
			discord_id       TEXT NOT NULL DEFAULT '',
			username         TEXT NOT NULL DEFAULT '',
			avatar_url       TEXT NOT NULL DEFAULT ''
		);
		`); err != nil {
			return fmt.Errorf("create sessions/device_flows tables: %w", err)
		}
		version = 5
	}

	if version < 6 {
		// Migrate webhooks table: add encrypted token column, keep old for zero-downtime.
		// Old webhook_token column stays until all webhooks are re-registered encrypted.
		_, _ = s.db.Exec(`ALTER TABLE webhooks ADD COLUMN webhook_token_encrypted TEXT NOT NULL DEFAULT ''`)
		version = 6
	}

	_, err := s.db.Exec("INSERT OR REPLACE INTO _meta (key, value) VALUES ('schema_version', ?)", strconv.Itoa(version))
	if err != nil {
		return fmt.Errorf("persist schema version: %w", err)
	}
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
	args := []interface{}{channelID}

	switch {
	case before != "" && after != "":
		query = "SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp > (SELECT timestamp FROM messages WHERE id = ?) AND timestamp < (SELECT timestamp FROM messages WHERE id = ?) ORDER BY timestamp DESC LIMIT ?"
		args = append(args, after, before, limit)
	case before != "":
		query = "SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp < (SELECT timestamp FROM messages WHERE id = ?) ORDER BY timestamp DESC LIMIT ?"
		args = append(args, before, limit)
	case after != "":
		query = "SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp > (SELECT timestamp FROM messages WHERE id = ?) ORDER BY timestamp ASC LIMIT ?"
		args = append(args, after, limit)
	default:
		query = "SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? ORDER BY timestamp DESC LIMIT ?"
		args = append(args, limit)
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

func (s *Store) GetMessagesByChannelTimestamp(channelID string, limit int, before, since *time.Time) ([]models.Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	var rows *sql.Rows
	var err error
	switch {
	case before != nil && since != nil:
		rows, err = s.db.Query(
			"SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp < ? AND timestamp > ? ORDER BY timestamp DESC LIMIT ?",
			channelID, before.UTC(), since.UTC(), limit,
		)
	case before != nil:
		rows, err = s.db.Query(
			"SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp < ? ORDER BY timestamp DESC LIMIT ?",
			channelID, before.UTC(), limit,
		)
	case since != nil:
		rows, err = s.db.Query(
			"SELECT id, channel_id, author, content, timestamp FROM messages WHERE channel_id = ? AND timestamp > ? ORDER BY timestamp DESC LIMIT ?",
			channelID, since.UTC(), limit,
		)
	default:
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

func (s *Store) UpsertGuild(id, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT INTO guilds (id, name) VALUES (?, ?) ON CONFLICT(id) DO UPDATE SET name = excluded.name",
		id, name,
	)
	return err
}

func (s *Store) ReplaceGuilds(guilds []models.Guild) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM guilds"); err != nil {
		return err
	}

	for _, g := range guilds {
		if _, err := tx.Exec("INSERT INTO guilds (id, name) VALUES (?, ?)", g.ID, g.Name); err != nil {
			return err
		}
	}

	return tx.Commit()
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

func (s *Store) ReplaceChannels(channels []models.Channel) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec("DELETE FROM channels"); err != nil {
		return err
	}

	for _, c := range channels {
		if c.Type == "" {
			c.Type = "text"
		}
		if _, err := tx.Exec(
			"INSERT INTO channels (id, name, guild_id, type) VALUES (?, ?, ?, ?)",
			c.ID, c.Name, c.GuildID, c.Type,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
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

// SaveWebhook persists a webhook token (plaintext) for backward compatibility.
// Prefer SaveWebhookEncrypted when an encryption key is available.
func (s *Store) SaveWebhook(channelID, webhookID, webhookToken string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO webhooks (channel_id, webhook_id, webhook_token) VALUES (?, ?, ?)",
		channelID, webhookID, webhookToken,
	)
	return err
}

// SaveWebhookEncrypted persists a webhook token encrypted with AES-256-GCM.
// encryptionKeyHex must be a 64-char hex string (32 bytes).
func (s *Store) SaveWebhookEncrypted(channelID, webhookID, webhookToken, encryptionKeyHex string) error {
	encrypted, err := auth.Encrypt(webhookToken, encryptionKeyHex, []byte(channelID))
	if err != nil {
		return fmt.Errorf("encrypt webhook token: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO webhooks
		 (channel_id, webhook_id, webhook_token, webhook_token_encrypted)
		 VALUES (?, ?, '', ?)`,
		channelID, webhookID, encrypted,
	)
	return err
}

// GetWebhook returns the stored webhook entry for a channel.
// If an encrypted token exists and encryptionKeyHex is provided, it decrypts it.
// Falls back to plaintext token for zero-downtime migration.
func (s *Store) GetWebhook(channelID string) (id, token string, err error) {
	return s.GetWebhookWithKey(channelID, "")
}

// GetWebhookWithKey returns the webhook, decrypting the token if a key is provided.
func (s *Store) GetWebhookWithKey(channelID, encryptionKeyHex string) (id, token string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var plain, encrypted string
	err = s.db.QueryRow(
		`SELECT webhook_id, COALESCE(webhook_token,''), COALESCE(webhook_token_encrypted,'')
		 FROM webhooks WHERE channel_id = ?`, channelID,
	).Scan(&id, &plain, &encrypted)
	if err != nil {
		return
	}

	// Prefer encrypted token if available and key is set.
	if encrypted != "" && encryptionKeyHex != "" {
		token, err = auth.Decrypt(encrypted, encryptionKeyHex, []byte(channelID))
		if err != nil {
			err = fmt.Errorf("decrypt webhook token for channel %s: %w", channelID, err)
		}
		return
	}

	// Fall back to plaintext (migration period).
	token = plain
	return
}

// DeleteWebhook removes a stored webhook (called when Discord reports it no longer exists).
func (s *Store) DeleteWebhook(channelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM webhooks WHERE channel_id = ?", channelID)
	return err
}

func (s *Store) GetGuilds() ([]models.Guild, error) {
	return s.SearchGuilds("")
}

func (s *Store) SearchGuilds(query string) ([]models.Guild, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var rows *sql.Rows
	var err error

	baseQuery := `
		SELECT c.gid, COALESCE(gu.name, ch_fallback.name, c.gid) as guild_name
		FROM (
			SELECT id as gid FROM guilds
			UNION
			SELECT guild_id as gid FROM channels WHERE guild_id IS NOT NULL AND guild_id != ''
		) c
		LEFT JOIN guilds gu ON c.gid = gu.id
		LEFT JOIN (
			SELECT guild_id, MIN(name) as name FROM channels WHERE type = 'text' GROUP BY guild_id
		) ch_fallback ON c.gid = ch_fallback.guild_id
	`

	if query != "" {
		// Escape LIKE special characters so a search for "%" or "_" matches
		// the literal character rather than acting as a wildcard.
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
		rows, err = s.db.Query(baseQuery+`
			WHERE COALESCE(gu.name, ch_fallback.name, c.gid) LIKE ? ESCAPE '\' COLLATE NOCASE
			   OR c.gid LIKE ? ESCAPE '\' COLLATE NOCASE
			ORDER BY guild_name
		`, "%"+escaped+"%", "%"+escaped+"%")
	} else {
		rows, err = s.db.Query(baseQuery + "ORDER BY guild_name")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var guilds []models.Guild
	for rows.Next() {
		var g models.Guild
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, err
		}
		guilds = append(guilds, g)
	}
	return guilds, rows.Err()
}

func (s *Store) GetChannelsByGuild(guildID string) ([]models.Channel, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		"SELECT id, name, guild_id, type FROM channels WHERE guild_id = ? AND type = 'text' ORDER BY name",
		guildID,
	)
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

func (s *Store) HasGuild(guildID string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM channels WHERE guild_id = ?",
		guildID,
	).Scan(&count)
	return count > 0, err
}

// LoadAllWebhooks returns all persisted channel→(id,token) pairs for cache preload.
// If encryptionKeyHex is provided, encrypted tokens are decrypted; others fall back to plaintext.
func (s *Store) LoadAllWebhooks(encryptionKeyHex string) (map[string][2]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.Query(
		`SELECT channel_id, webhook_id,
		 COALESCE(webhook_token,''), COALESCE(webhook_token_encrypted,'')
		 FROM webhooks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][2]string)
	for rows.Next() {
		var channelID, webhookID, plain, encrypted string
		if err := rows.Scan(&channelID, &webhookID, &plain, &encrypted); err != nil {
			return nil, err
		}
		token := plain
		if encrypted != "" && encryptionKeyHex != "" {
			if dec, err := auth.Decrypt(encrypted, encryptionKeyHex, []byte(channelID)); err == nil {
				token = dec
			} else {
				slog.Warn("failed to decrypt webhook token", "channel_id", channelID, "error", err)
			}
		}
		out[channelID] = [2]string{webhookID, token}
	}
	return out, rows.Err()
}

// --- Session management ---

// SaveSession persists a new session. Only the token hash is stored, never the raw token.
func (s *Store) SaveSession(sess auth.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Enforce max 5 active sessions per Discord user: delete oldest if over limit.
	var count int
	s.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE discord_id = ?", sess.DiscordID).Scan(&count)
	if count >= 5 {
		_, _ = s.db.Exec(
			`DELETE FROM sessions WHERE token_hash IN (
				SELECT token_hash FROM sessions WHERE discord_id = ?
				ORDER BY last_seen_at ASC LIMIT ?)`,
			sess.DiscordID, count-4,
		)
	}

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO sessions
		 (token_hash, discord_id, username, avatar_url, access_token, refresh_token,
		  created_at, expires_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.DiscordID, sess.Username, sess.AvatarURL,
		sess.AccessToken, sess.RefreshToken,
		sess.CreatedAt, sess.ExpiresAt, sess.LastSeenAt,
	)
	return err
}

// GetSessionByHash retrieves a session by its token hash and updates last_seen_at.
func (s *Store) GetSessionByHash(tokenHash string) (*auth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var sess auth.Session
	err := s.db.QueryRow(
		`SELECT token_hash, discord_id, username, avatar_url, access_token, refresh_token,
		        created_at, expires_at, last_seen_at
		 FROM sessions WHERE token_hash = ?`, tokenHash,
	).Scan(
		&sess.TokenHash, &sess.DiscordID, &sess.Username, &sess.AvatarURL,
		&sess.AccessToken, &sess.RefreshToken,
		&sess.CreatedAt, &sess.ExpiresAt, &sess.LastSeenAt,
	)
	if err != nil {
		return nil, err
	}

	// Update last_seen_at asynchronously — don't block on lock.
	go func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		_, _ = s.db.Exec(
			"UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?",
			time.Now(), tokenHash,
		)
	}()

	return &sess, nil
}

// DeleteSession removes a session by its token hash (logout/revoke).
func (s *Store) DeleteSession(tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	return err
}

// CleanupSessions removes expired sessions and those idle for >7 days.
func (s *Store) CleanupSessions() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.db.Exec(
		`DELETE FROM sessions WHERE
		 expires_at < datetime('now')
		 OR last_seen_at < datetime('now', '-7 days')`,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// --- Device flow management ---

// SaveDeviceFlow persists a pending device authorization flow.
func (s *Store) SaveDeviceFlow(flow auth.DeviceFlowState) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO device_flows
		 (device_code, user_code, verification_uri, expires_at, poll_interval,
		  status, session_token, discord_id, username, avatar_url)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		flow.DeviceCode, flow.UserCode, flow.VerificationURI, flow.ExpiresAt,
		flow.Interval, flow.Status, flow.SessionToken, flow.DiscordID,
		flow.Username, flow.AvatarURL,
	)
	return err
}

// GetDeviceFlow retrieves a device flow by its device code.
func (s *Store) GetDeviceFlow(deviceCode string) (*auth.DeviceFlowState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var flow auth.DeviceFlowState
	err := s.db.QueryRow(
		`SELECT device_code, user_code, verification_uri, expires_at, poll_interval,
		        status, session_token, discord_id, username, avatar_url
		 FROM device_flows WHERE device_code = ?`, deviceCode,
	).Scan(
		&flow.DeviceCode, &flow.UserCode, &flow.VerificationURI, &flow.ExpiresAt,
		&flow.Interval, &flow.Status, &flow.SessionToken, &flow.DiscordID,
		&flow.Username, &flow.AvatarURL,
	)
	if err != nil {
		return nil, err
	}
	return &flow, nil
}

// UpdateDeviceFlow updates status, session_token, and identity fields for a flow.
func (s *Store) UpdateDeviceFlow(deviceCode, status, sessionToken, discordID, username, avatarURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		`UPDATE device_flows
		 SET status = ?, session_token = ?, discord_id = ?, username = ?, avatar_url = ?
		 WHERE device_code = ?`,
		status, sessionToken, discordID, username, avatarURL, deviceCode,
	)
	return err
}

// CleanupDeviceFlows removes expired device flows.
func (s *Store) CleanupDeviceFlows() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM device_flows WHERE expires_at < datetime('now')")
	return err
}

type SetupConfig struct {
	DiscordID   string `json:"discord_id"`
	GuildID     string `json:"guild_id"`
	GuildName   string `json:"guild_name"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name"`
}

func (s *Store) SaveSetupConfig(cfg SetupConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec(
		"INSERT OR REPLACE INTO setup_configs (discord_id, guild_id, guild_name, channel_id, channel_name) VALUES (?, ?, ?, ?, ?)",
		cfg.DiscordID, cfg.GuildID, cfg.GuildName, cfg.ChannelID, cfg.ChannelName,
	)
	return err
}

func (s *Store) GetSetupConfig(discordID string) (*SetupConfig, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var cfg SetupConfig
	err := s.db.QueryRow(
		"SELECT discord_id, guild_id, guild_name, channel_id, channel_name FROM setup_configs WHERE discord_id = ?",
		discordID,
	).Scan(&cfg.DiscordID, &cfg.GuildID, &cfg.GuildName, &cfg.ChannelID, &cfg.ChannelName)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (s *Store) DeleteSetupConfig(discordID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.Exec("DELETE FROM setup_configs WHERE discord_id = ?", discordID)
	return err
}
