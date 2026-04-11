package main

// instance_validation.go — Request validation for the public instance API.
//
// PASS 1 scope: validateCreateRequest only.
//
// P2-M1/WS-H1 fix: aligned image_id and instance_type catalogs with schema seed values.
//
// M10 Slice 4: added block_devices validation.
//   - Phase 1: exactly one block device entry is permitted.
//   - Phase 1: delete_on_termination must be true. Setting false returns 400.
//   - When block_devices is omitted, the handler synthesizes the default.
//     Validation runs on the already-synthesized slice.
//   - image_id in block_devices[0] must match the top-level image_id (Phase 1 invariant).
//   - size_gb in block_devices[0] must be > 0 and <= shape max.
//
// Source: 08-02-validation-rules-and-error-contracts.md,
//         INSTANCE_MODEL_V1 §2 (field constraints), §6 (shape catalog), §7 (image catalog),
//         API_ERROR_CONTRACT_V1 §4 (invalid_block_device_mapping, delete_on_termination_required),
//         execution_blueprint §7.7, P2_VOLUME_MODEL §1, P2_MIGRATION_COMPATIBILITY_RULES §7.2,
//         db/migrations/001_initial.up.sql (authoritative seed values).

import (
	"regexp"
	"strings"
)

// ── Instance type catalog ─────────────────────────────────────────────────────
// Values must match instance_types.id seeded in db/migrations/001_initial.up.sql.
// Source: INSTANCE_MODEL_V1 §6 (Phase 1 shape catalog).

var validInstanceTypes = map[string]bool{
	"c1.small":  true, // 2 vCPU, 4 GB RAM, 50 GB disk
	"c1.medium": true, // 4 vCPU, 8 GB RAM, 100 GB disk
	"c1.large":  true, // 8 vCPU, 16 GB RAM, 200 GB disk
	"c1.xlarge": true, // 16 vCPU, 32 GB RAM, 500 GB disk
}

// shapeDiskSizeGB maps instance type → default root disk size in GB.
// Used to synthesize block_devices when omitted and to validate size_gb bounds.
// Source: INSTANCE_MODEL_V1 §6, worker/handlers/create.go shapeDiskGB.
var shapeDiskSizeGB = map[string]int{
	"c1.small":  50,
	"c1.medium": 100,
	"c1.large":  200,
	"c1.xlarge": 500,
}

// ── Image catalog ─────────────────────────────────────────────────────────────
// Values must be UUID strings matching images.id seeded in 001_initial.up.sql.
// The instances.image_id column is UUID NOT NULL REFERENCES images(id) — passing
// any other string produces a PostgreSQL FK violation at INSERT time.
// Source: INSTANCE_MODEL_V1 §7 (Phase 1 curated platform images).

var validImageIDs = map[string]bool{
	"00000000-0000-0000-0000-000000000010": true, // ubuntu-22.04-lts
	"00000000-0000-0000-0000-000000000011": true, // debian-12
}

// ── AZ catalog ────────────────────────────────────────────────────────────────
// Phase 1: single region, two AZs. Source: 07-01 §Phase 1 network architecture.

var validAZs = map[string]bool{
	"us-east-1a": true,
	"us-east-1b": true,
}

// ── Name validation ───────────────────────────────────────────────────────────
// Source: INSTANCE_MODEL_V1 §2: ^[a-z][a-z0-9-]{0,61}[a-z0-9]$

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// ── validateCreateRequest ─────────────────────────────────────────────────────

// validateCreateRequest returns one fieldErr per failing field.
// Returns nil if the request is valid.
// Source: API_ERROR_CONTRACT_V1 §6 (validation execution order: schema → resource existence).
func validateCreateRequest(req *CreateInstanceRequest) []fieldErr {
	var errs []fieldErr

	// name
	if strings.TrimSpace(req.Name) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'name' is required.", "name"})
	} else if !nameRE.MatchString(req.Name) {
		errs = append(errs, fieldErr{errInvalidName,
			"Name must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$.", "name"})
	}

	// instance_type
	if strings.TrimSpace(req.InstanceType) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'instance_type' is required.", "instance_type"})
	} else if !validInstanceTypes[req.InstanceType] {
		errs = append(errs, fieldErr{errInvalidInstanceType,
			"'" + req.InstanceType + "' is not a valid instance type.", "instance_type"})
	}

	// image_id
	if strings.TrimSpace(req.ImageID) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'image_id' is required.", "image_id"})
	} else if !validImageIDs[req.ImageID] {
		errs = append(errs, fieldErr{errInvalidImageID,
			"Image '" + req.ImageID + "' does not exist or is not accessible.", "image_id"})
	}

	// availability_zone
	if strings.TrimSpace(req.AvailabilityZone) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'availability_zone' is required.", "availability_zone"})
	} else if !validAZs[req.AvailabilityZone] {
		errs = append(errs, fieldErr{errInvalidAZ,
			"Availability zone '" + req.AvailabilityZone + "' is not valid.", "availability_zone"})
	}

	// ssh_key_name
	if strings.TrimSpace(req.SSHKeyName) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'ssh_key_name' is required.", "ssh_key_name"})
	}

	// block_devices (M10 Slice 4)
	// Validation runs after the handler has synthesized defaults for omitted block_devices.
	// At this point, req.BlockDevices is guaranteed non-nil by the caller.
	errs = append(errs, validateBlockDevices(req)...)

	return errs
}

// validateBlockDevices validates the block_devices array.
// Phase 1 rules per INSTANCE_MODEL_V1 §2, execution_blueprint §7.7,
// API_ERROR_CONTRACT_V1 §4:
//   - Exactly one entry.
//   - delete_on_termination must be true (Phase 1 hardcoded; Phase 2 unlocks false).
//   - image_id must match top-level image_id (Phase 1: single root disk).
//   - size_gb must be > 0 and <= shape disk limit.
func validateBlockDevices(req *CreateInstanceRequest) []fieldErr {
	var errs []fieldErr

	if len(req.BlockDevices) == 0 {
		// Omitted block_devices: no error — the handler synthesizes defaults before
		// calling validation. If we reach here with empty, it means no synthesis
		// was needed (backward-compat path where the caller didn't set anything).
		return nil
	}

	// Phase 1: exactly one block device entry.
	if len(req.BlockDevices) > 1 {
		errs = append(errs, fieldErr{errInvalidBlockDeviceMapping,
			"Phase 1 supports exactly one block device entry.", "block_devices"})
		return errs
	}

	bd := req.BlockDevices[0]

	// delete_on_termination: Phase 1 must be true.
	// Source: P2_VOLUME_MODEL §1 "Phase 1 contract: delete_on_termination must be true at API layer."
	// Source: API_ERROR_CONTRACT_V1 §4 (delete_on_termination_required).
	if !bd.DeleteOnTermination {
		errs = append(errs, fieldErr{errDeleteOnTerminationRequired,
			"Phase 1 requires delete_on_termination to be true.",
			"block_devices[0].delete_on_termination"})
	}

	// image_id consistency: must match top-level image_id.
	// Phase 1: the root disk image must be the instance image.
	if bd.ImageID != "" && req.ImageID != "" && bd.ImageID != req.ImageID {
		errs = append(errs, fieldErr{errInvalidBlockDeviceMapping,
			"block_devices[0].image_id must match the top-level image_id.",
			"block_devices[0].image_id"})
	}

	// size_gb: must be > 0.
	if bd.SizeGB <= 0 {
		errs = append(errs, fieldErr{errInvalidBlockDeviceMapping,
			"block_devices[0].size_gb must be greater than 0.",
			"block_devices[0].size_gb"})
	} else if validInstanceTypes[req.InstanceType] {
		// Check size_gb does not exceed shape maximum.
		maxDisk := shapeDiskSizeGB[req.InstanceType]
		if maxDisk > 0 && bd.SizeGB > maxDisk {
			errs = append(errs, fieldErr{errInvalidBlockDeviceMapping,
				"block_devices[0].size_gb exceeds the maximum for instance type '" + req.InstanceType + "'.",
				"block_devices[0].size_gb"})
		}
	}

	return errs
}
