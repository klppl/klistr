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

	profileChanged := false
	var changed []string // accumulate changed fields for the audit log

	if req.ShowSourceLink != nil {
		s.showSourceLink.Store(*req.ShowSourceLink)
		s.cfg.ShowSourceLink = *req.ShowSourceLink
		if err := s.store.SetKV(kvShowSourceLink, strconv.FormatBool(*req.ShowSourceLink)); err != nil {
			slog.Warn("settings: failed to persist show_source_link", "error", err)
		}
		slog.Info("settings: show_source_link updated", "value", *req.ShowSourceLink)
		changed = append(changed, "show_source_link="+strconv.FormatBool(*req.ShowSourceLink))
	}

	if req.AutoAcceptFollows != nil {
		s.autoAcceptFollows.Store(*req.AutoAcceptFollows)
		if err := s.store.SetKV(kvAutoAcceptFollows, strconv.FormatBool(*req.AutoAcceptFollows)); err != nil {
			slog.Warn("settings: failed to persist auto_accept_follows", "error", err)
		}
		slog.Info("settings: auto_accept_follows updated", "value", *req.AutoAcceptFollows)
		changed = append(changed, "auto_accept_follows="+strconv.FormatBool(*req.AutoAcceptFollows))
	}

	if req.DisplayName != nil {
		s.cfg.NostrDisplayName = *req.DisplayName
		if err := s.store.SetKV(kvDisplayName, *req.DisplayName); err != nil {
			slog.Warn("settings: failed to persist display_name", "error", err)
		}
		profileChanged = true
		changed = append(changed, "display_name="+*req.DisplayName)
	}

	if req.Summary != nil {
		s.cfg.NostrSummary = *req.Summary
		if err := s.store.SetKV(kvSummary, *req.Summary); err != nil {
			slog.Warn("settings: failed to persist summary", "error", err)
		}
		profileChanged = true
		changed = append(changed, "summary=<updated>")
	}

	if req.Picture != nil {
		s.cfg.NostrPicture = *req.Picture
		if err := s.store.SetKV(kvPicture, *req.Picture); err != nil {
			slog.Warn("settings: failed to persist picture", "error", err)
		}
		profileChanged = true
		changed = append(changed, "picture=<updated>")
	}

	if req.Banner != nil {
		s.cfg.NostrBanner = *req.Banner
		if err := s.store.SetKV(kvBanner, *req.Banner); err != nil {
			slog.Warn("settings: failed to persist banner", "error", err)
		}
		profileChanged = true
		changed = append(changed, "banner=<updated>")
	}

	if req.ExternalBaseURL != nil {
		s.cfg.ExternalBaseURL = *req.ExternalBaseURL
		if err := s.store.SetKV(kvExternalBaseURL, *req.ExternalBaseURL); err != nil {
			slog.Warn("settings: failed to persist external_base_url", "error", err)
		}
		changed = append(changed, "external_base_url="+*req.ExternalBaseURL)
	}

	if req.ZapPubkey != nil {
		s.cfg.ZapPubkey = *req.ZapPubkey
		if err := s.store.SetKV(kvZapPubkey, *req.ZapPubkey); err != nil {
			slog.Warn("settings: failed to persist zap_pubkey", "error", err)
		}
		changed = append(changed, "zap_pubkey=<updated>")
	}

	if req.ZapSplit != nil {
		s.cfg.ZapSplit = *req.ZapSplit
		if err := s.store.SetKV(kvZapSplit, strconv.FormatFloat(*req.ZapSplit, 'f', -1, 64)); err != nil {
			slog.Warn("settings: failed to persist zap_split", "error", err)
		}
		changed = append(changed, "zap_split="+strconv.FormatFloat(*req.ZapSplit, 'f', -1, 64))
	}

	if profileChanged && s.followPublisher != nil {
		s.publishLocalKind0(r.Context())
	}

	if len(changed) > 0 {
		s.auditLog("settings_changed", strings.Join(changed, " "))
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
