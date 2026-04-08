package domainmodel

import "net/http"

// ErrorEnvelope is the canonical API error response. Source: API_ERROR_CONTRACT_V1 §1.
type ErrorEnvelope struct {
	Error     ErrorDetail `json:"error"`
	RequestID string      `json:"request_id,omitempty"`
}

// ErrorDetail contains the structured error fields.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

// Error code constants. Source: API_ERROR_CONTRACT_V1 §4.
const (
	ErrCodeNotFound             = "INSTANCE_NOT_FOUND"
	ErrCodeInvalidState         = "INVALID_STATE_TRANSITION"
	ErrCodeInvalidRequest       = "INVALID_REQUEST"
	ErrCodeCapacityExceeded     = "CAPACITY_EXCEEDED"
	ErrCodeAuthRequired         = "AUTH_REQUIRED"
	ErrCodeForbidden            = "FORBIDDEN"
	ErrCodeConflict             = "CONFLICT"
	ErrCodeInternalError        = "INTERNAL_ERROR"
	ErrCodeHostUnreachable      = "HOST_UNREACHABLE"
	ErrCodeProvisioningFailed   = "PROVISIONING_FAILED"
)

// ErrorCodeToHTTPStatus maps error codes to HTTP status codes. Source: API_ERROR_CONTRACT_V1 §2.
func ErrorCodeToHTTPStatus(code string) int {
	switch code {
	case ErrCodeNotFound:
		return http.StatusNotFound
	case ErrCodeInvalidState, ErrCodeConflict:
		return http.StatusConflict
	case ErrCodeInvalidRequest:
		return http.StatusBadRequest
	case ErrCodeCapacityExceeded:
		return http.StatusUnprocessableEntity
	case ErrCodeAuthRequired:
		return http.StatusUnauthorized
	case ErrCodeForbidden:
		// Auth rule: return 404 on cross-account access, not 403. Source: AUTH_OWNERSHIP_MODEL_V1.
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
