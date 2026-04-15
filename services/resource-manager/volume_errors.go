package main

// volume_errors.go — Volume-specific error codes and error writers.
//
// VM-P2B Slice 1: volume error codes.
// VM-P2B-S3: Added errVolumeHasSnapshots for SNAP-I-3 enforcement at API admission.
//
// All error infrastructure (writeAPIError, writeAPIErrors, writeDBError,
// fieldErr, writeInternalError, writeServiceUnavailable) is reused from
// instance_errors.go. This file adds only the new volume-scoped code constants.
//
// Source: API_ERROR_CONTRACT_V1 §4 (error code catalog),
//         P2_VOLUME_MODEL.md §3.3 (VOL-SM-1, VOL-SM-2), §4.1, §7,
//         P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.

// Volume-specific public error codes.
// Prefix "volume_" distinguishes from instance error codes.
// Source: P2_VOLUME_MODEL.md §3.3, §4.1, §7; API_ERROR_CONTRACT_V1 §4.
const (
	// errVolumeNotFound is returned when the volume does not exist or is owned
	// by a different principal. Always 404, never 403.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	errVolumeNotFound = "volume_not_found"

	// errVolumeInUse is returned when an operation requires the volume to be
	// not attached (e.g. delete while in_use).
	// Source: P2_VOLUME_MODEL.md §3.3 VOL-SM-1, §7 VOL-I-4.
	errVolumeInUse = "volume_in_use"

	// errVolumeInvalidState is returned when the requested operation is not
	// valid for the volume's current state (e.g. attach while attaching).
	// Source: P2_VOLUME_MODEL.md §3.3 VOL-SM-2.
	errVolumeInvalidState = "volume_invalid_state"

	// errVolumeAZMismatch is returned when the volume and instance are in
	// different availability zones.
	// Source: P2_VOLUME_MODEL.md §4.1 (AZ affinity), §7 VOL-I-2.
	errVolumeAZMismatch = "volume_az_mismatch"

	// errVolumeLimitExceeded is returned when attaching would exceed the
	// maximum number of volumes per instance (16).
	// Source: P2_VOLUME_MODEL.md §4.1.
	errVolumeLimitExceeded = "volume_limit_exceeded"

	// errInvalidVolumeSize is returned when size_gb is out of range [1, 16000].
	// Source: P2_VOLUME_MODEL.md §2.3.
	errInvalidVolumeSize = "invalid_volume_size"

	// errVolumeAlreadyAttached is returned when trying to attach a volume that
	// already has an active attachment (VOL-I-1).
	// Source: P2_VOLUME_MODEL.md §7 VOL-I-1.
	errVolumeAlreadyAttached = "volume_already_attached"

	// errVolumeNotAttached is returned when trying to detach a volume that is
	// not currently attached to the specified instance.
	errVolumeNotAttached = "volume_not_attached"

	// errVolumeHasSnapshots is returned when trying to delete a volume that
	// has one or more non-deleted snapshots referencing it.
	// Enforces SNAP-I-3 at the API admission layer.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.
	// VM-P2B-S3.
	errVolumeHasSnapshots = "volume_has_snapshots"
)
