package main

// compatibility_handlers.go — API compatibility, versioning, and readiness seams.
//
// Phase 16B (vm-16-03): Developer platform surface compatibility.
//
// What this file adds:
//   - GET /healthz                   — liveness/readiness probe (no auth required)
//   - GET /v1/version                — platform version and API version info
//   - apiVersionMiddleware           — reads Api-Version header; echoes X-Api-Version
//   - requestIDMiddleware            — injects X-Request-ID on every response
//   - writeJobLocation               — writes Location header on 202 async responses
//
// API versioning contract (vm-16-03__blueprint__ §implementation_decisions):
//   - Header: "Api-Version: YYYY-MM-DD" (optional; defaults to current stable)
//   - Current stable: currentAPIVersion const
//   - Unknown/future versions: accepted and echoed (forward-compatible)
//   - Removed versions (after 12-month deprecation): 410 Gone
//   - Response header: "X-Api-Version: YYYY-MM-DD" echoes the resolved version
//
// Location header contract (vm-16-03__blueprint__ §core_contracts
// "Asynchronous Operation Lifecycle"):
//   Any API operation returning 202 MUST include a Location header pointing
//   to a pollable resource the client can use to track progress.
//   For instance lifecycle: Location: /v1/instances/{id}/jobs/{job_id}
//
// X-Request-ID contract (API_ERROR_CONTRACT_V1 §7):
//   Every response includes the same request_id that appears in the error body.
//   This lets clients correlate log lines with specific API calls.
//
// Source: vm-16-03__blueprint__ §core_contracts,
//         vm-16-03__research__ §"API Compatibility, Versioning, and Deprecation Policy",
//         API_ERROR_CONTRACT_V1 §7.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/compute-platform/compute-platform/packages/idgen"
)

// currentAPIVersion is the current stable API version string.
// Clients that send Api-Version matching this value (or no header at all)
// receive current behaviour.
//
// Bump this constant when a new stable version is released.
//
// Source: vm-16-03__blueprint__ §implementation_decisions
//         "Use a date-based API versioning scheme (YYYY-MM-DD)".
const currentAPIVersion = "2024-01-15"

// removedAPIVersions lists API versions that have completed the 12-month
// deprecation period and are now removed. Requests using these versions
// receive 410 Gone.
//
// Source: vm-16-03__research__ §"API Compatibility, Versioning, and Deprecation Policy"
//         "After 12 months, the old version/field is removed."
var removedAPIVersions = map[string]bool{
	// Example — no versions removed yet at Phase 16B launch.
	// "2023-01-01": true,
}

// ── Middleware ────────────────────────────────────────────────────────────────

// apiVersionMiddleware reads the Api-Version request header and:
//   - Rejects versions in removedAPIVersions with 410 Gone.
//   - Echoes the resolved version in X-Api-Version response header.
//   - Uses currentAPIVersion when the header is absent or empty.
//
// This middleware is applied in routes() wrapping the public mux.
//
// Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth".
func apiVersionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("Api-Version")
		if v == "" {
			v = currentAPIVersion
		}

		// Reject removed versions.
		if removedAPIVersions[v] {
			reqID := idgen.New("req")
			writeJSON(w, http.StatusGone, map[string]interface{}{
				"error": map[string]interface{}{
					"code":       "api_version_removed",
					"message":    fmt.Sprintf("API version %q has been removed. Please upgrade to %s or later.", v, currentAPIVersion),
					"request_id": reqID,
					"details":    []interface{}{},
				},
			})
			return
		}

		// Echo the resolved version back so clients can confirm which version
		// is being served.
		w.Header().Set("X-Api-Version", v)
		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware injects an X-Request-ID header on every response.
//
// The same ID appears in error bodies (via writeAPIError) so clients can
// correlate HTTP responses with platform logs.
//
// Source: API_ERROR_CONTRACT_V1 §7 "request_id always present".
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := idgen.New("req")
		w.Header().Set("X-Request-ID", reqID)
		next.ServeHTTP(w, r)
	})
}

// ── Location header helper ────────────────────────────────────────────────────

// writeJobLocation sets the Location header to the job status endpoint.
//
// Called by handleCreateInstance and handleLifecycleAction immediately before
// writing the 202 response body, so clients have a canonical URL for polling.
//
// Pattern: /v1/instances/{instanceID}/jobs/{jobID}
//
// Source: vm-16-03__blueprint__ §core_contracts "Asynchronous Operation Lifecycle":
//   "Any API operation expected to take >500ms MUST be asynchronous, immediately
//   returning 202 Accepted with a Location header pointing to a pollable resource."
func writeJobLocation(w http.ResponseWriter, instanceID, jobID string) {
	w.Header().Set("Location", fmt.Sprintf("/v1/instances/%s/jobs/%s", instanceID, jobID))
}

// ── /healthz ──────────────────────────────────────────────────────────────────

type healthResponse struct {
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

// handleHealthz handles GET /healthz.
//
// Returns 200 {"status":"ok"} when the service is operational.
// Returns 503 when the DB ping fails (used by load balancers and Kubernetes).
//
// No authentication required — liveness probes must work before auth is bootstrapped.
//
// Source: P2_M1_GATE_CHECKLIST §"Phase 1 Lifecycle Regression (WS-H7)"
//         operational readiness requirement: health probe must be present.
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	if err := s.repo.Ping(ctx); err != nil {
		s.log.Error("healthz: DB ping failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, healthResponse{
			Status:    "unavailable",
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		})
		return
	}

	writeJSON(w, http.StatusOK, healthResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	})
}

// ── GET /v1/version ───────────────────────────────────────────────────────────

type versionResponse struct {
	APIVersion    string `json:"api_version"`
	MinAPIVersion string `json:"min_api_version"`
	Service       string `json:"service"`
}

// handleVersion handles GET /v1/version.
//
// Returns the current stable API version and the minimum supported version.
// Clients (SDK, CLI, Terraform provider) call this at startup to verify they
// are compatible with the server.
//
// Source: vm-16-03__research__ §"API Compatibility, Versioning, and Deprecation Policy".
func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, versionResponse{
		APIVersion:    currentAPIVersion,
		MinAPIVersion: currentAPIVersion, // no older versions active yet
		Service:       "compute-platform/resource-manager",
	})
}

// ── openapi.json stub ─────────────────────────────────────────────────────────

// handleOpenAPI handles GET /v1/openapi.json.
//
// Returns a minimal OpenAPI 3.0 document identifying the platform's API contract.
// Phase 16B seam: the full spec is generated from handler annotations in a CI
// step. This endpoint returns the version metadata needed by tooling pipelines
// to verify they are targeting the correct API version.
//
// Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth".
func (s *server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Minimal stub: real spec is generated by the CI tooling pipeline.
	// The `info.version` field matches currentAPIVersion.
	spec := map[string]interface{}{
		"openapi": "3.0.3",
		"info": map[string]interface{}{
			"title":   "Compute Platform API",
			"version": currentAPIVersion,
			"description": "VM compute instances lifecycle and management API. " +
				"Full specification generated by CI from source annotations.",
		},
		"paths": map[string]interface{}{},
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(spec) //nolint:errcheck
}
