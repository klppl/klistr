package server

import (
	"context"
	"net/http"
	"time"
)

// outboxDeadAlertThreshold is the dead-letter count above which the
// healthcheck flips `ok` to false. 100 is generous for a single-user bridge
// — well above transient retry-and-give-up bursts but below a runaway.
const outboxDeadAlertThreshold = 100

// healthcheckResponse is the JSON shape returned by /api/healthcheck.
type healthcheckResponse struct {
	OK     bool                  `json:"ok"`
	Checks healthcheckSubsystems `json:"checks"`
}

type healthcheckSubsystems struct {
	DB     dbCheck     `json:"db"`
	Relays *relayCheck `json:"relays,omitempty"`
	Outbox *outboxCheck `json:"outbox,omitempty"`
}

type dbCheck struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

type relayCheck struct {
	Total       int  `json:"total"`
	Healthy     int  `json:"healthy"` // circuit closed
	OpenCircuit int  `json:"open_circuit"`
	OK          bool `json:"ok"` // at least one healthy relay configured
}

type outboxCheck struct {
	Pending int    `json:"pending"`
	Dead    int    `json:"dead"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// handleHealthcheck reports per-subsystem status. HTTP status is always 200;
// the JSON `ok` field aggregates the subsystem checks (all `ok` true → top OK).
func (s *Server) handleHealthcheck(w http.ResponseWriter, r *http.Request) {
	resp := healthcheckResponse{OK: true}

	// DB ping — bounded so a hung driver can't stall the healthcheck.
	pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	dbStart := time.Now()
	dbErr := s.store.Ping(pingCtx)
	resp.Checks.DB = dbCheck{
		OK:        dbErr == nil,
		LatencyMS: time.Since(dbStart).Milliseconds(),
	}
	if dbErr != nil {
		resp.Checks.DB.Error = dbErr.Error()
		resp.OK = false
	}

	// Relays — at least one closed circuit is required for liveness.
	if s.relayManager != nil {
		statuses := s.relayManager.RelayStatuses()
		c := &relayCheck{Total: len(statuses)}
		for _, st := range statuses {
			if st.CircuitOpen {
				c.OpenCircuit++
			} else {
				c.Healthy++
			}
		}
		c.OK = c.Total == 0 || c.Healthy > 0
		resp.Checks.Relays = c
		if !c.OK {
			resp.OK = false
		}
	}

	// Outbox — wired via SetOutboxStats from main.go; absent when not set up.
	if s.outboxStats != nil {
		pending, _, _, dead, err := s.outboxStats()
		c := &outboxCheck{Pending: pending, Dead: dead, OK: err == nil && dead < outboxDeadAlertThreshold}
		if err != nil {
			c.Error = err.Error()
		}
		resp.Checks.Outbox = c
		if !c.OK {
			resp.OK = false
		}
	}

	jsonResponse(w, resp, http.StatusOK)
}
