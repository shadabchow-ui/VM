package main

// image_handlers_test.go — VM-P2C-P1: tests for GET /v1/images, GET /v1/images/{id}.
//
// Coverage:
//   LIST:
//     - Empty list → 200 + empty images array
//     - Returns PUBLIC images to any principal
//     - Returns PRIVATE images only to owner
//     - Does not return PRIVATE images of other principals
//     - Missing auth → 401
//
//   GET:
//     - Happy path: PUBLIC image → 200 + ImageResponse
//     - PRIVATE image by owner → 200 + ImageResponse
//     - Image not found → 404 image_not_found
//     - PRIVATE image by non-owner → 404 image_not_found (ownership hidden)
//     - Non-existent subpath → 404
//     - Method not allowed → 405
//
//   ADMISSION (create instance with various image states):
//     - ACTIVE image → 202 (passes)
//     - DEPRECATED image → 202 (passes; deprecated is still launchable)
//     - OBSOLETE image → 422 image_not_launchable
//     - FAILED image → 422 image_not_launchable
//     - PENDING_VALIDATION image → 422 image_not_launchable
//     - Non-existent image_id → 422 invalid_image_id
//     - PRIVATE image not owned by caller → 422 invalid_image_id (treated as not found)
//
// Test strategy: in-process httptest.Server backed by memPool (fake db.Pool).
// Source: 11-02-phase-1-test-strategy.md §unit test approach.

import (
	"net/http"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Helpers ───────────────────────────────────────────────────────────────────

// seedImage adds an image to the memPool.
func seedImage(mem *memPool, img *db.ImageRow) {
	if img.CreatedAt.IsZero() {
		img.CreatedAt = time.Now()
	}
	if img.UpdatedAt.IsZero() {
		img.UpdatedAt = time.Now()
	}
	if img.ValidationStatus == "" {
		img.ValidationStatus = "passed"
	}
	mem.images[img.ID] = img
}

// publicImage returns a PUBLIC platform image row for seeding.
func publicImage(id, name, status string) *db.ImageRow {
	return &db.ImageRow{
		ID:               id,
		Name:             name,
		OSFamily:         "ubuntu",
		OSVersion:        "22.04",
		Architecture:     "x86_64",
		OwnerID:          "system",
		Visibility:       "PUBLIC",
		SourceType:       "PLATFORM",
		StorageURL:       "nfs://images/" + name + ".qcow2",
		MinDiskGB:        10,
		Status:           status,
		ValidationStatus: "passed",
	}
}

// privateImage returns a PRIVATE user-owned image row for seeding.
func privateImage(id, name, ownerID, status string) *db.ImageRow {
	return &db.ImageRow{
		ID:               id,
		Name:             name,
		OSFamily:         "ubuntu",
		OSVersion:        "22.04",
		Architecture:     "x86_64",
		OwnerID:          ownerID,
		Visibility:       "PRIVATE",
		SourceType:       "USER",
		StorageURL:       "nfs://images/" + name + ".qcow2",
		MinDiskGB:        10,
		Status:           status,
		ValidationStatus: "passed",
	}
}

// ── LIST tests ────────────────────────────────────────────────────────────────

func TestListImages_Empty(t *testing.T) {
	s := newTestSrv(t)
	// Remove the default seeded images to test truly empty.
	s.mem.images = make(map[string]*db.ImageRow)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImagesResponse
	decodeBody(t, resp, &out)
	if out.Total != 0 {
		t.Errorf("want total=0, got %d", out.Total)
	}
	if out.Images == nil {
		t.Error("want non-nil images slice (empty, not null)")
	}
}

func TestListImages_ReturnsPublicImages(t *testing.T) {
	s := newTestSrv(t)
	// newTestSrv already seeds two PUBLIC images.

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImagesResponse
	decodeBody(t, resp, &out)
	if out.Total < 2 {
		t.Errorf("want at least 2 public images, got %d", out.Total)
	}
}

func TestListImages_ReturnsOwnPrivateImages(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_priv_alice", "alice-custom", alice, "ACTIVE"))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImagesResponse
	decodeBody(t, resp, &out)

	found := false
	for _, img := range out.Images {
		if img.ID == "img_priv_alice" {
			found = true
			if img.Visibility != "PRIVATE" {
				t.Errorf("want visibility=PRIVATE, got %q", img.Visibility)
			}
		}
	}
	if !found {
		t.Error("alice's private image not returned in her own list")
	}
}

func TestListImages_DoesNotReturnOtherPrincipalsPrivateImages(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_priv_bob", "bob-custom", bob, "ACTIVE"))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImagesResponse
	decodeBody(t, resp, &out)

	for _, img := range out.Images {
		if img.ID == "img_priv_bob" {
			t.Error("bob's private image must not appear in alice's list")
		}
	}
}

func TestListImages_MissingAuth(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

func TestListImages_MethodNotAllowed(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images", nil, authHdr(alice))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", resp.StatusCode)
	}
}

// ── GET /v1/images/{id} tests ─────────────────────────────────────────────────

func TestGetImage_PublicImage(t *testing.T) {
	s := newTestSrv(t)
	// The default seeded image is PUBLIC.
	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/00000000-0000-0000-0000-000000000010",
		nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ImageResponse
	decodeBody(t, resp, &out)
	if out.ID != "00000000-0000-0000-0000-000000000010" {
		t.Errorf("want correct image ID, got %q", out.ID)
	}
	if out.Status != "ACTIVE" {
		t.Errorf("want status=ACTIVE, got %q", out.Status)
	}
	if out.Visibility != "PUBLIC" {
		t.Errorf("want visibility=PUBLIC, got %q", out.Visibility)
	}
}

func TestGetImage_PrivateImageByOwner(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_priv_alice2", "alice-custom2", alice, "ACTIVE"))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_priv_alice2", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ImageResponse
	decodeBody(t, resp, &out)
	if out.ID != "img_priv_alice2" {
		t.Errorf("want image ID img_priv_alice2, got %q", out.ID)
	}
}

func TestGetImage_NotFound(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/nonexistent-id", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

func TestGetImage_PrivateImageByNonOwner(t *testing.T) {
	// PRIVATE image owned by bob must not be accessible to alice.
	// Must return 404, not 403. Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_priv_bob2", "bob-custom2", bob, "ACTIVE"))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_priv_bob2", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (not 403), got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

func TestGetImage_ResponseShape(t *testing.T) {
	// Verify storage_url and owner_id are not leaked in the response.
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/00000000-0000-0000-0000-000000000010",
		nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	// Decode as raw map to check excluded fields.
	var raw map[string]any
	decodeBody(t, resp, &raw)
	if _, ok := raw["storage_url"]; ok {
		t.Error("storage_url must not be present in image response")
	}
	if _, ok := raw["owner_id"]; ok {
		t.Error("owner_id must not be present in image response")
	}
	if _, ok := raw["validation_status"]; ok {
		t.Error("validation_status must not be present in image response")
	}
	// Required fields must be present.
	for _, field := range []string{"id", "name", "os_family", "os_version", "architecture",
		"visibility", "source_type", "min_disk_gb", "status", "created_at", "updated_at"} {
		if _, ok := raw[field]; !ok {
			t.Errorf("required field %q missing from image response", field)
		}
	}
}

func TestGetImage_MissingAuth(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/00000000-0000-0000-0000-000000000010",
		nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// ── Admission tests (create instance with various image states) ───────────────

// imageAdmissionBody returns a CreateInstanceRequest using a custom image_id.
func imageAdmissionBody(imageID string) CreateInstanceRequest {
	return CreateInstanceRequest{
		Name:             "test-inst",
		InstanceType:     "c1.small",
		ImageID:          imageID,
		AvailabilityZone: "us-east-1a",
		SSHKeyName:       "my-key",
	}
}

func TestCreateInstance_ActiveImage_Passes(t *testing.T) {
	s := newTestSrv(t)
	// Default seeded image is ACTIVE — standard happy path.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("00000000-0000-0000-0000-000000000010"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
}

func TestCreateInstance_DeprecatedImage_Passes(t *testing.T) {
	// DEPRECATED images are still launchable.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.8, db.ImageIsLaunchable.
	s := newTestSrv(t)
	seedImage(s.mem, publicImage("img_deprecated", "old-ubuntu", "DEPRECATED"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_deprecated"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for DEPRECATED image, got %d", resp.StatusCode)
	}
}

func TestCreateInstance_ObsoleteImage_Blocked(t *testing.T) {
	// OBSOLETE images must be rejected.
	// Source: vm-13-01__blueprint__ §core_contracts "Image Lifecycle State Enforcement".
	s := newTestSrv(t)
	seedImage(s.mem, publicImage("img_obsolete", "very-old-ubuntu", "OBSOLETE"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_obsolete"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for OBSOLETE image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want code %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

func TestCreateInstance_FailedImage_Blocked(t *testing.T) {
	// FAILED images must be rejected.
	// Source: vm-13-01__blueprint__ §core_contracts.
	s := newTestSrv(t)
	seedImage(s.mem, publicImage("img_failed", "broken-ubuntu", "FAILED"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_failed"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for FAILED image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want code %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

func TestCreateInstance_PendingValidationImage_Blocked(t *testing.T) {
	// PENDING_VALIDATION images must be rejected.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.8 (only ACTIVE allowed for launch).
	s := newTestSrv(t)
	seedImage(s.mem, publicImage("img_pending", "pending-ubuntu", "PENDING_VALIDATION"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_pending"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for PENDING_VALIDATION image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want code %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

func TestCreateInstance_NonExistentImage_Blocked(t *testing.T) {
	// Non-existent image_id returns 422 invalid_image_id.
	// Source: image_handlers.go handleCreateInstance admission block.
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("00000000-0000-0000-0000-000000000099"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for non-existent image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidImageID {
		t.Errorf("want code %q, got %q", errInvalidImageID, env.Error.Code)
	}
}

func TestCreateInstance_PrivateImageNotOwnedByCaller_Blocked(t *testing.T) {
	// PRIVATE image owned by bob is not visible to alice → treated as not found.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_bob_private", "bob-private", bob, "ACTIVE"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_bob_private"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for non-accessible PRIVATE image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidImageID {
		t.Errorf("want code %q, got %q", errInvalidImageID, env.Error.Code)
	}
}

func TestCreateInstance_PrivateImageOwnedByCaller_Passes(t *testing.T) {
	// PRIVATE image owned by alice is accessible to alice.
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_alice_private", "alice-private", alice, "ACTIVE"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_alice_private"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for caller's own PRIVATE image, got %d", resp.StatusCode)
	}
}
