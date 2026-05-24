package storage

import (
	"testing"
	"time"

	"github.com/ploglabs/molly-discord-relay/internal/models"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestGetMessages_ArgOrdering(t *testing.T) {
	s := newTestStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	msgs := []models.Message{
		{ID: "1", ChannelID: "c1", Author: "alice", Content: "first", Timestamp: now.Add(-3 * time.Minute)},
		{ID: "2", ChannelID: "c1", Author: "alice", Content: "second", Timestamp: now.Add(-2 * time.Minute)},
		{ID: "3", ChannelID: "c1", Author: "alice", Content: "third", Timestamp: now.Add(-1 * time.Minute)},
		{ID: "4", ChannelID: "c1", Author: "alice", Content: "fourth", Timestamp: now},
	}
	for _, m := range msgs {
		if err := s.InsertMessage(m); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	t.Run("default (no before/after)", func(t *testing.T) {
		got, err := s.GetMessages("c1", 10, "", "")
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("want 4 messages, got %d", len(got))
		}
	})

	t.Run("before", func(t *testing.T) {
		// before "3" should return "1" and "2"
		got, err := s.GetMessages("c1", 10, "3", "")
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 messages before id=3, got %d", len(got))
		}
		for _, m := range got {
			if m.ID == "3" || m.ID == "4" {
				t.Errorf("unexpected message id %s in before-3 result", m.ID)
			}
		}
	})

	t.Run("after", func(t *testing.T) {
		// after "2" should return "3" and "4"
		got, err := s.GetMessages("c1", 10, "", "2")
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 messages after id=2, got %d", len(got))
		}
		for _, m := range got {
			if m.ID == "1" || m.ID == "2" {
				t.Errorf("unexpected message id %s in after-2 result", m.ID)
			}
		}
	})

	t.Run("before and after", func(t *testing.T) {
		// between "1" and "4" should return "2" and "3"
		got, err := s.GetMessages("c1", 10, "4", "1")
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 messages between id=1 and id=4, got %d", len(got))
		}
		for _, m := range got {
			if m.ID == "1" || m.ID == "4" {
				t.Errorf("unexpected message id %s in between result", m.ID)
			}
		}
	})

	t.Run("limit respected", func(t *testing.T) {
		got, err := s.GetMessages("c1", 2, "", "")
		if err != nil {
			t.Fatalf("GetMessages: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 (limit), got %d", len(got))
		}
	})
}

func TestReplaceChannels_ReplacesSnapshot(t *testing.T) {
	s := newTestStore(t)

	if err := s.UpsertChannel(models.Channel{ID: "voice-1", Name: "General Voice", GuildID: "g1", Type: "voice"}); err != nil {
		t.Fatalf("UpsertChannel voice: %v", err)
	}
	if err := s.UpsertChannel(models.Channel{ID: "text-old", Name: "old", GuildID: "g1", Type: "text"}); err != nil {
		t.Fatalf("UpsertChannel old text: %v", err)
	}

	err := s.ReplaceChannels([]models.Channel{
		{ID: "text-1", Name: "general", GuildID: "g1", Type: "text"},
		{ID: "text-2", Name: "dev", GuildID: "g1", Type: "text"},
	})
	if err != nil {
		t.Fatalf("ReplaceChannels: %v", err)
	}

	channels, err := s.GetChannels()
	if err != nil {
		t.Fatalf("GetChannels: %v", err)
	}
	if len(channels) != 2 {
		t.Fatalf("expected 2 text channels, got %d", len(channels))
	}
	if channels[0].Name != "dev" || channels[1].Name != "general" {
		t.Fatalf("unexpected channels after replace: %#v", channels)
	}
	if _, err := s.GetChannelByName("old"); err == nil {
		t.Fatal("expected stale text channel to be removed")
	}
}

// Bug 4 regression: GetMessagesByChannelTimestamp with since must return only
// messages strictly after the given timestamp.
func TestGetMessagesByChannelTimestamp_Since(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	msgs := []models.Message{
		{ID: "1", ChannelID: "c1", Author: "alice", Content: "old", Timestamp: base},
		{ID: "2", ChannelID: "c1", Author: "alice", Content: "boundary", Timestamp: base.Add(time.Minute)},
		{ID: "3", ChannelID: "c1", Author: "alice", Content: "new1", Timestamp: base.Add(2 * time.Minute)},
		{ID: "4", ChannelID: "c1", Author: "alice", Content: "new2", Timestamp: base.Add(3 * time.Minute)},
	}
	for _, m := range msgs {
		if err := s.InsertMessage(m); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	since := base.Add(time.Minute) // exclusive: should return "3" and "4" only
	got, err := s.GetMessagesByChannelTimestamp("c1", 100, nil, &since)
	if err != nil {
		t.Fatalf("GetMessagesByChannelTimestamp: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages after since, got %d: %#v", len(got), got)
	}
	for _, m := range got {
		if m.ID == "1" || m.ID == "2" {
			t.Errorf("message %s should be excluded by since filter", m.ID)
		}
	}
}

func TestSearchGuilds(t *testing.T) {
	s := newTestStore(t)

	channels := []models.Channel{
		{ID: "ch1", Name: "general", GuildID: "guild-alpha", Type: "text"},
		{ID: "ch2", Name: "chat", GuildID: "guild-alpha", Type: "text"},
		{ID: "ch3", Name: "general", GuildID: "guild-beta", Type: "text"},
		{ID: "ch4", Name: "dev", GuildID: "guild-beta", Type: "text"},
		{ID: "ch5", Name: "lobby", GuildID: "minecraft-server", Type: "text"},
	}
	for _, c := range channels {
		if err := s.UpsertChannel(c); err != nil {
			t.Fatalf("upsert channel: %v", err)
		}
	}

	t.Run("all guilds (no guilds table entries)", func(t *testing.T) {
		guilds, err := s.GetGuilds()
		if err != nil {
			t.Fatalf("GetGuilds: %v", err)
		}
		if len(guilds) != 3 {
			t.Fatalf("want 3 guilds, got %d", len(guilds))
		}
	})

	t.Run("search by guild_id match", func(t *testing.T) {
		results, err := s.SearchGuilds("mine")
		if err != nil {
			t.Fatalf("SearchGuilds: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("want 1 guild matching 'mine', got %d", len(results))
		}
		if results[0].ID != "minecraft-server" {
			t.Fatalf("want minecraft-server, got %s", results[0].ID)
		}
	})

	t.Run("search by guild ID exact", func(t *testing.T) {
		results, err := s.SearchGuilds("guild-alpha")
		if err != nil {
			t.Fatalf("SearchGuilds: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("want 1 guild, got %d", len(results))
		}
		if results[0].ID != "guild-alpha" {
			t.Fatalf("want guild-alpha, got %s", results[0].ID)
		}
	})

	t.Run("search case-insensitive", func(t *testing.T) {
		results, err := s.SearchGuilds("GUILD-BETA")
		if err != nil {
			t.Fatalf("SearchGuilds: %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("want 1 guild, got %d", len(results))
		}
		if results[0].ID != "guild-beta" {
			t.Fatalf("want guild-beta, got %s", results[0].ID)
		}
	})

	t.Run("search no match", func(t *testing.T) {
		results, err := s.SearchGuilds("nonexistent")
		if err != nil {
			t.Fatalf("SearchGuilds: %v", err)
		}
		if len(results) != 0 {
			t.Fatalf("want 0 guilds, got %d", len(results))
		}
	})

	t.Run("guilds table names take priority", func(t *testing.T) {
		if err := s.UpsertGuild("guild-alpha", "Alpha Server"); err != nil {
			t.Fatalf("UpsertGuild: %v", err)
		}
		if err := s.UpsertGuild("guild-beta", "Beta Server"); err != nil {
			t.Fatalf("UpsertGuild: %v", err)
		}
		if err := s.UpsertGuild("minecraft-server", "Minecraft Server"); err != nil {
			t.Fatalf("UpsertGuild: %v", err)
		}

		results, err := s.GetGuilds()
		if err != nil {
			t.Fatalf("GetGuilds: %v", err)
		}
		if len(results) != 3 {
			t.Fatalf("want 3 guilds, got %d", len(results))
		}

		names := make(map[string]string)
		for _, g := range results {
			names[g.ID] = g.Name
		}
		if names["guild-alpha"] != "Alpha Server" {
			t.Errorf("want 'Alpha Server', got %q", names["guild-alpha"])
		}
		if names["guild-beta"] != "Beta Server" {
			t.Errorf("want 'Beta Server', got %q", names["guild-beta"])
		}
		if names["minecraft-server"] != "Minecraft Server" {
			t.Errorf("want 'Minecraft Server', got %q", names["minecraft-server"])
		}

		searchResults, err := s.SearchGuilds("alpha")
		if err != nil {
			t.Fatalf("SearchGuilds: %v", err)
		}
		if len(searchResults) != 1 || searchResults[0].ID != "guild-alpha" {
			t.Errorf("search by real name failed: %+v", searchResults)
		}

		searchResults, err = s.SearchGuilds("minecraft")
		if err != nil {
			t.Fatalf("SearchGuilds: %v", err)
		}
		if len(searchResults) != 1 || searchResults[0].ID != "minecraft-server" {
			t.Errorf("search by real name failed: %+v", searchResults)
		}
	})
}

func TestGetMessagesByChannelTimestamp_BeforeAndSince(t *testing.T) {
	s := newTestStore(t)
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	msgs := []models.Message{
		{ID: "1", ChannelID: "c1", Author: "alice", Content: "a", Timestamp: base},
		{ID: "2", ChannelID: "c1", Author: "alice", Content: "b", Timestamp: base.Add(time.Minute)},
		{ID: "3", ChannelID: "c1", Author: "alice", Content: "c", Timestamp: base.Add(2 * time.Minute)},
		{ID: "4", ChannelID: "c1", Author: "alice", Content: "d", Timestamp: base.Add(3 * time.Minute)},
	}
	for _, m := range msgs {
		if err := s.InsertMessage(m); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	since := base                       // after base (exclusive)
	before := base.Add(3 * time.Minute) // before T+3 (exclusive)
	// Should return only "2" and "3"
	got, err := s.GetMessagesByChannelTimestamp("c1", 100, &before, &since)
	if err != nil {
		t.Fatalf("GetMessagesByChannelTimestamp: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages in range, got %d: %#v", len(got), got)
	}
	for _, m := range got {
		if m.ID != "2" && m.ID != "3" {
			t.Errorf("unexpected message %s in range result", m.ID)
		}
	}
}
