package server

import (
	"context"
	"encoding/json"
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

	// ── Step 2: Fetch existing kind-3 from relay to preserve current follows ─
	existingPubkeys := s.fetchExistingKind3(r.Context())
	fetchedExisting := len(existingPubkeys) > 0

	// ── Step 3: Merge existing + AP-bridged + newly resolved pubkeys ──────────
	allPubkeys := make(map[string]struct{})

	// Preserve all current Nostr follows (regular + AP-bridged).
	for pk := range existingPubkeys {
		allPubkeys[pk] = struct{}{}
	}

	// Include AP-bridged follows tracked in the local DB.
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)
	if following, err := s.store.GetFollowing(localActorURL); err == nil {
		for _, apURL := range following {
			if pk, err := s.actorResolver.PublicKey(apURL); err == nil {
				allPubkeys[pk] = struct{}{}
			}
		}
	}

	// Add newly resolved pubkeys.
	for _, res := range results {
		if res.Status == "ok" && res.Actor != "" {
			if pk, err := s.actorResolver.PublicKey(res.Actor); err == nil {
				allPubkeys[pk] = struct{}{}
			}
		}
	}

	// ── Step 4: Build kind-3 p-tags ───────────────────────────────────────────
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

	// ── Step 5: Sign and publish ──────────────────────────────────────────────
	var publishErr string
	published := false
	if err := s.followPublisher.SignAsUser(kind3); err != nil {
		publishErr = "sign failed: " + err.Error()
		slog.Warn("import following: sign failed", "error", err)
	} else if err := s.followPublisher.Publish(r.Context(), kind3); err != nil {
		publishErr = "publish failed: " + err.Error()
		slog.Warn("import following: publish failed", "error", err)
	} else {
		published = true
		slog.Info("import following: published kind-3",
			"total_follows", len(tags),
			"new_handles", len(handles),
			"id", kind3.ID[:8])
	}

	type response struct {
		Results        []importResult `json:"results"`
		Published      bool           `json:"published"`
		TotalFollows   int            `json:"total_follows"`
		FetchedExisting bool          `json:"fetched_existing"`
		Error          string         `json:"error,omitempty"`
	}
	jsonResponse(w, response{
		Results:         results,
		Published:       published,
		TotalFollows:    len(tags),
		FetchedExisting: fetchedExisting,
		Error:           publishErr,
	}, http.StatusOK)
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
