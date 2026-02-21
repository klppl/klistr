// klistr is a lightweight single-user bridge between Nostr and the ActivityPub Fediverse.
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
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/klppl/klistr/internal/ap"
	"github.com/klppl/klistr/internal/bsky"
	"github.com/klppl/klistr/internal/config"
	"github.com/klppl/klistr/internal/db"
	nostrpkg "github.com/klppl/klistr/internal/nostr"
	"github.com/klppl/klistr/internal/server"
)

func main() {
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
		LocalDomain:   cfg.LocalDomain,
		LocalActorURL: localActorURL,
		Signer:        signer,
		Publisher:     publisher,
		Store:         store,
		Federator:     federator,
		NostrRelay:    cfg.PrimaryRelay(),
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
	if cfg.BskyEnabled() {
		bskyClient := bsky.NewClient(cfg.BskyIdentifier, cfg.BskyAppPassword)
		if err := bskyClient.Authenticate(ctx); err != nil {
			slog.Warn("bsky auth failed, bridge disabled", "error", err)
		} else {
			nostrHandler.BskyPoster = &bsky.Poster{
				Client:          bskyClient,
				Store:           store,
				LocalDomain:     cfg.LocalDomain,
				ExternalBaseURL: cfg.ExternalBaseURL,
			}
			bskyTrigger = make(chan struct{}, 1)
			poller := &bsky.Poller{
				Client:      bskyClient,
				Publisher:   publisher,
				Signer:      signer,
				Store:       store,
				LocalPubKey: cfg.NostrPublicKey,
				Interval:    30 * time.Second,
				TriggerCh:   bskyTrigger,
			}
			go poller.Start(ctx)
			slog.Info("bsky bridge enabled", "identifier", cfg.BskyIdentifier)
		}
	}

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
	srv.Start(ctx) // blocks until ctx is cancelled

	slog.Info("klistr bridge stopped")
}
