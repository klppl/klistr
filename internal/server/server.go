// Package server implements the HTTP server for the klistr bridge.
// It serves ActivityPub endpoints (actors, objects, inbox, webfinger, etc.)
// and handles all inbound federation from the fediverse.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/klppl/klistr/internal/ap"
	"github.com/klppl/klistr/internal/config"
	"github.com/klppl/klistr/internal/db"
)

const (
	activityJSONType = `application/activity+json`
	ldJSONType       = `application/ld+json; profile="https://www.w3.org/ns/activitystreams"`
	version          = "1.0.0"
)

// ActorKeyStore persists derived pubkey ↔ AP actor URL mappings.
type ActorKeyStore interface {
	StoreActorKey(pubkey, apActorURL string) error
	GetActorForKey(pubkey string) (string, bool)
}

// ActorResolver derives a Nostr pubkey for a given AP actor URL.
type ActorResolver interface {
	PublicKey(apActorURL string) (string, error)
}

// maxConcurrentActivities is the maximum number of inbox activities processed
// concurrently. Activities arriving beyond this limit receive a 503 response.
const maxConcurrentActivities = 50

// Server is the main HTTP server for klistr.
type Server struct {
	cfg           *config.Config
	store         *db.Store
	keyPair       *ap.KeyPair
	apHandler     *ap.APHandler
	router        *chi.Mux
	actorKeyStore ActorKeyStore
	actorResolver ActorResolver
	startedAt     time.Time
	inboxSem      chan struct{} // bounded concurrency for inbox processing

	// Optional — set before Start() is called.
	logBroadcaster  *LogBroadcaster
	bskyTrigger     chan struct{}
	resyncTrigger   chan struct{}
	followPublisher FollowPublisher
	bskyClient      BskyClient
	relayManager    RelayManager
	showSourceLink  *atomic.Bool

	// nip05Cache caches NIP-05 remote handle lookups (lowercase name → pubkey).
	// Eliminates repeated WebFinger calls for the same handle across concurrent
	// requests. NIP-05 names are case-insensitive so the key is lowercased.
	nip05Cache sync.Map
}

// New creates a new Server.
func New(cfg *config.Config, store *db.Store, keyPair *ap.KeyPair, apHandler *ap.APHandler, actorKeyStore ActorKeyStore, actorResolver ActorResolver) *Server {
	s := &Server{
		cfg:            cfg,
		store:          store,
		keyPair:        keyPair,
		apHandler:      apHandler,
		actorKeyStore:  actorKeyStore,
		actorResolver:  actorResolver,
		startedAt:      time.Now(),
		inboxSem:       make(chan struct{}, maxConcurrentActivities),
		showSourceLink: &atomic.Bool{},
	}
	s.router = s.buildRouter()
	return s
}

// SetLogBroadcaster attaches a LogBroadcaster for the /web/log/stream SSE endpoint.
func (s *Server) SetLogBroadcaster(lb *LogBroadcaster) { s.logBroadcaster = lb }

// SetBskyTrigger attaches a channel that, when sent to, triggers an immediate
// Bluesky poll. Nil disables the Force Sync button.
func (s *Server) SetBskyTrigger(ch chan struct{}) { s.bskyTrigger = ch }

// SetFollowPublisher attaches the follow publisher used by the import endpoint.
func (s *Server) SetFollowPublisher(fp FollowPublisher) { s.followPublisher = fp }

// SetResyncTrigger attaches a channel that, when sent to, triggers an immediate
// account profile resync. Nil disables the Re-sync Accounts button.
func (s *Server) SetResyncTrigger(ch chan struct{}) { s.resyncTrigger = ch }

// SetBskyClient attaches the Bluesky client used for individual follow management.
// Nil disables the Bluesky follow/unfollow endpoints.
func (s *Server) SetBskyClient(c BskyClient) { s.bskyClient = c }

// SetRelayManager attaches the relay manager for the /web relay management endpoints.
func (s *Server) SetRelayManager(rm RelayManager) { s.relayManager = rm }

// SetShowSourceLink attaches the shared atomic bool controlling whether bridged
// notes include a source link. Updated live by the admin settings API.
func (s *Server) SetShowSourceLink(b *atomic.Bool) { s.showSourceLink = b }

// Start runs the HTTP server until ctx is cancelled.
func (s *Server) Start(ctx context.Context) {
	addr := ":" + s.cfg.Port
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	slog.Info("starting HTTP server", "addr", addr, "domain", s.cfg.LocalDomain)

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			slog.Error("server shutdown error", "error", err)
		}
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server error", "error", err)
	}
}

func (s *Server) buildRouter() *chi.Mux {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(loggingMiddleware)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)

	// Health check.
	r.Get("/api/healthcheck", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]string{"status": "ok"}, http.StatusOK)
	})

	// Discovery endpoints.
	r.Get("/.well-known/webfinger", s.handleWebFinger)
	r.Get("/.well-known/host-meta", s.handleHostMeta)
	r.Get("/.well-known/nodeinfo", s.handleNodeInfo)
	r.Get("/.well-known/nostr.json", s.handleNIP05)

	// NodeInfo schema.
	r.Get("/nodeinfo/{version}", s.handleNodeInfoSchema)

	// ActivityPub actor endpoints.
	r.Get("/users/{username}", s.handleActor)
	r.Get("/users/{username}/followers", s.handleFollowers)
	r.Get("/users/{username}/following", s.handleFollowing)
	r.Get("/users/{username}/outbox", s.handleOutbox)
	r.Post("/users/{username}/inbox", s.handleInbox)

	// ActivityPub object endpoints.
	r.Get("/objects/{id}", s.handleObject)

	// Shared inbox.
	r.Post("/inbox", s.handleInbox)

	// Service actor.
	r.Get("/actor", s.handleServiceActor)

	// Tags (stub).
	r.Get("/tags/{tag}", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, []interface{}{}, http.StatusOK)
	})

	// Root — basic info page.
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "klistr - a self-hosted personal bridge that connects your Nostr identity to the Fediverse and Bluesky.\nhttps://github.com/klppl/klistr\n\nRunning on %s\n", s.cfg.LocalDomain)
	})

	// Web admin UI — only mounted when WEB_ADMIN password is configured.
	if s.cfg.WebAdminPassword != "" {
		r.Route("/web", func(r chi.Router) {
			r.Use(s.adminAuth)
			r.Get("/", s.handleAdminDashboard)
			r.Get("/api/log", s.handleAdminLogSnapshot)
			r.Get("/api/status", s.handleAdminStatus)
			r.Get("/api/stats", s.handleAdminStats)
			r.Get("/api/followers", s.handleAdminFollowers)
			r.Post("/api/sync-bsky", s.handleAdminSyncBsky)
			r.Post("/api/resync-accounts", s.handleAdminResyncAccounts)
			r.Post("/api/import-following", s.handleImportFollowing)
			r.Post("/api/import-bsky-following", s.handleImportBskyFollowing)
			r.Get("/api/following", s.handleGetFollowing)
			r.Post("/api/follow", s.handleAddFollow)
			r.Post("/api/unfollow", s.handleRemoveFollow)
			r.Post("/api/resync-follows", s.handleResyncFollowProfiles)
			r.Get("/api/relays", s.handleGetRelays)
			r.Post("/api/relays", s.handleAddRelay)
			r.Delete("/api/relays", s.handleRemoveRelay)
			r.Post("/api/relays/test", s.handleTestRelay)
			r.Post("/api/relays/reset-circuit", s.handleResetRelayCircuit)
			r.Get("/api/settings", s.handleGetSettings)
			r.Patch("/api/settings", s.handleUpdateSettings)
			r.Post("/api/republish-kind0", s.handleRepublishKind0)
			r.Post("/api/republish-kind3", s.handleRepublishKind3)
		})
	}

	return r
}

// ─── ActivityPub Handlers ─────────────────────────────────────────────────────

func (s *Server) handleActor(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username != s.cfg.NostrUsername {
		http.NotFound(w, r)
		return
	}

	actorURL := s.cfg.BaseURL("/users/" + username)
	actor := &ap.Actor{
		ID:                actorURL,
		Type:              "Person",
		PreferredUsername: username,
		Name:              s.cfg.NostrDisplayName,
		Summary:           s.cfg.NostrSummary,
		Inbox:             actorURL + "/inbox",
		Outbox:            actorURL + "/outbox",
		Followers:         actorURL + "/followers",
		Following:         actorURL + "/following",
		PublicKey: &ap.PublicKey{
			ID:           actorURL + "#main-key",
			Owner:        actorURL,
			PublicKeyPem: s.keyPair.PublicPEM,
		},
		Endpoints: &ap.Endpoints{
			SharedInbox: s.cfg.BaseURL("/inbox"),
		},
		ProxyOf: []ap.Proxy{{
			Protocol:      ap.NostrProtocolURI,
			Proxied:       s.cfg.NostrNpub,
			Authoritative: true,
		}},
	}
	if s.cfg.NostrPicture != "" {
		actor.Icon = &ap.Image{Type: "Image", URL: s.cfg.NostrPicture}
	}
	if s.cfg.NostrBanner != "" {
		actor.Image = &ap.Image{Type: "Image", URL: s.cfg.NostrBanner}
	}

	apResponse(w, ap.WithContext(actor))
}

func (s *Server) handleObject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	// Return a minimal note — in a full implementation we'd fetch from relay.
	note := map[string]interface{}{
		"@context":     ap.DefaultContext,
		"id":           s.cfg.BaseURL("/objects/" + id),
		"type":         "Note",
		"attributedTo": s.cfg.BaseURL("/users/" + s.cfg.NostrUsername),
		"content":      "",
	}
	apResponse(w, note)
}

func (s *Server) handleFollowers(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username != s.cfg.NostrUsername {
		http.NotFound(w, r)
		return
	}

	// Only AP followers (http URLs) belong in the ActivityPub followers collection.
	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)
	followers, err := s.store.GetAPFollowers(localActorURL)
	if err != nil {
		slog.Error("get followers", "error", err)
		followers = []string{}
	}

	collection := ap.OrderedCollection{
		Context:      ap.DefaultContext,
		ID:           localActorURL + "/followers",
		Type:         "OrderedCollection",
		TotalItems:   len(followers),
		OrderedItems: followers,
	}
	apResponse(w, collection)
}

func (s *Server) handleFollowing(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username != s.cfg.NostrUsername {
		http.NotFound(w, r)
		return
	}

	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)
	following, err := s.store.GetFollowing(localActorURL)
	if err != nil {
		following = []string{}
	}

	collection := ap.OrderedCollection{
		Context:      ap.DefaultContext,
		ID:           localActorURL + "/following",
		Type:         "OrderedCollection",
		TotalItems:   len(following),
		OrderedItems: following,
	}
	apResponse(w, collection)
}

func (s *Server) handleOutbox(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if username != s.cfg.NostrUsername {
		http.NotFound(w, r)
		return
	}

	localActorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)
	collection := ap.OrderedCollection{
		Context:      ap.DefaultContext,
		ID:           localActorURL + "/outbox",
		Type:         "OrderedCollection",
		TotalItems:   0,
		OrderedItems: []interface{}{},
	}
	apResponse(w, collection)
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	// Verify HTTP signature.
	if s.cfg.SignFetch {
		if _, err := ap.VerifySignature(r); err != nil {
			slog.Warn("invalid HTTP signature", "error", err, "remote", r.RemoteAddr)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// Handle the activity asynchronously within a bounded concurrency limit.
	select {
	case s.inboxSem <- struct{}{}:
	default:
		slog.Warn("inbox overloaded, dropping activity", "remote", r.RemoteAddr)
		http.Error(w, "too many requests", http.StatusServiceUnavailable)
		return
	}
	go func() {
		defer func() { <-s.inboxSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.apHandler.HandleActivity(ctx, json.RawMessage(body)); err != nil {
			slog.Warn("failed to handle activity", "error", err)
		}
	}()

	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleServiceActor(w http.ResponseWriter, r *http.Request) {
	actor := &ap.Actor{
		ID:                s.cfg.BaseURL("/actor"),
		Type:              "Application",
		Name:              "klistr",
		PreferredUsername: "klistr",
		Inbox:             s.cfg.BaseURL("/inbox"),
		Outbox:            s.cfg.BaseURL("/actor/outbox"),
		PublicKey: &ap.PublicKey{
			ID:           s.cfg.BaseURL("/actor#main-key"),
			Owner:        s.cfg.BaseURL("/actor"),
			PublicKeyPem: s.keyPair.PublicPEM,
		},
		URL: "https://github.com/klppl/klistr",
	}
	apResponse(w, ap.WithContext(actor))
}

// ─── Discovery Handlers ───────────────────────────────────────────────────────

func (s *Server) handleWebFinger(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Query().Get("resource")
	if resource == "" {
		http.Error(w, "missing resource", http.StatusBadRequest)
		return
	}

	// Parse acct: URIs like acct:alice@example.com
	acct := strings.TrimPrefix(resource, "acct:")
	parts := strings.SplitN(acct, "@", 2)
	if len(parts) != 2 {
		http.Error(w, "invalid resource", http.StatusBadRequest)
		return
	}

	user := parts[0]
	host := parts[1]
	localHost := s.cfg.URL().Host

	if host != localHost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Only resolve the configured username.
	if user != s.cfg.NostrUsername {
		http.NotFound(w, r)
		return
	}

	actorURL := s.cfg.BaseURL("/users/" + s.cfg.NostrUsername)

	resp := ap.WebFingerResponse{
		Subject: resource,
		Aliases: []string{actorURL},
		Links: []ap.WebFingerLink{
			{
				Rel:  "self",
				Type: activityJSONType,
				Href: actorURL,
			},
			{
				Rel:      "http://ostatus.org/schema/1.0/subscribe",
				Template: s.cfg.BaseURL("/authorize_interaction?uri={uri}"),
			},
		},
	}

	w.Header().Set("Content-Type", "application/jrd+json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	cacheHeaders(w, 3600)
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleNIP05(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		jsonResponse(w, map[string]interface{}{"names": map[string]string{}}, http.StatusOK)
		return
	}

	// Local user.
	if name == s.cfg.NostrUsername {
		jsonResponse(w, map[string]interface{}{
			"names": map[string]string{s.cfg.NostrUsername: s.cfg.NostrPublicKey},
		}, http.StatusOK)
		return
	}

	// Fediverse handle lookup: "alice_at_mastodon.social" → "alice@mastodon.social"
	if s.actorKeyStore != nil && s.actorResolver != nil {
		if pubkey, ok := s.resolveRemoteHandle(r.Context(), name); ok {
			jsonResponse(w, map[string]interface{}{
				"names": map[string]string{name: pubkey},
			}, http.StatusOK)
			return
		}
	}

	// Bluesky handle lookup: "alice.bsky.social" → stored DID → derived pubkey.
	if s.actorResolver != nil {
		if pubkey, ok := s.resolveBskyHandle(name); ok {
			jsonResponse(w, map[string]interface{}{
				"names": map[string]string{name: pubkey},
			}, http.StatusOK)
			return
		}
	}

	jsonResponse(w, map[string]interface{}{"names": map[string]string{}}, http.StatusOK)
}

// resolveBskyHandle looks up the DID stored for a Bluesky handle (written by
// the Poller when it first bridges the author's profile) and returns the
// derived Nostr pubkey.
func (s *Server) resolveBskyHandle(handle string) (string, bool) {
	did, ok := s.store.GetKV("bsky_did_" + handle)
	if !ok || did == "" {
		return "", false
	}
	pubkey, err := s.actorResolver.PublicKey(did)
	if err != nil {
		slog.Warn("NIP-05: failed to derive pubkey for bsky handle", "handle", handle, "error", err)
		return "", false
	}
	slog.Info("NIP-05: resolved bsky handle", "handle", handle, "pubkey", pubkey[:8])
	return pubkey, true
}

// resolveRemoteHandle converts a name like "alice_at_mastodon.social" into a
// Fediverse handle, resolves it via WebFinger, and returns the derived Nostr pubkey.
// Results are cached in memory (keyed on lowercase name) so repeated NIP-05
// lookups for the same handle — including case variants — skip the network call.
func (s *Server) resolveRemoteHandle(ctx context.Context, name string) (string, bool) {
	handle := remoteHandleToFediverse(name)
	if handle == "" {
		return "", false
	}

	// NIP-05 names are case-insensitive; normalise before cache lookup so that
	// "FruH_at_mastodonsweden.se" and "fruh_at_mastodonsweden.se" share one entry.
	cacheKey := strings.ToLower(name)
	if cached, ok := s.nip05Cache.Load(cacheKey); ok {
		return cached.(string), true
	}

	actorURL, err := ap.WebFingerResolve(ctx, handle)
	if err != nil {
		slog.Debug("NIP-05: WebFinger failed", "handle", handle, "error", err)
		return "", false
	}

	pubkey, err := s.actorResolver.PublicKey(actorURL)
	if err != nil {
		slog.Warn("NIP-05: failed to derive pubkey", "actor", actorURL, "error", err)
		return "", false
	}

	if err := s.actorKeyStore.StoreActorKey(pubkey, actorURL); err != nil {
		slog.Warn("NIP-05: failed to store actor key", "error", err)
	}

	s.nip05Cache.Store(cacheKey, pubkey)
	slog.Info("NIP-05: resolved remote handle", "name", name, "handle", handle, "actor", actorURL, "pubkey", pubkey[:8])
	return pubkey, true
}

// remoteHandleToFediverse converts "alice_at_mastodon.social" → "alice@mastodon.social".
// Returns "" if the name doesn't match the expected pattern.
func remoteHandleToFediverse(name string) string {
	const sep = "_at_"
	idx := strings.Index(name, sep)
	if idx <= 0 {
		return ""
	}
	user := name[:idx]
	domain := name[idx+len(sep):]
	if user == "" || domain == "" || !strings.Contains(domain, ".") {
		return ""
	}
	return user + "@" + domain
}

func (s *Server) handleHostMeta(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xrd+xml")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<XRD xmlns="http://docs.oasis-open.org/ns/xri/xrd-1.0">
  <Link rel="lrdd" template="%s/.well-known/webfinger?resource={uri}"/>
</XRD>`, s.cfg.LocalDomain)
}

func (s *Server) handleNodeInfo(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"links": []map[string]string{
			{
				"rel":  "http://nodeinfo.diaspora.software/ns/schema/2.1",
				"href": s.cfg.BaseURL("/nodeinfo/2.1"),
			},
		},
	}
	cacheHeaders(w, 3600)
	jsonResponse(w, resp, http.StatusOK)
}

func (s *Server) handleNodeInfoSchema(w http.ResponseWriter, r *http.Request) {
	v := chi.URLParam(r, "version")
	if v != "2.0" && v != "2.1" {
		http.Error(w, "unsupported nodeinfo version", http.StatusNotFound)
		return
	}

	info := ap.NodeInfo{
		Version: "2.1",
		Software: ap.NodeInfoSoftware{
			Name:    "klistr",
			Version: version,
		},
		Protocols: []string{"activitypub"},
		Usage: ap.NodeInfoUsage{
			Users: ap.NodeInfoUsers{},
		},
		OpenRegistrations: false,
	}
	cacheHeaders(w, 3600)
	jsonResponse(w, info, http.StatusOK)
}

// ─── Utility functions ────────────────────────────────────────────────────────

func apResponse(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", activityJSONType)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode AP response", "error", err)
	}
}

func jsonResponse(w http.ResponseWriter, v interface{}, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("failed to encode JSON response", "error", err)
	}
}

func cacheHeaders(w http.ResponseWriter, maxAge int) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
}

// loggingMiddleware logs each HTTP request.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(wrapped, r)
		slog.Debug("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.status,
			"duration", time.Since(start),
			"remote", r.RemoteAddr,
		)
	})
}

// corsMiddleware adds CORS headers for fediverse compatibility.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(status int) {
	rw.status = status
	rw.ResponseWriter.WriteHeader(status)
}

// Unwrap allows http.ResponseController to reach the underlying ResponseWriter
// so SetWriteDeadline works correctly (e.g. for long-lived SSE connections).
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
}
