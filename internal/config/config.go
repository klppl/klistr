package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

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
	BskyIdentifier    string // BSKY_IDENTIFIER env var (handle or DID)
	BskyAppPassword   string // BSKY_APP_PASSWORD env var
	BskyPDSURL        string // BSKY_PDS_URL env var — PDS endpoint (default: https://bsky.social); set for third-party PDS / did:web accounts
	BskyBridgeTimeline bool  // BSKY_BRIDGE_TIMELINE env var — bridge followed accounts' timeline posts to Nostr (default: false)
	WebAdminPassword  string // WEB_ADMIN env var — enables /web admin UI when set
	ShowSourceLink    bool   // SHOW_SOURCE_LINK env var — append original post URL to bridged notes

	// Tunable performance constants (all have sensible defaults; rarely need changing).
	ResyncInterval          time.Duration // RESYNC_INTERVAL — how often AP actor profiles are re-fetched (default 24h)
	APCacheTTL              time.Duration // AP_CACHE_TTL — TTL for the AP object / WebFinger caches (default 1h)
	BskyPollInterval        time.Duration // BSKY_POLL_INTERVAL — how often the Bluesky notification poller runs (default 30s)
	APFederationConcurrency int           // AP_FEDERATION_CONCURRENCY — max concurrent outbound AP HTTP requests (default 10)
	RelayCBThreshold        int           // RELAY_CB_THRESHOLD — consecutive publish failures before circuit opens (default 3)
}

// BskyEnabled returns true if Bluesky bridge credentials are configured.
func (c *Config) BskyEnabled() bool {
	return c.BskyIdentifier != "" && c.BskyAppPassword != ""
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
		BskyIdentifier:     os.Getenv("BSKY_IDENTIFIER"),
		BskyAppPassword:    os.Getenv("BSKY_APP_PASSWORD"),
		BskyPDSURL:         getEnv("BSKY_PDS_URL", "https://bsky.social"),
		BskyBridgeTimeline: getEnvBool("BSKY_BRIDGE_TIMELINE"),
		WebAdminPassword:   os.Getenv("WEB_ADMIN"),
		ShowSourceLink:    getEnvBool("SHOW_SOURCE_LINK"),

		ResyncInterval:          parseDuration(os.Getenv("RESYNC_INTERVAL"), 24*time.Hour),
		APCacheTTL:              parseDuration(os.Getenv("AP_CACHE_TTL"), time.Hour),
		BskyPollInterval:        parseDuration(os.Getenv("BSKY_POLL_INTERVAL"), 30*time.Second),
		APFederationConcurrency: parseInt(os.Getenv("AP_FEDERATION_CONCURRENCY"), 10),
		RelayCBThreshold:        parseInt(os.Getenv("RELAY_CB_THRESHOLD"), 3),
	}
}

// getEnvBool returns true if the env var is "true" or "1" (case-insensitive).
func getEnvBool(key string) bool {
	v := strings.ToLower(os.Getenv(key))
	return v == "true" || v == "1"
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

func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

func parseInt(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return i
}
