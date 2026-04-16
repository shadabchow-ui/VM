package main

// instance_errors.go — Structured API error envelope for public instance endpoints.
//
// All public-facing errors flow through writeAPIError.
// Internal details (DB errors, stack traces) are never written to responses.
//
// PASS 3: Added errJobNotFound, errIdempotencyMismatch error codes.
// P2-M1/WS-H1: Added errServiceUnavailable, writeServiceUnavailable, isDBUnavailableError.
//   Gate item DB-6 requires HTTP 503 (not 500, not hang) with request_id when the
//   PostgreSQL primary is unavailable during a failover drill. Without this change
//   all DB connectivity errors produce HTTP 500, which fails DB-6.
// M10 Slice 4: Added errInvalidBlockDeviceMapping, errDeleteOnTerminationRequired.
//   Source: API_ERROR_CONTRACT_V1 §4 (error code catalog).
// VM-P2D Slice 4: Added errProjectNotFound.
//   Source: AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).
// VM-P16A: Added errServiceAccountNotFound, errRoleBindingNotFound,
//   errRoleBindingConflict, errBudgetExceeded.
//   Source: vm-16-01__blueprint__ §core_contracts (404-for-cross-account on SA),
//           vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement".
//
// Source: API_ERROR_CONTRACT_V1 §1 (envelope shape),
//         §2 (HTTP status mapping),
//         §4 (error code catalog),
//         §7 (invariant: request_id always present),
//         P2_M1_WS_H1_DB_HA_RUNBOOK §6 Step 11 (DB-6 pass conditions).

import (
	"net/http"
	"strings"

	"github.com/compute-platform/compute-platform/packages/idgen"
)

// Public error codes. Source: API_ERROR_CONTRACT_V1 §4.
const (
	errMissingField        = "missing_field"
	errInvalidValue        = "invalid_value"
	errInvalidInstanceType = "invalid_instance_type"
	errInvalidImageID      = "invalid_image_id"
	errInvalidAZ           = "invalid_availability_zone"
	errInvalidName         = "invalid_name"
	errInvalidRequest      = "invalid_request"
	errInstanceNotFound    = "instance_not_found"
	errJobNotFound         = "job_not_found"
	errInternalError       = "internal_error"
	errAuthRequired        = "authentication_required"
	errIllegalTransition   = "illegal_state_transition"
	errIdempotencyMismatch = "idempotency_key_mismatch"
	// errServiceUnavailable is returned when the PostgreSQL primary is unreachable.
	// Maps to HTTP 503. Source: API_ERROR_CONTRACT_V1 §2 and §4.
	// Added: P2-M1/WS-H1 gate item DB-6.
	errServiceUnavailable = "service_unavailable"
	// M10 Slice 4: block device mapping error codes.
	// Source: API_ERROR_CONTRACT_V1 §4 (invalid_block_device_mapping, delete_on_termination_required).
	errInvalidBlockDeviceMapping   = "invalid_block_device_mapping"
	errDeleteOnTerminationRequired = "delete_on_termination_required"

	// VM-P2D Slice 3: quota admission failure.
	// Returned when the create request exceeds the project's or account's instance
	// entitlement. Maps to HTTP 422 (client-correctable, not a platform capacity failure).
	// Must NOT be confused with errServiceUnavailable (503) or scheduler capacity failure.
	// Source: vm-13-02__blueprint__ §core_contracts "Error Code Separation".
	errQuotaExceeded = "quota_exceeded"

	// VM-P2D Slice 4: project scoping errors.
	// project_not_found is returned when the specified project_id does not exist,
	// is not owned by the calling principal (404-for-cross-account per
	// AUTH_OWNERSHIP_MODEL_V1 §3), is soft-deleted, or is not in active status.
	// Always HTTP 404 — never 403 — to prevent project enumeration.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1 §4.
	errProjectNotFound = "project_not_found"

	// VM-P2C-P3: image family alias errors.

	// VM-P16A: Service Account error codes.
	// service_account_not_found: SA does not exist, is soft-deleted, or belongs to
	// a different project (404-for-cross-account per AUTH_OWNERSHIP_MODEL_V1 §3).
	// Source: vm-16-01__blueprint__ §core_contracts, AUTH_OWNERSHIP_MODEL_V1 §3.
	errServiceAccountNotFound = "service_account_not_found"

	// VM-P16A: IAM Role Binding error codes.
	// role_binding_not_found: binding does not exist or belongs to a different project.
	// Source: vm-16-01__blueprint__ §components "IAM Policy Service".
	errRoleBindingNotFound = "role_binding_not_found"

	// role_binding_conflict: the (principal, role, resource) binding already exists.
	// Maps to HTTP 409. Source: API_ERROR_CONTRACT_V1 §2.
	errRoleBindingConflict = "role_binding_conflict"

	// VM-P16A: Budget enforcement error code.
	// budget_exceeded: the scope's active budget policy with enforcement_action=
	// 'block_create' has reached its spending limit. New resource creation is blocked.
	// Maps to HTTP 422 (client-correctable — reduce scope usage or raise limit).
	// Must NOT be confused with errQuotaExceeded (count-based) or
	// errServiceUnavailable (connectivity failure).
	// Source: vm-16-02__blueprint__ §core_contracts "Non-Destructive Budget Enforcement".
	errBudgetExceeded = "budget_exceeded"
)

// apiError is the structured error envelope sent in every error response.
// Source: API_ERROR_CONTRACT_V1 §1.
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Target    string         `json:"target,omitempty"`
	RequestID string         `json:"request_id"`
	Details   []apiErrorBody `json:"details"`
}

// writeAPIError writes a single-field error response.
// request_id is always generated and included, per API_ERROR_CONTRACT_V1 §7.
func writeAPIError(w http.ResponseWriter, status int, code, msg, target string) {
	reqID := idgen.New("req")
	env := apiError{
		Error: apiErrorBody{
			Code:      code,
			Message:   msg,
			Target:    target,
			RequestID: reqID,
			Details:   []apiErrorBody{},
		},
	}
	writeJSON(w, status, env)
}

// writeAPIErrors writes a 400 with one sub-error per failed field.
// Source: API_ERROR_CONTRACT_V1 §1 (details array).
func writeAPIErrors(w http.ResponseWriter, errs []fieldErr) {
	reqID := idgen.New("req")
	details := make([]apiErrorBody, 0, len(errs))
	for _, e := range errs {
		details = append(details, apiErrorBody{
			Code:    e.code,
			Message: e.message,
			Target:  e.target,
		})
	}
	env := apiError{
		Error: apiErrorBody{
			Code:      errInvalidRequest,
			Message:   "One or more request fields are invalid.",
			RequestID: reqID,
			Details:   details,
		},
	}
	writeJSON(w, http.StatusBadRequest, env)
}

// fieldErr is a single field-level validation failure used with writeAPIErrors.
type fieldErr struct {
	code    string
	message string
	target  string
}

// writeInternalError writes a safe 500 response. Never leaks internal detail.
// Source: API_ERROR_CONTRACT_V1 §7.
func writeInternalError(w http.ResponseWriter) {
	reqID := idgen.New("req")
	env := apiError{
		Error: apiErrorBody{
			Code:      errInternalError,
			Message:   "An internal error occurred.",
			RequestID: reqID,
			Details:   []apiErrorBody{},
		},
	}
	writeJSON(w, http.StatusInternalServerError, env)
}

// writeServiceUnavailable writes an HTTP 503 response with request_id.
//
// Called by any handler whose repo call returns an error that
// isDBUnavailableError classifies as a transient connectivity failure.
//
// DB-6 pass conditions (P2_M1_WS_H1_DB_HA_RUNBOOK §6 Step 11):
//   - HTTP status is 503 (not 500, not 502, not hang).
//   - Response completes within 2 seconds (non-blocking — writeJSON does not block).
//   - Response body contains request_id field.
//
// Source: API_ERROR_CONTRACT_V1 §2 (503), §4 (service_unavailable code), §7 (request_id).
func writeServiceUnavailable(w http.ResponseWriter) {
	reqID := idgen.New("req")
	env := apiError{
		Error: apiErrorBody{
			Code:      errServiceUnavailable,
			Message:   "Database unavailable. Please retry.",
			RequestID: reqID,
			Details:   []apiErrorBody{},
		},
	}
	writeJSON(w, http.StatusServiceUnavailable, env)
}

// isDBUnavailableError reports whether err represents a transient database
// connectivity failure — the class of errors produced during a PostgreSQL
// primary failover (connection refused, TCP reset, broken pipe, dial timeout,
// server restarting).
//
// Design constraints:
//   - lib/pq (the only driver; see go.mod) does not expose typed structs for
//     connection-class failures. They arrive as plain error values whose
//     message contains the relevant substrings.
//   - We match on the minimum conservative set of substrings that cover the
//     primary-failover failure surface. We intentionally do NOT match
//     application-level constraint errors (duplicate key, foreign key, syntax)
//     — those must remain 500/400.
//   - Matching is case-insensitive via strings.ToLower.
//   - If err is nil, returns false.
//
// Source: P2_M1_WS_H1_DB_HA_RUNBOOK §6 Step 11 (gate item DB-6),
//
//	P2_M1_INFRASTRUCTURE_HARDENING_PLAN §3.1.
func isDBUnavailableError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sub := range dbUnavailableSubstrings {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

// dbUnavailableSubstrings are the lowercase fragments that identify a transient
// DB connectivity failure vs. an application-level DB error.
// Keep this list minimal. False negatives (500 when 503 is correct) are safer
// than false positives (503 for logic bugs).
var dbUnavailableSubstrings = []string{
	"connection refused",
	"connection reset by peer",
	"broken pipe",
	"unexpected eof",
	"dial tcp",
	"no such host",
	"connection timed out",
	"i/o timeout",
	"server closed the connection unexpectedly",
	"the database system is starting up",
}
