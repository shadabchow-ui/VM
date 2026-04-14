package main

// snapshot_validation.go — Request validation for snapshot endpoints.
//
// VM-P2B-S2: validateCreateSnapshotRequest, validateRestoreSnapshotRequest.
//
// Follows the same validation style as volume_validation.go:
//   - Returns []fieldErr (reuses the existing type).
//   - Uses existing error codes where they apply.
//   - Snapshot-specific error codes are in snapshot_errors.go.
//   - No new validation framework introduced.
//
// Cross-resource checks (source ownership, source state) are done in handlers.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6 (creation rules),
//         API_ERROR_CONTRACT_V1 §4, 08-02-validation-rules-and-error-contracts.md.

import "strings"

// validateCreateSnapshotRequest validates POST /v1/snapshots request fields.
// Returns one fieldErr per failing field; nil if valid.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6.
func validateCreateSnapshotRequest(req *CreateSnapshotRequest) []fieldErr {
	var errs []fieldErr

	// name — required; reuse name regexp from instance_validation.go.
	if strings.TrimSpace(req.Name) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'name' is required.", "name"})
	} else if !nameRE.MatchString(req.Name) {
		errs = append(errs, fieldErr{errInvalidName,
			"Name must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$.", "name"})
	}

	// source — exactly one of source_volume_id or source_instance_id must be set.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6.
	hasVol := req.SourceVolumeID != nil && strings.TrimSpace(*req.SourceVolumeID) != ""
	hasInst := req.SourceInstanceID != nil && strings.TrimSpace(*req.SourceInstanceID) != ""

	if !hasVol && !hasInst {
		errs = append(errs, fieldErr{errSnapshotSourceRequired,
			"Exactly one of 'source_volume_id' or 'source_instance_id' is required.", "source"})
	} else if hasVol && hasInst {
		errs = append(errs, fieldErr{errSnapshotSourceAmbiguous,
			"Provide exactly one of 'source_volume_id' or 'source_instance_id', not both.", "source"})
	}

	return errs
}

// validateRestoreSnapshotRequest validates POST /v1/snapshots/{id}/restore.
// Returns one fieldErr per failing field; nil if valid.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (restore flow).
func validateRestoreSnapshotRequest(req *RestoreSnapshotRequest) []fieldErr {
	var errs []fieldErr

	// name — required; reuse name regexp from instance_validation.go.
	if strings.TrimSpace(req.Name) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'name' is required.", "name"})
	} else if !nameRE.MatchString(req.Name) {
		errs = append(errs, fieldErr{errInvalidName,
			"Name must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$.", "name"})
	}

	// availability_zone — required.
	if strings.TrimSpace(req.AvailabilityZone) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'availability_zone' is required.", "availability_zone"})
	} else if !validAZs[req.AvailabilityZone] {
		errs = append(errs, fieldErr{errInvalidAZ,
			"Availability zone '" + req.AvailabilityZone + "' is not valid.", "availability_zone"})
	}

	// size_gb — optional but if provided must be positive.
	// The handler enforces the "must be >= snapshot.SizeGB" constraint after
	// loading the snapshot.
	if req.SizeGB != nil && *req.SizeGB <= 0 {
		errs = append(errs, fieldErr{errInvalidVolumeSize,
			"size_gb must be greater than 0 when specified.", "size_gb"})
	}

	return errs
}
