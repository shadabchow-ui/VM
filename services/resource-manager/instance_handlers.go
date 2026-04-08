package main

// instance_handlers.go — HTTP handlers for the public instance management API.
//
// PASS 1: POST /v1/instances, GET /v1/instances, GET /v1/instances/{id}.
// PASS 2: auth middleware wired, ownership enforced, lifecycle endpoints added:
//         DELETE /v1/instances/{id}
//         POST   /v1/instances/{id}/stop
//         POST   /v1/instances/{id}/start
//         POST   /v1/instances/{id}/reboot
// PASS 3 (not yet): idempotency, job status endpoint.
//
// Control-plane rule: lifecycle handlers enqueue a job via InsertJob.
// They never call runtime directly. Workers drive all state transitions.
// Source: JOB_MODEL_V1, core-architecture-blueprint §control-plane semantics.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
	statemachine "github.com/compute-platform/compute-platform/packages/state-machine"
)

// Job type constants — aligned with JOB_MODEL_V1 §3 and worker handler names.
const (
	jobTypeStop   = "INSTANCE_STOP"
	jobTypeStart  = "INSTANCE_START"
	jobTypeReboot = "INSTANCE_REBOOT"
	jobTypeDelete = "INSTANCE_DELETE"
)

// jobMaxAttempts per job type. Source: JOB_MODEL_V1 §3.
var jobMaxAttempts = map[string]int{
	jobTypeStop:   5,
	jobTypeStart:  5,
	jobTypeReboot: 5,
	jobTypeDelete: 5,
}

// ── Route registration ────────────────────────────────────────────────────────

// registerInstanceRoutes registers the public instance API routes.
// PASS 2: requirePrincipal wraps all handlers.
func (s *server) registerInstanceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/instances", requirePrincipal(s.handleInstanceRoot))
	mux.HandleFunc("/v1/instances/", requirePrincipal(s.handleInstanceByID))
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

// handleInstanceByID dispatches based on method and subpath.
// Routes:
//   GET    /v1/instances/{id}         → handleGetInstance
//   DELETE /v1/instances/{id}         → handleDeleteInstance
//   POST   /v1/instances/{id}/stop    → handleLifecycleAction(stop)
//   POST   /v1/instances/{id}/start   → handleLifecycleAction(start)
//   POST   /v1/instances/{id}/reboot  → handleLifecycleAction(reboot)
func (s *server) handleInstanceByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/instances/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	if id == "" {
		http.NotFound(w, r)
		return
	}

	// /v1/instances/{id}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.handleGetInstance(w, r, id)
		case http.MethodDelete:
			s.handleDeleteInstance(w, r, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	// /v1/instances/{id}/{subpath}
	subpath := parts[1]
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch subpath {
	case "stop":
		s.handleLifecycleAction(w, r, id, statemachine.ActionStop, jobTypeStop)
	case "start":
		s.handleLifecycleAction(w, r, id, statemachine.ActionStart, jobTypeStart)
	case "reboot":
		s.handleLifecycleAction(w, r, id, statemachine.ActionReboot, jobTypeReboot)
	default:
		http.NotFound(w, r)
	}
}

// ── POST /v1/instances ────────────────────────────────────────────────────────

// handleCreateInstance handles POST /v1/instances.
// Returns 202 Accepted + CreateInstanceResponse.
// Source: 08-01 §CreateInstance, INSTANCE_MODEL_V1 §2.
func (s *server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

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
		OwnerPrincipalID: principal,
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
// Returns 200 + ListInstancesResponse scoped to the calling principal.
// Source: 08-01 §ListInstances.
func (s *server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	rows, err := s.repo.ListInstancesByOwner(r.Context(), principal)
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
// PASS 2: ownership enforced via loadOwnedInstance.
// Source: 08-01 §GetInstance, AUTH_OWNERSHIP_MODEL_V1 §3.
func (s *server) handleGetInstance(w http.ResponseWriter, r *http.Request, id string) {
	principal, _ := principalFromCtx(r.Context())

	row, ok := s.loadOwnedInstance(w, r, principal, id)
	if !ok {
		return
	}

	writeJSON(w, http.StatusOK, instanceToResponse(row, s.region))
}

// ── DELETE /v1/instances/{id} ─────────────────────────────────────────────────

// handleDeleteInstance handles DELETE /v1/instances/{id}.
// Returns 202 + LifecycleResponse with the enqueued job_id.
// Source: 04-02-lifecycle-action-flows.md §INSTANCE_DELETE, JOB_MODEL_V1.
func (s *server) handleDeleteInstance(w http.ResponseWriter, r *http.Request, id string) {
	s.handleLifecycleAction(w, r, id, statemachine.ActionDelete, jobTypeDelete)
}

// ── Lifecycle action shared handler ──────────────────────────────────────────

// handleLifecycleAction is the shared handler for stop, start, reboot, delete.
// Validates transition, enqueues job, returns 202.
// Never calls runtime directly — jobs consumed by worker.
// Source: LIFECYCLE_STATE_MACHINE_V1, JOB_MODEL_V1, 04-02-lifecycle-action-flows.md.
func (s *server) handleLifecycleAction(
	w http.ResponseWriter,
	r *http.Request,
	id string,
	action statemachine.Action,
	jobType string,
) {
	principal, _ := principalFromCtx(r.Context())

	row, ok := s.loadOwnedInstance(w, r, principal, id)
	if !ok {
		return
	}

	// Validate state transition before enqueuing.
	// Source: LIFECYCLE_STATE_MACHINE_V1, API_ERROR_CONTRACT_V1 §4.
	if _, err := statemachine.Transition(statemachine.State(row.VMState), action); err != nil {
		writeAPIError(w, http.StatusConflict, errIllegalTransition,
			fmt.Sprintf("Cannot perform '%s' on an instance in state '%s'.", action, row.VMState),
			"status")
		return
	}

	jobID := idgen.New(idgen.PrefixJob)
	if err := s.repo.InsertJob(r.Context(), &db.JobRow{
		ID:          jobID,
		InstanceID:  row.ID,
		JobType:     jobType,
		MaxAttempts: jobMaxAttempts[jobType],
	}); err != nil {
		s.log.Error("InsertJob failed", "job_type", jobType, "error", err)
		writeInternalError(w)
		return
	}

	writeJSON(w, http.StatusAccepted, LifecycleResponse{
		InstanceID: row.ID,
		JobID:      jobID,
		Action:     string(action),
	})
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// instanceToResponse maps a db.InstanceRow to the canonical InstanceResponse.
// Source: INSTANCE_MODEL_V1 §2, §4.
func instanceToResponse(row *db.InstanceRow, region string) InstanceResponse {
	return InstanceResponse{
		ID:               row.ID,
		Name:             row.Name,
		Status:           row.VMState,
		InstanceType:     row.InstanceTypeID,
		ImageID:          row.ImageID,
		AvailabilityZone: row.AvailabilityZone,
		Region:           region,
		Labels:           map[string]string{},
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

// isNoRows detects the "no rows" error returned by the DB layer.
func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no rows in result set")
}
