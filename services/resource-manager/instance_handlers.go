package main

// instance_handlers.go — HTTP handlers for the public instance management API.
//
// PASS 1: POST /v1/instances, GET /v1/instances, GET /v1/instances/{id}.
// PASS 2: auth middleware wired, ownership enforced, lifecycle endpoints added:
//         DELETE /v1/instances/{id}
//         POST   /v1/instances/{id}/stop
//         POST   /v1/instances/{id}/start
//         POST   /v1/instances/{id}/reboot
// PASS 3: Idempotency-Key support for create and lifecycle actions.
//         GET /v1/instances/{id}/jobs/{job_id} — job status endpoint.
// P2-M1/WS-H1: All DB-error paths now call writeDBError(w, s.log, err) which
//         returns 503 for transient connectivity failures and 500 for all others.
//         Gate item DB-6: API must return 503 (not 500, not hang) with request_id
//         during the PostgreSQL primary failover window.
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

// idempotencyHeader is the canonical header name for request deduplication.
// Source: JOB_MODEL_V1 §6, 03-02-async-job-model §idempotency.
const idempotencyHeader = "Idempotency-Key"

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
//
//	GET    /v1/instances/{id}                 → handleGetInstance
//	DELETE /v1/instances/{id}                 → handleDeleteInstance
//	POST   /v1/instances/{id}/stop            → handleLifecycleAction(stop)
//	POST   /v1/instances/{id}/start           → handleLifecycleAction(start)
//	POST   /v1/instances/{id}/reboot          → handleLifecycleAction(reboot)
//	GET    /v1/instances/{id}/jobs/{job_id}   → handleGetJob  (PASS 3)
func (s *server) handleInstanceByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/instances/")
	if rest == "" {
		http.NotFound(w, r)
		return
	}

	parts := strings.SplitN(rest, "/", 3)
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

	// /v1/instances/{id}/{subpath}[/{rest}]
	subpath := parts[1]

	// GET /v1/instances/{id}/jobs/{job_id}  (PASS 3)
	if subpath == "jobs" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if len(parts) < 3 || parts[2] == "" {
			http.NotFound(w, r)
			return
		}
		s.handleGetJob(w, r, id, parts[2])
		return
	}

	// GET /v1/instances/{id}/events  (M7)
	if subpath == "events" {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleListEvents(w, r, id)
		return
	}

	// POST lifecycle subpaths.
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
//
// PASS 3 idempotency: if Idempotency-Key header is present, composite key
// "{principalID}:{key}:create" is stored as an INSTANCE_CREATE sentinel job so
// that a duplicate request returns the same instance without creating a new one.
// If the header is absent, current behaviour is preserved unchanged.
//
// Source: 08-01 §CreateInstance, INSTANCE_MODEL_V1 §2, JOB_MODEL_V1 §6.
func (s *server) handleCreateInstance(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFromCtx(r.Context())

	// PASS 3: idempotency check — must run before any DB write.
	// Source: JOB_MODEL_V1 §10 "Idempotency key checked before job creation".
	if ikey := r.Header.Get(idempotencyHeader); ikey != "" {
		compositeKey := fmt.Sprintf("%s:%s:create", principal, ikey)
		existing, err := s.repo.GetJobByIdempotencyKey(r.Context(), compositeKey)
		if err != nil {
			s.log.Error("GetJobByIdempotencyKey failed on create", "error", err)
			writeDBError(w, err)
			return
		}
		if existing != nil {
			// Duplicate: return the original instance response.
			inst, err := s.repo.GetInstanceByID(r.Context(), existing.InstanceID)
			if err != nil {
				s.log.Error("idempotent create: GetInstanceByID failed", "error", err)
				writeDBError(w, err)
				return
			}
			ip, _ := s.repo.GetIPByInstance(r.Context(), existing.InstanceID)
			writeJSON(w, http.StatusAccepted, CreateInstanceResponse{
				Instance: instanceToResponse(inst, s.region, ip),
			})
			return
		}
	}

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
		writeDBError(w, err)
		return
	}

// M9: create networking if requested
var nic *db.NetworkInterfaceRow
if req.Networking != nil && req.Networking.SubnetID != "" {
	var err error
	nic, err = s.createInstanceNetworking(w, r, instanceID, principal, req.Networking)
	if err != nil {
		// rollback instance on failure
		return
	}
}

	// PASS 3: persist sentinel job so subsequent duplicate requests deduplicate.
	// Non-fatal: log on failure but do not fail the create response.
	// Source: JOB_MODEL_V1 §6.
	if ikey := r.Header.Get(idempotencyHeader); ikey != "" {
		compositeKey := fmt.Sprintf("%s:%s:create", principal, ikey)
		sentinelID := idgen.New(idgen.PrefixJob)
		if err := s.repo.InsertJob(r.Context(), &db.JobRow{
			ID:             sentinelID,
			InstanceID:     instanceID,
			JobType:        "INSTANCE_CREATE",
			MaxAttempts:    3,
			IdempotencyKey: compositeKey,
		}); err != nil {
			s.log.Warn("InsertJob sentinel for create idempotency failed", "error", err)
		}
	}

	created, err := s.repo.GetInstanceByID(r.Context(), instanceID)
	if err != nil {
		s.log.Error("GetInstanceByID after insert failed", "error", err)
		writeDBError(w, err)
		return
	}

	ip, _ := s.repo.GetIPByInstance(r.Context(), created.ID)
	resp := instanceToResponse(created, s.region, ip)

	if nic != nil {
		resp.Networking = &InstanceNetworkingResponse{
			VPCID:    nic.VPCID,
			SubnetID: nic.SubnetID,
			PrimaryInterface: &NetworkInterfaceResponse{
				ID:         nic.ID,
				PrivateIP:  nic.PrivateIP,
				MACAddress: nic.MACAddress,
				Status:     nic.Status,
			},
		}
		resp.PrivateIP = &nic.PrivateIP
		resp.PublicIP = nil
	}

	writeJSON(w, http.StatusAccepted, CreateInstanceResponse{
		Instance: resp,
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
		writeDBError(w, err)
		return
	}

	out := make([]InstanceResponse, 0, len(rows))
	for _, row := range rows {
		ip, _ := s.repo.GetIPByInstance(r.Context(), row.ID)
		resp := instanceToResponse(row, s.region, ip)
s.enrichResponseWithNetworking(r.Context(), &resp, row.ID)
out = append(out, resp)
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

	ip, _ := s.repo.GetIPByInstance(r.Context(), row.ID)
	resp := instanceToResponse(row, s.region, ip)
s.enrichResponseWithNetworking(r.Context(), &resp, row.ID)
writeJSON(w, http.StatusOK, resp)
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
//
// PASS 3 idempotency: if Idempotency-Key header is present, composite key
// "{principalID}:{key}:{jobType}" is looked up. If an existing job is found:
//   - same instance → return original LifecycleResponse (deduplicated)
//   - different instance → 409 idempotency_key_mismatch
//
// If the header is absent, current behaviour is preserved unchanged.
//
// Source: LIFECYCLE_STATE_MACHINE_V1, JOB_MODEL_V1 §6, 04-02-lifecycle-action-flows.md.
func (s *server) handleLifecycleAction(
	w http.ResponseWriter,
	r *http.Request,
	id string,
	action statemachine.Action,
	jobType string,
) {
	principal, _ := principalFromCtx(r.Context())

	// PASS 3: idempotency check — before ownership or state checks.
	// Source: JOB_MODEL_V1 §10 "Idempotency key checked before job creation".
	if ikey := r.Header.Get(idempotencyHeader); ikey != "" {
		compositeKey := fmt.Sprintf("%s:%s:%s", principal, ikey, jobType)
		existing, err := s.repo.GetJobByIdempotencyKey(r.Context(), compositeKey)
		if err != nil {
			s.log.Error("GetJobByIdempotencyKey failed", "job_type", jobType, "error", err)
			writeDBError(w, err)
			return
		}
		if existing != nil {
			// Same key reused for a different instance → payload mismatch.
			// Source: JOB_MODEL_V1 §6 "Key found, payload differs → Reject".
			if existing.InstanceID != id {
				writeAPIError(w, http.StatusConflict, errIdempotencyMismatch,
					"The Idempotency-Key has been used with a different request.", idempotencyHeader)
				return
			}
			// Duplicate request: return the original LifecycleResponse.
			writeJSON(w, http.StatusAccepted, LifecycleResponse{
				InstanceID: existing.InstanceID,
				JobID:      existing.ID,
				Action:     string(action),
			})
			return
		}
	}

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

	// Build the idempotency key for the new job.
	// Empty string when no header was provided — matches existing ON CONFLICT behaviour.
	var idempKey string
	if ikey := r.Header.Get(idempotencyHeader); ikey != "" {
		idempKey = fmt.Sprintf("%s:%s:%s", principal, ikey, jobType)
	}

	if err := s.repo.InsertJob(r.Context(), &db.JobRow{
		ID:             jobID,
		InstanceID:     row.ID,
		JobType:        jobType,
		MaxAttempts:    jobMaxAttempts[jobType],
		IdempotencyKey: idempKey,
	}); err != nil {
		s.log.Error("InsertJob failed", "job_type", jobType, "error", err)
		writeDBError(w, err)
		return
	}

	writeJSON(w, http.StatusAccepted, LifecycleResponse{
		InstanceID: row.ID,
		JobID:      jobID,
		Action:     string(action),
	})
}

// ── GET /v1/instances/{id}/jobs/{job_id} ──────────────────────────────────────

// handleGetJob handles GET /v1/instances/{id}/jobs/{job_id}.
// Returns 202 + JobResponse or 404.
//
// Ownership is enforced: the instance must be owned by the calling principal.
// The job must belong to the specified instance (instance_id FK validated in repo).
// Neither the instance nor the job existence is leaked on ownership failures.
//
// Source: JOB_MODEL_V1 §1, AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1 §3.
func (s *server) handleGetJob(w http.ResponseWriter, r *http.Request, instanceID, jobID string) {
	principal, _ := principalFromCtx(r.Context())

	// Step 1: enforce instance ownership — 404 on any miss or cross-account access.
	_, ok := s.loadOwnedInstance(w, r, principal, instanceID)
	if !ok {
		return
	}

	// Step 2: fetch job, validating it belongs to this instance.
	job, err := s.repo.GetJobByInstanceAndID(r.Context(), instanceID, jobID)
	if err != nil {
		s.log.Error("GetJobByInstanceAndID failed", "instance_id", instanceID, "job_id", jobID, "error", err)
		writeDBError(w, err)
		return
	}
	if job == nil {
		writeAPIError(w, http.StatusNotFound, errJobNotFound,
			"The job does not exist or does not belong to this instance.", "job_id")
		return
	}

	writeJSON(w, http.StatusAccepted, jobToResponse(job))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// writeDBError selects the correct HTTP error response for a DB error.
// Transient connectivity failures (primary failover) → 503 writeServiceUnavailable.
// All other DB errors → 500 writeInternalError.
//
// This is the single call site for all repo-layer errors in this package.
// Do not call writeInternalError directly for DB errors — always use writeDBError.
//
// Source: P2_M1_WS_H1_DB_HA_RUNBOOK §6 Step 11 (gate item DB-6).
func writeDBError(w http.ResponseWriter, err error) {
	if isDBUnavailableError(err) {
		writeServiceUnavailable(w)
	} else {
		writeInternalError(w)
	}
}

// instanceToResponse maps a db.InstanceRow to the canonical InstanceResponse.
// M7: ip param carries the allocated IP from GetIPByInstance (empty string = nil fields).
// Source: INSTANCE_MODEL_V1 §2, §4.
func instanceToResponse(row *db.InstanceRow, region, ip string) InstanceResponse {
	resp := InstanceResponse{
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
	if ip != "" {
		// Phase 1: single VPC, public and private IP are both the allocated address.
		// NAT model documented in 07-01-phase-1-network-architecture.
		resp.PublicIP = &ip
		resp.PrivateIP = &ip
	}
	return resp
}

// jobToResponse maps a db.JobRow to the canonical JobResponse.
// Internal-only fields (idempotency_key, claimed_at) are not exposed.
// Source: JOB_MODEL_V1 §1.
func jobToResponse(row *db.JobRow) JobResponse {
	return JobResponse{
		ID:           row.ID,
		InstanceID:   row.InstanceID,
		JobType:      row.JobType,
		Status:       row.Status,
		AttemptCount: row.AttemptCount,
		MaxAttempts:  row.MaxAttempts,
		ErrorMessage: row.ErrorMessage,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
		CompletedAt:  row.CompletedAt,
	}
}

// isNoRows detects the "no rows" error returned by the DB layer.
func isNoRows(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no rows in result set")
}
