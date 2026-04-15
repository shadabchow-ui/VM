package main

// image_errors.go — Structured API error codes for image endpoints.
//
// VM-P2C-P1: image not found, image not launchable admission errors.
// VM-P2C-P2: added custom image creation and import error codes.
// VM-P2C-P3: added family resolution error codes.
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

	// errImageSnapshotNotFound is returned when the source_snapshot_id in a
	// create-from-snapshot request does not exist or is not owned by the caller.
	// Returns HTTP 422. Source: AUTH_OWNERSHIP_MODEL_V1.md §3.
	errImageSnapshotNotFound = "image_snapshot_not_found"

	// errImageSnapshotNotAvailable is returned when the snapshot exists and is
	// owned by the caller but is not in "available" status.
	// Returns HTTP 422. Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.6.
	errImageSnapshotNotAvailable = "image_snapshot_not_available"

	// errImageImportURLInvalid is returned when the import_url field is empty or
	// does not pass basic format validation.
	// Returns HTTP 400. Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (import flow).
	errImageImportURLInvalid = "image_import_url_invalid"

	// errImageInvalidState is returned when a lifecycle action (deprecate, obsolete)
	// is requested for an image that is not in a valid source state for that transition.
	// Returns HTTP 422. Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.4.
	errImageInvalidState = "image_invalid_state"

	// errImageNotOwned is returned when the caller attempts a mutating action
	// (deprecate, obsolete) on an image they do not own.
	// Returns HTTP 404 (ownership-hiding — not 403).
	// Source: AUTH_OWNERSHIP_MODEL_V1.md §3.
	errImageNotOwned = "image_not_found"

	// errImageFamilyNotFound is returned when a family alias is requested but the
	// named family has no images visible to the caller (family does not exist or
	// all images are owned by a different principal for PRIVATE families).
	// Returns HTTP 422. Using 422 (not 404) because the resource reference is in
	// the request body, not the URL path, which matches the existing image_id
	// admission pattern (errInvalidImageID is also 422).
	// Source: vm-13-01__blueprint__ §family_seam, AUTH_OWNERSHIP_MODEL_V1.md §3.
	errImageFamilyNotFound = "image_family_not_found"

	// errImageFamilyNoLaunchable is returned when the family exists and has visible
	// images but none are in a launchable state (all OBSOLETE / FAILED /
	// PENDING_VALIDATION). Distinct from errImageFamilyNotFound so the caller can
	// distinguish "family doesn't exist" from "family exists but is blocked".
	// Returns HTTP 422.
	// Source: vm-13-01__blueprint__ §family_seam,
	//         P2_IMAGE_SNAPSHOT_MODEL.md §3.8 (ACTIVE or DEPRECATED required).
	errImageFamilyNoLaunchable = "image_family_no_launchable_image"

	// errImageFamilyVersionNotFound is returned when a specific family+version
	// combination was requested (via image_family.family_version) but no matching
	// launchable image was found that is also visible to the caller.
	// Returns HTTP 422.
	// Source: vm-13-01__blueprint__ §family_seam.
	errImageFamilyVersionNotFound = "image_family_version_not_found"

	// errImageFamilyInvalidRequest is returned when the image_family field in
	// CreateInstanceRequest is malformed (e.g. family_name empty).
	// Returns HTTP 400.
	errImageFamilyInvalidRequest = "image_family_invalid_request"
)
