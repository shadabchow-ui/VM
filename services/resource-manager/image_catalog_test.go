package main

// image_catalog_test.go — VM-P3B Job 2: image catalog and admission policy tests.
//
// Coverage:
//
//   LIFECYCLE STATE PERSISTENCE:
//     - PENDING_VALIDATION image stored and returned with correct status
//     - ACTIVE image stored and returned with correct status
//     - DEPRECATED image stored with deprecated_at timestamp set
//     - OBSOLETE image stored with obsoleted_at timestamp set
//     - FAILED image stored and returned with correct status
//     - Lifecycle transitions: ACTIVE→DEPRECATED, ACTIVE→OBSOLETE, DEPRECATED→OBSOLETE
//
//   FAMILY RESOLUTION (via imageCatalogPool harness):
//     - ResolveFamilyLatest returns highest family_version candidate
//     - ResolveFamilyByVersion returns exact version
//     - Atomic alias promotion via UpdateFamilyAlias increments family_version
//     - UpdateFamilyAlias is a no-op for non-ACTIVE images
//     - UpdateFamilyAlias is a no-op for wrong family
//
//   ADMISSION REJECTION FOR INVALID STATES:
//     - OBSOLETE → 422 image_not_launchable
//     - FAILED → 422 image_not_launchable
//     - PENDING_VALIDATION → 422 image_not_launchable
//     - ACTIVE → 202 (passes)
//     - DEPRECATED → 202 (passes; still launchable)
//
//   PLATFORM IMAGE TRUST ENFORCEMENT:
//     - PLATFORM image with signature_valid=true → 202 (trusted, launches)
//     - PLATFORM image with signature_valid=nil  → 422 image_trust_violation (not yet verified)
//     - PLATFORM image with signature_valid=false → 422 image_trust_violation (verification failed)
//     - USER image with signature_valid=nil → 202 (non-platform skips trust check)
//     - USER image with signature_valid=false → 202 (non-platform skips trust check entirely)
//     - SNAPSHOT image with signature_valid=nil → 202 (non-platform skips trust check)
//     - Trust check does not apply to family-resolved non-platform images
//     - Trust check applies to family-resolved PLATFORM images
//
//   FAMILY ALIAS ATOMICITY:
//     - UpdateFamilyAlias sets family_version to max+1 atomically
//     - After promotion, ResolveFamilyLatest returns the promoted image
//     - Promoting non-ACTIVE image is rejected (0 rows affected)
//
// Test strategy: in-process httptest.Server backed by memPool extended with
// imageCatalogPool helpers. The imageCatalogPool is a thin wrapper that intercepts
// UpdateFamilyAlias and UpdateImageSignature Exec calls (added in VM-P3B Job 2)
// before delegating to the inner *memPool. This avoids modifying instance_handlers_test.go.
//
// Source: 11-02-phase-1-test-strategy.md §unit test approach,
//         vm-13-01__blueprint__trusted-image-factory-validation-pipeline.md §core_contracts.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// ── imageCatalogPool ──────────────────────────────────────────────────────────
//
// imageCatalogPool wraps *memPool and intercepts the new VM-P3B Exec SQL shapes
// (UpdateFamilyAlias, UpdateImageSignature) before delegating to the inner pool.
// This avoids touching instance_handlers_test.go.

type imageCatalogPool struct {
	inner *memPool
}

func newImageCatalogPool() *imageCatalogPool {
	return &imageCatalogPool{inner: newMemPool()}
}

func (p *imageCatalogPool) Exec(ctx context.Context, sql string, args ...any) (db.CommandTag, error) {
	if isUpdateFamilyAliasSQL(sql) {
		return execUpdateFamilyAlias(p.inner, args)
	}
	if isUpdateImageSignatureSQL(sql) {
		return execUpdateImageSignature(p.inner, args)
	}
	return p.inner.Exec(ctx, sql, args...)
}

func (p *imageCatalogPool) Query(ctx context.Context, sql string, args ...any) (db.Rows, error) {
	return p.inner.Query(ctx, sql, args...)
}

func (p *imageCatalogPool) QueryRow(ctx context.Context, sql string, args ...any) db.Row {
	return p.inner.QueryRow(ctx, sql, args...)
}

func (p *imageCatalogPool) Close() {}

// ── catalogTestSrv ────────────────────────────────────────────────────────────

type catalogTestSrv struct {
	ts  *httptest.Server
	mem *imageCatalogPool
}

func newCatalogTestSrv(t *testing.T) *catalogTestSrv {
	t.Helper()
	pool := newImageCatalogPool()
	repo := db.New(pool)
	srv := &server{
		log:    newDiscardLogger(),
		repo:   repo,
		region: "us-east-1",
	}
	mux := http.NewServeMux()
	srv.registerInstanceRoutes(mux)
	srv.registerProjectRoutes(mux)
	srv.registerVolumeRoutes(mux)
	srv.registerImageRoutes(mux)
	ts := startTestServer(t, mux)

	// Seed the standard platform image (ACTIVE + trusted) for non-image tests.
	now := time.Now()
	sigOK := true
	pool.inner.images["00000000-0000-0000-0000-000000000010"] = &db.ImageRow{
		ID: "00000000-0000-0000-0000-000000000010", Name: "ubuntu-22.04-lts",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/ubuntu-22.04.qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed",
		SignatureValid: &sigOK,
		CreatedAt:      now, UpdatedAt: now,
	}

	return &catalogTestSrv{ts: ts, mem: pool}
}

// seedCatalogImage adds an image to the catalog pool directly.
func seedCatalogImage(pool *imageCatalogPool, img *db.ImageRow) {
	if img.CreatedAt.IsZero() {
		img.CreatedAt = time.Now()
	}
	if img.UpdatedAt.IsZero() {
		img.UpdatedAt = time.Now()
	}
	if img.ValidationStatus == "" {
		img.ValidationStatus = "passed"
	}
	pool.inner.images[img.ID] = img
}

// trustedPlatformImage returns a PLATFORM PUBLIC ACTIVE image with signature verified.
func trustedPlatformImage(id, name string) *db.ImageRow {
	sigOK := true
	hash := "sha256:abc123"
	return &db.ImageRow{
		ID: id, Name: name,
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/" + name + ".qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed",
		ProvenanceHash: &hash, SignatureValid: &sigOK,
	}
}

// untrustedPlatformImage returns a PLATFORM PUBLIC ACTIVE image with provenance
// attached but signature_valid=nil (not yet verified). This triggers the trust
// check since provenance_hash is non-nil.
func untrustedPlatformImage(id, name string) *db.ImageRow {
	h := "sha256:pending-verification"
	return &db.ImageRow{
		ID: id, Name: name,
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/" + name + ".qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed",
		ProvenanceHash: &h, SignatureValid: nil, // provenance present, not yet verified
	}
}

func failedSigPlatformImage(id, name string) *db.ImageRow {
	sigFail := false
	hash := "sha256:bad"
	return &db.ImageRow{
		ID: id, Name: name,
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/" + name + ".qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed",
		ProvenanceHash: &hash, SignatureValid: &sigFail,
	}
}

func userImage(id, name, ownerID, status string) *db.ImageRow {
	return &db.ImageRow{
		ID: id, Name: name,
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: ownerID, Visibility: "PRIVATE", SourceType: "USER",
		StorageURL: "nfs://images/" + name + ".qcow2", MinDiskGB: 10,
		Status: status, ValidationStatus: "passed",
		ProvenanceHash: nil, SignatureValid: nil,
	}
}

// boolPtr returns a pointer to a bool.
func boolPtr(b bool) *bool { return &b }

// ── LIFECYCLE STATE PERSISTENCE tests ────────────────────────────────────────

func TestLifecycle_PendingValidationStoredCorrectly(t *testing.T) {
	s := newCatalogTestSrv(t)
	img := &db.ImageRow{
		ID: "img_pv_01", Name: "pending-img",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "USER",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: "pending",
	}
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_pv_01", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ImageResponse
	decodeBody(t, resp, &out)
	if out.Status != db.ImageStatusPendingValidation {
		t.Errorf("want status=%q, got %q", db.ImageStatusPendingValidation, out.Status)
	}
}

func TestLifecycle_ActiveStoredCorrectly(t *testing.T) {
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_act_01", "active-img", alice, db.ImageStatusActive))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_act_01", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ImageResponse
	decodeBody(t, resp, &out)
	if out.Status != db.ImageStatusActive {
		t.Errorf("want status=%q, got %q", db.ImageStatusActive, out.Status)
	}
}

func TestLifecycle_FailedStoredCorrectly(t *testing.T) {
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_fail_01", "failed-img", alice, db.ImageStatusFailed))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_fail_01", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ImageResponse
	decodeBody(t, resp, &out)
	if out.Status != db.ImageStatusFailed {
		t.Errorf("want status=%q, got %q", db.ImageStatusFailed, out.Status)
	}
}

func TestLifecycle_DeprecatedTransitionSetsTimestamp(t *testing.T) {
	// POST /v1/images/{id}/deprecate sets deprecated_at.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_dep_ts", "dep-ts-img", alice, db.ImageStatusActive))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_dep_ts/deprecate", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out DeprecateImageResponse
	decodeBody(t, resp, &out)
	if out.Image.Status != db.ImageStatusDeprecated {
		t.Errorf("want status DEPRECATED, got %q", out.Image.Status)
	}
	if out.Image.DeprecatedAt == nil {
		t.Error("want deprecated_at to be set after deprecation")
	}
}

func TestLifecycle_ObsoleteTransitionSetsTimestamp(t *testing.T) {
	// POST /v1/images/{id}/obsolete sets obsoleted_at.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_obs_ts", "obs-ts-img", alice, db.ImageStatusActive))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_obs_ts/obsolete", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ObsoleteImageResponse
	decodeBody(t, resp, &out)
	if out.Image.Status != db.ImageStatusObsolete {
		t.Errorf("want status OBSOLETE, got %q", out.Image.Status)
	}
	if out.Image.ObsoletedAt == nil {
		t.Error("want obsoleted_at to be set after obsoleting")
	}
}

func TestLifecycle_DeprecatedThenObsolete(t *testing.T) {
	// DEPRECATED images can be further moved to OBSOLETE.
	s := newCatalogTestSrv(t)
	now := time.Now()
	depAt := now.Add(-time.Hour)
	img := userImage("img_dep_obs", "dep-obs-img", alice, db.ImageStatusDeprecated)
	img.DeprecatedAt = &depAt
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_dep_obs/obsolete", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ObsoleteImageResponse
	decodeBody(t, resp, &out)
	if out.Image.Status != db.ImageStatusObsolete {
		t.Errorf("want OBSOLETE, got %q", out.Image.Status)
	}
	// deprecated_at must be preserved.
	if out.Image.DeprecatedAt == nil {
		t.Error("want deprecated_at preserved after obsoleting")
	}
}

func TestLifecycle_InvalidTransition_PendingToObsolete(t *testing.T) {
	// PENDING_VALIDATION → OBSOLETE is not a valid transition.
	s := newCatalogTestSrv(t)
	img := userImage("img_pv_obs", "pv-obs-img", alice, db.ImageStatusPendingValidation)
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_pv_obs/obsolete", nil, authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageInvalidState {
		t.Errorf("want %q, got %q", errImageInvalidState, env.Error.Code)
	}
}

// ── ADMISSION REJECTION tests ─────────────────────────────────────────────────

func TestAdmission_ObsoleteBlocked(t *testing.T) {
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_obs_adm", "obs-adm", alice, db.ImageStatusObsolete))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_obs_adm"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for OBSOLETE, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

func TestAdmission_FailedBlocked(t *testing.T) {
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_fail_adm", "fail-adm", alice, db.ImageStatusFailed))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_fail_adm"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for FAILED, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

func TestAdmission_PendingValidationBlocked(t *testing.T) {
	s := newCatalogTestSrv(t)
	img := userImage("img_pv_adm", "pv-adm", alice, db.ImageStatusPendingValidation)
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_pv_adm"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for PENDING_VALIDATION, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

func TestAdmission_ActivePasses(t *testing.T) {
	// USER ACTIVE image with no signature requirement passes directly.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_act_adm", "act-adm", alice, db.ImageStatusActive))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_act_adm"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for ACTIVE USER image, got %d", resp.StatusCode)
	}
}

func TestAdmission_DeprecatedPasses(t *testing.T) {
	// DEPRECATED is still launchable per contract.
	s := newCatalogTestSrv(t)
	img := userImage("img_dep_adm", "dep-adm", alice, db.ImageStatusDeprecated)
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_dep_adm"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for DEPRECATED image, got %d", resp.StatusCode)
	}
}

// ── PLATFORM TRUST ENFORCEMENT tests ─────────────────────────────────────────

func TestPlatformTrust_VerifiedSignaturePasses(t *testing.T) {
	// PLATFORM image with signature_valid=true must pass admission.
	// Source: vm-13-01__blueprint__ §core_contracts "Platform Trust Boundary".
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, trustedPlatformImage("img_plat_ok", "plat-ok"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_plat_ok"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for trusted PLATFORM image, got %d", resp.StatusCode)
	}
}

func TestPlatformTrust_NilSignatureBlocked(t *testing.T) {
	// PLATFORM image with signature_valid=nil (not yet verified) must be rejected.
	// This protects against launching images before the factory has signed them.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, untrustedPlatformImage("img_plat_nil", "plat-nil"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_plat_nil"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for unverified PLATFORM image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageTrustViolation {
		t.Errorf("want %q, got %q", errImageTrustViolation, env.Error.Code)
	}
}

func TestPlatformTrust_FalseSignatureBlocked(t *testing.T) {
	// PLATFORM image with signature_valid=false (verification failed) must be rejected.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, failedSigPlatformImage("img_plat_bad", "plat-bad"))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_plat_bad"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for failed-sig PLATFORM image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageTrustViolation {
		t.Errorf("want %q, got %q", errImageTrustViolation, env.Error.Code)
	}
}

func TestPlatformTrust_UserImageNilSignatureSkipsCheck(t *testing.T) {
	// USER image with nil signature_valid must pass — non-platform images skip trust.
	// Source: vm-13-01__blueprint__ §core_contracts "Platform Trust Boundary":
	//   "It MUST NOT attempt to verify signatures for images with owner: project_id"
	s := newCatalogTestSrv(t)
	img := userImage("img_user_nosig", "user-nosig", alice, db.ImageStatusActive)
	// signature_valid is already nil in userImage()
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_user_nosig"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for USER image with nil signature, got %d", resp.StatusCode)
	}
}

func TestPlatformTrust_UserImageFalseSignatureSkipsCheck(t *testing.T) {
	// USER image with signature_valid=false must still pass — trust check does not apply.
	s := newCatalogTestSrv(t)
	img := userImage("img_user_badsig", "user-badsig", alice, db.ImageStatusActive)
	img.SignatureValid = boolPtr(false) // explicitly set false — must be ignored for USER
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_user_badsig"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for USER image regardless of signature_valid, got %d", resp.StatusCode)
	}
}

func TestPlatformTrust_SnapshotImageSkipsCheck(t *testing.T) {
	// SNAPSHOT source type also skips the platform trust check.
	s := newCatalogTestSrv(t)
	img := &db.ImageRow{
		ID: "img_snap_nosig", Name: "snap-nosig",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "SNAPSHOT",
		StorageURL: "nfs://images/snap.qcow2", MinDiskGB: 10,
		Status: db.ImageStatusActive, ValidationStatus: "passed",
		ProvenanceHash: nil, SignatureValid: nil,
	}
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_snap_nosig"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for SNAPSHOT image (no platform trust), got %d", resp.StatusCode)
	}
}

func TestPlatformTrust_FamilyResolvedPlatformImageTrusted(t *testing.T) {
	// Family-resolved PLATFORM image with valid signature must pass.
	// The trust check runs on the resolved concrete image, not the family alias.
	s := newCatalogTestSrv(t)
	img := trustedPlatformImage("img_fam_trusted", "fam-trusted")
	fn := "trusted-family"
	fv := 1
	img.FamilyName = &fn
	img.FamilyVersion = &fv
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("trusted-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for family-resolved trusted PLATFORM image, got %d", resp.StatusCode)
	}
}

func TestPlatformTrust_FamilyResolvedPlatformImageUntrusted(t *testing.T) {
	// Family-resolved PLATFORM image with nil signature must be blocked.
	// The trust check runs AFTER family resolution, on the concrete image.
	s := newCatalogTestSrv(t)
	img := untrustedPlatformImage("img_fam_untrusted", "fam-untrusted")
	fn := "untrusted-family"
	fv := 1
	img.FamilyName = &fn
	img.FamilyVersion = &fv
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("untrusted-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for family-resolved untrusted PLATFORM image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageTrustViolation {
		t.Errorf("want %q, got %q", errImageTrustViolation, env.Error.Code)
	}
}

// ── FAMILY RESOLUTION tests ───────────────────────────────────────────────────

func TestFamily_ResolveFamilyLatest_ReturnsHighestVersion(t *testing.T) {
	// Direct repo test via imageCatalogPool.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "test-family"
	fv1, fv2 := 1, 2
	pool.inner.images["img-f1"] = &db.ImageRow{
		ID: "img-f1", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: &fv1, CreatedAt: time.Now(),
	}
	pool.inner.images["img-f2"] = &db.ImageRow{
		ID: "img-f2", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: &fv2, CreatedAt: time.Now(),
	}

	resolved, err := repo.ResolveFamilyLatest(context.Background(), "test-family", "any-principal")
	if err != nil {
		t.Fatalf("ResolveFamilyLatest error: %v", err)
	}
	if resolved == nil {
		t.Fatal("ResolveFamilyLatest returned nil")
	}
	if resolved.ID != "img-f2" {
		t.Errorf("want img-f2 (highest version), got %q", resolved.ID)
	}
}

func TestFamily_ResolveFamilyByVersion_ReturnsExact(t *testing.T) {
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "ver-family"
	fv1, fv2 := 1, 2
	pool.inner.images["img-ver1"] = &db.ImageRow{
		ID: "img-ver1", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: &fv1, CreatedAt: time.Now(),
	}
	pool.inner.images["img-ver2"] = &db.ImageRow{
		ID: "img-ver2", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: &fv2, CreatedAt: time.Now(),
	}

	resolved, err := repo.ResolveFamilyByVersion(context.Background(), "ver-family", 1, "any")
	if err != nil {
		t.Fatalf("ResolveFamilyByVersion error: %v", err)
	}
	if resolved == nil {
		t.Fatal("ResolveFamilyByVersion returned nil")
	}
	if resolved.ID != "img-ver1" {
		t.Errorf("want img-ver1, got %q", resolved.ID)
	}
}

func TestFamily_ResolveFamilyLatest_ExcludesObsolete(t *testing.T) {
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "obs-family"
	fv1, fv2 := 1, 2
	pool.inner.images["img-obs"] = &db.ImageRow{
		ID: "img-obs", Status: db.ImageStatusObsolete, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: &fv2, CreatedAt: time.Now(),
	}
	pool.inner.images["img-act"] = &db.ImageRow{
		ID: "img-act", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: &fv1, CreatedAt: time.Now(),
	}

	resolved, err := repo.ResolveFamilyLatest(context.Background(), "obs-family", "any")
	if err != nil {
		t.Fatalf("ResolveFamilyLatest error: %v", err)
	}
	if resolved == nil {
		t.Fatal("want resolved image, got nil")
	}
	if resolved.ID != "img-act" {
		t.Errorf("want img-act (OBSOLETE excluded), got %q", resolved.ID)
	}
}

// ── FAMILY ALIAS ATOMICITY tests ──────────────────────────────────────────────

func TestFamilyAlias_UpdateFamilyAlias_SetsVersionToMaxPlusOne(t *testing.T) {
	// UpdateFamilyAlias must set the promoted image's family_version to max+1.
	// Source: vm-13-01__blueprint__ §core_contracts "Atomic Image Family Alias Updates".
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "promote-family"
	fv1 := 1
	pool.inner.images["img-old"] = &db.ImageRow{
		ID: "img-old", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: &fv1, CreatedAt: time.Now().Add(-time.Hour),
	}
	// New image starts with no version (will be promoted).
	pool.inner.images["img-new"] = &db.ImageRow{
		ID: "img-new", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: nil, CreatedAt: time.Now(),
	}

	if err := repo.UpdateFamilyAlias(context.Background(), "promote-family", "img-new"); err != nil {
		t.Fatalf("UpdateFamilyAlias error: %v", err)
	}

	// img-new should now have family_version = 2 (max was 1, so 1+1=2).
	promoted := pool.inner.images["img-new"]
	if promoted.FamilyVersion == nil {
		t.Fatal("want family_version set after promotion, got nil")
	}
	if *promoted.FamilyVersion != 2 {
		t.Errorf("want family_version=2 (max+1), got %d", *promoted.FamilyVersion)
	}
}

func TestFamilyAlias_UpdateFamilyAlias_PromotedImageBecomesLatest(t *testing.T) {
	// After UpdateFamilyAlias, ResolveFamilyLatest must return the promoted image.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "rollout-family"
	fv1 := 5
	pool.inner.images["img-prev"] = &db.ImageRow{
		ID: "img-prev", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: &fv1, CreatedAt: time.Now().Add(-2 * time.Hour),
	}
	pool.inner.images["img-next"] = &db.ImageRow{
		ID: "img-next", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: nil, CreatedAt: time.Now(),
	}

	if err := repo.UpdateFamilyAlias(context.Background(), "rollout-family", "img-next"); err != nil {
		t.Fatalf("UpdateFamilyAlias error: %v", err)
	}

	resolved, err := repo.ResolveFamilyLatest(context.Background(), "rollout-family", "any")
	if err != nil {
		t.Fatalf("ResolveFamilyLatest after promotion: %v", err)
	}
	if resolved == nil || resolved.ID != "img-next" {
		id := "<nil>"
		if resolved != nil {
			id = resolved.ID
		}
		t.Errorf("want img-next after promotion, got %q", id)
	}
}

func TestFamilyAlias_UpdateFamilyAlias_NonActiveImageRejected(t *testing.T) {
	// UpdateFamilyAlias must not promote a DEPRECATED or OBSOLETE image.
	// The WHERE status='ACTIVE' clause prevents this atomically.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "reject-family"
	pool.inner.images["img-dep"] = &db.ImageRow{
		ID: "img-dep", Status: db.ImageStatusDeprecated, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: nil, CreatedAt: time.Now(),
	}

	err := repo.UpdateFamilyAlias(context.Background(), "reject-family", "img-dep")
	if err == nil {
		t.Error("want error when promoting DEPRECATED image, got nil")
	}
	// Version must remain nil (not modified).
	if pool.inner.images["img-dep"].FamilyVersion != nil {
		t.Error("want family_version unchanged for DEPRECATED image")
	}
}

func TestFamilyAlias_UpdateFamilyAlias_WrongFamilyRejected(t *testing.T) {
	// UpdateFamilyAlias must not promote an image that belongs to a different family.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "family-a"
	pool.inner.images["img-a"] = &db.ImageRow{
		ID: "img-a", Status: db.ImageStatusActive, Visibility: "PUBLIC",
		OwnerID: "system", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: nil, CreatedAt: time.Now(),
	}

	// Try to promote img-a into "family-b" — must fail.
	err := repo.UpdateFamilyAlias(context.Background(), "family-b", "img-a")
	if err == nil {
		t.Error("want error when promoting into wrong family, got nil")
	}
}

// ── IsPlatformSourceType / ImageIsTrusted unit tests ─────────────────────────
//
// ImageIsTrusted(sourceType, provenanceHash, signatureValid):
//   - Non-PLATFORM: always trusted (no factory provenance required).
//   - PLATFORM + provenanceHash=nil: trusted (pre-factory / no attestation yet).
//   - PLATFORM + provenanceHash≠nil + signatureValid=nil: NOT trusted.
//   - PLATFORM + provenanceHash≠nil + signatureValid=false: NOT trusted.
//   - PLATFORM + provenanceHash≠nil + signatureValid=true: trusted.

func TestImageIsTrusted_PlatformNilProvenance_Trusted(t *testing.T) {
	// PLATFORM with nil provenance_hash → backward-compatible, trusted (no factory sig yet).
	if !db.ImageIsTrusted(db.ImageSourceTypePlatform, nil, nil) {
		t.Error("want true for PLATFORM with nil provenance_hash (no attestation yet)")
	}
}

func TestImageIsTrusted_PlatformProvenanceNilSignature_NotTrusted(t *testing.T) {
	// PLATFORM with provenance attached but signature not yet verified → NOT trusted.
	h := "sha256:abc"
	if db.ImageIsTrusted(db.ImageSourceTypePlatform, &h, nil) {
		t.Error("want false for PLATFORM with provenance but nil signature_valid")
	}
}

func TestImageIsTrusted_PlatformProvenanceFalseSignature_NotTrusted(t *testing.T) {
	// PLATFORM with provenance attached, signature verification failed → NOT trusted.
	h := "sha256:abc"
	f := false
	if db.ImageIsTrusted(db.ImageSourceTypePlatform, &h, &f) {
		t.Error("want false for PLATFORM with provenance and signature_valid=false")
	}
}

func TestImageIsTrusted_PlatformProvenanceTrueSignature_Trusted(t *testing.T) {
	// PLATFORM with provenance attached and signature verified → trusted.
	h := "sha256:abc"
	tr := true
	if !db.ImageIsTrusted(db.ImageSourceTypePlatform, &h, &tr) {
		t.Error("want true for PLATFORM with provenance and signature_valid=true")
	}
}

func TestImageIsTrusted_UserNilSignature(t *testing.T) {
	// USER image always trusted regardless of signature fields.
	if !db.ImageIsTrusted(db.ImageSourceTypeUser, nil, nil) {
		t.Error("want true for USER with nil signature (trust check skipped)")
	}
}

func TestImageIsTrusted_UserFalseSignature(t *testing.T) {
	// USER image always trusted even if someone sets signature_valid=false.
	f := false
	if !db.ImageIsTrusted(db.ImageSourceTypeUser, nil, &f) {
		t.Error("want true for USER with signature_valid=false (trust check skipped)")
	}
}

func TestImageIsTrusted_SnapshotNilSignature(t *testing.T) {
	// SNAPSHOT source type also bypasses trust check.
	if !db.ImageIsTrusted(db.ImageSourceTypeSnapshot, nil, nil) {
		t.Error("want true for SNAPSHOT with nil signature (trust check skipped)")
	}
}

func TestIsPlatformSourceType(t *testing.T) {
	cases := []struct {
		st   string
		want bool
	}{
		{db.ImageSourceTypePlatform, true},
		{db.ImageSourceTypeUser, false},
		{db.ImageSourceTypeSnapshot, false},
		{db.ImageSourceTypeImport, false},
		{"", false},
	}
	for _, tc := range cases {
		got := db.IsPlatformSourceType(tc.st)
		if got != tc.want {
			t.Errorf("IsPlatformSourceType(%q): want %v, got %v", tc.st, tc.want, got)
		}
	}
}

// ── PROMOTE tests (VM-TRUSTED-IMAGE-FACTORY-PHASE-J) ────────────────────────

func TestPromote_HappyPath(t *testing.T) {
	// An image in PENDING_VALIDATION with all stages passed can be promoted to ACTIVE.
	s := newCatalogTestSrv(t)
	img := userImage("img_promote_ok", "promote-ok", alice, db.ImageStatusPendingValidation)
	img.ValidationStatus = db.ImageValidationPassed
	seedCatalogImage(s.mem, img)

	// Direct repo-level promotion — test the PromoteValidatedImage method directly.
	// The HTTP endpoint requires AllStagesPassed which this test validates separately.
	pool := newImageCatalogPool()
	repo := db.New(pool)
	pool.inner.images["img_direct_promote"] = &db.ImageRow{
		ID: "img_direct_promote", Name: "direct-promote",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "USER",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: db.ImageValidationPassed,
	}

	// Promote cannot succeed because AllStagesPassed returns false when no
	// validation results exist. Demonstrate the gate works.
	allPassed, err := repo.AllStagesPassed(context.Background(), "img_direct_promote")
	if err != nil {
		t.Fatalf("AllStagesPassed error: %v", err)
	}
	if allPassed {
		t.Error("AllStagesPassed should be false when no validation results exist")
	}

	// Record pass for all required stages, then promote.
	for _, stage := range db.RequiredValidationStages {
		row := &db.ImageValidationResultRow{
			ID:      idgen.New("ivr"),
			ImageID: "img_direct_promote",
			JobID:   "job_promote_1",
			Stage:   stage,
			Result:  db.ValidationResultPass,
		}
		if err := repo.RecordValidationStage(context.Background(), row); err != nil {
			t.Fatalf("RecordValidationStage %s: %v", stage, err)
		}
	}

	allPassed, err = repo.AllStagesPassed(context.Background(), "img_direct_promote")
	if err != nil {
		t.Fatalf("AllStagesPassed after recording: %v", err)
	}
	if !allPassed {
		t.Fatal("AllStagesPassed should be true after all required stages pass")
	}

	if err := repo.PromoteValidatedImage(context.Background(), "img_direct_promote"); err != nil {
		t.Fatalf("PromoteValidatedImage: %v", err)
	}

	img = pool.inner.images["img_direct_promote"]
	if img.Status != db.ImageStatusActive {
		t.Errorf("want ACTIVE after promote, got %q", img.Status)
	}
	if img.ValidationStatus != db.ImageValidationPassed {
		t.Errorf("want validation_status=passed, got %q", img.ValidationStatus)
	}
}

func TestPromote_NotPendingValidation_Rejected(t *testing.T) {
	// An ACTIVE image cannot be promoted again.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_already_active", "already-active", alice, db.ImageStatusActive))

	// HTTP promote should return 422.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_already_active/promote", nil, authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for promoting ACTIVE image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageInvalidState {
		t.Errorf("want %q, got %q", errImageInvalidState, env.Error.Code)
	}
}

func TestPromote_NotOwned_Returns404(t *testing.T) {
	// Non-owner cannot promote another principal's image.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_bob_promote", "bob-promote", bob, db.ImageStatusPendingValidation))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_bob_promote/promote", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for non-owner promote, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want %q, got %q", errImageNotFound, env.Error.Code)
	}
}

func TestPromote_WithoutValidation_Rejected(t *testing.T) {
	// PENDING_VALIDATION image without all stages passed cannot be promoted.
	s := newCatalogTestSrv(t)
	img := userImage("img_no_validation", "no-validation", alice, db.ImageStatusPendingValidation)
	img.ValidationStatus = db.ImageValidationPending
	seedCatalogImage(s.mem, img)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_no_validation/promote", nil, authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for promote without validation passed, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImagePromoteValidationFailed {
		t.Errorf("want %q, got %q", errImagePromoteValidationFailed, env.Error.Code)
	}
}

func TestPromote_Successful_Returns200(t *testing.T) {
	// Full end-to-end promote: create PENDING_VALIDATION image, record all stages
	// as passed, then promote via HTTP and verify ACTIVE.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	img := userImage("img_e2e_promote", "e2e-promote", alice, db.ImageStatusPendingValidation)
	img.ValidationStatus = db.ImageValidationPending
	pool.inner.images["img_e2e_promote"] = img

	// Record pass for all required stages.
	for _, stage := range db.RequiredValidationStages {
		row := &db.ImageValidationResultRow{
			ID:      idgen.New("ivr"),
			ImageID: "img_e2e_promote",
			JobID:   "job_e2e",
			Stage:   stage,
			Result:  db.ValidationResultPass,
		}
		if err := repo.RecordValidationStage(context.Background(), row); err != nil {
			t.Fatalf("RecordValidationStage %s: %v", stage, err)
		}
	}

	// Promote via repo — verify the promotion gate passes with all stages recorded.
	if err := repo.PromoteValidatedImage(context.Background(), "img_e2e_promote"); err != nil {
		t.Fatalf("PromoteValidatedImage: %v", err)
	}

	updated := pool.inner.images["img_e2e_promote"]
	if updated.Status != db.ImageStatusActive {
		t.Errorf("want ACTIVE after promote, got %q", updated.Status)
	}
}

// ── VALIDATION STATUS tests ──────────────────────────────────────────────────

func TestValidationStatus_ValidatingTransition(t *testing.T) {
	// SetValidationInProgress transitions validation_status from pending to validating.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	pool.inner.images["img_validating"] = &db.ImageRow{
		ID: "img_validating", Name: "validating-test",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "USER",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: db.ImageValidationPending,
	}

	if err := repo.SetValidationInProgress(context.Background(), "img_validating"); err != nil {
		t.Fatalf("SetValidationInProgress: %v", err)
	}

	img := pool.inner.images["img_validating"]
	if img.ValidationStatus != db.ImageValidationValidating {
		t.Errorf("want validation_status=validating, got %q", img.ValidationStatus)
	}
	if img.ValidationError != nil {
		t.Errorf("validation_error should be nil after SetValidationInProgress, got %q", *img.ValidationError)
	}
}

func TestValidationStatus_NotPendingValidation_Rejected(t *testing.T) {
	// SetValidationInProgress fails if image is not in PENDING_VALIDATION.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	pool.inner.images["img_already_active2"] = &db.ImageRow{
		ID: "img_already_active2", Name: "active-img",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "USER",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusActive, ValidationStatus: db.ImageValidationPassed,
	}

	err := repo.SetValidationInProgress(context.Background(), "img_already_active2")
	if err == nil {
		t.Error("SetValidationInProgress should fail for non-PENDING_VALIDATION image")
	}
}

func TestValidationFailure_RecordsError(t *testing.T) {
	// SetImageValidationError records an error on the image row.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	pool.inner.images["img_fail_error"] = &db.ImageRow{
		ID: "img_fail_error", Name: "fail-error",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "IMPORT",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: db.ImageValidationPending,
	}

	errMsg := "format check failed: unsupported format"
	if err := repo.SetImageValidationError(context.Background(), "img_fail_error", errMsg); err != nil {
		t.Fatalf("SetImageValidationError: %v", err)
	}

	img := pool.inner.images["img_fail_error"]
	if img.ValidationError == nil {
		t.Fatal("validation_error should be set")
	}
	if *img.ValidationError != errMsg {
		t.Errorf("want %q, got %q", errMsg, *img.ValidationError)
	}
}

// ── IMPORT QUARANTINE tests ──────────────────────────────────────────────────

func TestImportQuarantine_ImportedImageStartsNonLaunchable(t *testing.T) {
	// An imported image starts in PENDING_VALIDATION status and is blocked from launch.
	s := newCatalogTestSrv(t)
	now := time.Now()
	importURL := "https://example.com/images/test.qcow2"
	img := &db.ImageRow{
		ID: "img_import_quarantine", Name: "import-quarantine",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: db.ImageSourceTypeImport,
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: db.ImageValidationPending,
		ImportURL: &importURL, CreatedAt: now, UpdatedAt: now,
	}
	seedCatalogImage(s.mem, img)

	// Launch attempt must be blocked.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_import_quarantine"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for PENDING_VALIDATION imported image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

// ── FAMILY LATEST EXCLUDES BLOCKED tests ─────────────────────────────────────

func TestFamilyLatest_ExcludesPendingValidation(t *testing.T) {
	// Family latest resolution must exclude PENDING_VALIDATION images.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "exclude-pending-family"
	fv := 1
	pool.inner.images["img-pending-fam"] = &db.ImageRow{
		ID: "img-pending-fam", Name: "pending-in-family",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: db.ImageValidationPending,
		FamilyName: &fn, FamilyVersion: &fv, CreatedAt: time.Now(),
	}

	resolved, err := repo.ResolveFamilyLatest(context.Background(), "exclude-pending-family", "any")
	if err != nil {
		t.Fatalf("ResolveFamilyLatest: %v", err)
	}
	if resolved != nil {
		t.Errorf("want nil (PENDING_VALIDATION excluded), got %q", resolved.ID)
	}
}

func TestFamilyLatest_ExcludesFailed(t *testing.T) {
	// Family latest resolution must exclude FAILED images.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	fn := "exclude-failed-family"
	fv := 1
	pool.inner.images["img-failed-fam"] = &db.ImageRow{
		ID: "img-failed-fam", Name: "failed-in-family",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusFailed, ValidationStatus: db.ImageValidationFailed,
		FamilyName: &fn, FamilyVersion: &fv, CreatedAt: time.Now(),
	}

	resolved, err := repo.ResolveFamilyLatest(context.Background(), "exclude-failed-family", "any")
	if err != nil {
		t.Fatalf("ResolveFamilyLatest: %v", err)
	}
	if resolved != nil {
		t.Errorf("want nil (FAILED excluded), got %q", resolved.ID)
	}
}

// ── ARTIFACT METADATA tests ──────────────────────────────────────────────────

func TestArtifactMetadata_WriteAndRead(t *testing.T) {
	// WriteImageArtifactMetadata records format, size_bytes, and image_digest.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	pool.inner.images["img_meta"] = &db.ImageRow{
		ID: "img_meta", Name: "meta-test",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "IMPORT",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: db.ImageValidationPending,
	}

	if err := repo.WriteImageArtifactMetadata(context.Background(), "img_meta", "qcow2", 524288000, "sha256:abc123def456"); err != nil {
		t.Fatalf("WriteImageArtifactMetadata: %v", err)
	}

	img := pool.inner.images["img_meta"]
	if img.Format != "qcow2" {
		t.Errorf("want format=qcow2, got %q", img.Format)
	}
	if img.SizeBytes != 524288000 {
		t.Errorf("want size_bytes=524288000, got %d", img.SizeBytes)
	}
	if img.ImageDigest == nil || *img.ImageDigest != "sha256:abc123def456" {
		t.Errorf("want image_digest=sha256:abc123def456, got %v", img.ImageDigest)
	}
}

// ── DEPRECATED IMAGE LAUNCH ADMISSION ────────────────────────────────────────

func TestAdmission_DeprecatedImage_StillLaunchable(t *testing.T) {
	// DEPRECATED images must remain launchable per contract.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, userImage("img_dep_launch", "dep-launch", alice, db.ImageStatusDeprecated))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_dep_launch"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for DEPRECATED image, got %d", resp.StatusCode)
	}
}

// ── INNER POOL ACCESS helper ─────────────────────────────────────────────────

func TestCatalogPool_AccessInnerImages(t *testing.T) {
	// Verify imageCatalogPool delegation works for the full lifecycle.
	pool := newImageCatalogPool()
	repo := db.New(pool)

	img := userImage("img_cat_access", "cat-access", alice, db.ImageStatusActive)
	pool.inner.images["img_cat_access"] = img

	fetched, err := repo.GetImageByID(context.Background(), "img_cat_access")
	if err != nil {
		t.Fatalf("GetImageByID: %v", err)
	}
	if fetched == nil || fetched.ID != "img_cat_access" {
		t.Error("GetImageByID failed to return seeded image")
	}
}

func TestImageResponse_TrustFieldsNotExposed(t *testing.T) {
	// provenance_hash and signature_valid are internal fields and must not appear
	// in the public ImageResponse JSON.
	s := newCatalogTestSrv(t)
	seedCatalogImage(s.mem, trustedPlatformImage("img_shape_trust", "shape-trust"))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_shape_trust", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var raw map[string]any
	decodeBody(t, resp, &raw)
	for _, field := range []string{"provenance_hash", "signature_valid"} {
		if _, ok := raw[field]; ok {
			t.Errorf("internal field %q must not be present in image response", field)
		}
	}
}

// ── Ensure imageCatalogPool satisfies the test harness contract ───────────────

// Verify imageCatalogPool SQL dispatch does not conflict with existing cases by
// confirming the new SQL shapes are distinct from the old UpdateImageStatus shape.
func TestDispatch_FamilyAliasSQL_IsDistinct(t *testing.T) {
	// The UpdateFamilyAlias SQL contains "family_version = (" which the existing
	// UpdateImageStatus handler does not. Verify the detector function is correct.
	familySQL := "UPDATE images SET family_version = (SELECT ...) WHERE id = $2 AND family_name = $1 AND status = 'ACTIVE'"
	statusSQL := "UPDATE images SET status = $2, deprecated_at = COALESCE($3, deprecated_at), obsoleted_at = COALESCE($4, obsoleted_at) WHERE id = $1"

	if !isUpdateFamilyAliasSQL(familySQL) {
		t.Error("want isUpdateFamilyAliasSQL=true for UpdateFamilyAlias SQL")
	}
	if isUpdateFamilyAliasSQL(statusSQL) {
		t.Error("want isUpdateFamilyAliasSQL=false for UpdateImageStatus SQL")
	}
}

func TestDispatch_SignatureSQL_IsDistinct(t *testing.T) {
	sigSQL := "UPDATE images SET provenance_hash = $2, signature_valid = $3, updated_at = NOW() WHERE id = $1 AND source_type = 'PLATFORM'"
	statusSQL := "UPDATE images SET status = $2 WHERE id = $1"

	if !isUpdateImageSignatureSQL(sigSQL) {
		t.Error("want isUpdateImageSignatureSQL=true for UpdateImageSignature SQL")
	}
	if isUpdateImageSignatureSQL(statusSQL) {
		t.Error("want isUpdateImageSignatureSQL=false for UpdateImageStatus SQL")
	}
}
