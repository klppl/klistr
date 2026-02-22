package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	gonostr "github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
	"github.com/klppl/klistr/internal/ap"
)

// FollowPublisher can sign and publish a Nostr event as the local user.
// Implemented by a thin adapter in main.go wrapping the Signer + Publisher.
type FollowPublisher interface {
	SignAsUser(event *gonostr.Event) error
	Publish(ctx context.Context, event *gonostr.Event) error
}

// importResult is the per-handle outcome returned to the admin UI.
type importResult struct {
	Handle string `json:"handle"`
	Status string `json:"status"` // "ok" | "error"
	Npub   string `json:"npub,omitempty"`
	Actor  string `json:"actor,omitempty"`
	Error  string `json:"error,omitempty"`
}

// handleImportFollowing receives a list of Fediverse handles, resolves them
// via WebFinger, derives deterministic Nostr pubkeys, and publishes a kind-3
// contact-list event that merges the new follows with the user's existing ones.
//
// POST /web/api/import-following
// Body: {"handles":["alice@mastodon.social","bob@hachyderm.io"]}
func (s *Server) handleImportFollowing(w http.ResponseWriter, r *http.Request) {
	if s.followPublisher == nil {
		http.Error(w, "import not available (follow publisher not configured)", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Handles []string `json:"handles"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Normalize: strip leading @, trim whitespace, deduplicate, skip blanks.
	seen := make(map[string]bool)
	var handles []string
	for _, h := range req.Handles {
		h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "@"))
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		handles = append(handles, h)
	}
	if len(handles) == 0 {
		http.Error(w, "no handles provided", http.StatusBadRequest)
		return
	}
	if len(handles) > 100 {
		http.Error(w, "max 100 handles per import", http.StatusBadRequest)
		return
	}

	// ── Step 1: Resolve handles concurrently ─────────────────────────────────
	results := make([]importResult, len(handles))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, handle := range handles {
		wg.Add(1)
		go func(i int, handle string) {
			defer wg.Done()
			res := s.resolveFollowHandle(r.Context(), handle)
			mu.Lock()
			results[i] = res
			mu.Unlock()
		}(i, handle)
	}
	wg.Wait()

	// ── Step 2: Collect newly resolved pubkeys ────────────────────────────────
	var addPubkeys []string
	for _, res := range results {
		if res.Status == "ok" && res.Actor != "" {
			if pk, err := s.actorResolver.PublicKey(res.Actor); err == nil {
				addPubkeys = append(addPubkeys, pk)
			}
		}
	}

	// ── Step 3: Merge and publish kind-3 ─────────────────────────────────────
	totalFollows, fetchedExisting, err := s.mergeAndPublishKind3(r.Context(), addPubkeys, nil)
	var publishErr string
	published := err == nil
	if err != nil {
		publishErr = err.Error()
		slog.Warn("import following: publish failed", "error", err)
	} else {
		slog.Info("import following: published kind-3",
			"total_follows", totalFollows,
			"new_handles", len(handles))
	}

	type response struct {
		Results         []importResult `json:"results"`
		Published       bool           `json:"published"`
		TotalFollows    int            `json:"total_follows"`
		FetchedExisting bool           `json:"fetched_existing"`
		Error           string         `json:"error,omitempty"`
	}
	jsonResponse(w, response{
		Results:         results,
		Published:       published,
		TotalFollows:    totalFollows,
		FetchedExisting: fetchedExisting,
		Error:           publishErr,
	}, http.StatusOK)
}

// mergeAndPublishKind3 builds a new kind-3 contact list by:
//  1. Fetching the user's existing kind-3 from relays (to preserve current follows).
//  2. Including all AP- and Bluesky-bridged follows tracked in the local DB.
//  3. Adding addPubkeys and removing removePubkeys.
//  4. Signing and publishing the resulting kind-3 event.
//
// Returns the total number of follows in the published event, whether an
// existing kind-3 was found on the relay, and any publish error.
func (s *Server) mergeAndPublishKind3(ctx context.Context, addPubkeys, removePubkeys []string) (int, bool, error) {
	if s.followPublisher == nil {
		return 0, false, fmt.Errorf("follow publisher not configured")
	}

	// Fetch existing kind-3 from relay (preserves non-bridge follows).
	existingPubkeys := s.fetchExistingKind3(ctx)
	fetchedExisting := len(existingPubkeys) > 0

	allPubkeys := make(map[string]struct{})
	for pk := range existingPubkeys {
		allPubkeys[pk] = struct{}{}
	}

	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	// Include AP-bridged follows from DB.
	if apFollows, err := s.store.GetAPFollowing(localActorURL); err == nil {
		for _, apURL := range apFollows {
			if pk, err := s.actorResolver.PublicKey(apURL); err == nil {
				allPubkeys[pk] = struct{}{}
			}
		}
	}

	// Include Bluesky-bridged follows from DB.
	if bskyFollows, err := s.store.GetBskyFollowing(localActorURL); err == nil {
		for _, bskyID := range bskyFollows {
			if pk, err := s.actorResolver.PublicKey(bskyID); err == nil {
				allPubkeys[pk] = struct{}{}
			}
		}
	}

	// Add new pubkeys.
	for _, pk := range addPubkeys {
		allPubkeys[pk] = struct{}{}
	}

	// Remove specified pubkeys (applied last so removals win).
	for _, pk := range removePubkeys {
		delete(allPubkeys, pk)
	}

	tags := make(gonostr.Tags, 0, len(allPubkeys))
	for pk := range allPubkeys {
		tags = append(tags, gonostr.Tag{"p", pk})
	}

	kind3 := &gonostr.Event{
		Kind:      3,
		Tags:      tags,
		Content:   "",
		CreatedAt: gonostr.Now(),
	}

	if err := s.followPublisher.SignAsUser(kind3); err != nil {
		return 0, fetchedExisting, fmt.Errorf("sign failed: %w", err)
	}
	if err := s.followPublisher.Publish(ctx, kind3); err != nil {
		return 0, fetchedExisting, fmt.Errorf("publish failed: %w", err)
	}

	slog.Info("mergeAndPublishKind3: published kind-3", "total_follows", len(tags), "id", kind3.ID[:8])
	return len(tags), fetchedExisting, nil
}

// resolveFollowHandle WebFingers a handle, derives its Nostr pubkey, and stores
// the actor_key mapping so that handleKind3 can later resolve it when the
// published kind-3 event is picked up by the relay subscription.
func (s *Server) resolveFollowHandle(ctx context.Context, handle string) importResult {
	res := importResult{Handle: handle}

	if !strings.Contains(handle, "@") {
		res.Status = "error"
		res.Error = "invalid format — expected user@domain"
		return res
	}

	actorURL, err := ap.WebFingerResolve(ctx, handle)
	if err != nil {
		res.Status = "error"
		res.Error = "WebFinger failed: " + err.Error()
		slog.Debug("import following: WebFinger failed", "handle", handle, "error", err)
		return res
	}

	pubkey, err := s.actorResolver.PublicKey(actorURL)
	if err != nil {
		res.Status = "error"
		res.Error = "key derivation failed: " + err.Error()
		return res
	}

	// Must store the actor_key mapping before publishing the kind-3 so that
	// handleKind3 (triggered when the relay echoes the event back) can resolve
	// this pubkey to an AP actor URL and send the ActivityPub Follow activity.
	if err := s.actorKeyStore.StoreActorKey(pubkey, actorURL); err != nil {
		slog.Warn("import following: failed to store actor key", "handle", handle, "error", err)
	}

	npub, err := nip19.EncodePublicKey(pubkey)
	if err != nil {
		npub = pubkey
	}

	res.Status = "ok"
	res.Npub = npub
	res.Actor = actorURL
	slog.Info("import following: resolved handle", "handle", handle, "actor", actorURL, "pubkey", pubkey[:8])
	return res
}

// fetchExistingKind3 queries the configured relays for the user's most recent
// kind-3 event and returns the set of pubkeys they currently follow. Returns
// an empty map if no kind-3 is found within the timeout (8 s).
// This preserves the user's existing follows when building the new kind-3.
func (s *Server) fetchExistingKind3(parentCtx context.Context) map[string]struct{} {
	pubkeys := make(map[string]struct{})

	ctx, cancel := context.WithTimeout(parentCtx, 8*time.Second)
	defer cancel()

	pool := gonostr.NewSimplePool(ctx)
	filters := gonostr.Filters{{
		Kinds:   []int{3},
		Authors: []string{s.cfg.NostrPublicKey},
		Limit:   1,
	}}

	for ev := range pool.SubMany(ctx, s.cfg.NostrRelays, filters) {
		if ev.Event == nil {
			continue
		}
		for _, tag := range ev.Event.Tags {
			if len(tag) >= 2 && tag[0] == "p" {
				pubkeys[tag[1]] = struct{}{}
			}
		}
		cancel() // got the latest kind-3, stop subscribing
		break
	}

	if len(pubkeys) > 0 {
		slog.Debug("import following: fetched existing kind-3", "follows", len(pubkeys))
	} else {
		slog.Debug("import following: no existing kind-3 found on relays")
	}
	return pubkeys
}
