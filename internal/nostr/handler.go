package nostr

import (
	"context"
	"log/slog"

	"github.com/nbd-wtf/go-nostr"
	"github.com/klppl/klistr/internal/ap"
)

// FollowStore is the subset of db.Store used by the kind-3 handler.
type FollowStore interface {
	AddFollow(followerID, followedID string) error
	RemoveFollow(followerID, followedID string) error
	// GetAPFollowing returns only ActivityPub follows (http-prefixed URLs),
	// excluding Bluesky entries so they don't trigger spurious AP Undo Follow activities.
	GetAPFollowing(followerID string) ([]string, error)
	GetActorForKey(pubkey string) (string, bool)
}

// BskyPoster is the interface for the optional Bluesky outbound bridge.
type BskyPoster interface {
	Handle(ctx context.Context, event *nostr.Event)
}

// Handler processes incoming Nostr events from the relay subscription
// and federates them to ActivityPub servers.
type Handler struct {
	TC        *ap.TransmuteContext
	Federator *ap.Federator
	// Store enables kind-3 AP follow bridging (optional).
	Store FollowStore
	// BskyPoster mirrors events to Bluesky when non-nil.
	BskyPoster BskyPoster
}

// Handle processes a single Nostr event.
func (h *Handler) Handle(ctx context.Context, event *nostr.Event) {
	if !nostr.IsValidPublicKey(event.PubKey) {
		return
	}

	// Verify event signature.
	if ok, err := event.CheckSignature(); !ok || err != nil {
		slog.Debug("invalid event signature", "id", event.ID)
		return
	}

	if !h.isEligible(event) {
		return
	}

	slog.Debug("handling nostr event", "id", event.ID, "kind", event.Kind, "pubkey", event.PubKey[:8])

	switch event.Kind {
	case 0:
		h.handleKind0(ctx, event)
	case 1:
		h.handleKind1(ctx, event)
	case 3:
		h.handleKind3(ctx, event)
	case 5:
		h.handleKind5(ctx, event)
	case 6:
		h.handleKind6(ctx, event)
	case 7:
		h.handleKind7(ctx, event)
	case 9735:
		h.handleKind9735(ctx, event)
	}

	// Mirror to Bluesky if bridge is configured.
	if h.BskyPoster != nil {
		go func() {
			defer func() { recover() }()
			h.BskyPoster.Handle(ctx, event)
		}()
	}
}

// ─── Event handlers ───────────────────────────────────────────────────────────

func (h *Handler) handleKind0(ctx context.Context, event *nostr.Event) {
	actor := ap.ToActor(event, h.TC)
	activity := ap.BuildUpdate(actor)
	h.Federator.Federate(ctx, activity)
}

func (h *Handler) handleKind1(ctx context.Context, event *nostr.Event) {
	if ap.IsRepost(event) {
		activity := ap.ToAnnounce(event, h.TC)
		if activity != nil {
			h.Federator.Federate(ctx, ap.ActivityToMap(activity))
		}
	} else {
		note := ap.ToNote(event, h.TC)
		activity := ap.BuildCreate(note, h.TC.LocalDomain)
		h.Federator.Federate(ctx, activity)
	}
}

func (h *Handler) handleKind5(ctx context.Context, event *nostr.Event) {
	activity := ap.ToDelete(event, h.TC)
	if activity != nil {
		h.Federator.Federate(ctx, ap.ActivityToMap(activity))
	}
}

func (h *Handler) handleKind6(ctx context.Context, event *nostr.Event) {
	activity := ap.ToAnnounce(event, h.TC)
	if activity != nil {
		h.Federator.Federate(ctx, ap.ActivityToMap(activity))
	}
}

func (h *Handler) handleKind7(ctx context.Context, event *nostr.Event) {
	content := event.Content
	if content == "+" || content == "" {
		activity := ap.ToLike(event, h.TC)
		if activity != nil {
			h.Federator.Federate(ctx, ap.ActivityToMap(activity))
		}
	} else if isEmojiContent(content) {
		activity := ap.ToEmojiReact(event, h.TC)
		if activity != nil {
			h.Federator.Federate(ctx, activity)
		}
	}
}

func (h *Handler) handleKind9735(ctx context.Context, event *nostr.Event) {
	// Zaps: currently logged but not fully federated.
	slog.Debug("received zap event", "id", event.ID)
	// TODO: Federate zap as AP Note/Announce
}

func (h *Handler) handleKind3(ctx context.Context, event *nostr.Event) {
	if h.Store == nil {
		return
	}

	localActorURL := h.TC.LocalActorURL

	// Build the set of Nostr pubkeys in the new contact list.
	newPubkeys := make(map[string]struct{})
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "p" {
			newPubkeys[tag[1]] = struct{}{}
		}
	}

	// Resolve each pubkey to an AP actor URL via their kind-0 proxy tag.
	newAPFollows := make(map[string]struct{})
	for pubkey := range newPubkeys {
		if apURL := h.resolveAPActor(ctx, pubkey); apURL != "" {
			newAPFollows[apURL] = struct{}{}
		}
	}

	// Get the AP actors we were already following (excludes Bluesky entries).
	current, err := h.Store.GetAPFollowing(localActorURL)
	if err != nil {
		slog.Warn("kind3: failed to load current AP follows", "error", err)
		return
	}
	currentSet := make(map[string]struct{}, len(current))
	for _, apURL := range current {
		currentSet[apURL] = struct{}{}
	}

	// Send Follow for newly added AP actors.
	for apURL := range newAPFollows {
		if _, already := currentSet[apURL]; already {
			continue
		}
		slog.Info("kind3: following AP actor", "actor", apURL)
		follow := ap.BuildFollow(localActorURL, apURL)
		go h.Federator.Federate(ctx, follow)
		if err := h.Store.AddFollow(localActorURL, apURL); err != nil {
			slog.Warn("kind3: failed to store follow", "actor", apURL, "error", err)
		}
	}

	// Send Undo Follow for AP actors no longer in the contact list.
	for apURL := range currentSet {
		if _, still := newAPFollows[apURL]; still {
			continue
		}
		slog.Info("kind3: unfollowing AP actor", "actor", apURL)
		undo := ap.BuildUndoFollow(localActorURL, apURL)
		go h.Federator.Federate(ctx, undo)
		if err := h.Store.RemoveFollow(localActorURL, apURL); err != nil {
			slog.Warn("kind3: failed to remove follow", "actor", apURL, "error", err)
		}
	}
}

// resolveAPActor returns the ActivityPub actor URL for a Nostr pubkey,
// using the mapping stored during NIP-05 lookups.
func (h *Handler) resolveAPActor(ctx context.Context, pubkey string) string {
	apURL, _ := h.Store.GetActorForKey(pubkey)
	return apURL
}

// ─── Eligibility check ────────────────────────────────────────────────────────

// isEligible returns true if this event should be processed by the bridge.
// Since the relay subscription is already filtered to the local user's pubkey,
// the only check needed is to skip events bridged from AP (loop prevention).
func (h *Handler) isEligible(event *nostr.Event) bool {
	return !ap.IsProxyEvent(event)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// isEmojiContent returns true if the string contains at least one rune that
// falls within a known Unicode emoji block. The length-based fallback was
// intentionally removed: it false-positived on short non-emoji strings like
// "ok", "→" (U+2192), and "↩" (U+21A9).
func isEmojiContent(s string) bool {
	if s == "" || s == "+" || s == "-" {
		return false
	}
	for _, r := range s {
		if (r >= 0x1F000 && r <= 0x1FAFF) || // Emoji, emoticons, pictographs (main blocks)
			(r >= 0x2600 && r <= 0x27BF) || // Misc Symbols + Dingbats
			(r >= 0x2300 && r <= 0x23FF) || // Misc Technical (⌚⌛⏩ etc.)
			(r >= 0x2B00 && r <= 0x2BFF) { // Misc Symbols and Arrows (⭐⬛⬜ etc.)
			return true
		}
	}
	return false
}
