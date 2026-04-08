package main

// instance_errors.go — Structured API error envelope for public instance endpoints.
//
// All public-facing errors flow through writeAPIError.
// Internal details (DB errors, stack traces) are never written to responses.
//
// Source: API_ERROR_CONTRACT_V1 §1 (envelope shape),
//         §2 (HTTP status mapping),
//         §4 (error code catalog),
//         §7 (invariant: request_id always present).

import (
	"net/http"

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
	errInternalError       = "internal_error"
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
