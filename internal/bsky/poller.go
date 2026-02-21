package bsky

import (
	"context"
	"log/slog"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

const kvCursorKey = "bsky_notif_cursor"

// Publisher is the subset of nostr.Publisher used by the Poller.
type Publisher interface {
	Publish(ctx context.Context, event *nostr.Event) error
}

// Signer is the subset of nostr.Signer used by the Poller.
type Signer interface {
	SignAsUser(event *nostr.Event) error
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

// poll fetches new notifications from Bluesky and processes them.
func (p *Poller) poll(ctx context.Context) {
	cursor, _ := p.Store.GetKV(kvCursorKey)

	resp, err := p.Client.ListNotifications(ctx, cursor)
	if err != nil {
		slog.Warn("bsky poller: list notifications failed", "error", err)
		return
	}

	if len(resp.Notifications) == 0 {
		return
	}

	// Process oldest-first (API returns newest-first, so reverse).
	notifs := resp.Notifications
	for i, j := 0, len(notifs)-1; i < j; i, j = i+1, j-1 {
		notifs[i], notifs[j] = notifs[j], notifs[i]
	}

	for i := range notifs {
		n := &notifs[i]
		if n.IsRead {
			continue
		}
		p.handleNotification(ctx, n)
	}

	// Save the new cursor position.
	if resp.Cursor != "" {
		if err := p.Store.SetKV(kvCursorKey, resp.Cursor); err != nil {
			slog.Warn("bsky poller: failed to save cursor", "error", err)
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

	case "like", "repost", "reply", "mention", "quote":
		// Skip if this notification's URI belongs to content we bridged.
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

	default:
		// Unknown reason type; ignore.
	}
}
