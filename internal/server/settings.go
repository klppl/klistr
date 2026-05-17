package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	gonostr "github.com/nbd-wtf/go-nostr"
)

// KV keys used to persist admin settings across restarts.
const (
	kvShowSourceLink    = "setting_show_source_link"
	kvAutoAcceptFollows = "setting_auto_accept_follows"
	kvDisplayName       = "setting_display_name"
	kvSummary           = "setting_summary"
	kvPicture           = "setting_picture"
	kvBanner            = "setting_banner"
	kvExternalBaseURL   = "setting_external_base_url"
	kvZapPubkey         = "setting_zap_pubkey"
	kvZapSplit          = "setting_zap_split"
)

type settingsResponse struct {
	ShowSourceLink    bool    `json:"show_source_link"`
	AutoAcceptFollows bool    `json:"auto_accept_follows"`
	DisplayName       string  `json:"display_name"`
	Summary           string  `json:"summary"`
	Picture           string  `json:"picture"`
	Banner            string  `json:"banner"`
	ExternalBaseURL   string  `json:"external_base_url"`
	ZapPubkey         string  `json:"zap_pubkey"`
	ZapSplit          float64 `json:"zap_split"`
}

// handleGetSettings returns all user-configurable settings.
// GET /web/api/settings
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, settingsResponse{
		ShowSourceLink:    s.showSourceLink.Load(),
		AutoAcceptFollows: s.autoAcceptFollows.Load(),
		DisplayName:       s.cfg.NostrDisplayName,
		Summary:         s.cfg.NostrSummary,
		Picture:         s.cfg.NostrPicture,
		Banner:          s.cfg.NostrBanner,
		ExternalBaseURL: s.cfg.ExternalBaseURL,
		ZapPubkey:       s.cfg.ZapPubkey,
		ZapSplit:        s.cfg.ZapSplit,
	}, http.StatusOK)
}

// handleUpdateSettings applies partial updates and persists them to the KV store.
// Each field is persisted to KV BEFORE in-memory state is updated, so a KV
// write failure leaves in-memory and on-disk state consistent (both unchanged
// for the failing field). Successful fields are still applied — partial
// success is reported honestly via 207 Multi-Status with a failed_fields list.
//
// PATCH /web/api/settings
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ShowSourceLink    *bool    `json:"show_source_link"`
		AutoAcceptFollows *bool    `json:"auto_accept_follows"`
		DisplayName       *string  `json:"display_name"`
		Summary         *string  `json:"summary"`
		Picture         *string  `json:"picture"`
		Banner          *string  `json:"banner"`
		ExternalBaseURL *string  `json:"external_base_url"`
		ZapPubkey       *string  `json:"zap_pubkey"`
		ZapSplit        *float64 `json:"zap_split"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// persist applies one field: writes KV first, then runs `apply` to update
	// the in-memory state only on success. Failure is recorded into failed and
	// no in-memory update occurs, keeping memory consistent with disk.
	var changed []string
	var failed []string
	profilePersisted := false
	persist := func(field, key, value string, apply func()) {
		if err := s.store.SetKV(key, value); err != nil {
			slog.Warn("settings: KV write failed", "field", field, "error", err)
			failed = append(failed, field)
			return
		}
		apply()
		changed = append(changed, field+"="+value)
	}

	if req.ShowSourceLink != nil {
		v := *req.ShowSourceLink
		persist("show_source_link", kvShowSourceLink, strconv.FormatBool(v), func() {
			s.showSourceLink.Store(v)
			s.cfg.ShowSourceLink = v
		})
	}

	if req.AutoAcceptFollows != nil {
		v := *req.AutoAcceptFollows
		persist("auto_accept_follows", kvAutoAcceptFollows, strconv.FormatBool(v), func() {
			s.autoAcceptFollows.Store(v)
		})
	}

	if req.DisplayName != nil {
		v := *req.DisplayName
		persist("display_name", kvDisplayName, v, func() {
			s.cfg.NostrDisplayName = v
			profilePersisted = true
		})
	}

	if req.Summary != nil {
		v := *req.Summary
		persist("summary", kvSummary, v, func() {
			s.cfg.NostrSummary = v
			profilePersisted = true
		})
	}

	if req.Picture != nil {
		v := *req.Picture
		persist("picture", kvPicture, v, func() {
			s.cfg.NostrPicture = v
			profilePersisted = true
		})
	}

	if req.Banner != nil {
		v := *req.Banner
		persist("banner", kvBanner, v, func() {
			s.cfg.NostrBanner = v
			profilePersisted = true
		})
	}

	if req.ExternalBaseURL != nil {
		v := *req.ExternalBaseURL
		persist("external_base_url", kvExternalBaseURL, v, func() {
			s.cfg.ExternalBaseURL = v
		})
	}

	if req.ZapPubkey != nil {
		v := *req.ZapPubkey
		persist("zap_pubkey", kvZapPubkey, v, func() {
			s.cfg.ZapPubkey = v
		})
	}

	if req.ZapSplit != nil {
		v := *req.ZapSplit
		persist("zap_split", kvZapSplit, strconv.FormatFloat(v, 'f', -1, 64), func() {
			s.cfg.ZapSplit = v
		})
	}

	// Only re-publish kind-0 if at least one profile field actually persisted.
	if profilePersisted && s.followPublisher != nil {
		s.publishLocalKind0(r.Context())
	}

	if len(changed) > 0 {
		s.auditLog("settings_changed", strings.Join(changed, " "))
	}

	if len(failed) > 0 {
		// Report partial success: 207 Multi-Status. Body includes the failed
		// fields plus the post-update settings snapshot so the UI can refresh.
		jsonResponse(w, map[string]interface{}{
			"failed_fields": failed,
			"settings": settingsResponse{
				ShowSourceLink:    s.showSourceLink.Load(),
				AutoAcceptFollows: s.autoAcceptFollows.Load(),
				DisplayName:       s.cfg.NostrDisplayName,
				Summary:           s.cfg.NostrSummary,
				Picture:           s.cfg.NostrPicture,
				Banner:            s.cfg.NostrBanner,
				ExternalBaseURL:   s.cfg.ExternalBaseURL,
				ZapPubkey:         s.cfg.ZapPubkey,
				ZapSplit:          s.cfg.ZapSplit,
			},
		}, http.StatusMultiStatus)
		return
	}

	s.handleGetSettings(w, r)
}

// handleRepublishKind0 re-publishes the local user's kind-0 profile metadata to all relays.
// Useful after adding a new relay — the relay won't have your profile until it's re-published.
//
// POST /web/api/republish-kind0
func (s *Server) handleRepublishKind0(w http.ResponseWriter, r *http.Request) {
	if s.followPublisher == nil {
		jsonResponse(w, map[string]string{"message": "Follow publisher not configured."}, http.StatusOK)
		return
	}
	s.publishLocalKind0(r.Context())
	jsonResponse(w, map[string]string{"message": "Kind-0 profile published to all relays."}, http.StatusOK)
}

// publishLocalKind0 signs and publishes a kind-0 metadata event for the local
// user using the current profile settings in cfg.
func (s *Server) publishLocalKind0(ctx context.Context) {
	type profileContent struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name,omitempty"`
		About       string `json:"about,omitempty"`
		Picture     string `json:"picture,omitempty"`
		Banner      string `json:"banner,omitempty"`
	}

	content, err := json.Marshal(profileContent{
		Name:        s.cfg.NostrUsername,
		DisplayName: s.cfg.NostrDisplayName,
		About:       s.cfg.NostrSummary,
		Picture:     s.cfg.NostrPicture,
		Banner:      s.cfg.NostrBanner,
	})
	if err != nil {
		slog.Warn("settings: failed to marshal kind-0 content", "error", err)
		return
	}

	event := &gonostr.Event{
		Kind:      0,
		Content:   string(content),
		CreatedAt: gonostr.Now(),
	}
	if err := s.followPublisher.SignAsUser(event); err != nil {
		slog.Warn("settings: failed to sign kind-0", "error", err)
		return
	}
	if err := s.followPublisher.Publish(ctx, event); err != nil {
		slog.Warn("settings: failed to publish kind-0", "error", err)
		return
	}
	slog.Info("settings: published kind-0 profile update", "display_name", s.cfg.NostrDisplayName)
}
