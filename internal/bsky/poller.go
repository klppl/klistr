package bsky

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// kvLastSeenKey stores the indexedAt timestamp of the most-recently processed
// notification. Used to skip already-handled notifications on subsequent polls
// (the API cursor is for backwards pagination, not forward polling).
const kvLastSeenKey = "bsky_last_seen_at"

// kvLastPollKey stores the RFC3339 timestamp of the most-recent successful
// ListNotifications API call, regardless of whether any notifications were new.
const kvLastPollKey = "bsky_last_poll_at"

// kvTimelineLastSeenKey stores the indexedAt timestamp of the most-recently
// processed timeline post, used to skip already-handled items on next poll.
const kvTimelineLastSeenKey = "bsky_timeline_last_seen_at"

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
	AddObject(apID, nostrID string) error
	GetNostrIDForObject(apID string) (string, bool)
	AddFollow(followerID, followedID string) error
	SetKV(key, value string) error
	GetKV(key string) (string, bool)
}

// Poller polls Bluesky notifications and publishes them as Nostr events.
type Poller struct {
	Client        *Client
	Publisher     Publisher
	Signer        Signer
	Store         PollerStore
	LocalPubKey   string
	LocalActorURL string // used to record inbound Bluesky followers
	Interval      time.Duration
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

// poll runs one full polling cycle: notifications then timeline.
func (p *Poller) poll(ctx context.Context) {
	p.pollNotifications(ctx)
	p.pollTimeline(ctx)
}

// pollNotifications fetches new Bluesky notifications (likes, reposts, replies,
// mentions, follows) and converts them to Nostr events.
func (p *Poller) pollNotifications(ctx context.Context) {
	// Always fetch from the top (newest first); we filter by indexedAt ourselves.
	// The API cursor is for backwards pagination, not forward polling.
	resp, err := p.Client.ListNotifications(ctx, "")
	if err != nil {
		slog.Warn("bsky poller: list notifications failed", "error", err)
		return
	}

	// Record that the poll completed successfully, regardless of new notifications.
	_ = p.Store.SetKV(kvLastPollKey, time.Now().UTC().Format(time.RFC3339))

	if len(resp.Notifications) == 0 {
		return
	}

	lastSeen, _ := p.Store.GetKV(kvLastSeenKey)

	// Process oldest-first (API returns newest-first, so reverse).
	notifs := make([]Notification, len(resp.Notifications))
	copy(notifs, resp.Notifications)
	slices.Reverse(notifs)

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

// pollTimeline fetches posts from followed Bluesky accounts and bridges them
// to Nostr kind-1 events, mirroring how Fediverse follows work via AP inbox.
func (p *Poller) pollTimeline(ctx context.Context) {
	resp, err := p.Client.GetTimeline(ctx)
	if err != nil {
		slog.Warn("bsky poller: get timeline failed", "error", err)
		return
	}
	if len(resp.Feed) == 0 {
		return
	}

	lastSeen, _ := p.Store.GetKV(kvTimelineLastSeenKey)

	// Process oldest-first (API returns newest-first, so reverse).
	items := make([]TimelineFeedPost, len(resp.Feed))
	copy(items, resp.Feed)
	slices.Reverse(items)

	var newest string
	for i := range items {
		item := &items[i]
		if lastSeen != "" && item.Post.IndexedAt <= lastSeen {
			continue
		}
		p.bridgeTimelinePost(ctx, item)
		if item.Post.IndexedAt > newest {
			newest = item.Post.IndexedAt
		}
	}

	if newest != "" {
		_ = p.Store.SetKV(kvTimelineLastSeenKey, newest)
	}
}

// bridgeTimelinePost converts a single timeline feed item into a Nostr kind-1
// event signed with a derived key for the Bluesky author's DID.
func (p *Poller) bridgeTimelinePost(ctx context.Context, item *TimelineFeedPost) {
	post := &item.Post

	// Skip the bridge account's own posts — they originate from Nostr.
	if post.Author.DID == p.Client.DID() {
		return
	}

	// Reposts (boosts) in the timeline are already bridged as kind-6 events
	// via the notification poller when the local user's content is reposted,
	// or are irrelevant third-party reposts. Skip them here.
	if item.Reason != nil && strings.HasSuffix(item.Reason.Type, "#reasonRepost") {
		return
	}

	// Idempotency: skip if this AT URI is already in the DB.
	if _, ok := p.Store.GetNostrIDForObject(post.URI); ok {
		return
	}

	record, _ := post.Record.(map[string]interface{})
	content, _ := record["text"].(string)

	// Parse the post's own createdAt; fall back to indexedAt.
	var createdAt nostr.Timestamp
	if ts, _ := record["createdAt"].(string); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			createdAt = nostr.Timestamp(t.Unix())
		}
	}
	if createdAt == 0 {
		if t, err := time.Parse(time.RFC3339, post.IndexedAt); err == nil {
			createdAt = nostr.Timestamp(t.Unix())
		} else {
			createdAt = nostr.Now()
		}
	}

	tags := nostr.Tags{{"proxy", post.URI, "atproto"}}

	// Thread reply posts if the parent is already bridged.
	if replyBlock, ok := record["reply"].(map[string]interface{}); ok {
		parentNostrID, rootNostrID := p.resolveReplyRefs(replyBlock)
		if parentNostrID != "" {
			// NIP-10 positional convention: first e-tag = root, last = direct parent.
			tags = append(tags, nostr.Tag{"e", rootNostrID}, nostr.Tag{"e", parentNostrID})
			tags = append(tags, nostr.Tag{"p", p.LocalPubKey})
		}
	}

	event := &nostr.Event{
		Kind:      1,
		Content:   content,
		CreatedAt: createdAt,
		Tags:      tags,
	}

	// Publish a kind-0 for the author so clients can display their profile.
	p.publishAuthorProfile(ctx, post.Author.DID, post.Author.Handle, post.Author.DisplayName)

	if err := p.Signer.Sign(event, post.Author.DID); err != nil {
		slog.Warn("bsky poller: timeline: sign failed", "author", post.Author.Handle, "error", err)
		return
	}
	if err := p.Publisher.Publish(ctx, event); err != nil {
		slog.Warn("bsky poller: timeline: publish failed", "author", post.Author.Handle, "error", err)
		return
	}
	if err := p.Store.AddObject(post.URI, event.ID); err != nil {
		slog.Warn("bsky poller: timeline: store mapping failed", "uri", post.URI, "error", err)
	}
	slog.Info("bsky poller: bridged timeline post", "author", post.Author.Handle, "uri", post.URI)
}

// resolveReplyRefs looks up Nostr event IDs for the parent and root of a
// Bluesky reply record. Returns empty strings if either is not bridged.
func (p *Poller) resolveReplyRefs(replyBlock map[string]interface{}) (parentNostrID, rootNostrID string) {
	if parent, ok := replyBlock["parent"].(map[string]interface{}); ok {
		if uri, _ := parent["uri"].(string); uri != "" {
			parentNostrID, _ = p.Store.GetNostrIDForObject(uri)
		}
	}
	if root, ok := replyBlock["root"].(map[string]interface{}); ok {
		if uri, _ := root["uri"].(string); uri != "" {
			rootNostrID, _ = p.Store.GetNostrIDForObject(uri)
		}
	}
	if rootNostrID == "" {
		rootNostrID = parentNostrID
	}
	return
}

// handleNotification converts a single Bluesky notification to a Nostr event.
func (p *Poller) handleNotification(ctx context.Context, n *Notification) {
	slog.Debug("bsky poller: handling notification", "reason", n.Reason, "uri", n.URI, "author", n.Author.Handle)

	switch n.Reason {
	case "follow":
		// Persist the follower so the admin UI can display them.
		if p.LocalActorURL != "" && n.Author.DID != "" {
			followerID := "bsky:" + n.Author.DID
			if err := p.Store.AddFollow(followerID, p.LocalActorURL); err != nil {
				slog.Warn("bsky poller: failed to store follower", "did", n.Author.DID, "error", err)
			}
			if n.Author.Handle != "" {
				if err := p.Store.SetKV("bsky_follower_handle_"+n.Author.DID, n.Author.Handle); err != nil {
					slog.Warn("bsky poller: failed to store follower handle", "did", n.Author.DID, "error", err)
				}
			}
		}
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

	// Publish a kind-0 for the Bluesky author so Nostr clients can resolve
	// their profile and link back to the original Bluesky account.
	p.publishBskyAuthorProfile(ctx, n)

	content := extractNotifText(n)
	event := &nostr.Event{
		Kind:      1,
		Content:   content,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			// Use 2-element e-tags (no relay hint) to satisfy strict relay schemas.
			// Positional convention (NIP-10): first = root, last = direct parent.
			{"e", rootNostrID},
			{"e", parentNostrID},
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

	// Store AT URI → Nostr event ID so that when the local user replies to this
	// event from Nostr, buildReply can resolve the AT URI for correct threading.
	if err := p.Store.AddObject(n.URI, event.ID); err != nil {
		slog.Warn("bsky poller: failed to store reply mapping", "atURI", n.URI, "error", err)
	}

	slog.Info("bsky poller: bridged reply into nostr thread",
		"author", n.Author.Handle, "parentNostrID", parentNostrID[:8])
	return true
}

// publishAuthorProfile publishes a Nostr kind-0 metadata event for a Bluesky
// author using their derived pseudonymous keypair, so Nostr clients can display
// their profile and link back to their Bluesky account.
func (p *Poller) publishAuthorProfile(ctx context.Context, did, handle, displayName string) {
	if did == "" || handle == "" {
		return
	}

	profileURL := "https://bsky.app/profile/" + handle
	name := displayName
	if name == "" {
		name = handle
	}

	// Default bio is just the profile link; replaced with the real bio if the
	// full profile fetch succeeds.
	about := profileURL
	var avatarURL, bannerURL string

	if profile, err := p.Client.GetProfile(ctx, did); err == nil {
		if profile.DisplayName != "" {
			name = profile.DisplayName
		}
		if profile.Description != "" {
			about = profile.Description + "\n\n" + profileURL
		}
		avatarURL = profile.Avatar
		bannerURL = profile.Banner
	} else {
		slog.Debug("bsky poller: could not fetch full profile, using minimal metadata",
			"handle", handle, "error", err)
	}

	profileMeta := struct {
		Name    string `json:"name"`
		About   string `json:"about"`
		Picture string `json:"picture,omitempty"`
		Banner  string `json:"banner,omitempty"`
		Website string `json:"website"`
	}{
		Name:    name,
		About:   about,
		Picture: avatarURL,
		Banner:  bannerURL,
		Website: profileURL,
	}
	metaBytes, err := json.Marshal(profileMeta)
	if err != nil {
		return
	}

	meta := &nostr.Event{
		Kind:      0,
		Content:   string(metaBytes),
		CreatedAt: nostr.Now(),
	}

	if err := p.Signer.Sign(meta, did); err != nil {
		return
	}
	if err := p.Publisher.Publish(ctx, meta); err != nil {
		slog.Debug("bsky poller: failed to publish author profile", "handle", handle, "error", err)
	}
}

// publishBskyAuthorProfile is a convenience wrapper used by the notification path.
func (p *Poller) publishBskyAuthorProfile(ctx context.Context, n *Notification) {
	p.publishAuthorProfile(ctx, n.Author.DID, n.Author.Handle, n.Author.DisplayName)
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
