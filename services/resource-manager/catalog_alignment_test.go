package main

// catalog_alignment_test.go — Tests that validate catalog values match DB schema.
//
// P2-M1/WS-H1: Added after the image_id/instance_type mismatch caused 500s on
// POST /v1/instances. These tests make the contract between instance_validation.go
// and db/migrations/001_initial.up.sql explicit and machine-checkable.
//
// VM-P2C-P1: Image admission moved from static validImageIDs map to DB lookup.
//
// CHANGED from original:
//   - TestValidation_CatalogImageUUIDs: reworked. validateCreateRequest no longer
//     checks image existence; it only checks presence (empty string). The alignment
//     between DB seed values and admission is now enforced by the handler's
//     GetImageForAdmission + ImageIsLaunchable call. The catalog UUIDs are still
//     documented here for reference, but the test now confirms that validateCreateRequest
//     accepts any non-empty image_id (presence-only check) and that the E2E path
//     succeeds when the image is seeded in the memPool.
//   - TestValidation_OldImageIDsRejected: removed. Old invented IDs (img_ubuntu2204,
//     img_debian12) are no longer rejected at the validation layer with 400. They are
//     rejected at the handler's DB admission layer with 422 invalid_image_id (image
//     not found in DB). The test for this behavior lives in image_handlers_test.go
//     (TestCreateInstance_NonExistentImage_Blocked).
//   - TestCreate_ImageUUID_E2E: preserved and still passes because newTestSrv seeds
//     the standard catalog UUIDs in the memPool.
//
// UNCHANGED:
//   - TestValidation_CatalogInstanceTypes: instance_type catalog is still a static
//     in-memory check in validateCreateRequest; no DB lookup needed for Phase 1.
//   - TestValidation_OldInstanceTypesRejected: still valid; gp1.* is not in validInstanceTypes.
//
// Source: instance_validation.go, image_repo.go GetImageForAdmission,
//         vm-13-01__blueprint__ §core_contracts, API_ERROR_CONTRACT_V1 §6.

import (
	"net/http"
	"testing"
)

// catalogImageUUIDs are the exact UUIDs seeded in db/migrations/001_initial.up.sql §images.
// These are documented here for cross-reference. Admission is now enforced by the
// handler's DB lookup, not by a static map in validateCreateRequest.
// Source: INSTANCE_MODEL_V1 §7 (Phase 1 curated platform images).
var catalogImageUUIDs = []string{
	"00000000-0000-0000-0000-000000000010", // ubuntu-22.04-lts
	"00000000-0000-0000-0000-000000000011", // debian-12
}

// catalogInstanceTypeIDs are the exact IDs seeded in db/migrations/001_initial.up.sql §instance_types.
var catalogInstanceTypeIDs = []string{
	"c1.small",
	"c1.medium",
	"c1.large",
	"c1.xlarge",
}

// TestValidation_CatalogImageUUIDs verifies that validateCreateRequest accepts
// any non-empty image_id (presence-only check post VM-P2C-P1).
//
// Image admission (existence + lifecycle state) is now a handler-layer DB check.
// This test confirms the field validator does not erroneously reject catalog UUIDs.
//
// Source: instance_validation.go (image_id: presence-only after VM-P2C-P1).
func TestValidation_CatalogImageUUIDs(t *testing.T) {
	base := validCreateBody()
	for _, imageID := range catalogImageUUIDs {
		t.Run(imageID, func(t *testing.T) {
			req := base
			req.ImageID = imageID
			errs := validateCreateRequest(&req)
			for _, e := range errs {
				if e.target == "image_id" {
					t.Errorf("image_id %q unexpectedly rejected by validateCreateRequest: %s — "+
						"image_id is presence-only in validateCreateRequest post VM-P2C-P1; "+
						"DB admission happens in the handler", imageID, e.message)
				}
			}
		})
	}
}

// TestValidation_EmptyImageIDRejected verifies that an empty image_id is rejected
// by validateCreateRequest with missing_field.
// Source: instance_validation.go (presence-only check preserved).
func TestValidation_EmptyImageIDRejected(t *testing.T) {
	req := validCreateBody()
	req.ImageID = ""
	errs := validateCreateRequest(&req)
	found := false
	for _, e := range errs {
		if e.target == "image_id" && e.code == errMissingField {
			found = true
		}
	}
	if !found {
		t.Error("empty image_id: want missing_field error on image_id, got none")
	}
}

// TestValidation_CatalogInstanceTypes verifies every instance type ID seeded in
// the migration is accepted by validateCreateRequest.
// instance_type is still a static in-memory check; no DB lookup required.
func TestValidation_CatalogInstanceTypes(t *testing.T) {
	base := validCreateBody()
	for _, itID := range catalogInstanceTypeIDs {
		t.Run(itID, func(t *testing.T) {
			req := base
			req.InstanceType = itID
			errs := validateCreateRequest(&req)
			for _, e := range errs {
				if e.target == "instance_type" {
					t.Errorf("instance_type %q rejected by validateCreateRequest: %s — "+
						"validInstanceTypes in instance_validation.go is out of sync with "+
						"db/migrations/001_initial.up.sql", itID, e.message)
				}
			}
		})
	}
}

// TestValidation_OldInstanceTypesRejected verifies the old invented instance type
// prefix (gp1.*) is rejected with 400.
func TestValidation_OldInstanceTypesRejected(t *testing.T) {
	oldTypes := []string{
		"gp1.small",  // old invented value — not in instance_types table
		"gp1.medium", // old invented value — not in instance_types table
		"gp1.large",
		"gp1.xlarge",
	}
	s := newTestSrv(t)
	for _, it := range oldTypes {
		t.Run(it, func(t *testing.T) {
			body := validCreateBody()
			body.InstanceType = it
			resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("old instance_type %q: want 400 (rejected at validation), got %d — "+
					"this value would cause a FK violation at DB INSERT if it reaches InsertInstance",
					it, resp.StatusCode)
			}
			var env apiError
			decodeBody(t, resp, &env)
			assertDetailCode(t, env, "instance_type", errInvalidInstanceType)
		})
	}
}

// TestCreate_ImageUUID_E2E verifies the full handler path: a request with a
// valid catalog UUID reaches InsertInstance without a validation or admission error.
// newTestSrv seeds the standard catalog UUIDs in the memPool so DB admission passes.
// Source: image_handlers_test.go (broader admission coverage),
//
//	instance_handlers_test.go (newTestSrv seeding).
func TestCreate_ImageUUID_E2E(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.ImageID = "00000000-0000-0000-0000-000000000010" // ubuntu-22.04-lts UUID

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		var env apiError
		decodeBody(t, resp, &env)
		t.Fatalf("want 202, got %d — error: %+v", resp.StatusCode, env.Error)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "00000000-0000-0000-0000-000000000010" {
		t.Errorf("image_id roundtrip: want UUID, got %q", out.Instance.ImageID)
	}
}
