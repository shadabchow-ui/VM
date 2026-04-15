package main

// image_errors.go — Structured API error codes for image endpoints.
//
// VM-P2C-P1: image not found, image not launchable admission errors.
//
// Follows the same pattern as snapshot_errors.go and instance_errors.go:
// constants only; all error writing goes through writeAPIError / writeAPIErrors.
//
// Source: API_ERROR_CONTRACT_V1.md §4 (error code catalog),
//         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md
//             §core_contracts "Image Lifecycle State Enforcement",
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.8 (launch admission: status must be ACTIVE).

const (
	// errImageNotFound is returned when the image does not exist or is not
	// accessible to the requesting principal (PRIVATE image, different owner).
	// Returns HTTP 404. Leaking image existence to non-owners is not permitted.
	// Source: AUTH_OWNERSHIP_MODEL_V1.md §3 (404-for-cross-account).
	errImageNotFound = "image_not_found"

	// errImageNotLaunchable is returned when the image exists and is visible but
	// is in a state that blocks VM launch: OBSOLETE, FAILED, or PENDING_VALIDATION.
	// DEPRECATED images remain launchable.
	// Returns HTTP 422 Unprocessable Entity.
	// Source: vm-13-01__blueprint__ §core_contracts "Image Lifecycle State Enforcement",
	//         P2_IMAGE_SNAPSHOT_MODEL.md §3.8.
	errImageNotLaunchable = "image_not_launchable"
)
