package ap

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// AccountResyncStore is the DB interface used by AccountResyncer.
type AccountResyncStore interface {
	GetAllActorURLs() ([]string, error)
	SetKV(key, value string) error
}

// AccountResyncSigner can derive and use keys for AP actors.
type AccountResyncSigner interface {
	Sign(event *nostr.Event, apID string) error
}

// AccountResyncPublisher can publish Nostr events to relays.
type AccountResyncPublisher interface {
	Publish(ctx context.Context, event *nostr.Event) error
}

// AccountResyncer periodically re-fetches all known AP actors and re-publishes
// their kind-0 metadata events. This keeps derived Nostr profile data fresh
// (name, bio, picture, website, nip05) without waiting for a new AP activity.
type AccountResyncer struct {
	Signer      AccountResyncSigner
	Publisher   AccountResyncPublisher
	Store       AccountResyncStore
	LocalDomain string // used to build the nip05 field in kind-0 metadata
	// Interval between automatic resyncs. Defaults to 24h if zero.
	Interval time.Duration
	// TriggerCh, if non-nil, causes an immediate resync when sent to.
	TriggerCh <-chan struct{}
}

// Start begins the periodic resync loop. Blocks until ctx is cancelled.
// Does NOT run an initial sync on startup — the first run is after one Interval
// (or when triggered manually via TriggerCh).
func (r *AccountResyncer) Start(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		interval = 24 * time.Hour
	}

	slog.Info("account resyncer started", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	trigCh := r.TriggerCh

	for {
		select {
		case <-ctx.Done():
			slog.Info("account resyncer stopped")
			return
		case <-ticker.C:
			r.resyncAll(ctx)
		case <-trigCh:
			slog.Info("account resync triggered manually")
			r.resyncAll(ctx)
		}
	}
}

// resyncAll iterates all known AP actor URLs and re-publishes their kind-0.
func (r *AccountResyncer) resyncAll(ctx context.Context) {
	urls, err := r.Store.GetAllActorURLs()
	if err != nil {
		slog.Warn("resync: failed to list actor URLs", "error", err)
		return
	}

	// Filter to AP actors only (http/https); skip Bluesky DIDs.
	var apURLs []string
	for _, u := range urls {
		if strings.HasPrefix(u, "http") {
			apURLs = append(apURLs, u)
		}
	}

	if len(apURLs) == 0 {
		slog.Debug("resync: no AP actors to sync")
		_ = r.Store.SetKV("last_resync_at", time.Now().UTC().Format(time.RFC3339))
		_ = r.Store.SetKV("last_resync_count", "0/0")
		return
	}

	slog.Info("resync: starting actor refresh", "count", len(apURLs))

	ok, failed := 0, 0
	for _, actorURL := range apURLs {
		select {
		case <-ctx.Done():
			slog.Info("resync: interrupted", "ok", ok, "failed", failed)
			return
		default:
		}

		if err := r.resyncOne(ctx, actorURL); err != nil {
			slog.Debug("resync: actor fetch failed", "actor", actorURL, "error", err)
			failed++
		} else {
			ok++
		}

		// Small pause between fetches to avoid hammering remote servers.
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}

	total := ok + failed
	slog.Info("resync: complete", "ok", ok, "failed", failed, "total", total)

	_ = r.Store.SetKV("last_resync_at", time.Now().UTC().Format(time.RFC3339))
	_ = r.Store.SetKV("last_resync_count", fmt.Sprintf("%d/%d", ok, total))
}

// resyncOne re-fetches a single AP actor and publishes an updated kind-0.
func (r *AccountResyncer) resyncOne(ctx context.Context, actorURL string) error {
	actor, err := FetchActor(ctx, actorURL)
	if err != nil {
		return err
	}

	meta := buildMetadataContentFromActor(actor, r.LocalDomain)
	event := &nostr.Event{
		Kind:      0,
		Content:   meta,
		CreatedAt: nostr.Now(),
		Tags:      nostr.Tags{{"proxy", actorURL, "activitypub"}},
	}

	if err := r.Signer.Sign(event, actorURL); err != nil {
		return fmt.Errorf("sign: %w", err)
	}

	return r.Publisher.Publish(ctx, event)
}
