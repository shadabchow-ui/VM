package main

// image_share_types.go — Public API request/response DTOs for image sharing.
//
// VM-P3B Job 1: cross-principal private image sharing contract.
//
// Endpoints:
//   POST   /v1/images/{id}/grants           → GrantImageAccess  (200)
//   DELETE /v1/images/{id}/grants/{grantee} → RevokeImageAccess (200)
//   GET    /v1/images/{id}/grants            → ListImageGrants   (200)
//
// Design rules:
//   - Owner-only: all three endpoints enforce owner identity; non-owners get 404.
//   - grantee_principal_id is the grantee's principal UUID (not a project ID).
//     The caller supplies the principal UUID directly.  Project-to-principal
//     resolution (if needed) is the caller's responsibility at the API surface.
//   - Response structs never expose owner_id (same rule as ImageResponse).
//   - Re-granting an existing grantee is idempotent (200, no error).
//   - Revoking a non-existent grant is idempotent (200, no error).
//
// Source: VM-P3B Job 1 §2–§4,
//         AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-non-owner),
//         API_ERROR_CONTRACT_V1.md §1 (envelope shape).

import "time"

// ── Grant resource ────────────────────────────────────────────────────────────

// ImageGrantResponse is the canonical JSON shape for one share grant.
type ImageGrantResponse struct {
	ID                 string    `json:"id"`
	ImageID            string    `json:"image_id"`
	GranteePrincipalID string    `json:"grantee_principal_id"`
	CreatedAt          time.Time `json:"created_at"`
}

// ── POST /v1/images/{id}/grants ───────────────────────────────────────────────

// GrantImageAccessRequest is the payload for POST /v1/images/{id}/grants.
//
// grantee_principal_id must be an existing principal UUID.
// Granting access to the image's own owner is a no-op (idempotent).
// Granting access to a PUBLIC image is rejected (no grant needed).
//
// Source: VM-P3B Job 1 §2.
type GrantImageAccessRequest struct {
	GranteePrincipalID string `json:"grantee_principal_id"`
}

// GrantImageAccessResponse is returned from POST /v1/images/{id}/grants with 200.
// Returns the grant (new or pre-existing if already granted).
type GrantImageAccessResponse struct {
	Grant ImageGrantResponse `json:"grant"`
}

// ── DELETE /v1/images/{id}/grants/{grantee_principal_id} ─────────────────────

// RevokeImageAccessResponse is returned from DELETE /v1/images/{id}/grants/{grantee}
// with 200. Revoke is idempotent: revoking a non-existent grant returns 200.
//
// Source: VM-P3B Job 1 §8 (revoke semantics — future access/launches only).
type RevokeImageAccessResponse struct {
	Revoked bool `json:"revoked"` // true = grant existed and was deleted; false = was not present
}

// ── GET /v1/images/{id}/grants ───────────────────────────────────────────────

// ListImageGrantsResponse is returned from GET /v1/images/{id}/grants with 200.
// Source: VM-P3B Job 1 §3 (owner-only list).
type ListImageGrantsResponse struct {
	Grants []ImageGrantResponse `json:"grants"`
	Total  int                  `json:"total"`
}
