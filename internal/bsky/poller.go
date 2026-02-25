package bsky

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nbd-wtf/go-nostr"

	"github.com/klppl/klistr/internal/bridge"
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
	LocalPubKey    string
	LocalActorURL  string // used to record inbound Bluesky followers
	LocalDomain    string // used to build NIP-05 identifiers for bridged Bluesky authors
	Interval       time.Duration
	ShowSourceLink *atomic.Bool // append bsky.app post URL at the bottom of bridged notes
	// BridgeTimeline, when true, enables bridging posts from followed accounts'
	// home timeline to Nostr kind-1 events. Off by default — only notifications
	// (likes, reposts, replies, mentions, new followers) are bridged unless this
	// is explicitly set. Controlled by BSKY_BRIDGE_TIMELINE=true env var.
	BridgeTimeline bool
	// TriggerCh, if non-nil, triggers an immediate poll when sent to.
	TriggerCh <-chan struct{}

	// pollSeenDIDs tracks DIDs whose profiles have already been published in
	// the current poll cycle. Reset at the start of each poll() call.
	// Not goroutine-safe — only accessed from the single poll goroutine.
	pollSeenDIDs map[string]struct{}
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

// poll runs one full polling cycle: notifications, then (optionally) timeline.
func (p *Poller) poll(ctx context.Context) {
	// Reset per-cycle profile dedup map so each DID gets at most one
	// GetProfile API call per poll, regardless of how many posts they authored.
	p.pollSeenDIDs = make(map[string]struct{})
	p.pollNotifications(ctx)
	if p.BridgeTimeline {
		p.pollTimeline(ctx)
	}
	p.pollSeenDIDs = nil // release for GC between polls
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
	// Skip the bridge account's own posts — they originate from Nostr.
	if item.Post.Author.DID == p.Client.DID() {
		return
	}

	// Reposts (boosts) in the timeline are already bridged as kind-6 events
	// via the notification poller when the local user's content is reposted,
	// or are irrelevant third-party reposts. Skip them here.
	if item.Reason != nil && strings.HasSuffix(item.Reason.Type, "#reasonRepost") {
		return
	}

	p.bridgePost(ctx, &item.Post)
}

// bridgePost bridges a single Bluesky post to a Nostr kind-1 event.
// If the post is a reply and its parent is not yet in the DB, it fetches the
// full ancestor chain and bridges any missing posts first so the thread is
// preserved in Nostr.
func (p *Poller) bridgePost(ctx context.Context, post *TimelinePost) {
	// Idempotency: skip if this AT URI is already in the DB.
	if _, ok := p.Store.GetNostrIDForObject(post.URI); ok {
		return
	}

	record, _ := post.Record.(map[string]interface{})
	content := extractContentFromRecord(record)

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

	// Thread reply posts. If the parent is not yet bridged, fetch and bridge
	// the full ancestor chain first so we can attach a proper reply tag.
	var replyToID, rootID string
	var mentionPubkeys []string
	if replyBlock, ok := record["reply"].(map[string]interface{}); ok {
		replyToID, rootID = p.resolveReplyRefs(replyBlock)
		if replyToID == "" {
			// Parent not in DB — fetch the thread and bridge missing ancestors.
			if parentMap, ok2 := replyBlock["parent"].(map[string]interface{}); ok2 {
				if parentURI, _ := parentMap["uri"].(string); parentURI != "" {
					p.ensureAncestorsBridged(ctx, parentURI)
					replyToID, rootID = p.resolveReplyRefs(replyBlock)
				}
			}
		}
		if replyToID != "" {
			mentionPubkeys = []string{p.LocalPubKey}
		}
	}

	// Quote post: resolve the quoted AT URI to a Nostr event ID.
	var quoteEventID string
	if uri := extractQuoteURI(record); uri != "" {
		quoteEventID, _ = p.Store.GetNostrIDForObject(uri)
	}

	np := bridge.NormalizedPost{
		Content:        content,
		CreatedAt:      createdAt,
		Images:         extractImagesFromRecord(record, post.Author.DID),
		ReplyToEventID: replyToID,
		RootEventID:    rootID,
		MentionPubkeys: mentionPubkeys,
		QuoteEventID:   quoteEventID,
		Hashtags:       extractHashtagsFromRecord(record),
		SourceURL:      atURIToHTTPS(post.URI),
		ShowSourceLink: p.ShowSourceLink.Load(),
		ProxyID:        post.URI,
		ProxyProtocol:  "atproto",
	}
	event := bridge.BuildKind1Event(np)

	// Publish a kind-0 for the author so clients can display their profile.
	p.publishAuthorProfile(ctx, post.Author.DID, post.Author.Handle, post.Author.DisplayName)

	if err := p.Signer.Sign(event, post.Author.DID); err != nil {
		slog.Warn("bsky poller: sign failed", "author", post.Author.Handle, "error", err)
		return
	}
	if err := p.Publisher.Publish(ctx, event); err != nil {
		slog.Warn("bsky poller: publish failed", "author", post.Author.Handle, "error", err)
		return
	}
	if err := p.Store.AddObject(post.URI, event.ID); err != nil {
		slog.Warn("bsky poller: store mapping failed", "uri", post.URI, "error", err)
	}
	slog.Info("bsky poller: bridged post", "author", post.Author.Handle, "uri", post.URI)
}

// ensureAncestorsBridged fetches the full ancestor chain for the given AT URI
// via app.bsky.feed.getPostThread and bridges any posts that are not yet in the
// DB, oldest-first, so each post can reference its parent's Nostr event ID.
func (p *Poller) ensureAncestorsBridged(ctx context.Context, parentURI string) {
	thread, err := p.Client.GetPostThread(ctx, parentURI)
	if err != nil {
		slog.Debug("bsky poller: could not fetch thread for ancestor bridging",
			"uri", parentURI, "error", err)
		return
	}

	// Collect the ancestor chain: current node first, root last.
	const threadViewPost = "app.bsky.feed.defs#threadViewPost"
	var chain []TimelinePost
	for node := &thread.Thread; node != nil; node = node.Parent {
		if node.Type == threadViewPost {
			chain = append(chain, node.Post)
		}
	}

	// Reverse to process oldest-first so each post can thread to its parent.
	slices.Reverse(chain)

	for i := range chain {
		p.bridgePost(ctx, &chain[i])
	}
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
		// If the parent is not in the DB, drop silently — no DM notification.
		p.bridgeReply(ctx, n)

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
		// Parent not in DB — fetch and bridge the full ancestor chain first,
		// then retry. Mirrors the behaviour of bridgePost (timeline path).
		p.ensureAncestorsBridged(ctx, parentURI)
		parentNostrID, ok = p.Store.GetNostrIDForObject(parentURI)
		if !ok {
			slog.Debug("bsky poller: reply parent not bridged after ancestor fetch, falling back to DM",
				"parentURI", parentURI, "author", n.Author.Handle)
			return false
		}
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

	record, _ := n.Record.(map[string]interface{})
	content := extractContentFromRecord(record)

	// Quote post: resolve the quoted AT URI to a Nostr event ID.
	var quoteEventID string
	if uri := extractQuoteURI(record); uri != "" {
		quoteEventID, _ = p.Store.GetNostrIDForObject(uri)
	}

	np := bridge.NormalizedPost{
		Content:        content,
		CreatedAt:      nostr.Now(),
		Images:         extractImagesFromRecord(record, n.Author.DID),
		ReplyToEventID: parentNostrID,
		RootEventID:    rootNostrID,
		MentionPubkeys: []string{p.LocalPubKey},
		QuoteEventID:   quoteEventID,
		Hashtags:       extractHashtagsFromRecord(record),
		SourceURL:      atURIToHTTPS(n.URI),
		ShowSourceLink: p.ShowSourceLink.Load(),
		ProxyID:        n.URI,
		ProxyProtocol:  "atproto",
	}
	event := bridge.BuildKind1Event(np)

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
// Within a single poll cycle, each DID is published at most once to avoid
// redundant GetProfile API calls that can trigger Bluesky rate limits.
func (p *Poller) publishAuthorProfile(ctx context.Context, did, handle, displayName string) {
	if did == "" || handle == "" {
		return
	}

	// Dedup within the current poll cycle.
	if p.pollSeenDIDs != nil {
		if _, seen := p.pollSeenDIDs[did]; seen {
			return
		}
		p.pollSeenDIDs[did] = struct{}{}
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

	// Store handle → DID mapping so the NIP-05 endpoint can resolve it without
	// making an outbound API call.
	if err := p.Store.SetKV("bsky_did_"+handle, did); err != nil {
		slog.Debug("bsky poller: failed to store handle→DID mapping", "handle", handle, "error", err)
	}

	// Build NIP-05 identifier: "<handle>@<localHost>" e.g. "alice.bsky.social@yourdomain.com"
	var nip05 string
	if p.LocalDomain != "" {
		localHost := bridge.ExtractHost(p.LocalDomain)
		if localHost != "" {
			nip05 = handle + "@" + localHost
		}
	}

	profileMeta := struct {
		Name    string `json:"name"`
		About   string `json:"about"`
		Picture string `json:"picture,omitempty"`
		Banner  string `json:"banner,omitempty"`
		Website string `json:"website"`
		NIP05   string `json:"nip05,omitempty"`
	}{
		Name:    name,
		About:   about,
		Picture: avatarURL,
		Banner:  bannerURL,
		Website: profileURL,
		NIP05:   nip05,
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
