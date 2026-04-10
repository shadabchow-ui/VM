package main

// catalog_alignment_test.go — Tests that validate catalog values match DB schema.
//
// P2-M1/WS-H1: Added after the image_id/instance_type mismatch caused 500s on
// POST /v1/instances. These tests make the contract between instance_validation.go
// and db/migrations/001_initial.up.sql explicit and machine-checkable.
//
// If these tests fail after a schema migration change, it means instance_validation.go
// was not updated in sync — the exact failure that caused this bug.

import (
	"net/http"
	"testing"
)

// catalogImageUUIDs are the exact UUIDs seeded in db/migrations/001_initial.up.sql §images.
// This list is the single source of truth for what the test suite treats as valid.
// If the migration adds or removes images, update this list and validImageIDs together.
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

// TestValidation_CatalogImageUUIDs verifies that every UUID in catalogImageUUIDs
// is accepted by validateCreateRequest (i.e. is in validImageIDs).
// Fails if instance_validation.go diverges from the migration seed again.
func TestValidation_CatalogImageUUIDs(t *testing.T) {
	base := validCreateBody()
	for _, imageID := range catalogImageUUIDs {
		t.Run(imageID, func(t *testing.T) {
			req := base
			req.ImageID = imageID
			errs := validateCreateRequest(&req)
			for _, e := range errs {
				if e.target == "image_id" {
					t.Errorf("image_id %q rejected by validateCreateRequest: %s — "+
						"validImageIDs in instance_validation.go is out of sync with "+
						"db/migrations/001_initial.up.sql", imageID, e.message)
				}
			}
		})
	}
}

// TestValidation_CatalogInstanceTypes verifies every instance type ID seeded in
// the migration is accepted by validateCreateRequest.
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

// TestValidation_OldImageIDsRejected verifies the old invented image IDs that
// previously caused FK violations at INSERT time now fail validation with 400.
func TestValidation_OldImageIDsRejected(t *testing.T) {
	oldIDs := []string{
		"img_ubuntu2204", // old invented value — not a UUID, not in images table
		"img_debian12",   // old invented value — not a UUID, not in images table
	}
	s := newTestSrv(t)
	for _, imageID := range oldIDs {
		t.Run(imageID, func(t *testing.T) {
			body := validCreateBody()
			body.ImageID = imageID
			resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("old image_id %q: want 400 (rejected at validation), got %d — "+
					"this value would cause a FK violation at DB INSERT if it reaches InsertInstance",
					imageID, resp.StatusCode)
			}
			var env apiError
			decodeBody(t, resp, &env)
			assertDetailCode(t, env, "image_id", errInvalidImageID)
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
// valid catalog UUID reaches InsertInstance without a validation error.
// (The fake memPool accepts any value; this test confirms no pre-insert rejection.)
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
