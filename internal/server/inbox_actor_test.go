package server

import (
	"encoding/json"
	"testing"
)

func TestParseActorURL_String(t *testing.T) {
	got := parseActorURL(json.RawMessage(`"https://mastodon.social/users/alice"`))
	want := "https://mastodon.social/users/alice"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseActorURL_Object(t *testing.T) {
	got := parseActorURL(json.RawMessage(`{"id":"https://example.com/users/bob","type":"Person"}`))
	want := "https://example.com/users/bob"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseActorURL_Missing(t *testing.T) {
	if got := parseActorURL(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := parseActorURL(json.RawMessage(`null`)); got != "" {
		t.Errorf("null actor: got %q, want empty", got)
	}
	if got := parseActorURL(json.RawMessage(`[]`)); got != "" {
		t.Errorf("array actor: got %q, want empty", got)
	}
}

func TestActorURLsEqual_ExactMatch(t *testing.T) {
	a := "https://mastodon.social/users/alice"
	if !actorURLsEqual(a, a) {
		t.Error("identical URLs should match")
	}
}

func TestActorURLsEqual_TrailingSlashTolerant(t *testing.T) {
	if !actorURLsEqual("https://x.com/users/a", "https://x.com/users/a/") {
		t.Error("trailing-slash variants should match")
	}
}

func TestActorURLsEqual_HostCaseInsensitive(t *testing.T) {
	if !actorURLsEqual("https://Mastodon.Social/users/alice", "https://mastodon.social/users/alice") {
		t.Error("host case differences should match")
	}
}

func TestActorURLsEqual_PathCaseSensitive(t *testing.T) {
	if actorURLsEqual("https://x.com/users/Alice", "https://x.com/users/alice") {
		t.Error("path case differences should NOT match (RFC 3986)")
	}
}

func TestActorURLsEqual_DifferentActors(t *testing.T) {
	if actorURLsEqual("https://x.com/users/alice", "https://x.com/users/bob") {
		t.Error("distinct actors must not match — this is the spoofing gate")
	}
	if actorURLsEqual("https://x.com/users/alice", "https://evil.com/users/alice") {
		t.Error("same path different host must not match")
	}
}
