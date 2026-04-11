package ap

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nbd-wtf/go-nostr"
)

func testTC() *TransmuteContext {
	return &TransmuteContext{
		LocalDomain:   "https://bridge.example.com",
		LocalActorURL: "https://bridge.example.com/users/alice",
		PublicKeyPem:  "test-key-pem",
		GetAPIDForObject: func(nostrID string) (string, bool) {
			return "https://bridge.example.com/objects/" + nostrID, true
		},
	}
}

// ─── ToNote ──────────────────────────────────────────────────────────────────

func TestToNote(t *testing.T) {
	t.Run("simple note", func(t *testing.T) {
		event := &nostr.Event{
			ID:        "note123",
			PubKey:    "pubkey123",
			Kind:      1,
			Content:   "Hello Fediverse!",
			CreatedAt: nostr.Timestamp(1700000000),
			Tags:      nostr.Tags{},
		}
		tc := testTC()
		note := ToNote(event, tc)

		if note.Type != "Note" {
			t.Errorf("Type = %q, want %q", note.Type, "Note")
		}
		if note.ID != "https://bridge.example.com/objects/note123" {
			t.Errorf("ID = %q, want URL with event ID", note.ID)
		}
		if note.AttributedTo != "https://bridge.example.com/users/alice" {
			t.Errorf("AttributedTo = %q, want local actor URL", note.AttributedTo)
		}
		if !strings.Contains(note.Content, "Hello Fediverse!") {
			t.Errorf("Content = %q, should contain original text", note.Content)
		}
		if note.Published == "" {
			t.Error("Published should not be empty")
		}
		if len(note.To) == 0 || note.To[0] != PublicURI {
			t.Errorf("To should include PublicURI, got %v", note.To)
		}
		if note.Generator == nil || note.Generator.Name != "klistr" {
			t.Error("Generator should be set to klistr")
		}
	})

	t.Run("note with content warning", func(t *testing.T) {
		event := &nostr.Event{
			ID:        "note456",
			PubKey:    "pubkey123",
			Kind:      1,
			Content:   "Spoiler content",
			CreatedAt: nostr.Timestamp(1700000000),
			Tags:      nostr.Tags{{"content-warning", "spoiler alert"}},
		}
		tc := testTC()
		note := ToNote(event, tc)

		if !note.Sensitive {
			t.Error("Sensitive should be true")
		}
		if note.Summary != "spoiler alert" {
			t.Errorf("Summary = %q, want %q", note.Summary, "spoiler alert")
		}
	})

	t.Run("note with imeta image attachment", func(t *testing.T) {
		event := &nostr.Event{
			ID:        "note789",
			PubKey:    "pubkey123",
			Kind:      1,
			Content:   "Check this image",
			CreatedAt: nostr.Timestamp(1700000000),
			Tags: nostr.Tags{
				{"imeta", "url https://cdn.example.com/photo.jpg", "m image/jpeg", "dim 800x600", "alt A nice photo"},
			},
		}
		tc := testTC()
		note := ToNote(event, tc)

		if len(note.Attachment) != 1 {
			t.Fatalf("Attachment count = %d, want 1", len(note.Attachment))
		}
		att := note.Attachment[0]
		if att.URL != "https://cdn.example.com/photo.jpg" {
			t.Errorf("Attachment URL = %q", att.URL)
		}
		if att.MediaType != "image/jpeg" {
			t.Errorf("MediaType = %q", att.MediaType)
		}
		if att.Type != "Image" {
			t.Errorf("Type = %q, want %q", att.Type, "Image")
		}
		if att.Width != 800 || att.Height != 600 {
			t.Errorf("Dimensions = %dx%d, want 800x600", att.Width, att.Height)
		}
		if att.Name != "A nice photo" {
			t.Errorf("Name (alt) = %q, want %q", att.Name, "A nice photo")
		}
	})

	t.Run("note with p-tag mention", func(t *testing.T) {
		event := &nostr.Event{
			ID:        "noteABC",
			PubKey:    "pubkey123",
			Kind:      1,
			Content:   "Hey @someone",
			CreatedAt: nostr.Timestamp(1700000000),
			Tags:      nostr.Tags{{"p", "mentionedpubkey1234567890abcdef"}},
		}
		tc := testTC()
		note := ToNote(event, tc)

		// Should have a Mention in tags. In single-user mode, actorURL always
		// returns the local actor URL, so the Mention Href is the local actor.
		found := false
		for _, tag := range note.Tag {
			data, _ := json.Marshal(tag)
			var m Mention
			if err := json.Unmarshal(data, &m); err == nil && m.Type == "Mention" {
				found = true
				if m.Href != tc.LocalActorURL {
					t.Errorf("Mention Href = %q, want %q", m.Href, tc.LocalActorURL)
				}
				// Name should be a truncated version of the pubkey.
				if !strings.HasPrefix(m.Name, "@") {
					t.Errorf("Mention Name = %q, should start with @", m.Name)
				}
				break
			}
		}
		if !found {
			t.Error("expected a Mention tag in the note")
		}

		// p-tag adds the actor URL to To (via actorURL, which is the local actor).
		if len(note.To) < 2 {
			t.Fatalf("To should have at least 2 entries (Public + mention), got %d", len(note.To))
		}
	})
}

// ─── ToActor ─────────────────────────────────────────────────────────────────

func TestToActor(t *testing.T) {
	t.Run("kind-0 with full metadata", func(t *testing.T) {
		meta := `{"name":"Alice","about":"A Nostr user","picture":"https://example.com/avatar.jpg","banner":"https://example.com/banner.jpg"}`
		event := &nostr.Event{
			ID:        "meta123",
			PubKey:    "pubkey123",
			Kind:      0,
			Content:   meta,
			CreatedAt: nostr.Timestamp(1700000000),
			Tags:      nostr.Tags{},
		}
		tc := testTC()
		actor := ToActor(event, tc)

		if actor.ID != "https://bridge.example.com/users/alice" {
			t.Errorf("ID = %q", actor.ID)
		}
		if actor.Type != "Person" {
			t.Errorf("Type = %q, want %q", actor.Type, "Person")
		}
		if actor.Name != "Alice" {
			t.Errorf("Name = %q, want %q", actor.Name, "Alice")
		}
		if !strings.Contains(actor.Summary, "A Nostr user") {
			t.Errorf("Summary = %q, should contain 'A Nostr user'", actor.Summary)
		}
		if actor.PreferredUsername != "pubkey123" {
			t.Errorf("PreferredUsername = %q, want %q", actor.PreferredUsername, "pubkey123")
		}
		if actor.Inbox != "https://bridge.example.com/users/alice/inbox" {
			t.Errorf("Inbox = %q", actor.Inbox)
		}
		if actor.Outbox != "https://bridge.example.com/users/alice/outbox" {
			t.Errorf("Outbox = %q", actor.Outbox)
		}
		if actor.Followers != "https://bridge.example.com/users/alice/followers" {
			t.Errorf("Followers = %q", actor.Followers)
		}
		if actor.Following != "https://bridge.example.com/users/alice/following" {
			t.Errorf("Following = %q", actor.Following)
		}

		// PublicKey
		if actor.PublicKey == nil {
			t.Fatal("PublicKey should not be nil")
		}
		if actor.PublicKey.PublicKeyPem != "test-key-pem" {
			t.Errorf("PublicKeyPem = %q", actor.PublicKey.PublicKeyPem)
		}
		if actor.PublicKey.Owner != "https://bridge.example.com/users/alice" {
			t.Errorf("PublicKey.Owner = %q", actor.PublicKey.Owner)
		}

		// Icon (profile picture)
		if actor.Icon == nil {
			t.Fatal("Icon should not be nil")
		}
		if actor.Icon.URL != "https://example.com/avatar.jpg" {
			t.Errorf("Icon.URL = %q", actor.Icon.URL)
		}

		// Image (banner)
		if actor.Image == nil {
			t.Fatal("Image should not be nil")
		}
		if actor.Image.URL != "https://example.com/banner.jpg" {
			t.Errorf("Image.URL = %q", actor.Image.URL)
		}

		// Endpoints
		if actor.Endpoints == nil {
			t.Fatal("Endpoints should not be nil")
		}
		if actor.Endpoints.SharedInbox != "https://bridge.example.com/inbox" {
			t.Errorf("SharedInbox = %q", actor.Endpoints.SharedInbox)
		}

		// ProxyOf
		if len(actor.ProxyOf) != 1 {
			t.Fatalf("ProxyOf count = %d, want 1", len(actor.ProxyOf))
		}
		if actor.ProxyOf[0].Protocol != NostrProtocolURI {
			t.Errorf("ProxyOf protocol = %q", actor.ProxyOf[0].Protocol)
		}
	})

	t.Run("kind-0 without picture and banner", func(t *testing.T) {
		meta := `{"name":"Bob","about":"Minimal profile"}`
		event := &nostr.Event{
			ID:        "meta456",
			PubKey:    "pubkey456",
			Kind:      0,
			Content:   meta,
			CreatedAt: nostr.Timestamp(1700000000),
			Tags:      nostr.Tags{},
		}
		tc := testTC()
		actor := ToActor(event, tc)

		if actor.Icon != nil {
			t.Error("Icon should be nil when no picture")
		}
		if actor.Image != nil {
			t.Error("Image should be nil when no banner")
		}
	})
}
