package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	nostrpkg "github.com/klppl/klistr/internal/nostr"
)

// RelayManager provides relay management for the /web admin API.
type RelayManager interface {
	// Relays returns the current relay list.
	Relays() []string
	// RelayStatuses returns circuit-breaker state for all configured relays.
	RelayStatuses() []nostrpkg.RelayStatus
	// AddRelay adds a relay. Returns false if already present.
	AddRelay(url string) bool
	// RemoveRelay removes a relay. Returns false if not found.
	RemoveRelay(url string) bool
	// ResetCircuit clears the circuit-breaker failure state for a relay.
	ResetCircuit(url string)
	// ResetCircuitWithRetry clears the circuit-breaker and retries dead letters for the relay.
	ResetCircuitWithRetry(url string) (int64, error)
	// TestRelay attempts to establish a WebSocket connection to the relay.
	TestRelay(ctx context.Context, url string) error
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (s *Server) handleGetRelays(w http.ResponseWriter, r *http.Request) {
	if s.relayManager == nil {
		jsonResponse(w, []nostrpkg.RelayStatus{}, http.StatusOK)
		return
	}
	statuses := s.relayManager.RelayStatuses()
	if statuses == nil {
		statuses = []nostrpkg.RelayStatus{}
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
	if !nostrpkg.IsValidRelayURL(url) {
		http.Error(w, "invalid relay URL: must be wss:// (ws:// allowed only for localhost)", http.StatusBadRequest)
		return
	}
	added := s.relayManager.AddRelay(url)
	if added {
		slog.Info("relay added via admin", "relay", url)
		s.auditLog("relay_added", url)
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
		s.auditLog("relay_removed", url)
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
	url := strings.TrimSpace(req.URL)
	retried, err := s.relayManager.ResetCircuitWithRetry(url)
	if err != nil {
		slog.Warn("relay circuit reset: failed to retry dead letters", "relay", url, "error", err)
	}
	s.auditLog("relay_circuit_reset", fmt.Sprintf("%s (retried %d dead letters)", url, retried))
	jsonResponse(w, map[string]interface{}{"ok": true, "url": url, "retried": retried}, http.StatusOK)
}
