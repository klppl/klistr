package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/klppl/klistr/internal/ap"
	"github.com/klppl/klistr/internal/bsky"
	gonostr "github.com/nbd-wtf/go-nostr"
)

// BskyClient is the interface for Bluesky operations used by follow management.
// *bsky.Client satisfies this interface directly.
type BskyClient interface {
	FollowActor(ctx context.Context, did string) (string, error)
	DeleteRecord(ctx context.Context, repo, collection, rkey string) error
	GetProfile(ctx context.Context, actor string) (*bsky.Profile, error)
	DID() string
}

// fedFollowItem is one Fediverse entry in the GET /web/api/following response.
type fedFollowItem struct {
	Handle string `json:"handle"` // @user@domain
	Actor  string `json:"actor"`  // full AP actor URL
}

// bskyFollowItem is one Bluesky entry in the GET /web/api/following response.
type bskyFollowItem struct {
	Handle string `json:"handle"` // user.bsky.social (may be empty if not stored)
	DID    string `json:"did"`    // did:plc:xxx
}

// handleGetFollowing returns the current following lists for both bridges.
//
// GET /web/api/following
func (s *Server) handleGetFollowing(w http.ResponseWriter, r *http.Request) {
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	// Fediverse follows.
	apFollows, _ := s.store.GetAPFollowing(localActorURL)
	fedItems := make([]fedFollowItem, 0, len(apFollows))
	for _, actorURL := range apFollows {
		fedItems = append(fedItems, fedFollowItem{
			Handle: apURLToHandle(actorURL),
			Actor:  actorURL,
		})
	}

	// Bluesky follows.
	bskyFollows, _ := s.store.GetBskyFollowing(localActorURL)
	bskyItems := make([]bskyFollowItem, 0, len(bskyFollows))
	for _, bskyID := range bskyFollows {
		// bskyID is stored as "bsky:<did>"
		did := strings.TrimPrefix(bskyID, "bsky:")
		// Look up stored handle for display.
		handle, _ := s.store.GetKV("bsky_follow_handle_" + did)
		bskyItems = append(bskyItems, bskyFollowItem{
			Handle: handle,
			DID:    did,
		})
	}

	jsonResponse(w, map[string]interface{}{
		"fediverse": fedItems,
		"bluesky":   bskyItems,
	}, http.StatusOK)
}

// handleAddFollow processes a follow request for a single handle on either bridge.
//
// POST /web/api/follow
// Body: {"handle":"alice@mastodon.social","bridge":"fediverse"} or {"handle":"user.bsky.social","bridge":"bsky"}
func (s *Server) handleAddFollow(w http.ResponseWriter, r *http.Request) {
	if s.followPublisher == nil {
		http.Error(w, "follow publisher not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Handle string `json:"handle"`
		Bridge string `json:"bridge"` // "fediverse" | "bsky"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.Handle = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Handle), "@"))
	if req.Handle == "" {
		http.Error(w, "handle required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	switch req.Bridge {
	case "fediverse":
		if err := s.addFediverseFollow(ctx, req.Handle, localActorURL); err != nil {
			slog.Warn("add fediverse follow failed", "handle", req.Handle, "error", err)
			jsonResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}

	case "bsky":
		if s.bskyClient == nil {
			http.Error(w, "Bluesky not configured", http.StatusServiceUnavailable)
			return
		}
		if err := s.addBskyFollow(ctx, req.Handle, localActorURL); err != nil {
			slog.Warn("add bluesky follow failed", "handle", req.Handle, "error", err)
			jsonResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}

	default:
		http.Error(w, "bridge must be 'fediverse' or 'bsky'", http.StatusBadRequest)
		return
	}

	s.auditLog("follow_added", "bridge="+req.Bridge+" handle="+req.Handle)
	jsonResponse(w, map[string]string{"status": "ok"}, http.StatusOK)
}

// handleRemoveFollow processes an unfollow request.
//
// POST /web/api/unfollow
// Body: {"handle":"alice@mastodon.social","bridge":"fediverse"} or
//
//	{"handle":"user.bsky.social","bridge":"bsky"}
func (s *Server) handleRemoveFollow(w http.ResponseWriter, r *http.Request) {
	if s.followPublisher == nil {
		http.Error(w, "follow publisher not configured", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Handle string `json:"handle"`
		Bridge string `json:"bridge"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	req.Handle = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Handle), "@"))
	if req.Handle == "" {
		http.Error(w, "handle required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	switch req.Bridge {
	case "fediverse":
		if err := s.removeFediverseFollow(ctx, req.Handle, localActorURL); err != nil {
			slog.Warn("remove fediverse follow failed", "handle", req.Handle, "error", err)
			jsonResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}

	case "bsky":
		if s.bskyClient == nil {
			http.Error(w, "Bluesky not configured", http.StatusServiceUnavailable)
			return
		}
		if err := s.removeBskyFollow(ctx, req.Handle, localActorURL); err != nil {
			slog.Warn("remove bluesky follow failed", "handle", req.Handle, "error", err)
			jsonResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}

	default:
		http.Error(w, "bridge must be 'fediverse' or 'bsky'", http.StatusBadRequest)
		return
	}

	s.auditLog("follow_removed", "bridge="+req.Bridge+" handle="+req.Handle)
	jsonResponse(w, map[string]string{"status": "ok"}, http.StatusOK)
}

// ─── Bridge-specific helpers ──────────────────────────────────────────────────

func (s *Server) addFediverseFollow(ctx context.Context, handle, localActorURL string) error {
	actorURL, err := ap.WebFingerResolve(ctx, handle)
	if err != nil {
		return err
	}

	pubkey, err := s.actorResolver.PublicKey(actorURL)
	if err != nil {
		return err
	}

	if err := s.actorKeyStore.StoreActorKey(pubkey, actorURL); err != nil {
		slog.Warn("add fediverse follow: failed to store actor key", "error", err)
	}

	_, _, err = s.mergeAndPublishKind3(ctx, []string{pubkey}, nil)
	if err != nil {
		return err
	}

	slog.Info("add fediverse follow: published kind-3", "handle", handle, "actor", actorURL)
	return nil
}

func (s *Server) removeFediverseFollow(ctx context.Context, handleOrURL, localActorURL string) error {
	// Accept either a handle (user@domain) or a direct actor URL.
	var actorURL string
	if strings.HasPrefix(handleOrURL, "http") {
		actorURL = handleOrURL
	} else {
		var err error
		actorURL, err = ap.WebFingerResolve(ctx, handleOrURL)
		if err != nil {
			return err
		}
	}

	pubkey, err := s.actorResolver.PublicKey(actorURL)
	if err != nil {
		return err
	}

	_, _, err = s.mergeAndPublishKind3(ctx, nil, []string{pubkey})
	if err != nil {
		return err
	}

	// handleKind3 (triggered by the relay echoing the kind-3 event) will detect
	// the removal and send the AP Undo Follow + remove from DB.
	slog.Info("remove fediverse follow: published kind-3 without actor", "actor", actorURL)
	return nil
}

func (s *Server) addBskyFollow(ctx context.Context, handle, localActorURL string) error {
	profile, err := s.bskyClient.GetProfile(ctx, handle)
	if err != nil {
		return err
	}
	did := profile.DID
	resolvedHandle := profile.Handle

	rkey, err := s.bskyClient.FollowActor(ctx, did)
	if err != nil {
		return err
	}

	// Persist rkey so we can delete the follow later.
	_ = s.store.SetKV("bsky_follow_"+did, rkey)
	// Persist handle for display in the following list.
	_ = s.store.SetKV("bsky_follow_handle_"+did, resolvedHandle)

	// Key derivation: use plain DID (not "bsky:"+did) to match the key the
	// poller uses when signing kind-1 replies and kind-0 profiles for this author.
	pubkey, err := s.actorResolver.PublicKey(did)
	if err != nil {
		return err
	}

	if err := s.store.AddFollow(localActorURL, "bsky:"+did); err != nil {
		slog.Warn("add bsky follow: failed to store follow in db", "error", err)
	}

	// Publish a kind-0 profile event for the followed account so Nostr clients
	// display their name, avatar, bio, and a link back to their Bluesky profile.
	s.publishBskyProfileKind0(ctx, profile)

	_, _, err = s.mergeAndPublishKind3(ctx, []string{pubkey}, nil)
	if err != nil {
		return err
	}

	slog.Info("add bsky follow: followed and published kind-3", "handle", resolvedHandle, "did", did)
	return nil
}

func (s *Server) removeBskyFollow(ctx context.Context, handleOrDID, localActorURL string) error {
	// Resolve the DID — accept either handle or DID directly.
	var did string
	if strings.HasPrefix(handleOrDID, "did:") {
		did = handleOrDID
	} else {
		profile, err := s.bskyClient.GetProfile(ctx, handleOrDID)
		if err != nil {
			return err
		}
		did = profile.DID
	}

	// Look up the stored rkey.
	rkey, ok := s.store.GetKV("bsky_follow_" + did)
	if !ok || rkey == "" {
		slog.Warn("remove bsky follow: no rkey stored for did", "did", did)
		// Fall through — still remove from DB and kind-3.
	} else {
		userDID := s.bskyClient.DID()
		if err := s.bskyClient.DeleteRecord(ctx, userDID, "app.bsky.graph.follow", rkey); err != nil {
			return err
		}
	}

	// Use plain DID for key derivation (matches poller and addBskyFollow).
	pubkey, err := s.actorResolver.PublicKey(did)
	if err != nil {
		return err
	}

	// Explicitly remove from DB (handleKind3 won't clean up Bluesky entries).
	if err := s.store.RemoveFollow(localActorURL, "bsky:"+did); err != nil {
		slog.Warn("remove bsky follow: failed to remove from db", "error", err)
	}

	_, _, err = s.mergeAndPublishKind3(ctx, nil, []string{pubkey})
	if err != nil {
		return err
	}

	slog.Info("remove bsky follow: unfollowed and published kind-3", "did", did)
	return nil
}

// publishBskyProfileKind0 publishes a Nostr kind-0 metadata event for a Bluesky
// profile, signed with the deterministic derived key for that account's DID.
// This allows Nostr clients to show the account's name, avatar, bio, and a link
// back to their Bluesky profile. Mirrors the logic in bsky.Poller.publishBskyAuthorProfile.
func (s *Server) publishBskyProfileKind0(ctx context.Context, profile *bsky.Profile) {
	if s.followPublisher == nil || profile.DID == "" || profile.Handle == "" {
		return
	}

	profileURL := "https://bsky.app/profile/" + profile.Handle
	name := profile.DisplayName
	if name == "" {
		name = profile.Handle
	}
	about := profileURL
	if profile.Description != "" {
		about = profile.Description + "\n\n" + profileURL
	}

	meta := struct {
		Name    string `json:"name"`
		About   string `json:"about"`
		Picture string `json:"picture,omitempty"`
		Banner  string `json:"banner,omitempty"`
		Website string `json:"website"`
	}{
		Name:    name,
		About:   about,
		Picture: profile.Avatar,
		Banner:  profile.Banner,
		Website: profileURL,
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		slog.Debug("publishBskyProfileKind0: marshal failed", "handle", profile.Handle, "error", err)
		return
	}

	event := &gonostr.Event{
		Kind:      0,
		Content:   string(metaBytes),
		CreatedAt: gonostr.Now(),
	}
	// Sign with derived key for DID — same derivation the poller uses.
	if err := s.followPublisher.Sign(event, profile.DID); err != nil {
		slog.Debug("publishBskyProfileKind0: sign failed", "handle", profile.Handle, "error", err)
		return
	}
	if err := s.followPublisher.Publish(ctx, event); err != nil {
		slog.Debug("publishBskyProfileKind0: publish failed", "handle", profile.Handle, "error", err)
		return
	}
	slog.Info("publishBskyProfileKind0: published kind-0", "handle", profile.Handle, "did", profile.DID)
}

// handleResyncFollowProfiles re-fetches and re-publishes kind-0 metadata for all
// followed accounts on both bridges.
//
// POST /web/api/resync-follows
//
// Bluesky: calls GetProfile for every followed DID and re-publishes kind-0.
// Fediverse: fires the existing AccountResyncer trigger so all AP actor profiles
// are re-fetched and re-published in the background (same as "Re-sync Accounts").
func (s *Server) handleResyncFollowProfiles(w http.ResponseWriter, r *http.Request) {
	// Use a background context with a generous timeout so that profile fetches
	// and relay publishes are not cut short when the HTTP response is written.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	var bskySynced, bskyErrors int

	// Bluesky: re-publish kind-0 for each followed account.
	if s.bskyClient != nil && s.followPublisher != nil {
		if bskyFollows, err := s.store.GetBskyFollowing(localActorURL); err == nil {
			for _, bskyID := range bskyFollows {
				did := strings.TrimPrefix(bskyID, "bsky:")
				profile, err := s.bskyClient.GetProfile(ctx, did)
				if err != nil {
					slog.Warn("resync-follows: failed to fetch bsky profile", "did", did, "error", err)
					bskyErrors++
					continue
				}
				s.publishBskyProfileKind0(ctx, profile)
				bskySynced++
			}
		}
	}

	// Fediverse: trigger the existing AccountResyncer (non-blocking).
	fedQueued := false
	if s.resyncTrigger != nil {
		select {
		case s.resyncTrigger <- struct{}{}:
			fedQueued = true
		default:
			fedQueued = true // already queued
		}
	}

	parts := []string{}
	if s.bskyClient != nil {
		part := fmt.Sprintf("Bluesky: %d profile(s) re-synced", bskySynced)
		if bskyErrors > 0 {
			part += fmt.Sprintf(" (%d error(s))", bskyErrors)
		}
		parts = append(parts, part)
	}
	if fedQueued {
		parts = append(parts, "Fediverse profile resync queued")
	}
	msg := strings.Join(parts, ". ")
	if msg == "" {
		msg = "Nothing to resync."
	} else {
		msg += "."
	}

	slog.Info("resync-follows: done", "bsky_synced", bskySynced, "bsky_errors", bskyErrors, "fediverse_queued", fedQueued)
	jsonResponse(w, map[string]string{"message": msg}, http.StatusOK)
}

// apURLToHandle converts an AP actor URL like https://mastodon.social/users/alice
// into a @alice@mastodon.social display string.
func apURLToHandle(actorURL string) string {
	u, err := url.Parse(actorURL)
	if err != nil {
		return actorURL
	}
	parts := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	user := parts[len(parts)-1]
	user = strings.TrimPrefix(user, "@")
	if user != "" {
		return "@" + user + "@" + u.Host
	}
	return actorURL
}

// ─── Danger Zone ─────────────────────────────────────────────────────────────

// handleRefollowAll forces a re-sync of all inbound Fediverse follows by
// iterating over the local database of followed AP actors and broadcasting
// a fresh Follow activity to each of them. Long timeout used for large lists.
//
// POST /web/api/refollow-all
func (s *Server) handleRefollowAll(w http.ResponseWriter, r *http.Request) {
	// We don't need a request context because we immediately detach to a background goroutine.
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)
	apFollows, err := s.store.GetAPFollowing(localActorURL)
	if err != nil {
		slog.Error("refollow-all: failed to read follows", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if len(apFollows) == 0 {
		jsonResponse(w, map[string]string{"message": "No Fediverse follows found to re-sync."}, http.StatusOK)
		return
	}

	if s.apHandler == nil || s.apHandler.Federator == nil {
		http.Error(w, "federator not configured", http.StatusServiceUnavailable)
		return
	}

	// Federate the 'Follow' activities in the background so the HTTP request
	// doesn't block waiting for hundreds of network calls.
	go func() {
		// Replace the short context with a detached long-running one.
		bgCtx, bgCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer bgCancel()

		slog.Info("refollow-all: starting bulk follow broadcast", "count", len(apFollows))
		for _, targetActorURL := range apFollows {
			follow := ap.BuildFollow(localActorURL, targetActorURL)
			s.apHandler.Federator.Federate(bgCtx, follow)
		}
		slog.Info("refollow-all: completed bulk follow broadcast")
	}()

	s.auditLog("refollow_all", "count="+fmt.Sprint(len(apFollows)))
	jsonResponse(w, map[string]string{
		"message": fmt.Sprintf("Re-sync initiated for %d Fediverse contacts in the background.", len(apFollows)),
	}, http.StatusOK)
}

// handleWipeFollows permanently deletes all Fediverse contacts from the local
// database and publishes an empty contact list (kind-3) to Nostr, triggering
// 'Undo Follow' activities to all remote peers.
//
// POST /web/api/wipe-follows
func (s *Server) handleWipeFollows(w http.ResponseWriter, r *http.Request) {
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)
	apFollows, err := s.store.GetAPFollowing(localActorURL)
	if err != nil {
		slog.Error("wipe-follows: failed to read follows", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	count := len(apFollows)
	if count == 0 {
		jsonResponse(w, map[string]string{"message": "There are no Fediverse follows to wipe."}, http.StatusOK)
		return
	}

	// 1. Unfollow from the DB directly.
	for _, targetActorURL := range apFollows {
		if err := s.store.RemoveFollow(localActorURL, targetActorURL); err != nil {
			slog.Warn("wipe-follows: failed to remove from db", "actor", targetActorURL, "error", err)
		}
	}

	// 2. Publish a merged kind-3 with empty desired additions but all current
	// known pubkeys marked as removals. By omitting pubkeys from the new kind-3,
	// the relay will echo it and our handleKind3 will issue Undo Follow for
	// everyone who was in the previous list.
	// Since we already deleted them from the DB, we just rely on `handleKind3`
	// processing the removal and doing the AP cleanup. Actually, handleKind3 reads
	// from the DB to see who to Undo. Since we just deleted them, it won't know!
	//
	// Better approach: just let handleKind3 do the work. We publish an empty kind-3
	// (or a kind-3 containing ONLY the Bluesky follows without the AP follows).

	// Get current Bluesky follows to preserve them in the kind-3.
	bskyKeys := []string{}
	if bskyFollows, err := s.store.GetBskyFollowing(localActorURL); err == nil {
		for _, bskyID := range bskyFollows {
			did := strings.TrimPrefix(bskyID, "bsky:")
			if pubkey, err := s.actorResolver.PublicKey(did); err == nil {
				bskyKeys = append(bskyKeys, pubkey)
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// mergeAndPublishKind3 will construct a new list using only bskyKeys.
	// It replaces the old kind-3. When handleKind3 sees the new list is missing
	// the AP keys, it will cross-reference with the DB, send Undo Follow to them,
	// and THEN delete them from the DB.
	_, _, err = s.mergeAndPublishKind3(ctx, bskyKeys, nil) // use Set semantics, actually mergeAndPublishKind3 ADDS keys.
	if err != nil {
		slog.Error("wipe-follows: failed to publish kind-3", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Wait, mergeAndPublishKind3(ctx, add, remove) takes lists.
	// We want to REMOVE all AP keys.
	apKeysToRemove := []string{}
	for _, targetActorURL := range apFollows {
		if pubkey, err := s.actorResolver.PublicKey(targetActorURL); err == nil {
			apKeysToRemove = append(apKeysToRemove, pubkey)
		}
	}

	_, _, err = s.mergeAndPublishKind3(ctx, nil, apKeysToRemove)
	if err != nil {
		slog.Error("wipe-follows: failed to publish kind-3 removals", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.auditLog("wipe_follows", "count="+fmt.Sprint(count))
	jsonResponse(w, map[string]string{
		"message": fmt.Sprintf("Wiped %d Fediverse contacts. Unfollow requests sent.", count),
	}, http.StatusOK)
}
