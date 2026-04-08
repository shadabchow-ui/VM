package main

// api.go — Resource Manager HTTP handlers.
//
// Endpoints:
//   POST /internal/v1/certificate_signing_request  ← bootstrap-token + CSR → signed cert
//   POST /internal/v1/hosts/register               ← Host Agent startup (mTLS required)
//   POST /internal/v1/hosts/{host_id}/heartbeat    ← periodic inventory update (mTLS required)
//   GET  /internal/v1/hosts                        ← scheduler reads available hosts (mTLS required)
//
// Source: IMPLEMENTATION_PLAN_V1 §B2, AUTH_OWNERSHIP_MODEL_V1 §6,
//         05-02-host-runtime-worker-design.md §Bootstrap + §Heartbeating.
//
// Auth model:
//   /certificate_signing_request: validated by bootstrap token only (no cert yet).
//   All other endpoints: RequireMTLS middleware enforces client cert. No exceptions.
//   Source: IMPLEMENTATION_PLAN_V1 §R-02.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/auth"
)

// csrRequest is the payload for the bootstrap CSR endpoint.
// Source: 05-02-host-runtime-worker-design.md §Bootstrap step 2.
type csrRequest struct {
	BootstrapToken string `json:"bootstrap_token"` // raw one-time token
	HostID         string `json:"host_id"`
	CSRPEM         string `json:"csr_pem"` // PEM-encoded Certificate Signing Request
}

type csrResponse struct {
	CertPEM   string `json:"cert_pem"`   // signed client cert for the host agent
	CACertPEM string `json:"ca_cert_pem"` // CA cert for server verification
}

// handleCSR handles POST /internal/v1/certificate_signing_request.
// This is the ONLY unauthenticated endpoint in the Resource Manager.
// It is protected by the bootstrap token, not a client cert.
// After this call, the host agent has a signed cert and all subsequent calls use mTLS.
func (s *server) handleCSR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req csrRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.BootstrapToken == "" || req.HostID == "" || req.CSRPEM == "" {
		writeError(w, http.StatusBadRequest, "bootstrap_token, host_id, and csr_pem are required")
		return
	}

	// Validate and consume the bootstrap token.
	// ConsumeBootstrapToken is atomic: if it returns a hostID, the token is now used.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §6.
	tokenHostID, err := s.inventory.ConsumeBootstrapToken(r.Context(), req.BootstrapToken)
	if err != nil {
		// Do not reveal whether the token exists. Always 401.
		writeError(w, http.StatusUnauthorized, "invalid bootstrap token")
		return
	}

	// Verify the host_id in the request matches what the token was issued for.
	if tokenHostID != req.HostID {
		writeError(w, http.StatusUnauthorized, "host_id does not match bootstrap token")
		return
	}

	// Sign the CSR. The CA verifies the CN matches host-{host_id}.
	certPEM, err := s.ca.SignCSR([]byte(req.CSRPEM), req.HostID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "CSR signing failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, csrResponse{
		CertPEM:   string(certPEM),
		CACertPEM: string(s.ca.CACertPEM()),
	})
}

// handleRegister handles POST /internal/v1/hosts/register.
// RequireMTLS middleware runs first — host_id is extracted from the cert CN.
// Idempotent: re-registration after agent restart is normal operation.
// Source: 05-02-host-runtime-worker-design.md §Bootstrap step 8.
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	certHostID, ok := auth.HostIDFromCtx(r.Context())
	if !ok {
		// Middleware should have caught this; defence-in-depth.
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if err := s.inventory.Register(r.Context(), certHostID, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"host_id": certHostID,
		"status":  "ready",
		"message": "host registered",
	})
}

// handleHeartbeat handles POST /internal/v1/hosts/{host_id}/heartbeat.
// host_id in the URL path is verified to match the cert CN — an agent may not
// update another host's heartbeat.
// Source: RUNTIMESERVICE_GRPC_V1 §8.
func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	certHostID, ok := auth.HostIDFromCtx(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	// Extract {host_id} from path: /internal/v1/hosts/{host_id}/heartbeat
	pathHostID := extractPathSegment(r.URL.Path, "hosts", "heartbeat")
	if pathHostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
		return
	}

	// A host agent may only update its own heartbeat.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §6 (CN authorises access to own resources).
	if certHostID != pathHostID {
		writeError(w, http.StatusForbidden, "cert host_id does not match path host_id")
		return
	}

	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if err := s.inventory.Heartbeat(r.Context(), certHostID, &req); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListHosts handles GET /internal/v1/hosts.
// Returns all ready, recently-heartbeating hosts for scheduler consumption.
// Requires mTLS — only authenticated internal services (scheduler, worker) call this.
// Source: IMPLEMENTATION_PLAN_V1 §C3 (Scheduler depends on Resource Manager).
func (s *server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if _, ok := auth.HostIDFromCtx(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	hosts, err := s.inventory.GetAvailableHosts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Return minimal fields needed by the scheduler.
	type hostSummary struct {
		ID               string `json:"id"`
		AvailabilityZone string `json:"availability_zone"`
		Status           string `json:"status"`
		TotalCPU         int    `json:"total_cpu"`
		TotalMemoryMB    int    `json:"total_memory_mb"`
		TotalDiskGB      int    `json:"total_disk_gb"`
		UsedCPU          int    `json:"used_cpu"`
		UsedMemoryMB     int    `json:"used_memory_mb"`
		UsedDiskGB       int    `json:"used_disk_gb"`
	}

	out := make([]hostSummary, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, hostSummary{
			ID:               h.ID,
			AvailabilityZone: h.AvailabilityZone,
			Status:           h.Status,
			TotalCPU:         h.TotalCPU,
			TotalMemoryMB:    h.TotalMemoryMB,
			TotalDiskGB:      h.TotalDiskGB,
			UsedCPU:          h.UsedCPU,
			UsedMemoryMB:     h.UsedMemoryMB,
			UsedDiskGB:       h.UsedDiskGB,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"hosts": out})
}

// --- routing ---

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	// Bootstrap endpoint — no mTLS (cert not yet issued).
	mux.HandleFunc("/internal/v1/certificate_signing_request", s.handleCSR)

	// All other endpoints require mTLS.
	protected := http.NewServeMux()
	protected.HandleFunc("/internal/v1/hosts/register", s.handleRegister)
	protected.HandleFunc("/internal/v1/hosts/", s.handleHostsSubpath)
	protected.HandleFunc("/internal/v1/hosts", s.handleListHosts)

	mux.Handle("/internal/v1/hosts", auth.RequireMTLS(protected))
	mux.Handle("/internal/v1/hosts/", auth.RequireMTLS(protected))

	return mux
}

// handleHostsSubpath routes /internal/v1/hosts/{id}/heartbeat to the right handler.
func (s *server) handleHostsSubpath(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/heartbeat") {
		s.handleHeartbeat(w, r)
		return
	}
	http.NotFound(w, r)
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{
			"message": msg,
		},
	})
}

// extractPathSegment extracts the segment between 'before' and 'after' in the URL path.
// Example: extractPathSegment("/internal/v1/hosts/host-abc/heartbeat", "hosts", "heartbeat") → "host-abc"
func extractPathSegment(path, before, after string) string {
	// Find the segment between /before/ and /after
	prefix := fmt.Sprintf("/%s/", before)
	suffix := fmt.Sprintf("/%s", after)
	idx := strings.Index(path, prefix)
	if idx < 0 {
		return ""
	}
	rest := path[idx+len(prefix):]
	if after != "" {
		end := strings.Index(rest, suffix)
		if end < 0 {
			return ""
		}
		rest = rest[:end]
	}
	return rest
}
