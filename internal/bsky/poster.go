package bsky

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
)

// PosterStore is the subset of db.Store used by the Poster.
type PosterStore interface {
	AddObject(apID, nostrID string) error
	DeleteObject(apID, nostrID string) error
	GetAPIDForObject(nostrID string) (string, bool)
	GetNostrIDForObject(apID string) (string, bool)
}

// PosterEnqueuer is an optional interface for persisting outbound Bluesky
// deliveries via the outbox queue.
type PosterEnqueuer interface {
	EnqueueBsky(destURL, payload string, priority int, sourceEventID string) error
}

// Poster handles outbound bridging from Nostr events to Bluesky records.
type Poster struct {
	Client          *Client
	Store           PosterStore
	LocalDomain     string
	ExternalBaseURL string
	// Enqueuer, when non-nil, routes Bluesky posts through the outbox queue.
	Enqueuer PosterEnqueuer
}

// Handle processes a Nostr event and mirrors it to Bluesky when appropriate.
// Called from nostr.Handler; runs in a goroutine.
func (p *Poster) Handle(ctx context.Context, event *nostr.Event) {
	if p.Enqueuer != nil {
		if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
			return
		}
		type bskyPayload struct {
			Kind    int          `json:"kind"`
			EventID string       `json:"event_id"`
			Event   *nostr.Event `json:"event"`
		}
		payload, err := json.Marshal(bskyPayload{
			Kind:    event.Kind,
			EventID: event.ID,
			Event:   event,
		})
		if err != nil {
			slog.Warn("bsky: failed to marshal event for outbox", "id", event.ID, "error", err)
			return
		}
		priority := 1
		switch event.Kind {
		case 7, 6, 5:
			priority = 0
		}
		if err := p.Enqueuer.EnqueueBsky(p.Client.PDSURL, string(payload), priority, event.ID); err != nil {
			slog.Warn("bsky: failed to enqueue", "id", event.ID, "error", err)
		}
		return
	}

	switch event.Kind {
	case 1:
		p.handleKind1(ctx, event)
	case 5:
		p.handleKind5(ctx, event)
	case 6:
		p.handleKind6(ctx, event)
	case 7:
		p.handleKind7(ctx, event)
	}
}

// HandleDirect processes a Nostr event with direct Bluesky delivery (no outbox).
// Called by outbox workers to perform the actual XRPC calls.
func (p *Poster) HandleDirect(ctx context.Context, event *nostr.Event) error {
	switch event.Kind {
	case 1:
		p.handleKind1(ctx, event)
	case 5:
		p.handleKind5(ctx, event)
	case 6:
		p.handleKind6(ctx, event)
	case 7:
		p.handleKind7(ctx, event)
	}
	return nil
}

// handleKind1 posts a Nostr note to Bluesky.
func (p *Poster) handleKind1(ctx context.Context, event *nostr.Event) {
	// Skip if already bridged to Bluesky.
	if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
		slog.Debug("bsky: skipping already-bridged note", "id", event.ID)
		return
	}

	if err := p.postNote(ctx, event); err != nil {
		slog.Warn("bsky: failed to post note", "id", event.ID, "error", err)
	}
}

// handleKind5 deletes a previously bridged post on Bluesky.
func (p *Poster) handleKind5(ctx context.Context, event *nostr.Event) {
	for _, tag := range event.Tags {
		if len(tag) < 2 || tag[0] != "e" {
			continue
		}
		deletedID := tag[1]
		atURI, ok := p.Store.GetAPIDForObject(deletedID)
		if !ok {
			continue
		}
		// Only delete if it looks like a Bluesky AT URI we created.
		if !strings.HasPrefix(atURI, "at://") {
			continue
		}
		collection := CollectionFromURI(atURI)
		rkey := RKeyFromURI(atURI)
		if collection == "" || rkey == "" {
			continue
		}
		slog.Info("bsky: deleting bridged post", "nostrID", deletedID, "atURI", atURI)
		if err := p.Client.DeleteRecord(ctx, p.Client.DID(), collection, rkey); err != nil {
			slog.Warn("bsky: delete record failed", "atURI", atURI, "error", err)
		}
		// Evict the mapping so the idempotency guard (GetAPIDForObject) no
		// longer blocks a potential re-post of the same Nostr event ID.
		if err := p.Store.DeleteObject(atURI, deletedID); err != nil {
			slog.Warn("bsky: failed to remove object mapping", "atURI", atURI, "error", err)
		}
	}
}

// handleKind6 reposts a bridged note on Bluesky.
func (p *Poster) handleKind6(ctx context.Context, event *nostr.Event) {
	// Skip if already bridged.
	if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
		return
	}

	// Find the reposted event ID.
	var repostedNostrID string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			repostedNostrID = tag[1]
			break
		}
	}
	if repostedNostrID == "" {
		return
	}

	// Resolve to AT URI.
	atURI, ok := p.Store.GetAPIDForObject(repostedNostrID)
	if !ok || !strings.HasPrefix(atURI, "at://") {
		slog.Debug("bsky: repost target not bridged", "repostedID", repostedNostrID)
		return
	}

	rec := RepostRecord{
		Type:      repostType,
		Subject:   Ref{URI: atURI},
		CreatedAt: event.CreatedAt.Time().UTC().Format(time.RFC3339),
	}
	resp, err := p.Client.CreateRecord(ctx, CreateRecordRequest{
		Repo:       p.Client.DID(),
		Collection: "app.bsky.feed.repost",
		Record:     rec,
	})
	if err != nil {
		slog.Warn("bsky: failed to repost", "id", event.ID, "error", err)
		return
	}
	slog.Info("bsky: reposted", "nostrID", event.ID, "atURI", resp.URI)
	if err := p.Store.AddObject(resp.URI, event.ID); err != nil {
		slog.Warn("bsky: failed to store repost mapping", "error", err)
	}
}

// handleKind7 likes a bridged note on Bluesky.
func (p *Poster) handleKind7(ctx context.Context, event *nostr.Event) {
	// Only bridge "+" reactions (not emoji reactions).
	if event.Content != "+" && event.Content != "" {
		return
	}

	// Skip if already bridged.
	if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
		return
	}

	// Find the liked event ID.
	var likedNostrID string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			likedNostrID = tag[1]
			break
		}
	}
	if likedNostrID == "" {
		return
	}

	// Resolve to AT URI.
	atURI, ok := p.Store.GetAPIDForObject(likedNostrID)
	if !ok || !strings.HasPrefix(atURI, "at://") {
		slog.Debug("bsky: like target not bridged", "likedID", likedNostrID)
		return
	}

	rec := LikeRecord{
		Type:      likeType,
		Subject:   Ref{URI: atURI},
		CreatedAt: event.CreatedAt.Time().UTC().Format(time.RFC3339),
	}
	resp, err := p.Client.CreateRecord(ctx, CreateRecordRequest{
		Repo:       p.Client.DID(),
		Collection: "app.bsky.feed.like",
		Record:     rec,
	})
	if err != nil {
		slog.Warn("bsky: failed to like", "id", event.ID, "error", err)
		return
	}
	slog.Info("bsky: liked", "nostrID", event.ID, "atURI", resp.URI)
	if err := p.Store.AddObject(resp.URI, event.ID); err != nil {
		slog.Warn("bsky: failed to store like mapping", "error", err)
	}
}

// postNote creates a Bluesky post from a Nostr kind-1 event.
func (p *Poster) postNote(ctx context.Context, event *nostr.Event) error {
	getATURI := func(nostrID string) (string, bool) {
		return p.Store.GetAPIDForObject(nostrID)
	}

	post, err := NostrNoteToFeedPost(event, p.ExternalBaseURL, getATURI)
	if err != nil {
		return err
	}

	resp, err := p.Client.CreateRecord(ctx, CreateRecordRequest{
		Repo:       p.Client.DID(),
		Collection: "app.bsky.feed.post",
		Record:     post,
	})
	if err != nil {
		return err
	}

	slog.Info("bsky: posted note", "nostrID", event.ID, "atURI", resp.URI)
	return p.Store.AddObject(resp.URI, event.ID)
}
