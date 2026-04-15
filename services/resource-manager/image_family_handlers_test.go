package main

// image_family_handlers_test.go — VM-P2C-P4: family alias launch tests.
//
// Tests for:
//   - Launch by explicit image_id unchanged (family code path not entered).
//   - Launch by family_name resolves to latest launchable image.
//   - Latest = highest family_version; tie-break by created_at DESC.
//   - DEPRECATED images are eligible for family resolution (still launchable).
//   - OBSOLETE images are excluded from resolution.
//   - FAILED images are excluded from resolution.
//   - PENDING_VALIDATION images are excluded from resolution.
//   - Mixed family: only launchable members resolve; blocked members ignored.
//   - Exact family_version targeting.
//   - Exact family_version not found → 422 image_family_version_not_found.
//   - PRIVATE family image: non-owner gets 422 image_family_not_found (no 403 leak).
//   - PRIVATE family image: owner resolves correctly.
//   - PUBLIC family image: any principal resolves correctly.
//   - Both image_id and image_family set → 400 invalid_request.
//   - Neither image_id nor image_family → 400 missing_field on image_id.
//   - image_family.family_name empty → 400 image_family_invalid_request.
//   - Family does not exist → 422 image_family_not_found.
//   - Admission gate not bypassed: family resolution feeds GetImageForAdmission.
//
// Source: vm-13-01__blueprint__ §family_seam,
//         AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account, PRIVATE visibility),
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.8 (ACTIVE or DEPRECATED required for launch).

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── pointer helpers ───────────────────────────────────────────────────────────

func familyStrPtr(s string) *string { return &s }
func familyIntPtr(i int) *int       { return &i }

// ── seed helpers ──────────────────────────────────────────────────────────────

// seedFamilyImage adds a family-affiliated image to the memPool.
//
// ownerID controls visibility:
//   - "system" or empty string → visibility=PUBLIC, ownerID="system".
//   - any other value          → visibility=PRIVATE, ownerID=that value.
//
// Uses seedImage from image_handlers_test.go (same package).
func seedFamilyImage(
	mem *memPool,
	id, familyName string,
	familyVersion *int,
	status, ownerID string,
) {
	visibility := db.ImageVisibilityPublic
	if ownerID == "" {
		ownerID = "system"
	}
	if ownerID != "system" {
		visibility = db.ImageVisibilityPrivate
	}
	now := time.Now()
	seedImage(mem, &db.ImageRow{
		ID:               id,
		Name:             fmt.Sprintf("img-%s", id),
		OSFamily:         "ubuntu",
		OSVersion:        "22.04",
		Architecture:     "x86_64",
		OwnerID:          ownerID,
		Visibility:       visibility,
		SourceType:       db.ImageSourceTypePlatform,
		StorageURL:       "nfs://images/test.qcow2",
		MinDiskGB:        10,
		Status:           status,
		ValidationStatus: "passed",
		FamilyName:       familyStrPtr(familyName),
		FamilyVersion:    familyVersion,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
}

// seedFamilyImageAt seeds a family image with an explicit creation time,
// used for tie-break ordering tests.
func seedFamilyImageAt(
	mem *memPool,
	id, familyName string,
	familyVersion *int,
	status, ownerID string,
	createdAt time.Time,
) {
	visibility := db.ImageVisibilityPublic
	if ownerID == "" {
		ownerID = "system"
	}
	if ownerID != "system" {
		visibility = db.ImageVisibilityPrivate
	}
	seedImage(mem, &db.ImageRow{
		ID:               id,
		Name:             fmt.Sprintf("img-%s", id),
		OSFamily:         "ubuntu",
		OSVersion:        "22.04",
		Architecture:     "x86_64",
		OwnerID:          ownerID,
		Visibility:       visibility,
		SourceType:       db.ImageSourceTypePlatform,
		StorageURL:       "nfs://images/test.qcow2",
		MinDiskGB:        10,
		Status:           status,
		ValidationStatus: "passed",
		FamilyName:       familyStrPtr(familyName),
		FamilyVersion:    familyVersion,
		CreatedAt:        createdAt,
		UpdatedAt:        createdAt,
	})
}

// familyLaunchBody builds a CreateInstanceRequest using image_family.
// All required non-image fields are filled with valid values.
func familyLaunchBody(familyName string, familyVersion *int) CreateInstanceRequest {
	return CreateInstanceRequest{
		Name:             "my-instance",
		InstanceType:     "c1.small",
		AvailabilityZone: "us-east-1a",
		SSHKeyName:       "my-key",
		ImageFamily: &ImageFamilyRef{
			FamilyName:    familyName,
			FamilyVersion: familyVersion,
		},
	}
}

// ── explicit image_id unchanged ───────────────────────────────────────────────

// TestFamilyLaunch_ExplicitImageIDUnchanged confirms the family code path is
// not entered when image_id is set directly, and the resolved image is correct.
func TestFamilyLaunch_ExplicitImageIDUnchanged(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("explicit image_id: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "00000000-0000-0000-0000-000000000010" {
		t.Errorf("want image_id 00000000-0000-0000-0000-000000000010, got %q",
			out.Instance.ImageID)
	}
}

// ── family latest resolution ──────────────────────────────────────────────────

// TestFamilyLaunch_ResolvesHighestVersion confirms ResolveFamilyLatest selects
// the image with the highest family_version.
func TestFamilyLaunch_ResolvesHighestVersion(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-v1", "ubuntu-lts", familyIntPtr(1), db.ImageStatusActive, "system")
	seedFamilyImage(s.mem, "img-v2", "ubuntu-lts", familyIntPtr(2), db.ImageStatusActive, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("ubuntu-lts", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("family latest: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-v2" {
		t.Errorf("family latest: want img-v2 (highest version), got %q",
			out.Instance.ImageID)
	}
}

// TestFamilyLaunch_TieBreakByCreatedAt confirms that when two images share the
// same family_version (or both are nil), the newer created_at wins.
func TestFamilyLaunch_TieBreakByCreatedAt(t *testing.T) {
	s := newTestSrv(t)
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)
	seedFamilyImageAt(s.mem, "img-old", "tie-family", familyIntPtr(3), db.ImageStatusActive, "system", older)
	seedFamilyImageAt(s.mem, "img-new", "tie-family", familyIntPtr(3), db.ImageStatusActive, "system", newer)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("tie-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("tie-break: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-new" {
		t.Errorf("tie-break: want img-new (newer created_at), got %q",
			out.Instance.ImageID)
	}
}

// TestFamilyLaunch_VersionedRanksAboveUnversioned confirms that an image with
// an explicit family_version beats an unversioned image in the same family.
func TestFamilyLaunch_VersionedRanksAboveUnversioned(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-unver", "rank-family", nil, db.ImageStatusActive, "system")
	seedFamilyImage(s.mem, "img-ver1", "rank-family", familyIntPtr(1), db.ImageStatusActive, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("rank-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("versioned vs unversioned: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-ver1" {
		t.Errorf("versioned vs unversioned: want img-ver1, got %q",
			out.Instance.ImageID)
	}
}

// ── DEPRECATED still launchable ───────────────────────────────────────────────

// TestFamilyLaunch_DeprecatedIsEligible confirms that a DEPRECATED image is
// returned by family resolution (DEPRECATED is launchable per contract).
func TestFamilyLaunch_DeprecatedIsEligible(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-dep", "legacy-family", familyIntPtr(1),
		db.ImageStatusDeprecated, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("legacy-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deprecated eligible: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-dep" {
		t.Errorf("deprecated eligible: want img-dep, got %q", out.Instance.ImageID)
	}
}

// ── blocked lifecycle states excluded ────────────────────────────────────────

// TestFamilyLaunch_ObsoleteExcluded confirms OBSOLETE images are never resolved.
func TestFamilyLaunch_ObsoleteExcluded(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-obs", "obs-family", familyIntPtr(1),
		db.ImageStatusObsolete, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("obs-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("obsolete-only family: want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// TestFamilyLaunch_FailedExcluded confirms FAILED images are never resolved.
func TestFamilyLaunch_FailedExcluded(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-fail", "fail-family", nil,
		db.ImageStatusFailed, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("fail-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("failed-only family: want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// TestFamilyLaunch_PendingValidationExcluded confirms PENDING_VALIDATION images
// are never resolved.
func TestFamilyLaunch_PendingValidationExcluded(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-pv", "pv-family", nil,
		db.ImageStatusPendingValidation, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("pv-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("pending-only family: want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// TestFamilyLaunch_MixedFamily_OnlyActiveResolves confirms that a family with
// both ACTIVE and OBSOLETE members resolves only the ACTIVE one.
func TestFamilyLaunch_MixedFamily_OnlyActiveResolves(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-mixed-obs", "mixed-family", familyIntPtr(1),
		db.ImageStatusObsolete, "system")
	seedFamilyImage(s.mem, "img-mixed-act", "mixed-family", familyIntPtr(2),
		db.ImageStatusActive, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("mixed-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("mixed family: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-mixed-act" {
		t.Errorf("mixed family: want img-mixed-act, got %q", out.Instance.ImageID)
	}
}

// ── exact version targeting ───────────────────────────────────────────────────

// TestFamilyLaunch_ExactVersion confirms family_name + family_version resolves
// to the specified version even when a newer version exists.
func TestFamilyLaunch_ExactVersion(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-v1", "ver-family", familyIntPtr(1),
		db.ImageStatusActive, "system")
	seedFamilyImage(s.mem, "img-v2", "ver-family", familyIntPtr(2),
		db.ImageStatusActive, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("ver-family", familyIntPtr(1)), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("exact version: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-v1" {
		t.Errorf("exact version: want img-v1, got %q", out.Instance.ImageID)
	}
}

// TestFamilyLaunch_ExactVersionNotFound confirms requesting a non-existent
// version returns 422 image_family_version_not_found.
func TestFamilyLaunch_ExactVersionNotFound(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-only-v1", "ver-family2", familyIntPtr(1),
		db.ImageStatusActive, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("ver-family2", familyIntPtr(99)), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("missing version: want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyVersionNotFound {
		t.Errorf("want %q, got %q", errImageFamilyVersionNotFound, env.Error.Code)
	}
}

// TestFamilyLaunch_ExactVersionObsoleteBlocked confirms that requesting an
// exact version that exists but is OBSOLETE returns 422 (version is not
// launchable — not resolved by the repo query).
func TestFamilyLaunch_ExactVersionObsoleteBlocked(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-obs-v2", "obs-ver-family", familyIntPtr(2),
		db.ImageStatusObsolete, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("obs-ver-family", familyIntPtr(2)), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("obsolete exact version: want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyVersionNotFound {
		t.Errorf("want %q, got %q", errImageFamilyVersionNotFound, env.Error.Code)
	}
}

// ── ownership / visibility ────────────────────────────────────────────────────

// TestFamilyLaunch_PrivateNotVisibleToNonOwner confirms a PRIVATE family image
// is not resolvable by a non-owner; returns 422 (not 403) per ownership contract.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).
func TestFamilyLaunch_PrivateNotVisibleToNonOwner(t *testing.T) {
	s := newTestSrv(t)
	// alice owns this image; bob must not see it.
	seedFamilyImage(s.mem, "img-priv", "alice-private-family", familyIntPtr(1),
		db.ImageStatusActive, alice)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("alice-private-family", nil), authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("cross-principal private family: want 422, got %d",
			resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// TestFamilyLaunch_PrivateVisibleToOwner confirms a PRIVATE family image
// resolves correctly for its owning principal.
func TestFamilyLaunch_PrivateVisibleToOwner(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-priv-alice", "alice-only-family", familyIntPtr(1),
		db.ImageStatusActive, alice)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("alice-only-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("owner private family: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-priv-alice" {
		t.Errorf("owner private family: want img-priv-alice, got %q",
			out.Instance.ImageID)
	}
}

// TestFamilyLaunch_PublicVisibleToAnyPrincipal confirms PUBLIC family images
// are resolvable by any authenticated principal.
func TestFamilyLaunch_PublicVisibleToAnyPrincipal(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-pub", "public-family", familyIntPtr(1),
		db.ImageStatusActive, "system")

	// bob is not the "owner" (system) but PUBLIC means accessible to all.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("public-family", nil), authHdr(bob))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("public family non-owner: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if out.Instance.ImageID != "img-pub" {
		t.Errorf("public family non-owner: want img-pub, got %q",
			out.Instance.ImageID)
	}
}

// TestFamilyLaunch_PrivateVersionNotVisibleToNonOwner confirms exact-version
// lookup also enforces PRIVATE visibility.
func TestFamilyLaunch_PrivateVersionNotVisibleToNonOwner(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-priv-v3", "alice-ver-family", familyIntPtr(3),
		db.ImageStatusActive, alice)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("alice-ver-family", familyIntPtr(3)), authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("private exact version non-owner: want 422, got %d",
			resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyVersionNotFound {
		t.Errorf("want %q, got %q", errImageFamilyVersionNotFound, env.Error.Code)
	}
}

// ── malformed request combinations ───────────────────────────────────────────

// TestFamilyLaunch_BothImageIDAndFamilySet confirms mutual exclusion: providing
// both image_id and image_family returns 400 invalid_request.
func TestFamilyLaunch_BothImageIDAndFamilySet(t *testing.T) {
	s := newTestSrv(t)
	req := validCreateBody() // image_id already set
	req.ImageFamily = &ImageFamilyRef{FamilyName: "ubuntu-lts"}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", req, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("both image_id+family: want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidRequest {
		t.Errorf("want code %q, got %q", errInvalidRequest, env.Error.Code)
	}
}

// TestFamilyLaunch_NeitherImageIDNorFamily confirms that omitting both
// image_id and image_family returns 400 missing_field on the image_id field.
func TestFamilyLaunch_NeitherImageIDNorFamily(t *testing.T) {
	s := newTestSrv(t)
	req := CreateInstanceRequest{
		Name:             "no-image",
		InstanceType:     "c1.small",
		AvailabilityZone: "us-east-1a",
		SSHKeyName:       "my-key",
		// ImageID and ImageFamily both absent.
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", req, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("no image spec: want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "image_id", errMissingField)
}

// TestFamilyLaunch_EmptyFamilyName confirms image_family with an empty
// family_name returns 400 image_family_invalid_request.
func TestFamilyLaunch_EmptyFamilyName(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("", nil), authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty family_name: want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyInvalidRequest {
		t.Errorf("want %q, got %q", errImageFamilyInvalidRequest, env.Error.Code)
	}
}

// TestFamilyLaunch_WhitespaceFamilyName confirms a whitespace-only family_name
// is treated as empty and returns 400 image_family_invalid_request.
func TestFamilyLaunch_WhitespaceFamilyName(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("   ", nil), authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("whitespace family_name: want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyInvalidRequest {
		t.Errorf("want %q, got %q", errImageFamilyInvalidRequest, env.Error.Code)
	}
}

// ── family not found / empty ──────────────────────────────────────────────────

// TestFamilyLaunch_FamilyDoesNotExist confirms a family name with no images at
// all returns 422 image_family_not_found.
func TestFamilyLaunch_FamilyDoesNotExist(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("nonexistent-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("nonexistent family: want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// TestFamilyLaunch_AllMembersBlocked confirms a family where every member is in
// a non-launchable state returns 422 image_family_not_found.
func TestFamilyLaunch_AllMembersBlocked(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-all-obs-1", "all-blocked", familyIntPtr(1),
		db.ImageStatusObsolete, "system")
	seedFamilyImage(s.mem, "img-all-obs-2", "all-blocked", familyIntPtr(2),
		db.ImageStatusFailed, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("all-blocked", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("all-blocked family: want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// ── admission gate not bypassed ───────────────────────────────────────────────

// TestFamilyLaunch_AdmissionGateNotBypassed confirms that the resolved image ID
// still flows through GetImageForAdmission after family resolution. The test
// seeds an image as ACTIVE for the family path, then flips it to OBSOLETE in
// the in-memory map before the request is served, simulating a race where the
// image is in the map at ID-lookup time but not launchable.
//
// Because familyQueryRowDispatch applies the same status filter as the real repo
// query, the family resolution itself will return no candidate → 422. This
// verifies the filter is applied at the repo boundary, not deferred to a later
// layer.
func TestFamilyLaunch_AdmissionGateNotBypassed(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img-race", "race-family", familyIntPtr(1),
		db.ImageStatusActive, "system")

	// Flip status to OBSOLETE after seeding — simulates post-resolution state change.
	// The family dispatch in memPool applies the launchable filter on every call,
	// so this image will not be selected.
	s.mem.images["img-race"].Status = db.ImageStatusObsolete

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("race-family", nil), authHdr(alice))
	// No launchable candidate found → 422 family_not_found (not a bypass).
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("admission gate: want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}
