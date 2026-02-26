package ap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"golang.org/x/net/html"

	"github.com/klppl/klistr/internal/bridge"
)

// APHandler handles incoming ActivityPub activities and converts them to Nostr events.
type APHandler struct {
	LocalDomain   string
	LocalActorURL string // full URL of the local AP actor
	Signer        interface {
		SignAsUser(event *nostr.Event) error
		Sign(event *nostr.Event, apID string) error
		PublicKey(apID string) (string, error)
		LocalPublicKey() string
		CreateDMToSelf(message string) (*nostr.Event, error)
	}
	Publisher interface {
		Publish(ctx context.Context, event *nostr.Event) error
	}
	Store interface {
		AddObject(apID, nostrID string) error
		DeleteObject(apID, nostrID string) error
		AddFollow(followerID, followedID string) error
		RemoveFollow(followerID, followedID string) error
		GetNostrIDForObject(apID string) (string, bool)
		// Used by Move handler to check and update follow relationships.
		GetAPFollowing(followerID string) ([]string, error)
		StoreActorKey(pubkey, actorURL string) error
	}
	Federator         *Federator
	NostrRelay        string
	ShowSourceLink    *atomic.Bool // append original post URL at the bottom of bridged notes
	AutoAcceptFollows *atomic.Bool // when false, incoming follows are rejected instead of accepted
}

// HandleActivity processes an incoming ActivityPub activity.
func (h *APHandler) HandleActivity(ctx context.Context, raw json.RawMessage) error {
	var activity IncomingActivity
	if err := json.Unmarshal(raw, &activity); err != nil {
		return fmt.Errorf("unmarshal activity: %w", err)
	}

	slog.Debug("handling AP activity",
		"id", activity.ID,
		"type", activity.Type,
		"actor", activity.Actor,
	)

	// Fetch and cache the actor so they're available in Nostr.
	// Use context.Background() so this goroutine outlives the HTTP handler's context.
	if activity.Type != "Update" {
		go h.fetchAndCacheActor(context.Background(), activity.Actor)
	}

	switch activity.Type {
	case "Follow":
		return h.handleFollow(ctx, activity)
	case "Create":
		return h.handleCreate(ctx, activity)
	case "Announce":
		return h.handleAnnounce(ctx, activity)
	case "Update":
		return h.handleUpdate(ctx, activity)
	case "Like":
		return h.handleLike(ctx, activity)
	case "EmojiReact":
		return h.handleEmojiReact(ctx, activity)
	case "Delete":
		return h.handleDelete(ctx, activity)
	case "Undo":
		return h.handleUndo(ctx, activity)
	case "Accept":
		return h.handleAccept(ctx, activity)
	case "Reject":
		return h.handleReject(ctx, activity)
	case "Move":
		return h.handleMove(ctx, activity)
	default:
		slog.Debug("unhandled activity type", "type", activity.Type)
		return nil
	}
}

func (h *APHandler) handleFollow(ctx context.Context, activity IncomingActivity) error {
	// The object of a Follow is the actor being followed.
	var followedID string
	if err := json.Unmarshal(activity.Object, &followedID); err != nil {
		return fmt.Errorf("parse follow object: %w", err)
	}

	followObj := map[string]interface{}{
		"id":     activity.ID,
		"type":   "Follow",
		"actor":  activity.Actor,
		"object": followedID,
	}

	// If auto-accept is disabled, reject and do not store the follower.
	if h.AutoAcceptFollows != nil && !h.AutoAcceptFollows.Load() {
		reject := BuildReject(followObj, followedID, activity.Actor)
		go h.Federator.Federate(context.Background(), reject)
		slog.Info("follow rejected (auto-accept disabled)", "actor", activity.Actor)
		return nil
	}

	// Store the follow relationship.
	if err := h.Store.AddFollow(activity.Actor, followedID); err != nil {
		slog.Warn("failed to store follow", "error", err)
	}

	// Send Accept back to the follower.
	accept := BuildAccept(followObj, followedID, activity.Actor)

	// Use context.Background() for fire-and-forget goroutines: handleFollow returns nil
	// immediately, which causes the HTTP handler goroutine (and its 30s ctx) to exit,
	// cancelling the context before these goroutines can complete their work.
	go h.Federator.Federate(context.Background(), accept)

	// Notify local user of the new Fediverse follower via a DM to self.
	go h.sendFollowNotification(context.Background(), activity.Actor)

	return nil
}

func (h *APHandler) handleCreate(ctx context.Context, activity IncomingActivity) error {
	// Parse the embedded object.
	var objMap map[string]interface{}
	if err := json.Unmarshal(activity.Object, &objMap); err != nil {
		return fmt.Errorf("parse create object: %w", err)
	}

	objType, _ := objMap["type"].(string)

	vis := h.postVisibility(activity)

	switch objType {
	case "Note":
		note := mapToNote(objMap)

		// Direct messages (addressed specifically to the local actor) are
		// delivered as NIP-04 encrypted self-DMs so the user is notified
		// without broadcasting private content as a public Nostr event.
		if vis == "direct" {
			return h.bridgeDirectNote(ctx, note, activity.Actor)
		}
		// "followers" visibility is bridged identically to public: we received
		// it in our inbox so we are an authorised audience, and the user's
		// relay access controls limit further distribution. Drop anything that
		// isn't addressed to us at all (vis == "" shouldn't happen, but guard).
		if vis != "public" && vis != "followers" {
			return nil
		}

		// Synchronously fetch parent posts before converting so that reply and
		// quote e/q-tags can be resolved. Without this, a thread reply would
		// arrive before its parent is cached and the tag would be silently dropped.
		if note.InReplyTo != "" {
			if _, ok := h.resolveNostrID(note.InReplyTo); !ok {
				h.fetchAndCacheObject(ctx, note.InReplyTo)
			}
		}
		if note.QuoteURL != "" {
			if _, ok := h.resolveNostrID(note.QuoteURL); !ok {
				h.fetchAndCacheObject(ctx, note.QuoteURL)
			}
		}

		event, err := h.noteToEvent(ctx, note)
		if err != nil {
			return fmt.Errorf("convert note to event: %w", err)
		}
		if event == nil {
			return nil
		}

		// Store the ID mapping.
		if err := h.Store.AddObject(note.ID, event.ID); err != nil {
			slog.Warn("failed to store object mapping", "error", err)
		}

		return h.Publisher.Publish(ctx, event)

	case "Article", "Page":
		if vis == "direct" || (vis != "public" && vis != "followers") {
			return nil
		}
		note := mapToNote(objMap)
		event, err := h.articleToEvent(note)
		if err != nil {
			return fmt.Errorf("convert article to event: %w", err)
		}
		if err := h.Store.AddObject(note.ID, event.ID); err != nil {
			slog.Warn("failed to store article mapping", "error", err)
		}
		return h.Publisher.Publish(ctx, event)

	case "Question":
		if vis == "direct" || (vis != "public" && vis != "followers") {
			return nil
		}
		note := mapToNote(objMap)
		event, err := h.questionToEvent(note)
		if err != nil {
			return fmt.Errorf("convert question to event: %w", err)
		}
		if err := h.Store.AddObject(note.ID, event.ID); err != nil {
			slog.Warn("failed to store question mapping", "error", err)
		}
		return h.Publisher.Publish(ctx, event)

	default:
		return nil
	}
}

// bridgeDirectNote converts an AP Note that was directly addressed to the local
// actor (a DM) into a NIP-04 encrypted self-DM so the user is notified without
// the content being published as a public Nostr event.
func (h *APHandler) bridgeDirectNote(ctx context.Context, note *Note, actorURL string) error {
	// Best-effort: resolve actor handle for a human-readable prefix.
	handle := actorURL
	if actor, err := FetchActor(ctx, actorURL); err == nil && actor != nil && actor.PreferredUsername != "" {
		handle = "@" + actor.PreferredUsername + "@" + bridge.ExtractHost(actorURL)
	}

	content := htmlToText(note.Content)
	msg := fmt.Sprintf("💬 Direct message from %s:\n\n%s", handle, content)
	if note.URL != "" && !strings.Contains(content, note.URL) {
		msg += "\n\n" + note.URL
	}

	event, err := h.Signer.CreateDMToSelf(msg)
	if err != nil {
		return fmt.Errorf("bridge direct note: create DM: %w", err)
	}
	return h.Publisher.Publish(ctx, event)
}

func (h *APHandler) handleAnnounce(ctx context.Context, activity IncomingActivity) error {
	if !isPublic(activity) {
		return nil
	}

	var objectID string
	if err := json.Unmarshal(activity.Object, &objectID); err != nil {
		// Object might be embedded.
		var objMap map[string]interface{}
		if err2 := json.Unmarshal(activity.Object, &objMap); err2 != nil {
			return fmt.Errorf("parse announce object: %w", err)
		}
		objectID, _ = objMap["id"].(string)
	}

	// Synchronously fetch the announced object so we can reference its Nostr ID.
	// Without this, the async goroutine would race against GetNostrIDForObject
	// and the repost would always be silently dropped.
	if _, ok := h.Store.GetNostrIDForObject(objectID); !ok {
		h.fetchAndCacheObject(ctx, objectID)
	}

	// Create a Nostr kind-6 repost event.
	nostrID, ok := h.Store.GetNostrIDForObject(objectID)
	if !ok {
		slog.Debug("announce: cannot resolve Nostr ID for announced object", "object", objectID)
		return nil
	}

	event := &nostr.Event{
		Kind:      6,
		Content:   "",
		CreatedAt: parseNostrTimestamp(activity.Published),
		Tags: nostr.Tags{
			{"e", nostrID, h.NostrRelay},
			{"proxy", activity.ID, "activitypub"},
		},
	}

	if err := h.signEvent(event, activity.Actor); err != nil {
		return fmt.Errorf("sign event: %w", err)
	}

	return h.Publisher.Publish(ctx, event)
}

func (h *APHandler) handleUpdate(ctx context.Context, activity IncomingActivity) error {
	var objMap map[string]interface{}
	if err := json.Unmarshal(activity.Object, &objMap); err != nil {
		return fmt.Errorf("parse update object: %w", err)
	}

	// Article/Page updates: re-publish as a new kind-30023 (addressable event,
	// same d-tag → naturally replaces the previous version on relays).
	objType, _ := objMap["type"].(string)
	if objType == "Article" || objType == "Page" {
		note := mapToNote(objMap)
		InvalidateCache(note.ID)
		event, err := h.articleToEvent(note)
		if err != nil {
			return fmt.Errorf("convert article update to event: %w", err)
		}
		if err := h.Store.AddObject(note.ID, event.ID); err != nil {
			slog.Warn("failed to store article mapping", "error", err)
		}
		return h.Publisher.Publish(ctx, event)
	}

	if !IsActor(objMap) {
		return nil // Only handle actor updates
	}

	actor := mapToActor(objMap)

	// Invalidate cache so we get fresh data next time.
	InvalidateCache(actor.ID)

	// Create Nostr kind-0 metadata event.
	meta := buildMetadataContent(actor, h.LocalDomain)
	event := &nostr.Event{
		Kind:      0,
		Content:   meta,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"proxy", actor.ID, "activitypub"},
		},
	}

	if err := h.signEvent(event, actor.ID); err != nil {
		return fmt.Errorf("sign metadata event: %w", err)
	}

	// Also publish relay list.
	relayEvent := &nostr.Event{
		Kind:      10002,
		Content:   "",
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"r", h.NostrRelay},
			{"proxy", actor.ID, "activitypub"},
		},
	}
	if err := h.signEvent(relayEvent, actor.ID); err == nil {
		go h.Publisher.Publish(context.Background(), relayEvent)
	}

	return h.Publisher.Publish(ctx, event)
}

func (h *APHandler) handleLike(ctx context.Context, activity IncomingActivity) error {
	if !isPublic(activity) {
		return nil
	}

	// Idempotency: skip if this Like activity was already processed (e.g. retry).
	if _, ok := h.Store.GetNostrIDForObject(activity.ID); ok {
		return nil
	}

	var objectID string
	if err := json.Unmarshal(activity.Object, &objectID); err != nil {
		return fmt.Errorf("parse like object: %w", err)
	}

	// Synchronously fetch the liked object if not yet cached so the Nostr ID
	// lookup succeeds even when the Like arrives before the target post.
	if _, ok := h.Store.GetNostrIDForObject(objectID); !ok {
		h.fetchAndCacheObject(ctx, objectID)
	}

	nostrID, ok := h.Store.GetNostrIDForObject(objectID)
	if !ok {
		slog.Debug("like: cannot resolve Nostr ID for liked object", "object", objectID)
		return nil
	}

	event := &nostr.Event{
		Kind:      7,
		Content:   "+",
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", nostrID},
			{"proxy", activity.ID, "activitypub"},
		},
	}

	if err := h.signEvent(event, activity.Actor); err != nil {
		return err
	}
	if err := h.Publisher.Publish(ctx, event); err != nil {
		return err
	}
	if err := h.Store.AddObject(activity.ID, event.ID); err != nil {
		slog.Warn("like: failed to store activity mapping", "error", err)
	}
	return nil
}

func (h *APHandler) handleEmojiReact(ctx context.Context, activity IncomingActivity) error {
	if !isPublic(activity) {
		return nil
	}

	// Idempotency: skip if this EmojiReact activity was already processed.
	if _, ok := h.Store.GetNostrIDForObject(activity.ID); ok {
		return nil
	}

	var objectID string
	if err := json.Unmarshal(activity.Object, &objectID); err != nil {
		return fmt.Errorf("parse emoji react object: %w", err)
	}

	// Synchronously fetch the target object if not yet cached, same as handleLike.
	if _, ok := h.Store.GetNostrIDForObject(objectID); !ok {
		h.fetchAndCacheObject(ctx, objectID)
	}

	nostrID, ok := h.Store.GetNostrIDForObject(objectID)
	if !ok {
		slog.Debug("emoji react: cannot resolve Nostr ID for target object", "object", objectID)
		return nil
	}

	event := &nostr.Event{
		Kind:      7,
		Content:   activity.Content,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", nostrID},
			{"proxy", activity.ID, "activitypub"},
		},
	}

	if err := h.signEvent(event, activity.Actor); err != nil {
		return err
	}
	if err := h.Publisher.Publish(ctx, event); err != nil {
		return err
	}
	if err := h.Store.AddObject(activity.ID, event.ID); err != nil {
		slog.Warn("emoji react: failed to store activity mapping", "error", err)
	}
	return nil
}

func (h *APHandler) handleDelete(ctx context.Context, activity IncomingActivity) error {
	var objectID string
	if err := json.Unmarshal(activity.Object, &objectID); err != nil {
		// Object may be a Tombstone: {"type": "Tombstone", "id": "https://..."}
		var objMap map[string]interface{}
		if err2 := json.Unmarshal(activity.Object, &objMap); err2 != nil {
			return fmt.Errorf("parse delete object: %w", err)
		}
		id, _ := objMap["id"].(string)
		if id == "" {
			return fmt.Errorf("parse delete object: no id in tombstone")
		}
		objectID = id
	}

	nostrID, ok := h.Store.GetNostrIDForObject(objectID)
	if !ok {
		return nil
	}

	// Evict the mapping before publishing so subsequent idempotency checks
	// for the same AP ID don't find a stale cache hit.
	if err := h.Store.DeleteObject(objectID, nostrID); err != nil {
		slog.Warn("handleDelete: failed to remove object mapping", "apID", objectID, "error", err)
	}

	event := &nostr.Event{
		Kind:      5,
		Content:   "",
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"e", nostrID},
			{"proxy", activity.ID, "activitypub"},
		},
	}

	if err := h.signEvent(event, activity.Actor); err != nil {
		return err
	}
	return h.Publisher.Publish(ctx, event)
}

func (h *APHandler) handleUndo(ctx context.Context, activity IncomingActivity) error {
	// Parse the undone activity.
	var inner IncomingActivity
	if err := json.Unmarshal(activity.Object, &inner); err != nil {
		return nil // Ignore malformed undos
	}

	if inner.Type == "Follow" {
		var followedID string
		if err := json.Unmarshal(inner.Object, &followedID); err != nil {
			return nil
		}
		slog.Debug("handling unfollow", "actor", activity.Actor, "followed", followedID)
		if err := h.Store.RemoveFollow(activity.Actor, followedID); err != nil {
			slog.Warn("failed to remove follow", "error", err)
		}
	}

	return nil
}

// ─── Helper methods ───────────────────────────────────────────────────────────

// signEvent signs a Nostr event. If the actorID matches the local AP actor,
// it signs with the user's real key; otherwise it uses a derived key.
func (h *APHandler) signEvent(event *nostr.Event, actorID string) error {
	if actorID == h.LocalActorURL {
		return h.Signer.SignAsUser(event)
	}
	return h.Signer.Sign(event, actorID)
}

func (h *APHandler) noteToEvent(ctx context.Context, note *Note) (*nostr.Event, error) {
	// Convert AP Note content (HTML) to plain text.
	content := htmlToText(note.Content)

	// Extract mentions: collect pubkeys for p-tags and actor URLs for href filtering.
	mentionHrefs := make(map[string]bool)
	var mentionPubkeys []string
	for _, tag := range note.Tag {
		m, ok := tag.(map[string]interface{})
		if !ok {
			continue
		}
		if tagType, _ := m["type"].(string); tagType == "Mention" {
			href, _ := m["href"].(string)
			if href != "" {
				mentionHrefs[href] = true
			}
			if nostrPubkey, err := h.Signer.PublicKey(href); err == nil {
				mentionPubkeys = append(mentionPubkeys, nostrPubkey)
			}
		}
	}

	// Append any <a href> URLs that are hidden behind anchor text and not yet
	// visible in the stripped plaintext. Skip mention actor URLs and Mastodon-style
	// hashtag search paths (/tags/ or /tag/) which would pollute the content.
	seen := make(map[string]bool)
	for _, href := range extractHrefsFromHTML(note.Content) {
		if seen[href] || mentionHrefs[href] {
			continue
		}
		if strings.Contains(href, "/tags/") || strings.Contains(href, "/tag/") {
			continue
		}
		if !strings.Contains(content, href) {
			content += "\n\n" + href
			seen[href] = true
		}
	}

	// Resolve reply threading and the NIP-10 thread root.
	var replyToEventID, rootEventID string
	if note.InReplyTo != "" {
		if id, ok := h.resolveNostrID(note.InReplyTo); ok {
			replyToEventID = id

			// Determine the thread root for the NIP-10 "root" marker.
			// The parent AP object is almost always already in the cache because
			// handleCreate pre-fetches it just before calling noteToEvent, so
			// this is typically a zero-latency cache hit.
			if parentObj, err := FetchObject(ctx, note.InReplyTo); err == nil {
				parentNote := mapToNote(parentObj)
				if parentNote != nil && parentNote.InReplyTo != "" {
					// Parent is itself a reply; resolve its InReplyTo as our root.
					if rootID, ok2 := h.resolveNostrID(parentNote.InReplyTo); ok2 {
						rootEventID = rootID
					}
				}
			}
			// If root is still empty (parent is the root, or grandparent is
			// not in our DB), set root = direct parent so BuildKind1Event
			// emits a single "reply" e-tag instead of a bare positional tag.
			if rootEventID == "" {
				rootEventID = replyToEventID
			}
		} else {
			// Parent is unresolvable even after the pre-fetch in handleCreate.
			// Drop the reply rather than publishing it without thread context,
			// which would make it appear as a top-level post in the main feed.
			slog.Debug("dropping reply: parent not resolvable", "inReplyTo", note.InReplyTo, "note", note.ID)
			return nil, nil
		}
	}

	// Resolve quote reference.
	var quoteEventID string
	if note.QuoteURL != "" {
		if id, ok := h.resolveNostrID(note.QuoteURL); ok {
			quoteEventID = id
		}
		// If unresolvable, the quoted URL is typically already visible in the content.
	}

	// Resolve nostr: URI references embedded in the content text.
	// npub/nprofile → p-tags (mentions); note/nevent → q-tag (first one wins).
	{
		seenPubkeys := make(map[string]bool)
		for _, pk := range mentionPubkeys {
			seenPubkeys[pk] = true
		}
		for _, match := range mentionRe.FindAllString(content, -1) {
			bech32 := match[6:] // strip "nostr:" prefix
			prefix, val, err := nip19.Decode(bech32)
			if err != nil {
				continue
			}
			switch prefix {
			case "npub":
				pk, _ := val.(string)
				if pk != "" && !seenPubkeys[pk] {
					mentionPubkeys = append(mentionPubkeys, pk)
					seenPubkeys[pk] = true
				}
			case "nprofile":
				pp, _ := val.(nostr.ProfilePointer)
				if pp.PublicKey != "" && !seenPubkeys[pp.PublicKey] {
					mentionPubkeys = append(mentionPubkeys, pp.PublicKey)
					seenPubkeys[pp.PublicKey] = true
				}
			case "note":
				eventID, _ := val.(string)
				if eventID != "" && quoteEventID == "" {
					quoteEventID = eventID
				}
			case "nevent":
				ep, _ := val.(nostr.EventPointer)
				if ep.ID != "" && quoteEventID == "" {
					quoteEventID = ep.ID
				}
			}
		}
	}

	// Extract hashtags.
	var hashtags []string
	for _, tag := range note.Tag {
		m, ok := tag.(map[string]interface{})
		if !ok {
			continue
		}
		if tagType, _ := m["type"].(string); tagType == "Hashtag" {
			name, _ := m["name"].(string)
			name = strings.TrimPrefix(name, "#")
			if name != "" {
				hashtags = append(hashtags, name)
			}
		}
	}

	// Content warning.
	var contentWarning string
	if note.Sensitive && note.Summary != "" {
		contentWarning = note.Summary
	}

	// Media attachments: images → ImageInfo for imeta tags; link cards → append
	// URL to content (imeta is for actual media, not HTML link previews).
	var images []bridge.ImageInfo
	for _, att := range note.Attachment {
		if att.URL == "" {
			continue
		}
		if att.Type == "Link" || strings.HasPrefix(att.MediaType, "text/") {
			if !strings.Contains(content, att.URL) {
				content += "\n\n" + att.URL
			}
			continue
		}
		images = append(images, bridge.ImageInfo{
			URL:      att.URL,
			Alt:      att.Name, // AP "name" field is the alt text / description
			MimeType: att.MediaType,
			Blurhash: att.Blurhash,
			Width:    att.Width,
			Height:   att.Height,
		})
	}

	// Source URL for attribution (note.URL is the canonical web URL of the post).
	sourceURL := note.URL
	if sourceURL == "" {
		sourceURL = note.ID
	}

	// NIP-40: map AP endTime to an expiration timestamp.
	var expiresAt int64
	if note.EndTime != "" {
		if t, err := time.Parse(time.RFC3339, note.EndTime); err == nil && t.Unix() > 0 {
			expiresAt = t.Unix()
		}
	}

	np := bridge.NormalizedPost{
		Content:        content,
		CreatedAt:      parseNostrTimestamp(note.Published),
		Images:         images,
		ReplyToEventID: replyToEventID,
		RootEventID:    rootEventID,
		RelayHint:      h.NostrRelay,
		MentionPubkeys: mentionPubkeys,
		QuoteEventID:   quoteEventID,
		Hashtags:       hashtags,
		ContentWarning: contentWarning,
		SourceURL:      sourceURL,
		ShowSourceLink: h.ShowSourceLink.Load(),
		ExpiresAt:      expiresAt,
		ProxyID:        note.ID,
		ProxyProtocol:  "activitypub",
	}
	event := bridge.BuildKind1Event(np)

	if err := h.signEvent(event, note.AttributedTo); err != nil {
		return nil, fmt.Errorf("sign event: %w", err)
	}
	return event, nil
}

// resolveNostrID returns the Nostr event ID for an AP object URL.
// For local objects (https://domain/objects/<nostr-id>) the ID is extracted
// directly from the URL — no DB lookup needed, and crucially this works even
// for events that were never explicitly stored (e.g. outbound Nostr posts).
// For remote objects it falls back to the DB.
func (h *APHandler) resolveNostrID(apObjectID string) (string, bool) {
	localPrefix := strings.TrimRight(h.LocalDomain, "/") + "/objects/"
	if strings.HasPrefix(apObjectID, localPrefix) {
		return strings.TrimPrefix(apObjectID, localPrefix), true
	}
	return h.Store.GetNostrIDForObject(apObjectID)
}

// questionToEvent converts an AP Question (poll) to a Nostr kind-1068 poll event (NIP-69).
func (h *APHandler) questionToEvent(note *Note) (*nostr.Event, error) {
	// Use HTML content as the question text; fall back to Name for servers that
	// put the question in the name field instead of content.
	content := htmlToText(note.Content)
	if content == "" {
		content = note.Name
	}

	tags := nostr.Tags{
		{"proxy", note.ID, "activitypub"},
	}

	// Determine single-choice (oneOf) vs. multi-choice (anyOf).
	options := note.OneOf
	pollType := "singlechoice"
	if len(options) == 0 {
		options = note.AnyOf
		pollType = "multiplechoice"
	}
	for i, opt := range options {
		tags = append(tags, nostr.Tag{"poll_option", fmt.Sprintf("%d", i), opt.Name})
	}

	// Close time from endTime or closed (closed = poll has already ended).
	closeTime := note.EndTime
	if closeTime == "" {
		closeTime = note.Closed
	}
	if closeTime != "" {
		if t, err := time.Parse(time.RFC3339, closeTime); err == nil {
			tags = append(tags, nostr.Tag{"closed_at", fmt.Sprintf("%d", t.Unix())})
		}
	}

	tags = append(tags, nostr.Tag{"poll_type", pollType})

	event := &nostr.Event{
		Kind:      1068,
		Content:   content,
		CreatedAt: nostr.Now(),
		Tags:      tags,
	}
	if note.Published != "" {
		if t, err := time.Parse(time.RFC3339, note.Published); err == nil {
			event.CreatedAt = nostr.Timestamp(t.Unix())
		}
	}

	if err := h.signEvent(event, note.AttributedTo); err != nil {
		return nil, fmt.Errorf("sign question event: %w", err)
	}
	return event, nil
}

// articleToEvent converts an AP Article or Page to a Nostr kind-30023 event.
// The AP object URL is used as the `d` tag identifier so that subsequent
// updates (via AP Update activity) replace the same addressable event on relays.
func (h *APHandler) articleToEvent(note *Note) (*nostr.Event, error) {
	content := htmlToText(note.Content)

	tags := nostr.Tags{
		{"proxy", note.ID, "activitypub"},
		// d-tag: stable identifier for this article (AP object URL).
		{"d", note.ID},
	}

	if note.Name != "" {
		tags = append(tags, nostr.Tag{"title", note.Name})
	}
	if note.Summary != "" {
		tags = append(tags, nostr.Tag{"summary", note.Summary})
	}

	// published_at: convert the AP RFC3339 timestamp to a Unix integer.
	if note.Published != "" {
		if t, err := time.Parse(time.RFC3339, note.Published); err == nil {
			tags = append(tags, nostr.Tag{"published_at", fmt.Sprintf("%d", t.Unix())})
		}
	}

	// Hashtags from AP tags array.
	for _, raw := range note.Tag {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if tagType, _ := m["type"].(string); tagType == "Hashtag" {
			name, _ := m["name"].(string)
			name = strings.TrimPrefix(name, "#")
			if name != "" {
				tags = append(tags, nostr.Tag{"t", name})
			}
		}
	}

	// Header image from the first image attachment.
	for _, att := range note.Attachment {
		if att.URL != "" && (att.Type == "Image" || strings.HasPrefix(att.MediaType, "image/")) {
			tags = append(tags, nostr.Tag{"image", att.URL})
			break
		}
	}

	event := &nostr.Event{
		Kind:      30023,
		Content:   content,
		CreatedAt: parseNostrTimestamp(note.Published),
		Tags:      tags,
	}

	if err := h.signEvent(event, note.AttributedTo); err != nil {
		return nil, fmt.Errorf("sign event: %w", err)
	}
	return event, nil
}

func (h *APHandler) fetchAndCacheActor(ctx context.Context, actorID string) {
	actor, err := FetchActor(ctx, actorID)
	if err != nil {
		return
	}

	// Publish metadata event to Nostr using derived key for remote actors.
	meta := buildMetadataContentFromActor(actor, h.LocalDomain)
	event := &nostr.Event{
		Kind:      0,
		Content:   meta,
		CreatedAt: nostr.Now(),
		Tags: nostr.Tags{
			{"proxy", actorID, "activitypub"},
		},
	}
	if err := h.Signer.Sign(event, actorID); err == nil {
		h.Publisher.Publish(ctx, event)
	}
}

func (h *APHandler) fetchAndCacheObject(ctx context.Context, objectID string) {
	if IsLocalID(objectID, h.LocalDomain) {
		return
	}
	obj, err := FetchObject(ctx, objectID)
	if err != nil {
		return
	}
	note := mapToNote(obj)
	if note == nil {
		return
	}
	// Fetch the original author's actor so their NIP-05 handle is published
	// as a kind-0 event. This matters for reposts (Announce) where the
	// booster's metadata is fetched via HandleActivity but the boosted post's
	// author would otherwise have no kind-0 with a verifiable handle.
	if note.AttributedTo != "" {
		go h.fetchAndCacheActor(context.Background(), note.AttributedTo)
	}
	event, err := h.noteToEvent(ctx, note)
	if err != nil || event == nil {
		return
	}
	if err := h.Store.AddObject(note.ID, event.ID); err == nil {
		h.Publisher.Publish(ctx, event)
	}
}

func (h *APHandler) handleAccept(ctx context.Context, activity IncomingActivity) error {
	followActor, followObject, err := parseFollowFromObject(activity.Object)
	if err != nil {
		return nil // Ignore malformed or string-only Accept objects
	}
	// Only care about accepts for follows *we* sent.
	if followActor != h.LocalActorURL {
		return nil
	}
	slog.Info("outbound follow accepted", "actor", activity.Actor, "followed", followObject)
	return nil
}

func (h *APHandler) handleReject(ctx context.Context, activity IncomingActivity) error {
	followActor, followObject, err := parseFollowFromObject(activity.Object)
	if err != nil {
		return nil // Ignore malformed or string-only Reject objects
	}
	// Only care about rejects for follows *we* sent.
	if followActor != h.LocalActorURL {
		return nil
	}
	slog.Info("outbound follow rejected", "actor", activity.Actor, "followed", followObject)

	// Remove the follow so the local DB reflects reality.
	if err := h.Store.RemoveFollow(h.LocalActorURL, followObject); err != nil {
		slog.Warn("reject: failed to remove follow", "error", err)
	}

	// Notify local user via NIP-04 DM.
	go h.sendRejectNotification(context.Background(), activity.Actor)
	return nil
}

// parseFollowFromObject extracts the Follow actor and object from an Accept/Reject
// payload. The inner object is typically an embedded Follow activity, but some
// servers send only the Follow activity ID (a string). In that case we return an
// error so the caller can skip gracefully rather than fetch the unknown activity.
// handleMove processes an AP Move activity, which signals that an actor has
// migrated to a new server. If klistr follows the old actor, it automatically
// updates the follow record, sends the AP handshake (Undo + Follow), and
// notifies the local user via a NIP-04 DM.
func (h *APHandler) handleMove(ctx context.Context, activity IncomingActivity) error {
	if len(activity.Target) == 0 {
		return nil // malformed Move: no target
	}

	// object = the old actor (who is moving); target = their new identity.
	var oldActorURL, newActorURL string
	if err := json.Unmarshal(activity.Object, &oldActorURL); err != nil {
		return nil
	}
	if err := json.Unmarshal(activity.Target, &newActorURL); err != nil {
		return nil
	}

	// Validate: the actor announcing the move must be the account being moved.
	if activity.Actor != oldActorURL {
		slog.Debug("move: actor does not match object, ignoring", "actor", activity.Actor, "object", oldActorURL)
		return nil
	}

	// Only act if we actually follow the old actor.
	following, err := h.Store.GetAPFollowing(h.LocalActorURL)
	if err != nil {
		return fmt.Errorf("move: get following list: %w", err)
	}
	followingOld := false
	for _, f := range following {
		if f == oldActorURL {
			followingOld = true
			break
		}
	}
	if !followingOld {
		slog.Debug("move: not following old actor, ignoring", "old", oldActorURL)
		return nil
	}

	slog.Info("AP actor migrated, updating follow", "old", oldActorURL, "new", newActorURL)

	// Update the follow record in the DB.
	if err := h.Store.RemoveFollow(h.LocalActorURL, oldActorURL); err != nil {
		slog.Warn("move: failed to remove old follow", "error", err)
	}
	if err := h.Store.AddFollow(h.LocalActorURL, newActorURL); err != nil {
		slog.Warn("move: failed to add new follow", "error", err)
	}

	// Store the derived key for the new actor so kind-3 can resolve it.
	if newPubkey, err := h.Signer.PublicKey(newActorURL); err == nil {
		if err := h.Store.StoreActorKey(newPubkey, newActorURL); err != nil {
			slog.Warn("move: failed to store new actor key", "error", err)
		}
	}

	// Send AP Undo Follow to old actor + Follow to new actor in the background.
	go func() {
		bgCtx := context.Background()
		h.Federator.Federate(bgCtx, BuildUndoFollow(h.LocalActorURL, oldActorURL))
		h.Federator.Federate(bgCtx, BuildFollow(h.LocalActorURL, newActorURL))
	}()

	// Notify local user.
	go h.sendMoveNotification(context.Background(), oldActorURL, newActorURL)
	return nil
}

// sendMoveNotification delivers a NIP-04 DM to the local user when a followed
// AP actor migrates to a new server.
func (h *APHandler) sendMoveNotification(ctx context.Context, oldActorURL, newActorURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resolve := func(actorURL string) string {
		actor, err := FetchActor(ctx, actorURL)
		if err != nil || actor == nil || actor.PreferredUsername == "" {
			return actorURL
		}
		if domain := bridge.ExtractHost(actorURL); domain != "" {
			return "@" + actor.PreferredUsername + "@" + domain
		}
		return actorURL
	}

	message := "📦 Followed account moved: " + resolve(oldActorURL) + " → " + resolve(newActorURL)

	event, err := h.Signer.CreateDMToSelf(message)
	if err != nil {
		slog.Warn("failed to create move notification DM", "error", err)
		return
	}
	if err := h.Publisher.Publish(ctx, event); err != nil {
		slog.Warn("failed to publish move notification DM", "error", err)
	}
}

func parseFollowFromObject(raw json.RawMessage) (followActor, followObject string, err error) {
	var inner IncomingActivity
	if err = json.Unmarshal(raw, &inner); err != nil || inner.Type != "Follow" {
		return "", "", fmt.Errorf("object is not an embedded Follow activity")
	}
	if err = json.Unmarshal(inner.Object, &followObject); err != nil {
		return "", "", fmt.Errorf("parse follow object: %w", err)
	}
	return inner.Actor, followObject, nil
}

// sendRejectNotification delivers a NIP-04 DM to the local user when a
// remote actor rejects an outbound follow request.
func (h *APHandler) sendRejectNotification(ctx context.Context, actorURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	handle := actorURL
	if actor, err := FetchActor(ctx, actorURL); err == nil && actor != nil {
		if actor.PreferredUsername != "" {
			if domain := bridge.ExtractHost(actorURL); domain != "" {
				handle = "@" + actor.PreferredUsername + "@" + domain
			}
		}
	}

	message := "🚫 Follow rejected by " + handle

	event, err := h.Signer.CreateDMToSelf(message)
	if err != nil {
		slog.Warn("failed to create reject notification DM", "error", err)
		return
	}
	if err := h.Publisher.Publish(ctx, event); err != nil {
		slog.Warn("failed to publish reject notification DM", "error", err)
	}
}

// sendFollowNotification delivers a NIP-04 DM to the local user when a
// Fediverse account follows them.
func (h *APHandler) sendFollowNotification(ctx context.Context, followerActorURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Try to get a human-readable handle for the follower.
	handle := followerActorURL
	if actor, err := FetchActor(ctx, followerActorURL); err == nil && actor != nil {
		if actor.PreferredUsername != "" {
			if domain := bridge.ExtractHost(followerActorURL); domain != "" {
				handle = "@" + actor.PreferredUsername + "@" + domain
			}
		}
	}

	message := "🔔 New Fediverse follower: " + handle

	event, err := h.Signer.CreateDMToSelf(message)
	if err != nil {
		slog.Warn("failed to create follow notification DM", "error", err)
		return
	}

	if err := h.Publisher.Publish(ctx, event); err != nil {
		slog.Warn("failed to publish follow notification DM", "error", err)
	}
}

// ─── Pure helpers ─────────────────────────────────────────────────────────────

// postVisibility classifies the audience of an incoming activity:
//   - "public"    — addressed to the ActivityStreams public URI
//   - "direct"    — addressed directly to the local actor (DM), but not public
//   - "followers" — everything else (followers-only, unlisted); we received
//     it in our inbox so we are a valid audience
func (h *APHandler) postVisibility(activity IncomingActivity) string {
	for _, r := range activity.To {
		if r == PublicURI {
			return "public"
		}
	}
	for _, r := range activity.CC {
		if r == PublicURI {
			return "public"
		}
	}
	for _, r := range activity.To {
		if r == h.LocalActorURL {
			return "direct"
		}
	}
	for _, r := range activity.CC {
		if r == h.LocalActorURL {
			return "direct"
		}
	}
	return "followers"
}

func isPublic(activity IncomingActivity) bool {
	for _, r := range activity.To {
		if r == PublicURI {
			return true
		}
	}
	for _, r := range activity.CC {
		if r == PublicURI {
			return true
		}
	}
	return false
}

func parseNostrTimestamp(s string) nostr.Timestamp {
	if s == "" {
		return nostr.Now()
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nostr.Now()
	}
	return nostr.Timestamp(t.Unix())
}

// htmlToText converts an ActivityPub HTML content field to plain text suitable
// for a Nostr event body. It uses the standard HTML tokenizer so that all
// entity references — named (&amp;), decimal (&#60;), and hexadecimal (&#x3C;)
// — are decoded correctly. <script> and <style> content is discarded entirely.
func htmlToText(h string) string {
	z := html.NewTokenizer(strings.NewReader(h))
	var sb strings.Builder
	skipContent := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.TextToken:
			if !skipContent {
				// z.Raw() returns the raw bytes of the text token;
				// html.UnescapeString decodes every entity reference.
				sb.WriteString(html.UnescapeString(string(z.Raw())))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style":
				skipContent = true
			case "p", "div", "blockquote", "li":
				sb.WriteString("\n\n")
			case "br":
				sb.WriteString("\n")
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch string(name) {
			case "script", "style":
				skipContent = false
			case "p", "div", "blockquote", "li":
				sb.WriteString("\n\n")
			}
		}
	}
	text := sb.String()
	// Collapse runs of blank lines left by adjacent block elements.
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}

func buildMetadataContent(actor *Actor, localDomain string) string {
	// Prefer the human-readable URL (e.g. https://mastodon.social/@alice) over
	// the AP actor ID URL so Nostr clients can link back to the original profile.
	profileURL := actor.URL
	if profileURL == "" {
		profileURL = actor.ID
	}

	about := htmlToText(actor.Summary)
	if profileURL != "" {
		if about != "" {
			about += "\n\n"
		}
		about += profileURL
	}

	meta := struct {
		Name    string `json:"name"`
		About   string `json:"about"`
		Picture string `json:"picture,omitempty"`
		Banner  string `json:"banner,omitempty"`
		Website string `json:"website,omitempty"`
		NIP05   string `json:"nip05,omitempty"`
	}{
		Name:    actor.Name,
		About:   about,
		Website: profileURL,
	}
	if actor.Icon != nil {
		meta.Picture = actor.Icon.URL
	}
	if actor.Image != nil {
		meta.Banner = actor.Image.URL
	}

	// NIP-05: build a verifiable bridge identifier so Nostr clients show
	// "username_at_domain@localdomain" with a ✓ badge instead of a raw npub.
	if localDomain != "" && actor.PreferredUsername != "" {
		actorHost := bridge.ExtractHost(actor.ID)
		localHost := bridge.ExtractHost(localDomain)
		if actorHost != "" && localHost != "" {
			meta.NIP05 = actor.PreferredUsername + "_at_" + actorHost + "@" + localHost
		}
	}

	b, err := json.Marshal(meta)
	if err != nil {
		return `{"name":"","about":""}`
	}
	return string(b)
}

func buildMetadataContentFromActor(actor *Actor, localDomain string) string {
	return buildMetadataContent(actor, localDomain)
}

// anchorHrefRe matches the href attribute value inside an <a> tag,
// restricted to http/https URLs (skips mailto:, javascript:, etc.).
var anchorHrefRe = regexp.MustCompile(`(?i)<a\s[^>]*\bhref\s*=\s*["'](https?://[^"']+)["']`)

// extractHrefsFromHTML returns all http/https hrefs from <a> tags in the HTML.
func extractHrefsFromHTML(html string) []string {
	matches := anchorHrefRe.FindAllStringSubmatch(html, -1)
	var hrefs []string
	for _, m := range matches {
		if len(m) >= 2 {
			hrefs = append(hrefs, m[1])
		}
	}
	return hrefs
}
