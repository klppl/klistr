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
	"github.com/klppl/klistr/internal/bridge"
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
		jsonResponse(w, map[string]string{"error": "follow publisher not configured"}, http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Handle string `json:"handle"`
		Bridge string `json:"bridge"` // "fediverse" | "bsky"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, map[string]string{"error": "invalid JSON body"}, http.StatusBadRequest)
		return
	}
	req.Handle = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Handle), "@"))
	if req.Handle == "" {
		jsonResponse(w, map[string]string{"error": "handle required"}, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	var kind3Published bool
	switch req.Bridge {
	case "fediverse":
		pub, err := s.addFediverseFollow(ctx, req.Handle, localActorURL)
		if err != nil {
			slog.Warn("add fediverse follow failed", "handle", req.Handle, "error", err)
			jsonResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		kind3Published = pub

	case "bsky":
		if s.bskyClient == nil {
			jsonResponse(w, map[string]string{"error": "Bluesky not configured"}, http.StatusServiceUnavailable)
			return
		}
		pub, err := s.addBskyFollow(ctx, req.Handle, localActorURL)
		if err != nil {
			slog.Warn("add bluesky follow failed", "handle", req.Handle, "error", err)
			jsonResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		kind3Published = pub

	default:
		jsonResponse(w, map[string]string{"error": "bridge must be 'fediverse' or 'bsky'"}, http.StatusBadRequest)
		return
	}

	s.auditLog("follow_added", "bridge="+req.Bridge+" handle="+req.Handle)
	resp := map[string]interface{}{"status": "ok"}
	if !kind3Published {
		resp["kind_3_pending"] = true
		resp["message"] = "Follow committed locally, but kind-3 distribution to relays failed. Use Re-publish kind-3 to retry."
	}
	jsonResponse(w, resp, http.StatusOK)
}

// handleRemoveFollow processes an unfollow request.
//
// POST /web/api/unfollow
// Body: {"handle":"alice@mastodon.social","bridge":"fediverse"} or
//
//	{"handle":"user.bsky.social","bridge":"bsky"}
func (s *Server) handleRemoveFollow(w http.ResponseWriter, r *http.Request) {
	if s.followPublisher == nil {
		jsonResponse(w, map[string]string{"error": "follow publisher not configured"}, http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Handle string `json:"handle"`
		Bridge string `json:"bridge"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, map[string]string{"error": "invalid JSON body"}, http.StatusBadRequest)
		return
	}
	req.Handle = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Handle), "@"))
	if req.Handle == "" {
		jsonResponse(w, map[string]string{"error": "handle required"}, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	var kind3Published bool
	switch req.Bridge {
	case "fediverse":
		// removeFediverseFollow's design relies on kind-3 echo via handleKind3
		// to send the AP Undo Follow — if kind-3 publish fails, nothing happened,
		// so propagate as a real error rather than kind_3_pending.
		if err := s.removeFediverseFollow(ctx, req.Handle, localActorURL); err != nil {
			slog.Warn("remove fediverse follow failed", "handle", req.Handle, "error", err)
			jsonResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		kind3Published = true

	case "bsky":
		if s.bskyClient == nil {
			jsonResponse(w, map[string]string{"error": "Bluesky not configured"}, http.StatusServiceUnavailable)
			return
		}
		pub, err := s.removeBskyFollow(ctx, req.Handle, localActorURL)
		if err != nil {
			slog.Warn("remove bluesky follow failed", "handle", req.Handle, "error", err)
			jsonResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		kind3Published = pub

	default:
		jsonResponse(w, map[string]string{"error": "bridge must be 'fediverse' or 'bsky'"}, http.StatusBadRequest)
		return
	}

	s.auditLog("follow_removed", "bridge="+req.Bridge+" handle="+req.Handle)
	resp := map[string]interface{}{"status": "ok"}
	if !kind3Published {
		resp["kind_3_pending"] = true
		resp["message"] = "Unfollow committed locally, but kind-3 distribution to relays failed. Use Re-publish kind-3 to retry."
	}
	jsonResponse(w, resp, http.StatusOK)
}

// ─── Bridge-specific helpers ──────────────────────────────────────────────────

// addFediverseFollow stores the actor_key mapping for the resolved AP actor and
// publishes a kind-3 contact list that includes the new pubkey. Returns
// (kind3Published, err): err covers WebFinger / key derivation failures;
// kind3Published=false means side effects (actor_key) are committed but the
// kind-3 publish step failed — the caller can recover via republish-kind3.
func (s *Server) addFediverseFollow(ctx context.Context, handle, localActorURL string) (bool, error) {
	actorURL, err := ap.WebFingerResolve(ctx, handle)
	if err != nil {
		return false, err
	}

	pubkey, err := s.actorResolver.PublicKey(actorURL)
	if err != nil {
		return false, err
	}

	if err := s.actorKeyStore.StoreActorKey(pubkey, actorURL); err != nil {
		slog.Warn("add fediverse follow: failed to store actor key", "error", err)
	}

	if _, _, err := s.mergeAndPublishKind3(ctx, []string{pubkey}, nil); err != nil {
		slog.Warn("add fediverse follow: kind-3 publish failed (recoverable via republish-kind3)",
			"handle", handle, "error", err)
		return false, nil
	}

	slog.Info("add fediverse follow: published kind-3", "handle", handle, "actor", actorURL)
	return true, nil
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

// bskyFollowCore performs the Bluesky API calls and DB writes for following a
// Bluesky account (by handle or DID). Returns the profile, derived Nostr pubkey,
// and any error. The caller is responsible for publishing kind-3 and error mapping.
func (s *Server) bskyFollowCore(ctx context.Context, handle, localActorURL string) (*bsky.Profile, string, error) {
	profile, err := s.bskyClient.GetProfile(ctx, handle)
	if err != nil {
		return nil, "", err
	}
	did := profile.DID
	resolvedHandle := profile.Handle

	rkey, err := s.bskyClient.FollowActor(ctx, did)
	if err != nil {
		return nil, "", err
	}

	_ = s.store.SetKV("bsky_follow_"+did, rkey)
	_ = s.store.SetKV("bsky_follow_handle_"+did, resolvedHandle)

	pubkey, err := s.actorResolver.PublicKey(did)
	if err != nil {
		return nil, "", err
	}

	if err := s.store.AddFollow(localActorURL, "bsky:"+did); err != nil {
		slog.Warn("bsky follow: failed to store follow in db", "error", err)
	}

	s.publishBskyProfileKind0(ctx, profile)
	return profile, pubkey, nil
}

// addBskyFollow creates the Bluesky follow record, persists DB state, and
// publishes a kind-3 contact list. Returns (kind3Published, err): err covers
// Bluesky API and key derivation failures; kind3Published=false means the
// Bluesky follow IS active and DB state is correct, only kind-3 distribution
// failed — the caller can recover via republish-kind3.
func (s *Server) addBskyFollow(ctx context.Context, handle, localActorURL string) (bool, error) {
	profile, pubkey, err := s.bskyFollowCore(ctx, handle, localActorURL)
	if err != nil {
		return false, err
	}

	if _, _, err := s.mergeAndPublishKind3(ctx, []string{pubkey}, nil); err != nil {
		slog.Warn("add bsky follow: kind-3 publish failed (recoverable via republish-kind3)",
			"handle", profile.Handle, "did", profile.DID, "error", err)
		return false, nil
	}

	slog.Info("add bsky follow: followed and published kind-3", "handle", profile.Handle, "did", profile.DID)
	return true, nil
}

// removeBskyFollow deletes the Bluesky follow record, removes the DB row, and
// publishes a kind-3 reflecting the change. Returns (kind3Published, err):
// err covers Bluesky API and key derivation failures; kind3Published=false
// means the Bluesky unfollow IS active and DB is updated, only kind-3
// distribution failed — caller can recover via republish-kind3.
func (s *Server) removeBskyFollow(ctx context.Context, handleOrDID, localActorURL string) (bool, error) {
	// Resolve the DID — accept either handle or DID directly.
	var did string
	if strings.HasPrefix(handleOrDID, "did:") {
		did = handleOrDID
	} else {
		profile, err := s.bskyClient.GetProfile(ctx, handleOrDID)
		if err != nil {
			return false, err
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
			return false, err
		}
	}

	// Use plain DID for key derivation (matches poller and addBskyFollow).
	pubkey, err := s.actorResolver.PublicKey(did)
	if err != nil {
		return false, err
	}

	// Explicitly remove from DB (handleKind3 won't clean up Bluesky entries).
	if err := s.store.RemoveFollow(localActorURL, "bsky:"+did); err != nil {
		slog.Warn("remove bsky follow: failed to remove from db", "error", err)
	}

	if _, _, err := s.mergeAndPublishKind3(ctx, nil, []string{pubkey}); err != nil {
		slog.Warn("remove bsky follow: kind-3 publish failed (recoverable via republish-kind3)",
			"did", did, "error", err)
		return false, nil
	}

	slog.Info("remove bsky follow: unfollowed and published kind-3", "did", did)
	return true, nil
}

// publishBskyProfileKind0 publishes a Nostr kind-0 metadata event for a Bluesky
// profile, signed with the deterministic derived key for that account's DID.
// This allows Nostr clients to show the account's name, avatar, bio, and a link
// back to their Bluesky profile.
func (s *Server) publishBskyProfileKind0(ctx context.Context, profile *bsky.Profile) {
	if s.followPublisher == nil || profile.DID == "" || profile.Handle == "" {
		return
	}

	metaContent := bridge.BuildBskyProfileMeta(bridge.BskyProfileMeta{
		DisplayName: profile.DisplayName,
		Handle:      profile.Handle,
		AvatarURL:   profile.Avatar,
		BannerURL:   profile.Banner,
		Description: profile.Description,
	})

	event := &gonostr.Event{
		Kind:      0,
		Content:   metaContent,
		CreatedAt: gonostr.Now(),
	}
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

	// Collect AP pubkeys to remove BEFORE touching the DB so the merge step
	// sees the same DB state mergeAndPublishKind3 would see during a normal
	// remove call. (Bluesky follows are preserved automatically by the merge.)
	apKeysToRemove := make([]string, 0, len(apFollows))
	for _, targetActorURL := range apFollows {
		if pubkey, err := s.actorResolver.PublicKey(targetActorURL); err == nil {
			apKeysToRemove = append(apKeysToRemove, pubkey)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Single merged kind-3 publish: the merge function preserves Bluesky
	// follows via its own DB scan; the removals list strips the AP ones.
	// The relay echoes the event back and handleKind3 reads the DB to compute
	// which AP actors to Undo Follow — so we must publish FIRST and only
	// then clear the AP rows from the DB, otherwise handleKind3 sees no
	// follows to undo and no Undo Follow activities go out.
	if _, _, err := s.mergeAndPublishKind3(ctx, nil, apKeysToRemove); err != nil {
		slog.Error("wipe-follows: failed to publish kind-3", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	s.auditLog("wipe_follows", "count="+fmt.Sprint(count))
	jsonResponse(w, map[string]string{
		"message": fmt.Sprintf("Wiped %d Fediverse contacts. Unfollow requests sent.", count),
	}, http.StatusOK)
}
