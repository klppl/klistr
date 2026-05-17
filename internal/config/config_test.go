package config

import (
	"strings"
	"testing"
)

// validCfg returns a Config that passes Validate() unmodified — tests then
// mutate one field and assert the expected error/warn behavior.
func validCfg() *Config {
	return &Config{
		LocalDomain:     "https://yourdomain.com",
		NostrRelays:     []string{"wss://relay.example"},
		Port:            "8000",
		ExternalBaseURL: "https://njump.me",
		ZapSplit:        0.1,
	}
}

func TestValidate_BaseCaseAccepts(t *testing.T) {
	if err := validCfg().Validate(); err != nil {
		t.Fatalf("baseline cfg should validate: %v", err)
	}
}

func TestValidate_RejectsBadLocalDomain(t *testing.T) {
	c := validCfg()
	c.LocalDomain = "not-a-url"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "LOCAL_DOMAIN") {
		t.Errorf("expected LOCAL_DOMAIN error, got: %v", err)
	}
}

func TestValidate_RejectsInsecureRelay(t *testing.T) {
	c := validCfg()
	c.NostrRelays = []string{"ws://public-relay.example"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "NOSTR_RELAY") {
		t.Errorf("expected NOSTR_RELAY error, got: %v", err)
	}
}

func TestValidate_AcceptsLoopbackWS(t *testing.T) {
	c := validCfg()
	c.NostrRelays = []string{"ws://localhost:7777"}
	if err := c.Validate(); err != nil {
		t.Errorf("loopback ws:// should be allowed: %v", err)
	}
}

func TestValidate_RejectsBadPort(t *testing.T) {
	c := validCfg()
	c.Port = "abc"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "PORT") {
		t.Errorf("expected PORT error, got: %v", err)
	}
}

func TestValidate_RejectsBadZapSplit(t *testing.T) {
	c := validCfg()
	c.ZapSplit = 10 // forgot to divide by 100
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "ZAP_SPLIT") {
		t.Errorf("expected ZAP_SPLIT error, got: %v", err)
	}
}

func TestValidate_RejectsInvalidBskyPDS(t *testing.T) {
	c := validCfg()
	c.BskyIdentifier = "user.bsky.social"
	c.BskyAppPassword = "xxxx-xxxx-xxxx-xxxx"
	c.BskyPDSURL = "http://example.com" // non-https, non-loopback
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "BSKY_PDS_URL") {
		t.Errorf("expected BSKY_PDS_URL error, got: %v", err)
	}
}

func TestValidate_BskyPDSIgnoredWhenBskyDisabled(t *testing.T) {
	c := validCfg()
	// Bsky credentials not set, so BskyPDSURL shouldn't be validated.
	c.BskyPDSURL = "garbage"
	if err := c.Validate(); err != nil {
		t.Errorf("BskyPDSURL should be ignored when Bsky disabled: %v", err)
	}
}

func TestValidate_CollectsAllErrors(t *testing.T) {
	c := &Config{
		LocalDomain:     "bad",
		NostrRelays:     []string{"http://x"},
		Port:            "nope",
		ExternalBaseURL: "https://njump.me",
		ZapSplit:        -1,
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	// All four issues should appear in one error.
	for _, want := range []string{"LOCAL_DOMAIN", "NOSTR_RELAY", "PORT", "ZAP_SPLIT"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %s in: %s", want, msg)
		}
	}
}
