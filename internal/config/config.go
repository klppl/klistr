package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	LocalDomain       string
	NostrRelays       []string // all relays; first is used as the hint relay in event tags
	NostrPrivateKey   string
	NostrPublicKey    string
	NostrNpub         string
	NostrUsername     string
	NostrDisplayName  string
	NostrSummary      string
	NostrPicture      string
	NostrBanner       string
	DatabaseURL       string
	RSAPrivateKeyPath string
	RSAPublicKeyPath  string
	SignFetch         bool
	ExternalBaseURL   string
	ZapPubkey         string
	ZapSplit          float64
	Port              string
}

// PrimaryRelay returns the first configured relay, used as the hint relay in event tags.
func (c *Config) PrimaryRelay() string {
	if len(c.NostrRelays) > 0 {
		return c.NostrRelays[0]
	}
	return ""
}

// Load reads configuration from environment variables.
// Panics if required variables (NOSTR_PRIVATE_KEY) are missing.
func Load() *Config {
	privKey := os.Getenv("NOSTR_PRIVATE_KEY")
	if privKey == "" {
		fmt.Fprintln(os.Stderr, "ERROR: NOSTR_PRIVATE_KEY is not set!")
		fmt.Fprintln(os.Stderr, "Set it to your Nostr hex private key.")
		os.Exit(1)
	}

	pubKey, err := nostr.GetPublicKey(privKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: invalid NOSTR_PRIVATE_KEY: %v\n", err)
		os.Exit(1)
	}

	username := os.Getenv("NOSTR_USERNAME")
	if username == "" {
		username = pubKey[:8]
	}

	npub, err := nip19.EncodePublicKey(pubKey)
	if err != nil {
		npub = pubKey // fallback to hex if encoding fails
	}

	displayName := os.Getenv("NOSTR_DISPLAY_NAME")
	if displayName == "" {
		displayName = username
	}

	nostrRelays := parseRelays(os.Getenv("NOSTR_RELAY"))
	if len(nostrRelays) == 0 {
		nostrRelays = []string{"wss://relay.mostr.pub"}
	}

	return &Config{
		LocalDomain:       getEnv("LOCAL_DOMAIN", "http://localhost:8000"),
		NostrRelays:       nostrRelays,
		NostrPrivateKey:   privKey,
		NostrPublicKey:    pubKey,
		NostrNpub:         npub,
		NostrUsername:     username,
		NostrDisplayName:  displayName,
		NostrSummary:      os.Getenv("NOSTR_SUMMARY"),
		NostrPicture:      os.Getenv("NOSTR_PICTURE"),
		NostrBanner:       os.Getenv("NOSTR_BANNER"),
		DatabaseURL:       getEnv("DATABASE_URL", "klistr.db"),
		RSAPrivateKeyPath: getEnv("RSA_PRIVATE_KEY_PATH", "private.pem"),
		RSAPublicKeyPath:  getEnv("RSA_PUBLIC_KEY_PATH", "public.pem"),
		SignFetch:         getEnv("SIGN_FETCH", "true") != "false",
		ExternalBaseURL:   getEnv("EXTERNAL_BASE_URL", "https://njump.me"),
		ZapPubkey:         os.Getenv("ZAP_PUBKEY"),
		ZapSplit:          parseFloat(os.Getenv("ZAP_SPLIT"), 0.1),
		Port:              getEnv("PORT", "8000"),
	}
}

// URL returns the parsed local domain as a *url.URL.
func (c *Config) URL() *url.URL {
	u, _ := url.Parse(c.LocalDomain)
	return u
}

// BaseURL constructs an absolute URL from a path.
func (c *Config) BaseURL(path string) string {
	return strings.TrimRight(c.LocalDomain, "/") + path
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseRelays(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func parseFloat(s string, fallback float64) float64 {
	if s == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return f
}
