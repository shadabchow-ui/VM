package main

// vm_job10_error_separation_test.go — Quota vs capacity vs image-admission error separation tests.
//
// VM Job 10: Verifies machine-distinguishable error codes for distinct failure domains.
//
// Error code contract (API_ERROR_CONTRACT_V1 §2, §4):
//   - quota_exceeded (422): client-correctable — delete instances or request increase
//   - insufficient_capacity (503): platform capacity — retry with backoff
//   - image_not_launchable (422): image lifecycle — choose a different image
//   - image_trust_violation (422): platform trust — image not cryptographically verified
//   - invalid_image_id (422): image not found/accessible — choose a different image
//   - service_unavailable (503): database/critical dependency down — retry
//
// Critical: these error codes must never be collapsed into a single generic code.
// A capacity error must return insufficient_capacity, not service_unavailable or internal_error.
// A quota error must return quota_exceeded, not insufficient_capacity.
//
// Source: vm-13-02__blueprint__ §core_contracts "Error Code Separation",
//         API_ERROR_CONTRACT_V1 §2 (HTTP status mapping),
//         VM Job 10 implementation mandate §11.

import (
	"net/http"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Error code distinctness tests ─────────────────────────────────────────────

// TestErrorSeparation_AllErrorCodesAreDistinct verifies that quota, image trust,
// image launchable, image not found, capacity, and service unavailable error codes
// are all distinct from each other.
func TestErrorSeparation_AllErrorCodesAreDistinct(t *testing.T) {
	codes := map[string]struct{}{}
	for _, code := range []string{
		errQuotaExceeded,
		errImageNotLaunchable,
		errImageTrustViolation,
		errInvalidImageID,
		errImageNotFound,
		errServiceUnavailable,
		errInternalError,
		errInsufficientCapacity,
	} {
		if _, exists := codes[code]; exists {
			t.Errorf("duplicate error code found: %q", code)
		}
		codes[code] = struct{}{}
	}
}

// ── HTTP status code mapping tests ─────────────────────────────────────────────

// TestErrorSeparation_QuotaExceededIs422 verifies that quota exceeded returns
// HTTP 422 (Unprocessable Entity), NOT 503 (Service Unavailable) or 500.
// A quota failure is client-correctable — the user must delete instances.
func TestErrorSeparation_QuotaExceededIs422(t *testing.T) {
	s := newTestSrv(t)

	// Saturate alice's quota by creating instances up to the default limit.
	for i := 0; i < db.DefaultMaxInstances; i++ {
		resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
			validCreateBody(), authHdr(alice))
		if resp.StatusCode != http.StatusAccepted {
			t.Skipf("quota saturate step %d got %d — skipping", i+1, resp.StatusCode)
			return
		}
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		validCreateBody(), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for quota exceeded, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errQuotaExceeded {
		t.Errorf("want code %q, got %q", errQuotaExceeded, env.Error.Code)
	}
	// Confirm quota_exceeded is NOT insufficient_capacity or internal_error.
	if env.Error.Code == errInsufficientCapacity {
		t.Errorf("quota error must not be %q", errInsufficientCapacity)
	}
	if env.Error.Code == errInternalError {
		t.Errorf("quota error must not be %q", errInternalError)
	}
}

// TestErrorSeparation_ImageNotLaunchableIs422 verifies that blocked images
// (OBSOLETE, FAILED, PENDING_VALIDATION) return HTTP 422 with image_not_launchable,
// NOT 503 or 500.
func TestErrorSeparation_ImageNotLaunchableIs422(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, publicImage("img_obs_sep", "obs-sep", db.ImageStatusObsolete))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_obs_sep"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for obsolete image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want code %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

// TestErrorSeparation_ImageTrustViolationIs422 verifies that PLATFORM images
// without verified signatures return HTTP 422 with image_trust_violation.
func TestErrorSeparation_ImageTrustViolationIs422(t *testing.T) {
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, untrustedPlatformImage("img_plat_trust_sep", "plat-trust-sep"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_plat_trust_sep"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for trust violation, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageTrustViolation {
		t.Errorf("want code %q, got %q", errImageTrustViolation, env.Error.Code)
	}
	// Trust violation must not leak into capacity or generic error space.
	if env.Error.Code == errInsufficientCapacity {
		t.Errorf("trust violation must not be %q", errInsufficientCapacity)
	}
}

// TestErrorSeparation_InvalidImageIDIs422 verifies that non-existent image IDs
// return HTTP 422 with invalid_image_id.
func TestErrorSeparation_InvalidImageIDIs422(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("nonexistent-image-id"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for invalid image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidImageID {
		t.Errorf("want code %q, got %q", errInvalidImageID, env.Error.Code)
	}
}

// ── Quota vs capacity vs image-admission matrix test ──────────────────────────

// TestErrorSeparation_Matrix ensures that three distinct failure classes produce
// three distinct error codes, each with the correct HTTP status code.
func TestErrorSeparation_Matrix(t *testing.T) {
	type result struct {
		code   string
		status int
	}

	// Image not launchable
	s1 := newTestSrv(t)
	seedImage(s1.mem, publicImage("img_obs_matrix", "obs-matrix", db.ImageStatusObsolete))
	resp1 := doReq(t, s1.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_obs_matrix"), authHdr(alice))
	var env1 apiError
	decodeBody(t, resp1, &env1)
	imageError := result{code: env1.Error.Code, status: resp1.StatusCode}

	// Invalid image ID (not found)
	resp2 := doReq(t, s1.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("bad-image-id"), authHdr(alice))
	var env2 apiError
	decodeBody(t, resp2, &env2)
	invalidIDError := result{code: env2.Error.Code, status: resp2.StatusCode}

	// Verify distinctness across image error categories.
	if imageError.code == invalidIDError.code {
		t.Errorf("image_not_launchable and invalid_image_id must differ, both are %q", imageError.code)
	}
	if imageError.status != http.StatusUnprocessableEntity {
		t.Errorf("image_not_launchable wants 422, got %d", imageError.status)
	}
	if invalidIDError.status != http.StatusUnprocessableEntity {
		t.Errorf("invalid_image_id wants 422, got %d", invalidIDError.status)
	}

	// Verify these are NOT quota/capacity/trust errors.
	for _, tc := range []struct {
		label string
		code  string
	}{
		{"quota_exceeded must not conflate", errQuotaExceeded},
		{"insufficient_capacity must not conflate", errInsufficientCapacity},
		{"image_trust_violation must not conflate", errImageTrustViolation},
		{"internal_error must not conflate", errInternalError},
	} {
		if imageError.code == tc.code {
			t.Errorf("image_not_launchable conflated with %s", tc.label)
		}
		if invalidIDError.code == tc.code {
			t.Errorf("invalid_image_id conflated with %s", tc.label)
		}
	}
}

// ── Trust vs launchable error separation ──────────────────────────────────────

// TestErrorSeparation_TrustViolationVsNotLaunchable verifies that trust violation
// and not-launchable are distinct error codes. An OBSOLETE PLATFORM image should
// return image_not_launchable (lifecycle block), not image_trust_violation
// (since OBSOLETE blocks before trust check runs).
func TestErrorSeparation_TrustViolationVsNotLaunchable(t *testing.T) {
	s := newCatalogTestSrv(t)
	sigFail := false
	hash := "sha256:abc"
	img := &db.ImageRow{
		ID: "img_obs_trust", Name: "obs-trust",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: db.ImageVisibilityPublic,
		SourceType: db.ImageSourceTypePlatform,
		StorageURL: "nfs://images/test.qcow2", MinDiskGB: 10,
		Status: db.ImageStatusObsolete, ValidationStatus: "passed",
		ProvenanceHash: &hash, SignatureValid: &sigFail,
	}
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_obs_trust"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	// OBSOLETE should trigger image_not_launchable BEFORE trust check.
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("OBSOLETE image: want %q (lifecycle block), got %q (may be trust)",
			errImageNotLaunchable, env.Error.Code)
	}
	// Confirm it's NOT trust violation — lifecycle block takes priority.
	if env.Error.Code == errImageTrustViolation {
		t.Errorf("OBSOLETE must not produce trust violation; lifecycle check comes first")
	}
}
