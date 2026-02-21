package ap

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
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
	}
	Publisher interface {
		Publish(ctx context.Context, event *nostr.Event) error
	}
	Store interface {
		AddObject(apID, nostrID string) error
		AddFollow(followerID, followedID string) error
		GetNostrIDForObject(apID string) (string, bool)
	}
	Federator  *Federator
	NostrRelay string
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
	if activity.Type != "Update" {
		go h.fetchAndCacheActor(ctx, activity.Actor)
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

	// Store the follow relationship.
	if err := h.Store.AddFollow(activity.Actor, followedID); err != nil {
		slog.Warn("failed to store follow", "error", err)
	}

	// Publish updated contact list to Nostr.
	go h.publishFollows(ctx, activity.Actor)

	// Send Accept back to the follower.
	accept := BuildAccept(map[string]interface{}{
		"id":     activity.ID,
		"type":   "Follow",
		"actor":  activity.Actor,
		"object": followedID,
	}, followedID)

	go h.Federator.Federate(ctx, accept)
	return nil
}

func (h *APHandler) handleCreate(ctx context.Context, activity IncomingActivity) error {
	// Parse the embedded object.
	var objMap map[string]interface{}
	if err := json.Unmarshal(activity.Object, &objMap); err != nil {
		return fmt.Errorf("parse create object: %w", err)
	}

	objType, _ := objMap["type"].(string)
	if objType != "Note" {
		return nil // Only handle notes
	}

	note := mapToNote(objMap)
	if !isPublic(activity) {
		return nil // Only handle public notes
	}

	// Prefetch referenced objects.
	if note.QuoteURL != "" {
		go h.fetchAndCacheObject(ctx, note.QuoteURL)
	}
	if note.InReplyTo != "" {
		go h.fetchAndCacheObject(ctx, note.InReplyTo)
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

	go h.fetchAndCacheObject(ctx, objectID)

	// Create a Nostr kind-6 repost event.
	nostrID, ok := h.Store.GetNostrIDForObject(objectID)
	if !ok {
		return nil // Can't repost without knowing the Nostr ID
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

	if !IsActor(objMap) {
		return nil // Only handle actor updates
	}

	actor := mapToActor(objMap)

	// Invalidate cache so we get fresh data next time.
	InvalidateCache(actor.ID)

	// Create Nostr kind-0 metadata event.
	meta := buildMetadataContent(actor)
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
		go h.Publisher.Publish(ctx, relayEvent)
	}

	return h.Publisher.Publish(ctx, event)
}

func (h *APHandler) handleLike(ctx context.Context, activity IncomingActivity) error {
	if !isPublic(activity) {
		return nil
	}

	var objectID string
	if err := json.Unmarshal(activity.Object, &objectID); err != nil {
		return fmt.Errorf("parse like object: %w", err)
	}

	nostrID, ok := h.Store.GetNostrIDForObject(objectID)
	if !ok {
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
	return h.Publisher.Publish(ctx, event)
}

func (h *APHandler) handleEmojiReact(ctx context.Context, activity IncomingActivity) error {
	if !isPublic(activity) {
		return nil
	}

	var objectID string
	if err := json.Unmarshal(activity.Object, &objectID); err != nil {
		return fmt.Errorf("parse emoji react object: %w", err)
	}

	nostrID, ok := h.Store.GetNostrIDForObject(objectID)
	if !ok {
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
	return h.Publisher.Publish(ctx, event)
}

func (h *APHandler) handleDelete(ctx context.Context, activity IncomingActivity) error {
	var objectID string
	if err := json.Unmarshal(activity.Object, &objectID); err != nil {
		return fmt.Errorf("parse delete object: %w", err)
	}

	nostrID, ok := h.Store.GetNostrIDForObject(objectID)
	if !ok {
		return nil
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
		// TODO: Remove follow from store
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

	event := &nostr.Event{
		Kind:      1,
		Content:   content,
		CreatedAt: parseNostrTimestamp(note.Published),
		Tags:      nostr.Tags{},
	}

	// Add p-tags for mentions.
	for _, tag := range note.Tag {
		m, ok := tag.(map[string]interface{})
		if !ok {
			continue
		}
		tagType, _ := m["type"].(string)
		if tagType == "Mention" {
			href, _ := m["href"].(string)
			nostrPubkey, err := h.Signer.PublicKey(href)
			if err == nil {
				event.Tags = append(event.Tags, nostr.Tag{"p", nostrPubkey, h.NostrRelay})
			}
		}
	}

	// Add reply tag.
	if note.InReplyTo != "" {
		replyNostrID, ok := h.Store.GetNostrIDForObject(note.InReplyTo)
		if !ok {
			// Can't resolve parent; skip this note.
			return nil, nil
		}
		event.Tags = append(event.Tags, nostr.Tag{"e", replyNostrID, h.NostrRelay, "reply"})
	}

	// Add quote tag.
	if note.QuoteURL != "" {
		quoteNostrID, ok := h.Store.GetNostrIDForObject(note.QuoteURL)
		if !ok {
			return nil, nil
		}
		event.Tags = append(event.Tags, nostr.Tag{"q", quoteNostrID, h.NostrRelay})
	}

	// Add hashtags.
	for _, tag := range note.Tag {
		m, ok := tag.(map[string]interface{})
		if !ok {
			continue
		}
		if tagType, _ := m["type"].(string); tagType == "Hashtag" {
			name, _ := m["name"].(string)
			name = strings.TrimPrefix(name, "#")
			if name != "" {
				event.Tags = append(event.Tags, nostr.Tag{"t", name})
			}
		}
	}

	// Content warning.
	if note.Sensitive && note.Summary != "" {
		event.Tags = append(event.Tags, nostr.Tag{"content-warning", note.Summary})
	}

	// Media attachments.
	for _, att := range note.Attachment {
		imeta := []string{"imeta", "url " + att.URL}
		if att.MediaType != "" {
			imeta = append(imeta, "m "+att.MediaType)
		}
		if att.Width > 0 && att.Height > 0 {
			imeta = append(imeta, fmt.Sprintf("dim %dx%d", att.Width, att.Height))
		}
		if att.Blurhash != "" {
			imeta = append(imeta, "blurhash "+att.Blurhash)
		}
		event.Tags = append(event.Tags, nostr.Tag(imeta))
		content += "\n\n" + att.URL
	}

	event.Content = content
	event.Tags = append(event.Tags, nostr.Tag{"proxy", note.ID, "activitypub"})

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
	meta := buildMetadataContentFromActor(actor)
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
	event, err := h.noteToEvent(ctx, note)
	if err != nil || event == nil {
		return
	}
	if err := h.Store.AddObject(note.ID, event.ID); err == nil {
		h.Publisher.Publish(ctx, event)
	}
}

func (h *APHandler) publishFollows(ctx context.Context, actorID string) {
	// TODO: Implement full contact list publishing.
	slog.Debug("publishing follows", "actor", actorID)
}

// ─── Pure helpers ─────────────────────────────────────────────────────────────

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

func htmlToText(h string) string {
	// Strip HTML tags and decode entities.
	var result strings.Builder
	inTag := false
	for _, c := range h {
		switch {
		case c == '<':
			inTag = true
		case c == '>':
			inTag = false
		case !inTag:
			result.WriteRune(c)
		}
	}
	// Decode common HTML entities.
	text := result.String()
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", `"`)
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	// Normalize multiple newlines.
	for strings.Contains(text, "\n\n\n") {
		text = strings.ReplaceAll(text, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(text)
}

func buildMetadataContent(actor *Actor) string {
	// Build JSON metadata from AP actor.
	parts := []string{
		fmt.Sprintf(`"name":"%s"`, jsonEscape(actor.Name)),
		fmt.Sprintf(`"about":"%s"`, jsonEscape(htmlToText(actor.Summary))),
	}
	if actor.Icon != nil {
		parts = append(parts, fmt.Sprintf(`"picture":"%s"`, jsonEscape(actor.Icon.URL)))
	}
	if actor.Image != nil {
		parts = append(parts, fmt.Sprintf(`"banner":"%s"`, jsonEscape(actor.Image.URL)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func buildMetadataContentFromActor(actor *Actor) string {
	return buildMetadataContent(actor)
}


func jsonEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", `\r`)
	s = strings.ReplaceAll(s, "\t", `\t`)
	return s
}
