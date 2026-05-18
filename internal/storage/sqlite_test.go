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
