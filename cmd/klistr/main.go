// klistr is a lightweight single-user bridge between Nostr and the ActivityPub Fediverse.
// It runs as a single binary with SQLite by default, requiring no external
// database for self-hosted deployments.
//
// Usage:
//
//	export NOSTR_PRIVATE_KEY=<your hex private key>
//	export NOSTR_USERNAME=alice
//	export LOCAL_DOMAIN=https://yourdomain.com
//	export NOSTR_RELAY=wss://relay.mostr.pub
//	./klistr
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/klppl/klistr/internal/ap"
	"github.com/klppl/klistr/internal/config"
	"github.com/klppl/klistr/internal/db"
	nostrpkg "github.com/klppl/klistr/internal/nostr"
	"github.com/klppl/klistr/internal/server"
)

func main() {
	// Structured JSON logging by default — easy to parse with any log aggregator.
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})))

	slog.Info("starting klistr bridge", "version", "1.0.0")

	// ─── Configuration ────────────────────────────────────────────────────────
	cfg := config.Load()
	slog.Info("config loaded",
		"domain", cfg.LocalDomain,
		"relay", cfg.NostrRelay,
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
	writeRelays := cfg.WriteRelays
	if cfg.NostrRelay != "" {
		writeRelays = append(writeRelays, cfg.NostrRelay)
	}
	publisher := nostrpkg.NewPublisher(writeRelays)

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
	serviceActorID := cfg.BaseURL("/actor")
	federator := &ap.Federator{
		LocalDomain: cfg.LocalDomain,
		KeyID:       serviceActorID + "#main-key",
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
		NostrRelay:    cfg.NostrRelay,
	}

	// ─── Nostr Handler (incoming Nostr → ActivityPub) ─────────────────────────
	nostrHandler := &nostrpkg.Handler{
		TC:        tc,
		Federator: federator,
	}

	// ─── Graceful shutdown ────────────────────────────────────────────────────
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ─── Start relay subscription ─────────────────────────────────────────────
	readRelays := cfg.ReadRelays
	if cfg.NostrRelay != "" {
		readRelays = append(readRelays, cfg.NostrRelay)
	}
	pool := nostrpkg.NewRelayPool(readRelays, writeRelays, cfg.NostrPublicKey, nostrHandler.Handle)
	go pool.Start(ctx)

	// ─── Start HTTP server ────────────────────────────────────────────────────
	srv := server.New(cfg, store, keyPair, apHandler)
	srv.Start(ctx) // blocks until ctx is cancelled

	slog.Info("klistr bridge stopped")
}
