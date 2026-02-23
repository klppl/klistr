package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// RelayStatus describes a relay and its circuit-breaker state (used in the admin API response).
type RelayStatus struct {
	URL               string `json:"url"`
	CircuitOpen       bool   `json:"circuit_open"`
	FailCount         int    `json:"fail_count"`
	CooldownRemaining int    `json:"cooldown_remaining_secs,omitempty"`
}

// RelayManager provides relay management for the /web admin API.
type RelayManager interface {
	// Relays returns the current relay list.
	Relays() []string
	// RelayStatuses returns circuit-breaker state for all configured relays.
	RelayStatuses() []RelayStatus
	// AddRelay adds a relay. Returns false if already present.
	AddRelay(url string) bool
	// RemoveRelay removes a relay. Returns false if not found.
	RemoveRelay(url string) bool
	// ResetCircuit clears the circuit-breaker failure state for a relay.
	ResetCircuit(url string)
	// TestRelay attempts to establish a WebSocket connection to the relay.
	TestRelay(ctx context.Context, url string) error
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (s *Server) handleGetRelays(w http.ResponseWriter, r *http.Request) {
	if s.relayManager == nil {
		jsonResponse(w, []RelayStatus{}, http.StatusOK)
		return
	}
	statuses := s.relayManager.RelayStatuses()
	if statuses == nil {
		statuses = []RelayStatus{}
	}
	jsonResponse(w, statuses, http.StatusOK)
}

func (s *Server) handleAddRelay(w http.ResponseWriter, r *http.Request) {
	if s.relayManager == nil {
		http.Error(w, "relay manager not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		http.Error(w, "invalid request: url required", http.StatusBadRequest)
		return
	}
	url := strings.TrimSpace(req.URL)
	if !strings.HasPrefix(url, "wss://") && !strings.HasPrefix(url, "ws://") {
		http.Error(w, "invalid relay URL: must start with wss:// or ws://", http.StatusBadRequest)
		return
	}
	added := s.relayManager.AddRelay(url)
	if added {
		slog.Info("relay added via admin", "relay", url)
	}
	jsonResponse(w, map[string]interface{}{
		"added":   added,
		"url":     url,
		"message": map[bool]string{true: "relay added", false: "relay already configured"}[added],
	}, http.StatusOK)
}

func (s *Server) handleRemoveRelay(w http.ResponseWriter, r *http.Request) {
	if s.relayManager == nil {
		http.Error(w, "relay manager not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		http.Error(w, "invalid request: url required", http.StatusBadRequest)
		return
	}
	url := strings.TrimSpace(req.URL)
	removed := s.relayManager.RemoveRelay(url)
	if removed {
		slog.Info("relay removed via admin", "relay", url)
	}
	jsonResponse(w, map[string]interface{}{
		"removed": removed,
		"url":     url,
		"message": map[bool]string{true: "relay removed", false: "relay not found"}[removed],
	}, http.StatusOK)
}

func (s *Server) handleTestRelay(w http.ResponseWriter, r *http.Request) {
	if s.relayManager == nil {
		http.Error(w, "relay manager not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		http.Error(w, "invalid request: url required", http.StatusBadRequest)
		return
	}
	url := strings.TrimSpace(req.URL)
	start := time.Now()
	err := s.relayManager.TestRelay(r.Context(), url)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		jsonResponse(w, map[string]interface{}{
			"ok":      false,
			"url":     url,
			"error":   err.Error(),
			"latency": latencyMs,
		}, http.StatusOK)
		return
	}
	jsonResponse(w, map[string]interface{}{
		"ok":      true,
		"url":     url,
		"latency": latencyMs,
	}, http.StatusOK)
}

func (s *Server) handleResetRelayCircuit(w http.ResponseWriter, r *http.Request) {
	if s.relayManager == nil {
		http.Error(w, "relay manager not available", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.URL) == "" {
		http.Error(w, "invalid request: url required", http.StatusBadRequest)
		return
	}
	s.relayManager.ResetCircuit(strings.TrimSpace(req.URL))
	jsonResponse(w, map[string]interface{}{"ok": true, "url": req.URL}, http.StatusOK)
}
