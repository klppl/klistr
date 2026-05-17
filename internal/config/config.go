package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"

	nostrpkg "github.com/klppl/klistr/internal/nostr"
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
	BskyBridgeTimeline bool  // BSKY_BRIDGE_TIMELINE env var — bridge followed accounts' timeline posts to Nostr (default: true)
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
		BskyBridgeTimeline: getEnv("BSKY_BRIDGE_TIMELINE", "true") != "false",
		WebAdminPassword:   os.Getenv("WEB_ADMIN"),
		ShowSourceLink:    getEnvBool("SHOW_SOURCE_LINK"),

		ResyncInterval:          parseDuration(os.Getenv("RESYNC_INTERVAL"), 24*time.Hour),
		APCacheTTL:              parseDuration(os.Getenv("AP_CACHE_TTL"), time.Hour),
		BskyPollInterval:        parseDuration(os.Getenv("BSKY_POLL_INTERVAL"), 30*time.Second),
		APFederationConcurrency: parseInt(os.Getenv("AP_FEDERATION_CONCURRENCY"), 10),
		RelayCBThreshold:        parseInt(os.Getenv("RELAY_CB_THRESHOLD"), 3),
	}
}

// Validate runs sanity checks on the loaded configuration. Fatal misconfigs
// return an error; lesser issues (insecure-but-legal choices) are logged at
// WARN level so the operator notices but the bridge still starts. This catches
// the most common self-host setup mistakes at boot instead of as cryptic
// runtime errors later.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	// LOCAL_DOMAIN must be a parseable absolute URL.
	domainURL, err := url.Parse(c.LocalDomain)
	if err != nil || domainURL.Host == "" || domainURL.Scheme == "" {
		add("LOCAL_DOMAIN=%q is not a valid absolute URL (example: https://yourdomain.com)", c.LocalDomain)
	} else if domainURL.Scheme != "https" && !isLoopbackHost(domainURL.Hostname()) {
		// Non-loopback http:// is legal but a security smell — log a warning.
		slog.Warn("LOCAL_DOMAIN uses http:// for non-localhost; AP federation typically requires https://",
			"local_domain", c.LocalDomain)
	}

	// PORT must be numeric (the http server will fail to bind otherwise, but
	// catching it here surfaces a clearer message).
	if _, err := strconv.Atoi(c.Port); err != nil {
		add("PORT=%q is not a number", c.Port)
	}

	// EXTERNAL_BASE_URL must parse (used to build njump.me / nostr-client URLs).
	if _, err := url.Parse(c.ExternalBaseURL); err != nil {
		add("EXTERNAL_BASE_URL=%q is not a valid URL", c.ExternalBaseURL)
	}

	// Each Nostr relay must pass the wss-only-or-loopback check applied
	// everywhere else (admin UI, kind-10002, KV restore). Anything else gets
	// rejected at use-time anyway — surfacing it at startup gives the operator
	// the chance to fix NOSTR_RELAY before traffic starts flowing.
	for _, relay := range c.NostrRelays {
		if !nostrpkg.IsValidRelayURL(relay) {
			add("NOSTR_RELAY entry %q is invalid (must be wss://; ws:// allowed only for localhost)", relay)
		}
	}

	// Bluesky bridge: if enabled, PDS URL must be a valid https endpoint.
	if c.BskyEnabled() {
		pdsURL, err := url.Parse(c.BskyPDSURL)
		if err != nil || pdsURL.Host == "" {
			add("BSKY_PDS_URL=%q is not a valid URL", c.BskyPDSURL)
		} else if pdsURL.Scheme != "https" && !isLoopbackHost(pdsURL.Hostname()) {
			add("BSKY_PDS_URL=%q must be https:// (got %s)", c.BskyPDSURL, pdsURL.Scheme)
		}
	}

	// ZapSplit must be a sane fraction. Out-of-range is almost certainly a
	// misconfig (e.g. forgot to divide by 100).
	if c.ZapSplit < 0 || c.ZapSplit > 1 {
		add("ZAP_SPLIT=%v must be between 0 and 1 (got %v)", c.ZapSplit, c.ZapSplit)
	}

	// ZAP_PUBKEY, when set, must be 64-hex Nostr pubkey.
	if c.ZapPubkey != "" {
		if len(c.ZapPubkey) != 64 || !isHex(c.ZapPubkey) {
			slog.Warn("ZAP_PUBKEY is not 64-hex; zap splits will fail",
				"zap_pubkey_length", len(c.ZapPubkey))
		}
	}

	// WEB_ADMIN password length: short passwords are a footgun for a
	// publicly-exposed admin UI.
	if c.WebAdminPassword != "" && len(c.WebAdminPassword) < 12 {
		slog.Warn("WEB_ADMIN password is short; recommend at least 12 chars",
			"length", len(c.WebAdminPassword))
	}

	if len(errs) > 0 {
		return errors.New("config validation failed:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

func isLoopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

func isHex(s string) bool {
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
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
