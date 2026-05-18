package ap

import "testing"

func TestExtractOrigin_LowercasesHost(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// Host case normalized (RFC 4343 — DNS is case-insensitive).
		{"https://Example.com/inbox", "https://example.com"},
		{"HTTPS://example.com/inbox", "https://example.com"},
		// Path is stripped, port preserved.
		{"https://relay.example.org:8443/users/alice/inbox", "https://relay.example.org:8443"},
		// No trailing slash, just scheme://host.
		{"https://example.com", "https://example.com"},
		// Two URLs differing only in host case yield identical origins —
		// this is the shared-inbox-dedup gate.
	}
	for _, tc := range tests {
		if got := extractOrigin(tc.in); got != tc.want {
			t.Errorf("extractOrigin(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestExtractOrigin_DedupGate(t *testing.T) {
	// The actual scenario: a mis-cased shared inbox URL and a properly-cased
	// one must collapse to the same origin so the federator only sends once.
	mixed := extractOrigin("https://Mastodon.Social/inbox")
	lower := extractOrigin("https://mastodon.social/inbox")
	if mixed != lower {
		t.Errorf("case-differing origins must match (dedup gate): %q vs %q", mixed, lower)
	}
}
