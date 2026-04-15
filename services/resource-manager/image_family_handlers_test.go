package main

// image_family_handlers_test.go — VM-P2C-P3: image family, versioning, and alias resolution tests.
//
// Coverage (12 required cases):
//
//  1.  Create image with family_name + family_version → 202, fields exposed in response.
//  2.  GET /v1/images/{id} exposes family_name and family_version when set.
//  3.  Launch by direct image_id still works unchanged (no regression).
//  4.  Launch by image_family (latest) resolves to correct ACTIVE image → 202.
//  5.  PRIVATE family resolves only for owner; non-owner gets 422 family_not_found.
//  6.  PUBLIC platform family resolves for any authenticated caller → 202.
//  7.  Family containing only OBSOLETE images → 422 image_family_not_found.
//  8.  DEPRECATED candidate is selected by family alias (still launchable).
//  9.  OBSOLETE / FAILED / PENDING_VALIDATION images not selected as family candidates.
//  10. Explicit family + version resolves to correct image → 202.
//  11. Family + version where version does not exist → 422 image_family_version_not_found.
//  12. No cross-principal image sharing introduced (PRIVATE family invisible cross-account).
//
// Test strategy: in-process httptest.Server backed by memPool (fake db.Pool).
// Source: 11-02-phase-1-test-strategy.md §unit test approach,
//         vm-13-01__blueprint__ §family_seam.

import (
	"net/http"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// strPtr returns a pointer to s.
func familyStrPtr(s string) *string { return &s }

// intPtr returns a pointer to n.
func intPtr(n int) *int { return &n }

// seedFamilyImage adds an image with family metadata to the memPool.
func seedFamilyImage(mem *memPool, id, name, ownerID, visibility, status, familyName string, familyVersion *int) *db.ImageRow {
	img := &db.ImageRow{
		ID:               id,
		Name:             name,
		OSFamily:         "ubuntu",
		OSVersion:        "22.04",
		Architecture:     "x86_64",
		OwnerID:          ownerID,
		Visibility:       visibility,
		SourceType:       "SNAPSHOT",
		StorageURL:       "nfs://images/" + name + ".qcow2",
		MinDiskGB:        20,
		Status:           status,
		ValidationStatus: "passed",
		FamilyName:       &familyName,
		FamilyVersion:    familyVersion,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if status == db.ImageStatusDeprecated {
		now := time.Now()
		img.DeprecatedAt = &now
	}
	if status == db.ImageStatusObsolete {
		now := time.Now()
		img.ObsoletedAt = &now
	}
	mem.images[id] = img
	return img
}

// launchByFamilyBody returns a POST /v1/instances body using image_family.
func launchByFamilyBody(familyName string, familyVersion *int) map[string]any {
	body := map[string]any{
		"name":              "test-instance",
		"instance_type":     "c1.small",
		"availability_zone": "us-east-1a",
		"ssh_key_name":      "my-key",
		"image_family": map[string]any{
			"family_name": familyName,
		},
	}
	if familyVersion != nil {
		body["image_family"].(map[string]any)["family_version"] = *familyVersion
	}
	return body
}

// ── Test 1: Create image with family_name + family_version ────────────────────

// Test 1: POST /v1/images with family_name and family_version → 202 + fields in response.
func TestCreateImageFromSnapshot_WithFamilyFields_Returns202WithFamily(t *testing.T) {
	s := newTestSrv(t)
	seedSnapshotForImage(s.mem, "snap-family", alice, db.SnapshotStatusAvailable, 20)

	body := map[string]any{
		"source_type":        "SNAPSHOT",
		"name":               "my-family-image",
		"source_snapshot_id": "snap-family",
		"os_family":          "ubuntu",
		"os_version":         "22.04",
		"architecture":       "x86_64",
		"family_name":        "ubuntu-base",
		"family_version":     3,
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out CreateImageFromSnapshotResponse
	decodeBody(t, resp, &out)

	if out.Image.FamilyName == nil || *out.Image.FamilyName != "ubuntu-base" {
		t.Errorf("want family_name=ubuntu-base, got %v", out.Image.FamilyName)
	}
	if out.Image.FamilyVersion == nil || *out.Image.FamilyVersion != 3 {
		t.Errorf("want family_version=3, got %v", out.Image.FamilyVersion)
	}
	// Status should be PENDING_VALIDATION (not yet resolved by worker).
	if out.Image.Status != "PENDING_VALIDATION" {
		t.Errorf("want PENDING_VALIDATION, got %q", out.Image.Status)
	}
}

// Test 1b: image without family fields → family_name and family_version absent from response.
func TestCreateImageFromSnapshot_NoFamilyFields_ResponseOmitsFamilyFields(t *testing.T) {
	s := newTestSrv(t)
	seedSnapshotForImage(s.mem, "snap-nofamily", alice, db.SnapshotStatusAvailable, 20)

	body := createFromSnapshotBody("snap-nofamily")
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images", body, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	// Decode to raw map to verify omitempty behaviour.
	var raw map[string]any
	decodeBody(t, resp, &raw)
	imgRaw, _ := raw["image"].(map[string]any)
	if _, ok := imgRaw["family_name"]; ok {
		t.Error("family_name must be omitted from response when not set")
	}
	if _, ok := imgRaw["family_version"]; ok {
		t.Error("family_version must be omitted from response when not set")
	}
}

// ── Test 2: GET /v1/images/{id} exposes family fields ────────────────────────

// Test 2: GET image with family fields set → response includes family_name + family_version.
func TestGetImage_WithFamilyFields_ExposesFamilyInResponse(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-family-get", "family-img", alice, "PRIVATE", "ACTIVE", "ubuntu-base", intPtr(2))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img-family-get", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var raw map[string]any
	decodeBody(t, resp, &raw)
	if raw["family_name"] != "ubuntu-base" {
		t.Errorf("want family_name=ubuntu-base, got %v", raw["family_name"])
	}
	// JSON numbers decode as float64.
	if raw["family_version"] != float64(2) {
		t.Errorf("want family_version=2, got %v", raw["family_version"])
	}
	// Internal fields must still be absent.
	if _, ok := raw["storage_url"]; ok {
		t.Error("storage_url must not appear in image response")
	}
	if _, ok := raw["owner_id"]; ok {
		t.Error("owner_id must not appear in image response")
	}
}

// ── Test 3: Direct image_id launch unchanged ──────────────────────────────────

// Test 3: launch by direct image_id still works — no regression from P2C-P3 changes.
func TestCreateInstance_DirectImageID_UnchangedBehavior(t *testing.T) {
	s := newTestSrv(t)
	// Use a seeded PUBLIC platform image (always present in newTestSrv).
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for direct image_id launch, got %d", resp.StatusCode)
	}
}

// ── Test 4: Launch by family alias (latest) ───────────────────────────────────

// Test 4: launch by image_family resolves latest ACTIVE image → 202.
func TestCreateInstance_ByFamilyLatest_ResolvesToActiveImage(t *testing.T) {
	s := newTestSrv(t)
	// Seed two ACTIVE images in the same family — version 1 and version 2.
	seedFamilyImage(s.mem, "img-family-v1", "ubuntu-v1", alice, "PRIVATE", "ACTIVE", "my-ubuntu", intPtr(1))
	seedFamilyImage(s.mem, "img-family-v2", "ubuntu-v2", alice, "PRIVATE", "ACTIVE", "my-ubuntu", intPtr(2))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("my-ubuntu", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for family latest launch, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	// Version 2 must be selected (highest version).
	if out.Instance.ImageID != "img-family-v2" {
		t.Errorf("want image_id=img-family-v2 (latest), got %q", out.Instance.ImageID)
	}
}

// ── Test 5: PRIVATE family resolves only for owner ────────────────────────────

// Test 5a: alice's PRIVATE family is resolved for alice → 202.
func TestCreateInstance_PrivateFamilyByOwner_Resolves(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-alice-fam", "alice-img", alice, "PRIVATE", "ACTIVE", "alice-family", intPtr(1))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("alice-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for owner resolving PRIVATE family, got %d", resp.StatusCode)
	}
}

// Test 5b: alice's PRIVATE family is NOT resolved for bob → 422 image_family_not_found.
func TestCreateInstance_PrivateFamilyByNonOwner_Returns422(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-alice-priv-fam", "alice-private", alice, "PRIVATE", "ACTIVE", "alice-only-family", intPtr(1))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("alice-only-family", nil), authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for non-owner resolving PRIVATE family, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want code %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// ── Test 6: PUBLIC platform family resolves for any caller ───────────────────

// Test 6: PUBLIC image family resolves for any authenticated caller → 202.
func TestCreateInstance_PublicFamilyAnyCallerResolves(t *testing.T) {
	s := newTestSrv(t)
	// Seed a PUBLIC image in a family (platform-style, owned by "system").
	seedFamilyImage(s.mem, "img-pub-fam", "pub-family-img", "system", "PUBLIC", "ACTIVE", "ubuntu-lts", intPtr(1))

	// Both alice and bob can resolve this family.
	for _, user := range []string{alice, bob} {
		resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
			launchByFamilyBody("ubuntu-lts", nil), authHdr(user))
		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("user %q: want 202 for PUBLIC family resolution, got %d", user, resp.StatusCode)
		}
	}
}

// ── Test 7: Family with only blocked images → 422 ─────────────────────────────

// Test 7: family exists but all images are OBSOLETE → 422 image_family_not_found.
func TestCreateInstance_FamilyAllObsolete_Returns422(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-all-obs-1", "obs-img-1", alice, "PRIVATE", "OBSOLETE", "dead-family", intPtr(1))
	seedFamilyImage(s.mem, "img-all-obs-2", "obs-img-2", alice, "PRIVATE", "OBSOLETE", "dead-family", intPtr(2))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("dead-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for all-blocked family, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want code %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// ── Test 8: DEPRECATED candidate selected by family alias ────────────────────

// Test 8: family contains one DEPRECATED image (no ACTIVE) → alias resolves it → 202.
func TestCreateInstance_FamilyDeprecatedCandidate_StillLaunchable(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-deprecated-fam", "depr-img", alice, "PRIVATE", "DEPRECATED", "sunset-family", intPtr(1))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("sunset-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for DEPRECATED family candidate (still launchable), got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-deprecated-fam" {
		t.Errorf("want resolved image=img-deprecated-fam, got %q", out.Instance.ImageID)
	}
}

// ── Test 9: Blocked statuses excluded from family resolution ──────────────────

// Test 9: family has OBSOLETE, FAILED, PENDING_VALIDATION, and one ACTIVE image.
// Only the ACTIVE image should be selected.
func TestCreateInstance_FamilyMixedStatuses_OnlyActiveSelected(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-mix-obs",  "obs",  alice, "PRIVATE", "OBSOLETE",           "mixed-family", intPtr(1))
	seedFamilyImage(s.mem, "img-mix-fail", "fail", alice, "PRIVATE", "FAILED",             "mixed-family", intPtr(2))
	seedFamilyImage(s.mem, "img-mix-pend", "pend", alice, "PRIVATE", "PENDING_VALIDATION", "mixed-family", intPtr(3))
	seedFamilyImage(s.mem, "img-mix-act",  "act",  alice, "PRIVATE", "ACTIVE",             "mixed-family", intPtr(4))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("mixed-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-mix-act" {
		t.Errorf("want only ACTIVE image selected from mixed family, got %q", out.Instance.ImageID)
	}
}

// Test 9b: family has ONLY FAILED images → 422.
func TestCreateInstance_FamilyAllFailed_Returns422(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-fail-fam", "fail-img", alice, "PRIVATE", "FAILED", "fail-family", intPtr(1))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("fail-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for all-failed family, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want code %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// Test 9c: family has ONLY PENDING_VALIDATION images → 422.
func TestCreateInstance_FamilyAllPending_Returns422(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-pend-fam", "pend-img", alice, "PRIVATE", "PENDING_VALIDATION", "pending-family", intPtr(1))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("pending-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for all-pending family, got %d", resp.StatusCode)
	}
}

// ── Test 10: Explicit family + version resolves correctly ─────────────────────

// Test 10: image_family with family_version resolves the exact version → 202.
func TestCreateInstance_FamilyExactVersion_ResolvesCorrectly(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-ver-1", "v1", alice, "PRIVATE", "ACTIVE", "versioned-family", intPtr(1))
	seedFamilyImage(s.mem, "img-ver-2", "v2", alice, "PRIVATE", "ACTIVE", "versioned-family", intPtr(2))
	seedFamilyImage(s.mem, "img-ver-3", "v3", alice, "PRIVATE", "ACTIVE", "versioned-family", intPtr(3))

	// Request version 2 specifically.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("versioned-family", intPtr(2)), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for exact version resolution, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-ver-2" {
		t.Errorf("want img-ver-2 for version 2, got %q", out.Instance.ImageID)
	}
}

// ── Test 11: Family + version does not exist → 422 ───────────────────────────

// Test 11: explicit family+version where version 99 doesn't exist → 422.
func TestCreateInstance_FamilyVersionNotFound_Returns422(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-exist-ver", "existing", alice, "PRIVATE", "ACTIVE", "partial-family", intPtr(1))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("partial-family", intPtr(99)), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for missing version, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyVersionNotFound {
		t.Errorf("want code %q, got %q", errImageFamilyVersionNotFound, env.Error.Code)
	}
}

// ── Test 12: No cross-principal image sharing ─────────────────────────────────

// Test 12: bob's PRIVATE family image is not visible to alice in LIST.
func TestListImages_FamilyImages_NoCrossPrincipalSharing(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-bob-family", "bob-fam", bob, "PRIVATE", "ACTIVE", "bob-secret-family", intPtr(1))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImagesResponse
	decodeBody(t, resp, &out)
	for _, img := range out.Images {
		if img.ID == "img-bob-family" {
			t.Error("bob's PRIVATE family image must not appear in alice's image list")
		}
	}
}

// Test 12b: bob's PRIVATE family cannot be resolved by alice via image_family alias.
func TestCreateInstance_FamilyAlias_NoCrossPrincipalResolution(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-bob-resolve", "bob-resolve", bob, "PRIVATE", "ACTIVE", "bob-exclusive", intPtr(1))

	// alice attempts to launch using bob's PRIVATE family name.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("bob-exclusive", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 (not 200/202) for cross-principal family alias, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want code %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// ── Additional edge cases ─────────────────────────────────────────────────────

// Test: both image_id and image_family set → 400 invalid_request.
func TestCreateInstance_BothImageIDAndFamily_Returns400(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	// validCreateBody sets image_id; now also set image_family.
	bodyMap := map[string]any{
		"name":              body.Name,
		"instance_type":     body.InstanceType,
		"image_id":          body.ImageID,
		"availability_zone": body.AvailabilityZone,
		"ssh_key_name":      body.SSHKeyName,
		"image_family": map[string]any{
			"family_name": "some-family",
		},
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", bodyMap, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 when both image_id and image_family set, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidRequest {
		t.Errorf("want code %q, got %q", errInvalidRequest, env.Error.Code)
	}
}

// Test: image_family with empty family_name → 400 image_family_invalid_request.
func TestCreateInstance_FamilyEmptyName_Returns400(t *testing.T) {
	s := newTestSrv(t)
	body := map[string]any{
		"name":              "test-instance",
		"instance_type":     "c1.small",
		"availability_zone": "us-east-1a",
		"ssh_key_name":      "my-key",
		"image_family": map[string]any{
			"family_name": "   ", // whitespace only
		},
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for empty family_name, got %d", resp.StatusCode)
	}
}

// Test: family resolution selects highest version over lower version.
func TestFamilyResolution_HighestVersionWins(t *testing.T) {
	s := newTestSrv(t)
	// Seed out-of-order to confirm sorting, not insertion order.
	seedFamilyImage(s.mem, "img-ord-v3", "v3", alice, "PRIVATE", "ACTIVE", "ordered-family", intPtr(3))
	seedFamilyImage(s.mem, "img-ord-v1", "v1", alice, "PRIVATE", "ACTIVE", "ordered-family", intPtr(1))
	seedFamilyImage(s.mem, "img-ord-v2", "v2", alice, "PRIVATE", "ACTIVE", "ordered-family", intPtr(2))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		launchByFamilyBody("ordered-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-ord-v3" {
		t.Errorf("want img-ord-v3 (highest version=3), got %q", out.Instance.ImageID)
	}
}

// Test: family validation — invalid family_name format in create image request.
func TestCreateImageFromSnapshot_InvalidFamilyName_Returns400(t *testing.T) {
	s := newTestSrv(t)
	seedSnapshotForImage(s.mem, "snap-badfam", alice, db.SnapshotStatusAvailable, 20)

	body := map[string]any{
		"source_type":        "SNAPSHOT",
		"name":               "my-image",
		"source_snapshot_id": "snap-badfam",
		"os_family":          "ubuntu",
		"os_version":         "22.04",
		"architecture":       "x86_64",
		"family_name":        "INVALID_CAPS", // uppercase not allowed
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for invalid family_name, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "family_name", errInvalidName)
}

// Test: family_version without family_name → 400.
func TestCreateImageFromSnapshot_FamilyVersionWithoutName_Returns400(t *testing.T) {
	s := newTestSrv(t)
	seedSnapshotForImage(s.mem, "snap-vonly", alice, db.SnapshotStatusAvailable, 20)

	body := map[string]any{
		"source_type":        "SNAPSHOT",
		"name":               "my-image",
		"source_snapshot_id": "snap-vonly",
		"os_family":          "ubuntu",
		"os_version":         "22.04",
		"architecture":       "x86_64",
		"family_version":     1,
		// family_name intentionally absent
	}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for family_version without family_name, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "family_version", errImageFamilyInvalidRequest)
}
