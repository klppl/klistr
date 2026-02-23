// klistr is a self-hosted personal bridge that connects your Nostr identity to the Fediverse.
// It runs as a single binary with SQLite by default, requiring no external
// database for self-hosted deployments.
//
// Usage:
//
//	export NOSTR_PRIVATE_KEY=<your hex private key>
//	export NOSTR_USERNAME=alice
//	export LOCAL_DOMAIN=https://yourdomain.com
//	export NOSTR_RELAY=wss://relay.mostr.pub,wss://relay.damus.io
//	./klistr
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gonostr "github.com/nbd-wtf/go-nostr"
	"github.com/klppl/klistr/internal/ap"
	"github.com/klppl/klistr/internal/bsky"
	"github.com/klppl/klistr/internal/config"
	"github.com/klppl/klistr/internal/db"
	nostrpkg "github.com/klppl/klistr/internal/nostr"
	"github.com/klppl/klistr/internal/server"
)

// relayManagerAdapter satisfies server.RelayManager by delegating to Publisher and RelayPool.
// It also persists relay list changes to the DB so they survive restarts.
type relayManagerAdapter struct {
	publisher *nostrpkg.Publisher
	pool      *nostrpkg.RelayPool
	store     *db.Store
}

func (a *relayManagerAdapter) Relays() []string { return a.publisher.Relays() }

func (a *relayManagerAdapter) RelayStatuses() []server.RelayStatus {
	src := a.publisher.RelayStatuses()
	out := make([]server.RelayStatus, len(src))
	for i, s := range src {
		out[i] = server.RelayStatus{
			URL:               s.URL,
			CircuitOpen:       s.CircuitOpen,
			FailCount:         s.FailCount,
			CooldownRemaining: s.CooldownRemaining,
		}
	}
	return out
}

func (a *relayManagerAdapter) AddRelay(url string) bool {
	added := a.publisher.AddRelay(url)
	if added {
		a.pool.AddRelay(url)
		a.persist()
	}
	return added
}

func (a *relayManagerAdapter) RemoveRelay(url string) bool {
	removed := a.publisher.RemoveRelay(url)
	if removed {
		a.pool.RemoveRelay(url)
		a.persist()
	}
	return removed
}

func (a *relayManagerAdapter) ResetCircuit(url string) { a.publisher.ResetCircuit(url) }

func (a *relayManagerAdapter) TestRelay(ctx context.Context, url string) error {
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	relay, err := gonostr.RelayConnect(tctx, url)
	if err != nil {
		return err
	}
	relay.Close()
	return nil
}

func (a *relayManagerAdapter) persist() {
	relays := a.publisher.Relays()
	if err := a.store.SetKV("nostr_relays", strings.Join(relays, ",")); err != nil {
		slog.Warn("failed to persist relay list", "error", err)
	}
}

// followPublisherAdapter satisfies server.FollowPublisher by delegating to
// the Nostr Signer (for signing) and Publisher (for relay delivery).
type followPublisherAdapter struct {
	signer    *nostrpkg.Signer
	publisher *nostrpkg.Publisher
}

func (a *followPublisherAdapter) SignAsUser(event *gonostr.Event) error {
	return a.signer.SignAsUser(event)
}
func (a *followPublisherAdapter) Sign(event *gonostr.Event, actorID string) error {
	return a.signer.Sign(event, actorID)
}
func (a *followPublisherAdapter) Publish(ctx context.Context, event *gonostr.Event) error {
	return a.publisher.Publish(ctx, event)
}

func main() {
	// Health check mode: invoked by the Docker healthcheck as "/klistr -health".
	// Runs before config.Load() so it works even without NOSTR_PRIVATE_KEY set
	// in the exec context. Exits 0 on HTTP 200, 1 on any error.
	if len(os.Args) > 1 && (os.Args[1] == "-health" || os.Args[1] == "--health") {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8000"
		}
		resp, err := http.Get("http://localhost:" + port + "/api/healthcheck")
		if err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "unhealthy: HTTP %d\n", resp.StatusCode)
		os.Exit(1)
	}

	// Structured JSON logging. When WEB_ADMIN is set, a LogBroadcaster wraps
	// os.Stdout so the live log stream at /web/log/stream can fan out entries.
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	var logBroadcaster *server.LogBroadcaster
	var logOut io.Writer = os.Stdout
	if os.Getenv("WEB_ADMIN") != "" {
		lb := server.NewLogBroadcaster(os.Stdout)
		logBroadcaster = lb
		logOut = lb
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{
		Level: logLevel,
	})))

	slog.Info("starting klistr bridge", "version", "1.0.0")

	// ─── Configuration ────────────────────────────────────────────────────────
	cfg := config.Load()
	slog.Info("config loaded",
		"domain", cfg.LocalDomain,
		"relays", cfg.NostrRelays,
		"database", cfg.DatabaseURL,
		"username", cfg.NostrUsername,
		"pubkey", cfg.NostrPublicKey[:8],
	)

	// ─── Database ─────────────────────────────────────────────────────────────
	store, err := db.Open(cfg.DatabaseURL)
	if err != nil {
		slog.Error("failed to open database", "error", err, "url", cfg.DatabaseURL)
		os.Exit(1)
	}
	defer store.Close()

	if err := store.Migrate(); err != nil {
		slog.Error("database migration failed", "error", err)
		os.Exit(1)
	}

	// ─── Relay list: prefer DB-persisted override over env ────────────────────
	// Relay list changes made via /web admin UI are stored in kv["nostr_relays"].
	if saved, ok := store.GetKV("nostr_relays"); ok && saved != "" {
		overrides := strings.Split(saved, ",")
		if len(overrides) > 0 {
			cfg.NostrRelays = overrides
			slog.Info("using persisted relay list", "relays", cfg.NostrRelays)
		}
	}

	// ─── RSA Key Pair (auto-generated if missing) ─────────────────────────────
	keyPair, err := ap.LoadOrGenerateKeyPair(cfg.RSAPrivateKeyPath, cfg.RSAPublicKeyPath)
	if err != nil {
		slog.Error("failed to load/generate RSA key pair", "error", err)
		os.Exit(1)
	}
	slog.Info("RSA key pair ready")

	// ─── Nostr Signer ─────────────────────────────────────────────────────────
	signer := nostrpkg.NewSigner(cfg.NostrPrivateKey, cfg.NostrPublicKey)

	// ─── Nostr Publisher ──────────────────────────────────────────────────────
	publisher := nostrpkg.NewPublisher(cfg.NostrRelays)

	// ─── AP Transmute Context ─────────────────────────────────────────────────
	localActorURL := cfg.BaseURL("/users/" + cfg.NostrUsername)
	tc := &ap.TransmuteContext{
		LocalDomain:   cfg.LocalDomain,
		LocalActorURL: localActorURL,
		PublicKeyPem:  keyPair.PublicPEM,
		GetAPIDForObject: func(nostrID string) (string, bool) {
			return store.GetAPIDForObject(nostrID)
		},
	}

	// ─── AP Federator ─────────────────────────────────────────────────────────
	federator := &ap.Federator{
		LocalDomain: cfg.LocalDomain,
		KeyID:       localActorURL + "#main-key",
		PrivateKey:  keyPair.Private,
		GetFollowers: func(actorURL string) ([]string, error) {
			return store.GetFollowers(actorURL)
		},
	}

	// ─── AP Handler (incoming ActivityPub → Nostr) ────────────────────────────
	apHandler := &ap.APHandler{
		LocalDomain:    cfg.LocalDomain,
		LocalActorURL:  localActorURL,
		Signer:         signer,
		Publisher:      publisher,
		Store:          store,
		Federator:      federator,
		NostrRelay:     cfg.PrimaryRelay(),
		ShowSourceLink: cfg.ShowSourceLink,
	}

	// ─── Nostr Handler (incoming Nostr → ActivityPub) ─────────────────────────
	nostrHandler := &nostrpkg.Handler{
		TC:        tc,
		Federator: federator,
		Store:     store,
	}

	// ─── Graceful shutdown ────────────────────────────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ─── Bluesky bridge (optional) ────────────────────────────────────────────
	var bskyTrigger chan struct{}
	var activeBskyClient *bsky.Client
	if cfg.BskyEnabled() {
		bskyClient := bsky.NewClient(cfg.BskyIdentifier, cfg.BskyAppPassword)
		if err := bskyClient.Authenticate(ctx); err != nil {
			slog.Warn("bsky auth failed, bridge disabled", "error", err)
		} else {
			activeBskyClient = bskyClient
			nostrHandler.BskyPoster = &bsky.Poster{
				Client:          bskyClient,
				Store:           store,
				LocalDomain:     cfg.LocalDomain,
				ExternalBaseURL: cfg.ExternalBaseURL,
			}
			bskyTrigger = make(chan struct{}, 1)
			poller := &bsky.Poller{
				Client:         bskyClient,
				Publisher:      publisher,
				Signer:         signer,
				Store:          store,
				LocalPubKey:    cfg.NostrPublicKey,
				LocalActorURL:  localActorURL,
				Interval:       30 * time.Second,
				ShowSourceLink: cfg.ShowSourceLink,
				TriggerCh:      bskyTrigger,
			}
			go poller.Start(ctx)
			slog.Info("bsky bridge enabled", "identifier", cfg.BskyIdentifier)
		}
	}

	// ─── Account Profile Resyncer ─────────────────────────────────────────────
	resyncTrigger := make(chan struct{}, 1)
	resyncer := &ap.AccountResyncer{
		Signer:    signer,
		Publisher: publisher,
		Store:     store,
		Interval:  24 * time.Hour,
		TriggerCh: resyncTrigger,
	}
	go resyncer.Start(ctx)

	// ─── Start relay subscription ─────────────────────────────────────────────
	pool := nostrpkg.NewRelayPool(cfg.NostrRelays, cfg.NostrRelays, cfg.NostrPublicKey, nostrHandler.Handle)
	go pool.Start(ctx)

	// ─── Start HTTP server ────────────────────────────────────────────────────
	srv := server.New(cfg, store, keyPair, apHandler, store, signer)
	if logBroadcaster != nil {
		srv.SetLogBroadcaster(logBroadcaster)
	}
	if bskyTrigger != nil {
		srv.SetBskyTrigger(bskyTrigger)
	}
	if activeBskyClient != nil {
		srv.SetBskyClient(activeBskyClient)
	}
	srv.SetResyncTrigger(resyncTrigger)
	srv.SetFollowPublisher(&followPublisherAdapter{signer: signer, publisher: publisher})
	srv.SetRelayManager(&relayManagerAdapter{publisher: publisher, pool: pool, store: store})
	srv.Start(ctx) // blocks until ctx is cancelled

	slog.Info("klistr bridge stopped")
}
