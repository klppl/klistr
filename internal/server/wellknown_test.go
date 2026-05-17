package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klppl/klistr/internal/config"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		cfg: &config.Config{
			LocalDomain:     "https://YourDomain.com",
			NostrUsername:   "alice",
			NostrPublicKey:  "deadbeef" + strings.Repeat("0", 56), // 64 hex
			NostrRelays:     []string{"wss://relay.example"},
			Port:            "8000",
			ExternalBaseURL: "https://njump.me",
		},
	}
}

func TestWebFinger_CaseInsensitiveHost(t *testing.T) {
	s := newTestServer(t)
	// Configured LOCAL_DOMAIN is "https://YourDomain.com" but query lowercases it.
	req := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:alice@yourdomain.com", nil)
	rec := httptest.NewRecorder()
	s.handleWebFinger(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestWebFinger_CaseInsensitiveUser(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:ALICE@yourdomain.com", nil)
	rec := httptest.NewRecorder()
	s.handleWebFinger(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200 for mixed-case user, got %d", rec.Code)
	}
}

func TestWebFinger_WrongUser(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:bob@yourdomain.com", nil)
	rec := httptest.NewRecorder()
	s.handleWebFinger(rec, req)

	if rec.Code != 404 {
		t.Errorf("expected 404 for unknown user, got %d", rec.Code)
	}
}

func TestWebFinger_WrongHost(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:alice@elsewhere.com", nil)
	rec := httptest.NewRecorder()
	s.handleWebFinger(rec, req)

	if rec.Code != 404 {
		t.Errorf("expected 404 for wrong host, got %d", rec.Code)
	}
}

func TestNIP05_CaseInsensitiveName(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("GET", "/.well-known/nostr.json?name=ALICE", nil)
	rec := httptest.NewRecorder()
	s.handleNIP05(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		Names map[string]string `json:"names"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// Server should echo back the queried name verbatim, mapped to the local pubkey.
	if got := resp.Names["ALICE"]; got != s.cfg.NostrPublicKey {
		t.Errorf("names[ALICE] = %q, want %q", got, s.cfg.NostrPublicKey)
	}
}
