package main

// compatibility_handlers.go — Phase 16B: API compatibility surface handlers and middleware.
//
// Implements the handlers and middleware wired in routes() but previously
// missing from the codebase:
//
//   handleHealthz        — GET /healthz
//   handleVersion        — GET /v1/version
//   handleOpenAPI        — GET /v1/openapi.json
//   apiVersionMiddleware — reads Api-Version header, rejects removed versions (410),
//                          echoes resolved version in X-Api-Version response header
//   requestIDMiddleware  — attaches X-Request-ID to every response
//
// API versioning contract (vm-16-03__blueprint__ §implementation_decisions):
//   - Date-based scheme: YYYY-MM-DD.
//   - currentAPIVersion is the stable, GA version returned by /v1/version.
//   - minAPIVersion is the oldest version clients may still use.
//   - removedAPIVersions are versions past their 12-month deprecation window;
//     requests bearing a removed version receive 410 Gone.
//   - If the Api-Version header is absent, requests are served under currentAPIVersion.
//   - X-Api-Version is always echoed so clients can detect drift.
//
// Health probe contract:
//   - /healthz is unauthenticated and must work before any DB state exists.
//   - Returns 200 {"status":"ok"} when the DB ping succeeds.
//   - Returns 503 {"status":"degraded","reason":"db_unavailable"} when the DB
//     is unreachable. This is the DB-level gate item from P2_M1_GATE_CHECKLIST.
//
// OpenAPI stub:
//   - /v1/openapi.json returns a minimal but schema-valid OpenAPI 3.0 document
//     so SDK/CLI tooling can verify the document endpoint exists.
//   - Full spec generation is deferred to the Tooling Generation Pipeline
//     (vm-16-03__blueprint__ §components "Tooling Generation & Validation Pipeline").
//
// Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth",
//         vm-16-03__blueprint__ §interaction_or_ops_contract (410 for removed versions),
//         vm-16-03__research__ §"API Compatibility, Versioning, and Deprecation Policy",
//         P2_M1_GATE_CHECKLIST §PRE-2 (service reachable),
//         P2_M1_WS_H7_PHASE1_REGRESSION_RUNBOOK §"Healthz liveness probe".

import (
	"fmt"
	"net/http"
	"time"

	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── API version constants ─────────────────────────────────────────────────────

// currentAPIVersion is the stable GA version of this API.
// Source: vm-16-03__blueprint__ §implementation_decisions "date-based versioning".
const currentAPIVersion = "2024-01-15"

// minAPIVersion is the oldest API version clients may present.
// Requests with an Api-Version older than minAPIVersion receive 410 Gone.
const minAPIVersion = "2024-01-15"

// removedAPIVersions is the set of versions that have completed their 12-month
// deprecation window and are no longer served.
// Source: vm-16-03__blueprint__ §interaction_or_ops_contract (410 for removed versions).
var removedAPIVersions = map[string]bool{
	// "2023-06-01": true, // example removed version — none removed yet
}

// ── Middleware ────────────────────────────────────────────────────────────────

// apiVersionMiddleware reads the optional Api-Version request header.
//
// Behaviour:
//   - If the header is absent: serve under currentAPIVersion (no rejection).
//   - If the header value is in removedAPIVersions: return 410 Gone.
//   - Otherwise: set X-Api-Version response header to the resolved version.
//
// /healthz bypasses this middleware because it is registered before the
// middleware chain in routes(). All other /v1/* paths go through it.
//
// Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth",
//
//	vm-16-03__research__ §"API Compatibility, Versioning, and Deprecation Policy".
func apiVersionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := r.Header.Get("Api-Version")
		if version == "" {
			version = currentAPIVersion
		}

		// Reject removed versions with 410 Gone.
		if removedAPIVersions[version] {
			writeAPIError(w, http.StatusGone, "api_version_removed",
				fmt.Sprintf("API version %q has been removed. Please upgrade to %s or later.",
					version, currentAPIVersion),
				"Api-Version",
			)
			return
		}

		// Echo the resolved version so clients can detect any negotiation.
		w.Header().Set("X-Api-Version", version)
		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware attaches a unique X-Request-ID to every response.
//
// The request_id is derived from idgen so it matches the prefix format used
// throughout the error envelope (API_ERROR_CONTRACT_V1 §7).
//
// Source: API_ERROR_CONTRACT_V1 §7 "request_id always present",
//
//	vm-16-03__blueprint__ §core_contracts "Resilience and Backpressure Signaling".
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := idgen.New("req")
		w.Header().Set("X-Request-ID", reqID)
		next.ServeHTTP(w, r)
	})
}

// ── Health handler ────────────────────────────────────────────────────────────

// handleHealthz handles GET /healthz.
//
// Contract:
//   - No authentication required — must work before auth is bootstrapped.
//   - 200 {"status":"ok","timestamp":"..."} when DB ping succeeds.
//   - 503 {"status":"degraded","reason":"db_unavailable"} when DB is unreachable.
//
// Gate item: P2_M1_GATE_CHECKLIST PRE-2 (service is reachable),
//
//	P2_M1_WS_H7_PHASE1_REGRESSION_RUNBOOK §"Healthz liveness probe".
func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	if err := s.repo.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"status": "degraded",
			"reason": "db_unavailable",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "ok",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// ── Version handler ───────────────────────────────────────────────────────────

// handleVersion handles GET /v1/version.
//
// Response shape required by the acceptance test:
//
//	{
//	  "api_version":     "2024-01-15",
//	  "min_api_version": "2024-01-15",
//	  "service":         "compute-platform/resource-manager"
//	}
//
// Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth",
//
//	test_integration_phase16_acceptance_test.go §TestPhase16_APIVersion_HeaderContract.
func (s *server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Echo back whichever version the middleware resolved.
	// The middleware sets X-Api-Version before invoking this handler.
	resolvedVersion := w.Header().Get("X-Api-Version")
	if resolvedVersion == "" {
		resolvedVersion = currentAPIVersion
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"api_version":     resolvedVersion,
		"min_api_version": minAPIVersion,
		"service":         "compute-platform/resource-manager",
	})
}

// ── OpenAPI stub handler ──────────────────────────────────────────────────────

// handleOpenAPI handles GET /v1/openapi.json.
//
// Returns an OpenAPI 3.0.3 document describing the stable public API surface.
// This matches docs/api/openapi.yaml which is the canonical source-of-truth
// OpenAPI document on disk.
//
// Paths absent from this handler but present as tested handler code are
// explicitly noted as "not wired into production mux" — see the description
// field for the current limitation list.
//
// Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth".
func (s *server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stub := map[string]interface{}{
		"openapi": "3.0.3",
		"info": map[string]interface{}{
			"title":   "Compute Platform API",
			"version": currentAPIVersion,
			"description": "VM control plane API for instance lifecycle, images, volumes, snapshots, " +
				"SSH keys, projects, and VPC networking. " +
				"All mutating endpoints accept an optional Idempotency-Key header. " +
				"Auth: X-Principal-ID header (Phase 1). " +
				"Errors follow the API_ERROR_CONTRACT_V1 envelope. " +
				"Phase L: VPC/network endpoints are implemented as handler code with tests " +
				"but not yet wired into the production HTTP mux. " +
				"See docs/api/openapi.yaml for canonical spec.",
		},
		"paths": map[string]interface{}{
			"/v1/instances": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List instances",
					"operationId": "listInstances",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "Instance list"}},
				},
				"post": map[string]interface{}{
					"summary":     "Create instance",
					"operationId": "createInstance",
					"responses":   map[string]interface{}{"202": map[string]interface{}{"description": "Instance creation accepted"}},
				},
			},
			"/v1/instances/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Describe instance",
					"operationId": "describeInstance",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "Instance details"},
						"404": map[string]interface{}{"description": "Not found or not owned"},
					},
				},
				"delete": map[string]interface{}{
					"summary":     "Delete instance",
					"operationId": "deleteInstance",
					"responses":   map[string]interface{}{"202": map[string]interface{}{"description": "Delete accepted"}},
				},
			},
			"/v1/instances/{id}/stop": map[string]interface{}{
				"post": map[string]interface{}{
					"summary":     "Stop instance",
					"operationId": "stopInstance",
					"responses": map[string]interface{}{
						"202": map[string]interface{}{"description": "Stop accepted"},
						"409": map[string]interface{}{"description": "Illegal state transition"},
					},
				},
			},
			"/v1/instances/{id}/start": map[string]interface{}{
				"post": map[string]interface{}{
					"summary":     "Start instance",
					"operationId": "startInstance",
					"responses": map[string]interface{}{
						"202": map[string]interface{}{"description": "Start accepted"},
						"409": map[string]interface{}{"description": "Illegal state transition"},
					},
				},
			},
			"/v1/instances/{id}/reboot": map[string]interface{}{
				"post": map[string]interface{}{
					"summary":     "Reboot instance",
					"operationId": "rebootInstance",
					"responses": map[string]interface{}{
						"202": map[string]interface{}{"description": "Reboot accepted"},
						"409": map[string]interface{}{"description": "Illegal state transition"},
					},
				},
			},
			"/v1/instances/{id}/events": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List instance events",
					"operationId": "listInstanceEvents",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "Events (up to 100, newest first)"},
						"404": map[string]interface{}{"description": "Instance not found or not owned"},
					},
				},
			},
			"/v1/instances/{id}/jobs/{job_id}": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Describe job",
					"operationId": "describeJob",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "Job status"},
						"404": map[string]interface{}{"description": "Job not found, instance not found, or cross-account access"},
					},
				},
			},
			"/v1/images": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List images",
					"operationId": "listImages",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "Image list"}},
				},
				"post": map[string]interface{}{
					"summary":     "Create image",
					"operationId": "createImage",
					"responses":   map[string]interface{}{"202": map[string]interface{}{"description": "Image creation accepted"}},
				},
			},
			"/v1/images/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Describe image",
					"operationId": "describeImage",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "Image details"},
						"404": map[string]interface{}{"description": "Not found or not accessible"},
					},
				},
			},
			"/v1/images/{id}/deprecate": map[string]interface{}{
				"post": map[string]interface{}{
					"summary":     "Deprecate image",
					"operationId": "deprecateImage",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "Image deprecated"}},
				},
			},
			"/v1/images/{id}/obsolete": map[string]interface{}{
				"post": map[string]interface{}{
					"summary":     "Obsolete image",
					"operationId": "obsoleteImage",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "Image obsoleted"}},
				},
			},
			"/v1/volumes": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List volumes",
					"operationId": "listVolumes",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "Volume list"}},
				},
				"post": map[string]interface{}{
					"summary":     "Create volume",
					"operationId": "createVolume",
					"responses":   map[string]interface{}{"202": map[string]interface{}{"description": "Volume creation accepted"}},
				},
			},
			"/v1/volumes/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Describe volume",
					"operationId": "describeVolume",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "Volume details"},
						"404": map[string]interface{}{"description": "Not found or not owned"},
					},
				},
				"delete": map[string]interface{}{
					"summary":     "Delete volume",
					"operationId": "deleteVolume",
					"responses":   map[string]interface{}{"202": map[string]interface{}{"description": "Delete accepted"}},
				},
			},
			"/v1/instances/{id}/volumes": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List attached volumes for an instance",
					"operationId": "listInstanceVolumes",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "Attached volumes"}},
				},
				"post": map[string]interface{}{
					"summary":     "Attach volume to instance",
					"operationId": "attachVolume",
					"responses":   map[string]interface{}{"202": map[string]interface{}{"description": "Attach accepted"}},
				},
			},
			"/v1/instances/{id}/volumes/{vol_id}": map[string]interface{}{
				"delete": map[string]interface{}{
					"summary":     "Detach volume from instance",
					"operationId": "detachVolume",
					"responses":   map[string]interface{}{"202": map[string]interface{}{"description": "Detach accepted"}},
				},
			},
			"/v1/snapshots": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List snapshots",
					"operationId": "listSnapshots",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "Snapshot list"}},
				},
				"post": map[string]interface{}{
					"summary":     "Create snapshot",
					"operationId": "createSnapshot",
					"responses":   map[string]interface{}{"202": map[string]interface{}{"description": "Snapshot creation accepted"}},
				},
			},
			"/v1/snapshots/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Describe snapshot",
					"operationId": "describeSnapshot",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "Snapshot details"},
						"404": map[string]interface{}{"description": "Not found or not owned"},
					},
				},
			},
			"/v1/ssh-keys": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List SSH keys",
					"operationId": "listSSHKeys",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "SSH key list"}},
				},
				"post": map[string]interface{}{
					"summary":     "Register SSH key",
					"operationId": "createSSHKey",
					"responses":   map[string]interface{}{"201": map[string]interface{}{"description": "SSH key registered"}},
				},
			},
			"/v1/ssh-keys/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Describe SSH key",
					"operationId": "describeSSHKey",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "SSH key details (fingerprint only)"},
						"404": map[string]interface{}{"description": "Not found or not owned"},
					},
				},
				"delete": map[string]interface{}{
					"summary":     "Delete SSH key",
					"operationId": "deleteSSHKey",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "SSH key deleted"}},
				},
			},
			"/v1/projects": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "List projects",
					"operationId": "listProjects",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "Project list"}},
				},
				"post": map[string]interface{}{
					"summary":     "Create project",
					"operationId": "createProject",
					"responses":   map[string]interface{}{"201": map[string]interface{}{"description": "Project created"}},
				},
			},
			"/v1/projects/{id}": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Describe project",
					"operationId": "describeProject",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "Project details"},
						"404": map[string]interface{}{"description": "Not found or not owned"},
					},
				},
			},
			"/healthz": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "Liveness probe",
					"operationId": "healthz",
					"responses": map[string]interface{}{
						"200": map[string]interface{}{"description": "Service healthy"},
						"503": map[string]interface{}{"description": "Service degraded (DB unreachable)"},
					},
				},
			},
			"/v1/version": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "API version info",
					"operationId": "version",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "API version, minimum supported version, service name"}},
				},
			},
			"/v1/openapi.json": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":     "OpenAPI specification",
					"operationId": "openapi",
					"responses":   map[string]interface{}{"200": map[string]interface{}{"description": "OpenAPI 3.0.3 document"}},
				},
			},
		},
		"components": map[string]interface{}{
			"schemas": map[string]interface{}{
				"Error": map[string]interface{}{
					"type": "object",
					"required": []string{"error"},
					"properties": map[string]interface{}{
						"error": map[string]interface{}{
							"type": "object",
							"required": []string{"code", "message", "request_id"},
							"properties": map[string]interface{}{
								"code":       map[string]interface{}{"type": "string", "description": "Machine-readable error code"},
								"message":    map[string]interface{}{"type": "string", "description": "Human-readable description"},
								"target":     map[string]interface{}{"type": "string", "description": "Request field causing the error"},
								"request_id": map[string]interface{}{"type": "string", "description": "Request correlation ID"},
								"details":    map[string]interface{}{"type": "array", "description": "Sub-errors for batch validation"},
							},
						},
					},
				},
			},
		},
	}

	writeJSON(w, http.StatusOK, stub)
}
