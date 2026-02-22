package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/klppl/klistr/internal/ap"
	"github.com/klppl/klistr/internal/bsky"
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

	pubkey, err := s.actorResolver.PublicKey("bsky:" + did)
	if err != nil {
		return err
	}

	if err := s.actorKeyStore.StoreActorKey(pubkey, "bsky:"+did); err != nil {
		slog.Warn("add bsky follow: failed to store actor key", "error", err)
	}

	if err := s.store.AddFollow(localActorURL, "bsky:"+did); err != nil {
		slog.Warn("add bsky follow: failed to store follow in db", "error", err)
	}

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

	pubkey, err := s.actorResolver.PublicKey("bsky:" + did)
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

// ─── Utility ──────────────────────────────────────────────────────────────────

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
