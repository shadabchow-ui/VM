package main

// instance_validation.go — Request validation for the public instance API.
//
// PASS 1 scope: validateCreateRequest only.
//
// P2-M1/WS-H1 fix: aligned image_id and instance_type catalogs with schema seed values.
//
//   BEFORE (broken):
//     validImageIDs:     {"img_ubuntu2204", "img_debian12"}          — opaque strings
//     validInstanceTypes: {"gp1.small", "gp1.medium", ...}           — invented prefix
//   These values are not in the DB. The instances.image_id column is
//   UUID NOT NULL REFERENCES images(id) and instances.instance_type_id is
//   VARCHAR(64) REFERENCES instance_types(id). Passing the old values to
//   InsertInstance produces a FK violation → 500.
//
//   AFTER (correct):
//     validImageIDs:     schema seed UUIDs from db/migrations/001_initial.up.sql §images
//     validInstanceTypes: schema seed IDs from db/migrations/001_initial.up.sql §instance_types
//
//   The human-readable "name" for each image is preserved as a comment so the
//   mapping to the migration is explicit and auditable.
//
// Source: 08-02-validation-rules-and-error-contracts.md,
//         INSTANCE_MODEL_V1 §2 (field constraints), §6 (shape catalog), §7 (image catalog),
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

	return errs
}
