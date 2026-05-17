package bsky

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
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
		case 7, 6, 5, 9735:
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
	case 9735:
		p.handleKind9735(ctx, event)
	}
}

// HandleDirect processes a Nostr event with direct Bluesky delivery (no outbox).
// Called by outbox workers to perform the actual XRPC calls.
// Returns an error if the primary delivery action fails, so the outbox can retry.
func (p *Poster) HandleDirect(ctx context.Context, event *nostr.Event) error {
	switch event.Kind {
	case 1:
		return p.handleKind1Direct(ctx, event)
	case 5:
		return p.handleKind5Direct(ctx, event)
	case 6:
		return p.handleKind6Direct(ctx, event)
	case 7:
		return p.handleKind7Direct(ctx, event)
	case 9735:
		return p.handleKind9735Direct(ctx, event)
	}
	return nil
}

// handleKind1Direct is like handleKind1 but returns errors for outbox retry.
func (p *Poster) handleKind1Direct(ctx context.Context, event *nostr.Event) error {
	if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
		return nil // already bridged, not an error
	}
	return p.postNote(ctx, event)
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

// handleKind5 deletes previously bridged posts on Bluesky. Errors are logged
// only; for retry-aware delivery use handleKind5Direct.
func (p *Poster) handleKind5(ctx context.Context, event *nostr.Event) {
	if err := p.handleKind5Direct(ctx, event); err != nil {
		slog.Warn("bsky: handleKind5 delete failed", "id", event.ID, "error", err)
	}
}

// handleKind5Direct deletes bridged posts and returns the first error encountered
// so the outbox can retry. The object mapping is removed only after a successful
// DeleteRecord, so a retried attempt finds the same URI and re-issues the delete.
func (p *Poster) handleKind5Direct(ctx context.Context, event *nostr.Event) error {
	var firstErr error
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
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		// Evict the mapping so the idempotency guard (GetAPIDForObject) no
		// longer blocks a potential re-post of the same Nostr event ID, and
		// so a retry doesn't re-issue an already-successful delete.
		if err := p.Store.DeleteObject(atURI, deletedID); err != nil {
			slog.Warn("bsky: failed to remove object mapping", "atURI", atURI, "error", err)
		}
	}
	return firstErr
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

// handleKind6Direct is like handleKind6 but returns errors for outbox retry.
func (p *Poster) handleKind6Direct(ctx context.Context, event *nostr.Event) error {
	if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
		return nil
	}
	var repostedNostrID string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			repostedNostrID = tag[1]
			break
		}
	}
	if repostedNostrID == "" {
		return nil
	}
	atURI, ok := p.Store.GetAPIDForObject(repostedNostrID)
	if !ok || !strings.HasPrefix(atURI, "at://") {
		return nil // target not bridged, not retryable
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
		return err
	}
	p.Store.AddObject(resp.URI, event.ID)
	return nil
}

// handleKind7Direct is like handleKind7 but returns errors for outbox retry.
func (p *Poster) handleKind7Direct(ctx context.Context, event *nostr.Event) error {
	if event.Content != "+" && event.Content != "" {
		return nil
	}
	if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
		return nil
	}
	var likedNostrID string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			likedNostrID = tag[1]
			break
		}
	}
	if likedNostrID == "" {
		return nil
	}
	atURI, ok := p.Store.GetAPIDForObject(likedNostrID)
	if !ok || !strings.HasPrefix(atURI, "at://") {
		return nil // target not bridged, not retryable
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
		return err
	}
	p.Store.AddObject(resp.URI, event.ID)
	return nil
}

// handleKind9735 bridges a zap receipt as a Bluesky like.
func (p *Poster) handleKind9735(ctx context.Context, event *nostr.Event) {
	if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
		return
	}
	var zappedNostrID string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			zappedNostrID = tag[1]
			break
		}
	}
	if zappedNostrID == "" {
		return
	}
	atURI, ok := p.Store.GetAPIDForObject(zappedNostrID)
	if !ok || !strings.HasPrefix(atURI, "at://") {
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
		slog.Warn("bsky: failed to bridge zap as like", "id", event.ID, "error", err)
		return
	}
	p.Store.AddObject(resp.URI, event.ID)
}

// handleKind9735Direct is like handleKind9735 but returns errors for outbox retry.
func (p *Poster) handleKind9735Direct(ctx context.Context, event *nostr.Event) error {
	if _, exists := p.Store.GetAPIDForObject(event.ID); exists {
		return nil
	}
	var zappedNostrID string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "e" {
			zappedNostrID = tag[1]
			break
		}
	}
	if zappedNostrID == "" {
		return nil
	}
	atURI, ok := p.Store.GetAPIDForObject(zappedNostrID)
	if !ok || !strings.HasPrefix(atURI, "at://") {
		return nil
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
		return err
	}
	p.Store.AddObject(resp.URI, event.ID)
	return nil
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

	// Outbound images: extract NIP-94 imeta tags, download, upload as blobs.
	images := ExtractImetaTags(event, 4)
	if embed := p.uploadImages(ctx, images); embed != nil {
		post.Embed = embed
	}

	// Quote post: detect q-tag and build embed.record (or recordWithMedia).
	var quoteATURI string
	for _, tag := range event.Tags {
		if len(tag) >= 2 && tag[0] == "q" {
			if uri, ok := getATURI(tag[1]); ok && strings.HasPrefix(uri, "at://") {
				quoteATURI = uri
			}
			break
		}
	}
	if quoteATURI != "" {
		if post.Embed != nil && post.Embed.Type == "app.bsky.embed.images" {
			post.Embed = &Embed{
				Type:   "app.bsky.embed.recordWithMedia",
				Record: &EmbedRef{URI: quoteATURI},
				Media:  post.Embed,
			}
		} else {
			post.Embed = &Embed{
				Type:   "app.bsky.embed.record",
				Record: &EmbedRef{URI: quoteATURI},
			}
		}
	}

	// Content warning: map NIP-36 content-warning tag to Bluesky self-labels.
	if labels := BuildSelfLabels(event); labels != nil {
		post.Labels = labels
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

// uploadImages downloads image URLs and uploads them as Bluesky blobs.
func (p *Poster) uploadImages(ctx context.Context, images []imetaInfo) *Embed {
	if len(images) == 0 {
		return nil
	}
	var embedImages []EmbedImage
	httpClient := &http.Client{Timeout: 10 * time.Second}
	for _, img := range images {
		resp, err := httpClient.Get(img.URL)
		if err != nil {
			slog.Debug("bsky: failed to download image", "url", img.URL, "error", err)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1_000_000))
		resp.Body.Close()
		if err != nil {
			continue
		}
		mimeType := img.MimeType
		if mimeType == "" {
			mimeType = resp.Header.Get("Content-Type")
		}
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		blob, err := p.Client.UploadBlob(ctx, data, mimeType)
		if err != nil {
			slog.Warn("bsky: failed to upload blob", "url", img.URL, "error", err)
			continue
		}
		ei := EmbedImage{Alt: img.Alt, Image: *blob}
		if img.Width > 0 && img.Height > 0 {
			ei.AspectRatio = &AspectRatio{Width: img.Width, Height: img.Height}
		}
		embedImages = append(embedImages, ei)
	}
	if len(embedImages) == 0 {
		return nil
	}
	return &Embed{Type: "app.bsky.embed.images", Images: embedImages}
}
