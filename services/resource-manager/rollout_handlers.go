package main

// rollout_handlers.go — VM-P3C: Rollout control admin endpoints.
//
// Endpoints (all internal, mTLS-protected — registered in routes() via
// registerRolloutRoutes which wraps with auth.RequireMTLS):
//   POST /internal/v1/rollout/pause   — suppress repair dispatch with a reason
//   POST /internal/v1/rollout/resume  — re-enable repair dispatch
//   GET  /internal/v1/rollout/status  — inspect current gate state
//
// These endpoints allow operators to safely pause the reconciler's repair
// dispatch path during risky rollouts (worker binary upgrades, DB schema
// migrations, host-agent patches) without terminating running VMs or
// cancelling in-flight jobs.
//
// Gate interface:
//   RolloutGateInterface is defined here with a simple status method signature
//   that returns the local RolloutGateStatus struct. The reconciler package's
//   concrete RolloutGate does NOT satisfy this interface directly because the
//   Status() return types differ across packages.
//
//   Wiring pattern (in main.go):
//     gate := reconciler.NewRolloutGate()
//     dispatcher.SetGate(gate)
//     srv.rolloutGate = rolloutGateAdapter{gate}
//
//   rolloutGateAdapter (defined in main.go) wraps *reconciler.RolloutGate and
//   satisfies RolloutGateInterface by converting reconciler.RolloutGateStatus →
//   the local RolloutGateStatus on each call.
//
// Source: VM_PHASE_ROADMAP §9 "deeper automation and rollout controls",
//         P2_M1_INFRASTRUCTURE_HARDENING_PLAN §3 (operator rollout procedures),
//         vm-16-03__blueprint__ §core_contracts "Asynchronous Operation Lifecycle".

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/compute-platform/compute-platform/internal/auth"
)

// ── Gate interface ────────────────────────────────────────────────────────────

// RolloutGateInterface is the minimal interface the resource-manager needs to
// control the rollout gate.
// The concrete *reconciler.RolloutGate does not directly satisfy this interface
// because Status() return types differ across packages. Use rolloutGateAdapter
// (defined in main.go) to wrap the concrete gate.
type RolloutGateInterface interface {
	Pause(reason string)
	Resume()
	IsPaused() bool
	// Status returns the local RolloutGateStatus. Adapters must convert from
	// the reconciler package type.
	GateStatus() RolloutGateStatus
}

// RolloutGateStatus is the observable snapshot of the gate state returned by
// the admin status endpoint.
type RolloutGateStatus struct {
	Paused   bool       `json:"paused"`
	PausedAt *time.Time `json:"paused_at,omitempty"`
	Reason   string     `json:"reason,omitempty"`
}

// ── Route registration ────────────────────────────────────────────────────────

// registerRolloutRoutes wires rollout gate endpoints onto mux behind mTLS.
// No-op if server.rolloutGate is nil (backward compat).
func (s *server) registerRolloutRoutes(mux *http.ServeMux) {
	if s.rolloutGate == nil {
		return
	}
	rolloutMux := http.NewServeMux()
	rolloutMux.HandleFunc("/internal/v1/rollout/pause", s.handleRolloutPause)
	rolloutMux.HandleFunc("/internal/v1/rollout/resume", s.handleRolloutResume)
	rolloutMux.HandleFunc("/internal/v1/rollout/status", s.handleRolloutStatus)
	mux.Handle("/internal/v1/rollout/", auth.RequireMTLS(rolloutMux))
}

// ── Request / response types ─────────────────────────────────────────────────

type rolloutPauseRequest struct {
	Reason string `json:"reason"`
}

type rolloutStatusResponse struct {
	RolloutGateStatus
	Message string `json:"message"`
}

// ── Handlers ─────────────────────────────────────────────────────────────────

// handleRolloutPause handles POST /internal/v1/rollout/pause.
// Idempotent. Pausing does NOT affect running VMs or in-flight jobs.
func (s *server) handleRolloutPause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req rolloutPauseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidValue,
			"invalid JSON: "+err.Error(), "")
		return
	}
	if req.Reason == "" {
		writeAPIError(w, http.StatusBadRequest, errMissingField,
			"reason is required — provide a human-readable explanation for auditing", "reason")
		return
	}

	s.rolloutGate.Pause(req.Reason)
	s.log.Warn("rollout gate PAUSED — repair dispatch suppressed", "reason", req.Reason)

	status := s.rolloutGate.GateStatus()
	writeJSON(w, http.StatusOK, rolloutStatusResponse{
		RolloutGateStatus: status,
		Message:           "Repair dispatch paused. Running VMs and in-flight jobs are unaffected. Call /resume when rollout is complete.",
	})
}

// handleRolloutResume handles POST /internal/v1/rollout/resume.
// Idempotent.
func (s *server) handleRolloutResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.rolloutGate.Resume()
	s.log.Info("rollout gate RESUMED — repair dispatch active")

	status := s.rolloutGate.GateStatus()
	writeJSON(w, http.StatusOK, rolloutStatusResponse{
		RolloutGateStatus: status,
		Message:           "Repair dispatch resumed. The next reconciler cycle will catch any drift accumulated during the pause.",
	})
}

// handleRolloutStatus handles GET /internal/v1/rollout/status.
// Non-mutating.
func (s *server) handleRolloutStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := s.rolloutGate.GateStatus()
	msg := "Repair dispatch is active."
	if status.Paused {
		msg = "Repair dispatch is paused. Call /resume to re-enable."
	}
	writeJSON(w, http.StatusOK, rolloutStatusResponse{
		RolloutGateStatus: status,
		Message:           msg,
	})
}
