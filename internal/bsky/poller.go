package bsky

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// kvLastSeenKey stores the indexedAt timestamp of the most-recently processed
// notification. Used to skip already-handled notifications on subsequent polls
// (the API cursor is for backwards pagination, not forward polling).
const kvLastSeenKey = "bsky_last_seen_at"

// Publisher is the subset of nostr.Publisher used by the Poller.
type Publisher interface {
	Publish(ctx context.Context, event *nostr.Event) error
}

// Signer is the subset of nostr.Signer used by the Poller.
type Signer interface {
	SignAsUser(event *nostr.Event) error
	// Sign derives a deterministic key for id and signs the event.
	// Used to give each Bluesky author a consistent pseudonymous Nostr identity.
	Sign(event *nostr.Event, id string) error
	CreateDMToSelf(message string) (*nostr.Event, error)
}

// PollerStore is the subset of db.Store used by the Poller.
type PollerStore interface {
	GetNostrIDForObject(apID string) (string, bool)
	SetKV(key, value string) error
	GetKV(key string) (string, bool)
}

// Poller polls Bluesky notifications and publishes them as Nostr events.
type Poller struct {
	Client      *Client
	Publisher   Publisher
	Signer      Signer
	Store       PollerStore
	LocalPubKey string
	Interval    time.Duration
	// TriggerCh, if non-nil, triggers an immediate poll when sent to.
	TriggerCh <-chan struct{}
}

// Start begins the notification polling loop. Blocks until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	slog.Info("bsky poller started", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Poll once immediately on start.
	p.poll(ctx)

	// A nil channel blocks forever — safe to select on when TriggerCh is unset.
	trigCh := p.TriggerCh

	for {
		select {
		case <-ctx.Done():
			slog.Info("bsky poller stopped")
			return
		case <-ticker.C:
			p.poll(ctx)
		case <-trigCh:
			slog.Info("bsky poll triggered manually")
			p.poll(ctx)
		}
	}
}

// poll fetches new notifications from Bluesky and processes any that are newer
// than the last-seen timestamp stored in the KV store.
func (p *Poller) poll(ctx context.Context) {
	// Always fetch from the top (newest first); we filter by indexedAt ourselves.
	// The API cursor is for backwards pagination, not forward polling.
	resp, err := p.Client.ListNotifications(ctx, "")
	if err != nil {
		slog.Warn("bsky poller: list notifications failed", "error", err)
		return
	}

	if len(resp.Notifications) == 0 {
		return
	}

	lastSeen, _ := p.Store.GetKV(kvLastSeenKey)

	// Process oldest-first (API returns newest-first, so reverse).
	notifs := make([]Notification, len(resp.Notifications))
	copy(notifs, resp.Notifications)
	for i, j := 0, len(notifs)-1; i < j; i, j = i+1, j-1 {
		notifs[i], notifs[j] = notifs[j], notifs[i]
	}

	var newest string
	for i := range notifs {
		n := &notifs[i]
		// Skip notifications we have already processed.
		if lastSeen != "" && n.IndexedAt <= lastSeen {
			continue
		}
		p.handleNotification(ctx, n)
		if n.IndexedAt > newest {
			newest = n.IndexedAt
		}
	}

	if newest != "" {
		if err := p.Store.SetKV(kvLastSeenKey, newest); err != nil {
			slog.Warn("bsky poller: failed to save last-seen timestamp", "error", err)
		}
	}
}

// handleNotification converts a single Bluesky notification to a Nostr event.
func (p *Poller) handleNotification(ctx context.Context, n *Notification) {
	slog.Debug("bsky poller: handling notification", "reason", n.Reason, "uri", n.URI, "author", n.Author.Handle)

	switch n.Reason {
	case "follow":
		// Send a NIP-04 self-DM notification.
		msg := "🔔 New Bluesky follower: @" + n.Author.Handle
		dm, err := p.Signer.CreateDMToSelf(msg)
		if err != nil {
			slog.Warn("bsky poller: create DM failed", "error", err)
			return
		}
		if err := p.Publisher.Publish(ctx, dm); err != nil {
			slog.Warn("bsky poller: publish DM failed", "error", err)
		}
		return

	case "like", "repost":
		// Skip if this notification's URI belongs to content we bridged (loop guard).
		if _, isBridged := p.Store.GetNostrIDForObject(n.URI); isBridged {
			slog.Debug("bsky poller: skipping notification for bridged content", "uri", n.URI)
			return
		}

		event, err := NotificationToNostrEvent(n, p.LocalPubKey)
		if err != nil {
			slog.Warn("bsky poller: transmute failed", "reason", n.Reason, "error", err)
			return
		}
		if event == nil {
			return
		}

		if err := p.Signer.SignAsUser(event); err != nil {
			slog.Warn("bsky poller: sign event failed", "error", err)
			return
		}

		if err := p.Publisher.Publish(ctx, event); err != nil {
			slog.Warn("bsky poller: publish event failed", "error", err)
			return
		}
		slog.Info("bsky poller: published nostr event", "reason", n.Reason, "kind", event.Kind)

	case "reply":
		// Try to thread the reply into the existing Nostr conversation.
		// If the parent post was bridged, we know its Nostr event ID and can
		// create a proper kind-1 reply signed with a derived key for the
		// Bluesky author's DID (same mechanism as AP actor bridging).
		if p.bridgeReply(ctx, n) {
			return
		}
		// Parent not found in DB — fall back to a DM notification.
		p.sendDMNotification(ctx, n)

	case "mention", "quote":
		// No clear parent Nostr post to thread into; notify via DM.
		p.sendDMNotification(ctx, n)

	default:
		// Unknown reason type; ignore.
	}
}

// bridgeReply attempts to publish the Bluesky reply as a threaded Nostr kind-1
// event. It extracts the parent/root AT URIs from the reply record, looks up
// their Nostr event IDs, and signs with a derived key for the Bluesky author's
// DID so each author has a consistent pseudonymous Nostr identity.
// Returns true if the reply was successfully bridged, false if it should fall
// back to a DM notification.
func (p *Poller) bridgeReply(ctx context.Context, n *Notification) bool {
	parentURI, rootURI := extractReplyRefs(n)
	if parentURI == "" {
		return false
	}

	parentNostrID, ok := p.Store.GetNostrIDForObject(parentURI)
	if !ok {
		slog.Debug("bsky poller: reply parent not bridged, falling back to DM",
			"parentURI", parentURI, "author", n.Author.Handle)
		return false
	}

	// Use root if available; fall back to parent as the root.
	rootNostrID := parentNostrID
	if rootURI != "" && rootURI != parentURI {
		if id, ok := p.Store.GetNostrIDForObject(rootURI); ok {
			rootNostrID = id
		}
	}

	content := extractNotifText(n)
	event := &nostr.Event{
		Kind:      1,
		Content:   content,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", rootNostrID, "", "root"},
			{"e", parentNostrID, "", "reply"},
			{"p", p.LocalPubKey},
			// proxy tag prevents the outbound Bluesky bridge from re-posting this.
			{"proxy", n.URI, "atproto"},
		},
	}

	// Sign with a derived key for the Bluesky author's DID, giving them a
	// stable pseudonymous Nostr identity across all their replies.
	if err := p.Signer.Sign(event, n.Author.DID); err != nil {
		slog.Warn("bsky poller: failed to sign reply event", "author", n.Author.Handle, "error", err)
		return false
	}

	if err := p.Publisher.Publish(ctx, event); err != nil {
		slog.Warn("bsky poller: failed to publish reply event", "author", n.Author.Handle, "error", err)
		return false
	}

	slog.Info("bsky poller: bridged reply into nostr thread",
		"author", n.Author.Handle, "parentNostrID", parentNostrID[:8])
	return true
}

// sendDMNotification delivers a Bluesky interaction as a NIP-04 self-DM.
func (p *Poller) sendDMNotification(ctx context.Context, n *Notification) {
	content := extractNotifText(n)
	msg := fmt.Sprintf("💬 New Bluesky %s from @%s: %s\n%s",
		n.Reason, n.Author.Handle, content, atURIToHTTPS(n.URI))
	dm, err := p.Signer.CreateDMToSelf(msg)
	if err != nil {
		slog.Warn("bsky poller: create DM failed", "reason", n.Reason, "error", err)
		return
	}
	if err := p.Publisher.Publish(ctx, dm); err != nil {
		slog.Warn("bsky poller: publish DM failed", "reason", n.Reason, "error", err)
	}
	slog.Info("bsky poller: notified via DM", "reason", n.Reason, "author", n.Author.Handle)
}
