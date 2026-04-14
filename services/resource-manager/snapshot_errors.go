package main

// snapshot_errors.go — Snapshot-specific error codes.
//
// VM-P2B-S2: snapshot error codes.
//
// All error infrastructure (writeAPIError, writeAPIErrors, writeDBError,
// fieldErr, writeInternalError) is reused from instance_errors.go.
// This file adds only the new snapshot-scoped code constants.
//
// Source: API_ERROR_CONTRACT_V1 §4 (error code catalog),
//         P2_IMAGE_SNAPSHOT_MODEL.md §2.9 (SNAP-I-1 through SNAP-I-5).

// Snapshot-specific public error codes.
// Prefix "snapshot_" distinguishes from instance and volume error codes.
const (
	// errSnapshotNotFound is returned when the snapshot does not exist or is
	// owned by a different principal. Always 404, never 403.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	errSnapshotNotFound = "snapshot_not_found"

	// errSnapshotInvalidState is returned when the requested operation is not
	// valid for the snapshot's current state.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.5.
	errSnapshotInvalidState = "snapshot_invalid_state"

	// errSnapshotNotAvailable is returned when a restore is requested but the
	// snapshot is not in 'available' state.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-1.
	errSnapshotNotAvailable = "snapshot_not_available"

	// errSnapshotSourceRequired is returned when neither source_volume_id nor
	// source_instance_id is provided in a create request.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6.
	errSnapshotSourceRequired = "snapshot_source_required"

	// errSnapshotSourceAmbiguous is returned when both source_volume_id and
	// source_instance_id are provided — exactly one is required.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6.
	errSnapshotSourceAmbiguous = "snapshot_source_ambiguous"

	// errSnapshotSourceInvalidState is returned when the source volume or
	// instance is in a state that does not permit snapshot creation.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.6, SNAP-I-4.
	errSnapshotSourceInvalidState = "snapshot_source_invalid_state"

	// errRestoreSizeTooSmall is returned when the requested size_gb override
	// is smaller than the snapshot's size_gb.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2 (restore must not shrink).
	errRestoreSizeTooSmall = "restore_size_too_small"
)
