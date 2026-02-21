package nostr

import (
	"context"
	"log/slog"

	"github.com/nbd-wtf/go-nostr"
	"github.com/klppl/klistr/internal/ap"
)

// Handler processes incoming Nostr events from the relay subscription
// and federates them to ActivityPub servers.
type Handler struct {
	TC        *ap.TransmuteContext
	Federator *ap.Federator
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
	case 5:
		h.handleKind5(ctx, event)
	case 6:
		h.handleKind6(ctx, event)
	case 7:
		h.handleKind7(ctx, event)
	case 9735:
		h.handleKind9735(ctx, event)
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

// ─── Eligibility check ────────────────────────────────────────────────────────

// isEligible returns true if this event should be processed by the bridge.
// Since the relay subscription is already filtered to the local user's pubkey,
// the only check needed is to skip events bridged from AP (loop prevention).
func (h *Handler) isEligible(event *nostr.Event) bool {
	return !ap.IsProxyEvent(event)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// isEmojiContent returns true if the content is an emoji or Extended_Pictographic character.
func isEmojiContent(s string) bool {
	if s == "" || s == "+" || s == "-" {
		return false
	}
	for _, r := range s {
		// Extended Pictographic range includes most emoji
		if (r >= 0x1F000 && r <= 0x1FFFF) || // Misc symbols, emoticons, etc.
			(r >= 0x2600 && r <= 0x27FF) || // Misc symbols
			(r >= 0x2300 && r <= 0x23FF) { // Misc technical
			return true
		}
	}
	return len([]rune(s)) <= 2 // Short strings might be emoji sequences
}
