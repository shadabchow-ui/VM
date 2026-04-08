package main

// instance_handlers.go — HTTP handlers for the public instance management API.
//
// PASS 1 scope: POST /v1/instances, GET /v1/instances, GET /v1/instances/{id}.
// No auth middleware yet (added in PASS 2).
// No idempotency yet (added in PASS 3).
// No lifecycle actions yet (added in PASS 2).
// No job status endpoint yet (added in PASS 3).
//
// Source: 08-01-api-resource-model-and-endpoint-design.md,
//         INSTANCE_MODEL_V1 §2, §4,
//         03-02-async-job-model-and-idempotency.md.

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// registerInstanceRoutes registers the public instance API routes.
// Called from routes() in api.go.
// No auth middleware in PASS 1 — added in PASS 2.
func (s *server) registerInstanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/instances", s.handleInstanceRoot)
	mux.HandleFunc("/v1/instances/", s.handleInstanceByID)
}

// handleInstanceRoot dispatches POST /v1/instances and GET /v1/instances.
func (s *server) handleInstanceRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleCreateInstance(w, r)
	case http.MethodGet:
		s.handleListInstances(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleInstanceByID dispatches GET /v1/instances/{id}.
// PASS 1: GET only. DELETE and POST /stop|start|reboot added in PASS 2.
func (s *server) handleInstanceByID(w http.ResponseWriter, r *http.Request) {
	// Strip the /v1/instances/ prefix and take the first segment as the ID.
	// Subpaths (e.g. /stop, /jobs/...) are handled in PASS 2 and PASS 3.
	id := strings.TrimPrefix(r.URL.Path, "/v1/instances/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetInstance(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ── POST /v1/instances ────────────────────────────────────────────────────────

// handleCreateInstance handles POST /v1/instances.
// Returns 202 Accepted + CreateInstanceResponse (instance record only in PASS 1).
// Source: 08-01 §CreateInstance, INSTANCE_MODEL_V1 §2.
func (s *server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	var req CreateInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, errInvalidRequest,
			"Request body is not valid JSON.", "")
		return
	}

	if errs := validateCreateRequest(&req); len(errs) > 0 {
		writeAPIErrors(w, errs)
		return
	}

	instanceID := idgen.New(idgen.PrefixInstance)
	row := &db.InstanceRow{
		ID:               instanceID,
		Name:             req.Name,
		OwnerPrincipalID: ownerFromRequest(r), // placeholder until PASS 2 auth
		VMState:          "requested",
		InstanceTypeID:   req.InstanceType,
		ImageID:          req.ImageID,
		AvailabilityZone: req.AvailabilityZone,
	}

	if err := s.repo.InsertInstance(r.Context(), row); err != nil {
		s.log.Error("InsertInstance failed", "error", err)
		writeInternalError(w)
		return
	}

	// Reload to pick up DB-generated created_at / updated_at timestamps.
	created, err := s.repo.GetInstanceByID(r.Context(), instanceID)
	if err != nil {
		s.log.Error("GetInstanceByID after insert failed", "error", err)
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusAccepted, CreateInstanceResponse{
		Instance: instanceToResponse(created, s.region),
	})
}

// ── GET /v1/instances ─────────────────────────────────────────────────────────

// handleListInstances handles GET /v1/instances.
// Returns 200 + ListInstancesResponse, scoped to the calling principal.
// PASS 1: principal derived from placeholder; real auth added in PASS 2.
// Source: 08-01 §ListInstances.
func (s *server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	ownerID := ownerFromRequest(r)

	rows, err := s.repo.ListInstancesByOwner(r.Context(), ownerID)
	if err != nil {
		s.log.Error("ListInstancesByOwner failed", "error", err)
		writeInternalError(w)
		return
	}

	out := make([]InstanceResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, instanceToResponse(row, s.region))
	}

	writeJSON(w, http.StatusOK, ListInstancesResponse{
		Instances: out,
		Total:     len(out),
	})
}

// ── GET /v1/instances/{id} ────────────────────────────────────────────────────

// handleGetInstance handles GET /v1/instances/{id}.
// Returns 200 + InstanceResponse or 404.
// PASS 1: no ownership check yet — added in PASS 2.
// Source: 08-01 §GetInstance.
func (s *server) handleGetInstance(w http.ResponseWriter, r *http.Request, id string) {
	row, err := s.repo.GetInstanceByID(r.Context(), id)
	if err != nil {
		// GetInstanceByID wraps pgx.ErrNoRows as "no rows in result set".
		if isNoRows(err) {
			writeAPIError(w, http.StatusNotFound, errInstanceNotFound,
				"The instance does not exist.", "id")
			return
		}
		s.log.Error("GetInstanceByID failed", "error", err)
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusOK, instanceToResponse(row, s.region))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// instanceToResponse maps a db.InstanceRow to the canonical InstanceResponse.
// Source: INSTANCE_MODEL_V1 §2, §4.
func instanceToResponse(row *db.InstanceRow, region string) InstanceResponse {
	labels := map[string]string{}
	return InstanceResponse{
		ID:               row.ID,
		Name:             row.Name,
		Status:           row.VMState,
		InstanceType:     row.InstanceTypeID,
		ImageID:          row.ImageID,
		AvailabilityZone: row.AvailabilityZone,
		Region:           region,
		Labels:           labels,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

// ownerFromRequest returns the principal ID for the request.
// PASS 1 placeholder: reads the X-Principal-ID header directly (no validation).
// Replaced in PASS 2 by requirePrincipal middleware + principalFromCtx.
func ownerFromRequest(r *http.Request) string {
	return r.Header.Get("X-Principal-ID")
}

// isNoRows detects the "no rows" error returned by the DB layer.
// The repo wraps pgx.ErrNoRows as a string containing "no rows in result set".
func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no rows in result set")
}
