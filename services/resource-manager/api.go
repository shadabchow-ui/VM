package main

// api.go — Resource Manager HTTP handlers.
//
// Endpoints:
//   POST /internal/v1/certificate_signing_request     ← bootstrap-token + CSR → signed cert
//   POST /internal/v1/hosts/register                  ← Host Agent startup (mTLS required)
//   POST /internal/v1/hosts/{host_id}/heartbeat       ← periodic inventory update (mTLS required)
//   GET  /internal/v1/hosts                           ← scheduler reads available hosts (mTLS required)
//   POST /internal/v1/hosts/{host_id}/drain           ← VM-P2E Slice 1: operator drain
//   POST /internal/v1/hosts/{host_id}/drain-complete  ← VM-P2E Slice 2: explicit draining→drained
//   GET  /internal/v1/hosts/{host_id}/status          ← VM-P2E Slice 1/2/3/4: observable host state
//   POST /internal/v1/hosts/{host_id}/degraded        ← VM-P2E Slice 3: mark host degraded
//   POST /internal/v1/hosts/{host_id}/unhealthy       ← VM-P2E Slice 3: mark host unhealthy
//   GET  /internal/v1/hosts/fence-required            ← VM-P2E Slice 3: fencing groundwork list
//   POST /internal/v1/hosts/{host_id}/retire          ← VM-P2E Slice 4: initiate retirement
//   POST /internal/v1/hosts/{host_id}/retired         ← VM-P2E Slice 4: complete retirement
//   GET  /internal/v1/hosts/retired                   ← VM-P2E Slice 4: replacement-seam list
//
// Source: IMPLEMENTATION_PLAN_V1 §B2, AUTH_OWNERSHIP_MODEL_V1 §6,
//         05-02-host-runtime-worker-design.md §Bootstrap + §Heartbeating,
//         vm-13-03__blueprint__ §components "Fleet Management Service".
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
	CertPEM   string `json:"cert_pem"`    // signed client cert for the host agent
	CACertPEM string `json:"ca_cert_pem"` // CA cert for server verification
}

// handleCSR handles POST /internal/v1/certificate_signing_request.
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

	tokenHostID, err := s.inventory.ConsumeBootstrapToken(r.Context(), req.BootstrapToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid bootstrap token")
		return
	}

	if tokenHostID != req.HostID {
		writeError(w, http.StatusUnauthorized, "host_id does not match bootstrap token")
		return
	}

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
func (s *server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	certHostID := r.Header.Get("X-TLS-CN") // set by RequireMTLS middleware
	if certHostID == "" {
		writeError(w, http.StatusUnauthorized, "mTLS CN missing")
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

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"host_id": req.HostID,
		"status":  "ready",
	})
}

// handleHeartbeat handles POST /internal/v1/hosts/{host_id}/heartbeat.
func (s *server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pathHostID := extractPathSegment(r.URL.Path, "hosts", "heartbeat")
	if pathHostID == "" {
		writeError(w, http.StatusBadRequest, "missing host_id in path")
		return
	}

	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if err := s.inventory.Heartbeat(r.Context(), pathHostID, &req); err != nil {
		writeError(w, http.StatusNotFound, "host not found or heartbeat failed: "+err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleListHosts handles GET /internal/v1/hosts.
// Returns all ready, recently-heartbeating hosts for the scheduler.
func (s *server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	hosts, err := s.inventory.GetAvailableHosts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list hosts failed: "+err.Error())
		return
	}

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
	// VM-P2E Slice 3: /fence-required is a fixed subpath under /hosts — register
	// before the wildcard /hosts/ handler to prevent shadowing.
	protected.HandleFunc("/internal/v1/hosts/fence-required", s.handleGetFenceRequired)
	// VM-P2E Slice 4: /retired is a fixed subpath — register before the wildcard
	// /hosts/ handler. Must come after /fence-required (both are fixed paths;
	// order between them does not matter, but both must precede /hosts/).
	protected.HandleFunc("/internal/v1/hosts/retired", s.handleGetRetiredHosts)
	protected.HandleFunc("/internal/v1/hosts/", s.handleHostsSubpath)
	protected.HandleFunc("/internal/v1/hosts", s.handleListHosts)

	mux.Handle("/internal/v1/hosts", auth.RequireMTLS(protected))
	mux.Handle("/internal/v1/hosts/", auth.RequireMTLS(protected))

	// Public instance management API.
	s.registerInstanceRoutes(mux)

	// M7: SSH key management.
	s.registerSSHKeyRoutes(mux)

	// VM-P2B: Volume management API.
	s.registerVolumeRoutes(mux)

	// VM-P2B-S2: Snapshot management API.
	s.registerSnapshotRoutes(mux)

	// VM-P2D: Project management API.
	s.registerProjectRoutes(mux)

	return corsMiddleware(mux)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Principal-ID, Idempotency-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleHostsSubpath routes /internal/v1/hosts/{id}/* to the right handler.
//
// VM-P2E Slice 1: added /drain and /status.
// VM-P2E Slice 2: added /drain-complete.
// VM-P2E Slice 3: added /degraded and /unhealthy.
// VM-P2E Slice 4: added /retire and /retired.
//
// Ordering rules:
//   - /drain-complete must be checked before /drain (HasSuffix("/drain") is true
//     for "/drain-complete" paths).
//   - /retired must be checked before /retire (HasSuffix("/retire") is true
//     for "/retired" paths — same shadow risk as drain/drain-complete).
//   - /degraded and /unhealthy are unambiguous.
//   - /fence-required and /retired (the list endpoint) are fixed paths registered
//     directly on the protected mux, so they do NOT flow through this handler.
//
// Source: vm-13-03__blueprint__ §components "Fleet Management Service" REST API.
func (s *server) handleHostsSubpath(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		s.handleHeartbeat(w, r)
	case strings.HasSuffix(r.URL.Path, "/drain-complete"):
		// POST /internal/v1/hosts/{host_id}/drain-complete
		// VM-P2E Slice 2: explicit draining→drained transition.
		s.handleCompleteDrainHost(w, r)
	case strings.HasSuffix(r.URL.Path, "/drain"):
		// POST /internal/v1/hosts/{host_id}/drain
		// VM-P2E Slice 1: operator-initiated single-host drain.
		s.handleDrainHost(w, r)
	case strings.HasSuffix(r.URL.Path, "/degraded"):
		// POST /internal/v1/hosts/{host_id}/degraded
		// VM-P2E Slice 3: mark host degraded with reason_code.
		s.handleMarkDegraded(w, r)
	case strings.HasSuffix(r.URL.Path, "/unhealthy"):
		// POST /internal/v1/hosts/{host_id}/unhealthy
		// VM-P2E Slice 3: mark host unhealthy; sets fence_required for ambiguous failures.
		s.handleMarkUnhealthy(w, r)
	case strings.HasSuffix(r.URL.Path, "/retired"):
		// POST /internal/v1/hosts/{host_id}/retired
		// VM-P2E Slice 4: complete retirement (retiring→retired); sets retired_at.
		// Must be checked before /retire to avoid suffix shadow.
		s.handleMarkRetired(w, r)
	case strings.HasSuffix(r.URL.Path, "/retire"):
		// POST /internal/v1/hosts/{host_id}/retire
		// VM-P2E Slice 4: initiate retirement (→retiring); blocked if active VMs remain.
		s.handleMarkRetiring(w, r)
	case strings.HasSuffix(r.URL.Path, "/status"):
		// GET /internal/v1/hosts/{host_id}/status
		// VM-P2E Slice 1/2/3/4: observable host lifecycle state.
		s.handleGetHostStatus(w, r)
	default:
		http.NotFound(w, r)
	}
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
	prefix := fmt.Sprintf("/%s/", before)
	suffix := fmt.Sprintf("/%s", after)
	idx := strings.Index(path, prefix)
	if idx < 0 {
		return ""
	}
	rest := path[idx+len(prefix):]
	end := strings.Index(rest, suffix)
	if end < 0 {
		return ""
	}
	return rest[:end]
}
