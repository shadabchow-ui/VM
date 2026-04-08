package main

// instance_validation.go — Request validation for the public instance API.
//
// PASS 1 scope: validateCreateRequest only.
//
// Source: 08-02-validation-rules-and-error-contracts.md,
//         INSTANCE_MODEL_V1 §2 (field constraints), §6 (shape catalog), §7 (image catalog).

import (
	"regexp"
	"strings"
)

// ── Instance type catalog ─────────────────────────────────────────────────────
// Source: INSTANCE_MODEL_V1 §6 (Phase 1 shape catalog).

var validInstanceTypes = map[string]bool{
	"gp1.small":  true,
	"gp1.medium": true,
	"gp1.large":  true,
	"gp1.xlarge": true,
}

// ── Image catalog ─────────────────────────────────────────────────────────────
// Source: INSTANCE_MODEL_V1 §7 (Phase 1 curated platform images).

var validImageIDs = map[string]bool{
	"img_ubuntu2204": true,
	"img_debian12":   true,
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
