package main

// image_share_errors.go — Structured API error codes for image share grant endpoints.
//
// VM-P3B Job 1: cross-principal private image sharing contract.
//
// Follows the same pattern as image_errors.go and instance_errors.go:
// constants only; all error writing goes through writeAPIError / writeAPIErrors.
//
// Source: API_ERROR_CONTRACT_V1.md §4 (error code catalog),
//         AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-non-owner).

const (
	// errImageGrantGranteeRequired is returned when grantee_principal_id is
	// missing or empty in a grant request.
	// Returns HTTP 400.
	errImageGrantGranteeRequired = "image_grant_grantee_required"

	// errImageGrantSelfGrant is returned when the caller attempts to grant
	// access to themselves (the image owner). This is a no-op by definition.
	// Returns HTTP 422. Kept distinct so callers can identify the case.
	errImageGrantSelfGrant = "image_grant_self_grant"

	// errImageGrantPublicImage is returned when the caller attempts to add a
	// share grant to a PUBLIC image. PUBLIC images are accessible to all
	// principals without grants.
	// Returns HTTP 422.
	errImageGrantPublicImage = "image_grant_public_image"

	// errImageGrantNotOwner is returned when a non-owner attempts a grant,
	// revoke, or list operation. Uses 404 (not 403) per AUTH_OWNERSHIP_MODEL_V1
	// §3: ownership must not be leaked to non-owners.
	// Value reuses errImageNotFound to preserve the same 404 response body.
	errImageGrantNotOwner = "image_not_found"
)
