package main

// image_validation_test.go — VM-P3C Job 1: validation/trust write-path and
// trust-state integration tests.
//
// Coverage:
//
//   BUILD MANIFEST WRITES:
//     - UpsertBuildManifest persists and is retrievable via GetBuildManifest
//     - UpsertBuildManifest is idempotent (second call updates fields)
//     - SetManifestSignature records provenance_json, signature, signed_at
//     - IsBuildManifestSigned returns false before signing, true after
//
//   VALIDATION RESULT WRITES:
//     - RecordValidationStage stores result; ListValidationResults returns it
//     - Multiple stages stored independently
//     - Re-recording a stage appends (does not overwrite prior rows)
//
//   ALL-STAGES-PASSED GATE:
//     - All four required stages pass → AllStagesPassed = true
//     - Missing one stage → AllStagesPassed = false
//     - Any required stage fails → AllStagesPassed = false
//     - Most-recent result wins per stage (re-run pass overrides prior fail)
//
//   IMAGE LIFECYCLE INTEGRATION:
//     - PromoteValidatedImage: PENDING_VALIDATION → ACTIVE
//     - PromoteValidatedImage on already-ACTIVE image: error (0 rows affected)
//     - FailValidatedImage: PENDING_VALIDATION → FAILED
//     - After PromoteValidatedImage, launch admission passes (202)
//     - After FailValidatedImage, launch admission blocked (422 image_not_launchable)
//
//   PLATFORM TRUST INTEGRATION:
//     - Promoted image with signature_valid=true passes trust gate (202)
//     - PLATFORM image without provenance_hash passes (backward-compat seam)
//     - UpdateImageSignature records trust fields on PLATFORM image
//
//   SHARED-IMAGE LAUNCH (non-regression):
//     - Grantee launch of a promoted PRIVATE user image passes (202)
//
// Test strategy: direct db.Repo calls via validationPool, plus HTTP round-trips
// via validationTestSrv for admission tests.
//
// Source: vm-13-01__blueprint__ §Image Validation Service,
//         vm-13-01__blueprint__ §core_contracts "Platform Trust Boundary",
//         P2_IMAGE_SNAPSHOT_MODEL.md §3.4,
//         11-02-phase-1-test-strategy.md §unit test approach.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── newValidationTestSrv ──────────────────────────────────────────────────────

// newValidationTestSrv creates a full HTTP test server backed by validationPool.
// Used for admission integration tests that need the complete handler stack.
func newValidationTestSrv(t *testing.T) *validationTestSrv {
	t.Helper()
	pool := newValidationPool()
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
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Seed the default platform image (trusted, no provenance_hash — backward compat)
	// so standard instance tests pass without trust-check interference.
	now := time.Now()
	pool.inner.inner.images["00000000-0000-0000-0000-000000000010"] = &db.ImageRow{
		ID: "00000000-0000-0000-0000-000000000010", Name: "ubuntu-22.04-lts",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/ubuntu-22.04.qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed",
		// No ProvenanceHash → backward-compat seam → trusted without signature check.
		CreatedAt: now, UpdatedAt: now,
	}

	return &validationTestSrv{ts: ts, mem: pool}
}

// seedValidationImage adds an image to the inner memPool.
func seedValidationImage(pool *validationPool, img *db.ImageRow) {
	if img.CreatedAt.IsZero() {
		img.CreatedAt = time.Now()
	}
	if img.UpdatedAt.IsZero() {
		img.UpdatedAt = time.Now()
	}
	pool.inner.inner.images[img.ID] = img
}

// pendingImage returns a PRIVATE USER image in PENDING_VALIDATION status.
func pendingImage(id, ownerID string) *db.ImageRow {
	return &db.ImageRow{
		ID: id, Name: "pending-" + id,
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: ownerID, Visibility: "PRIVATE", SourceType: "USER",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: "pending",
	}
}

// pendingPlatformImage returns a PUBLIC PLATFORM image in PENDING_VALIDATION status.
func pendingPlatformImage(id string) *db.ImageRow {
	return &db.ImageRow{
		ID: id, Name: "platform-pending-" + id,
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "", MinDiskGB: 10,
		Status: db.ImageStatusPendingValidation, ValidationStatus: "pending",
	}
}

// ── BUILD MANIFEST tests ──────────────────────────────────────────────────────

func TestUpsertBuildManifest_PersistsAndRetrieves(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	err := repo.UpsertBuildManifest(ctx, &db.ImageBuildManifestRow{
		ImageID:         "img_manifest_01",
		BuildConfigRef:  "configs/ubuntu-22.04.yaml@sha256:abc",
		BaseImageDigest: "sha256:base123",
		ImageDigest:     "sha256:img456",
	})
	if err != nil {
		t.Fatalf("UpsertBuildManifest: %v", err)
	}

	got, err := repo.GetBuildManifest(ctx, "img_manifest_01")
	if err != nil {
		t.Fatalf("GetBuildManifest: %v", err)
	}
	if got == nil {
		t.Fatal("want manifest, got nil")
	}
	if got.ImageID != "img_manifest_01" {
		t.Errorf("want image_id=%q, got %q", "img_manifest_01", got.ImageID)
	}
	if got.ImageDigest != "sha256:img456" {
		t.Errorf("want image_digest=%q, got %q", "sha256:img456", got.ImageDigest)
	}
	if got.Signature != nil {
		t.Error("want signature=nil before signing, got non-nil")
	}
}

func TestUpsertBuildManifest_IsIdempotent(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	first := &db.ImageBuildManifestRow{
		ImageID: "img_idem_01", BuildConfigRef: "cfg-v1",
		BaseImageDigest: "sha256:base-v1", ImageDigest: "sha256:img-v1",
	}
	if err := repo.UpsertBuildManifest(ctx, first); err != nil {
		t.Fatalf("first UpsertBuildManifest: %v", err)
	}

	// Second upsert with updated digest — must overwrite.
	second := &db.ImageBuildManifestRow{
		ImageID: "img_idem_01", BuildConfigRef: "cfg-v2",
		BaseImageDigest: "sha256:base-v2", ImageDigest: "sha256:img-v2",
	}
	if err := repo.UpsertBuildManifest(ctx, second); err != nil {
		t.Fatalf("second UpsertBuildManifest: %v", err)
	}

	got, err := repo.GetBuildManifest(ctx, "img_idem_01")
	if err != nil {
		t.Fatalf("GetBuildManifest: %v", err)
	}
	if got.BuildConfigRef != "cfg-v2" {
		t.Errorf("want build_config_ref=%q after update, got %q", "cfg-v2", got.BuildConfigRef)
	}
	if got.ImageDigest != "sha256:img-v2" {
		t.Errorf("want image_digest=%q after update, got %q", "sha256:img-v2", got.ImageDigest)
	}
}

func TestGetBuildManifest_NilWhenAbsent(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	got, err := repo.GetBuildManifest(ctx, "img_nonexistent")
	if err != nil {
		t.Fatalf("GetBuildManifest for missing image: %v", err)
	}
	if got != nil {
		t.Error("want nil for missing manifest, got non-nil")
	}
}

func TestSetManifestSignature_RecordsProvenance(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	// Create manifest first.
	if err := repo.UpsertBuildManifest(ctx, &db.ImageBuildManifestRow{
		ImageID: "img_sign_01", BuildConfigRef: "cfg",
		BaseImageDigest: "sha256:base", ImageDigest: "sha256:img",
	}); err != nil {
		t.Fatalf("UpsertBuildManifest: %v", err)
	}

	signedAt := time.Now()
	if err := repo.SetManifestSignature(ctx, "img_sign_01",
		`{"_type":"https://in-toto.io/Statement/v0.1"}`,
		"sig:abc123",
		signedAt,
	); err != nil {
		t.Fatalf("SetManifestSignature: %v", err)
	}

	got, err := repo.GetBuildManifest(ctx, "img_sign_01")
	if err != nil {
		t.Fatalf("GetBuildManifest after signing: %v", err)
	}
	if got.Signature == nil || *got.Signature != "sig:abc123" {
		t.Errorf("want signature=%q, got %v", "sig:abc123", got.Signature)
	}
	if got.ProvenanceJSON == nil {
		t.Error("want provenance_json set after signing, got nil")
	}
	if got.SignedAt == nil {
		t.Error("want signed_at set after signing, got nil")
	}
}

func TestIsBuildManifestSigned_FalseBeforeSigning(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.UpsertBuildManifest(ctx, &db.ImageBuildManifestRow{
		ImageID: "img_gate_01", BuildConfigRef: "cfg",
		BaseImageDigest: "sha256:base", ImageDigest: "sha256:img",
	}); err != nil {
		t.Fatalf("UpsertBuildManifest: %v", err)
	}

	signed, err := repo.IsBuildManifestSigned(ctx, "img_gate_01")
	if err != nil {
		t.Fatalf("IsBuildManifestSigned: %v", err)
	}
	if signed {
		t.Error("want IsBuildManifestSigned=false before signing")
	}
}

func TestIsBuildManifestSigned_TrueAfterSigning(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.UpsertBuildManifest(ctx, &db.ImageBuildManifestRow{
		ImageID: "img_gate_02", BuildConfigRef: "cfg",
		BaseImageDigest: "sha256:base", ImageDigest: "sha256:img",
	}); err != nil {
		t.Fatalf("UpsertBuildManifest: %v", err)
	}
	if err := repo.SetManifestSignature(ctx, "img_gate_02", `{"provenance":"ok"}`, "sig:xyz", time.Now()); err != nil {
		t.Fatalf("SetManifestSignature: %v", err)
	}

	signed, err := repo.IsBuildManifestSigned(ctx, "img_gate_02")
	if err != nil {
		t.Fatalf("IsBuildManifestSigned: %v", err)
	}
	if !signed {
		t.Error("want IsBuildManifestSigned=true after signing")
	}
}

// ── VALIDATION RESULT WRITE tests ────────────────────────────────────────────

func TestRecordValidationStage_PersistsAndLists(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	row := &db.ImageValidationResultRow{
		ID: "ivr_01", ImageID: "img_vr_01", JobID: "job_01",
		Stage: db.ValidationStageBoot, Result: db.ValidationResultPass,
	}
	if err := repo.RecordValidationStage(ctx, row); err != nil {
		t.Fatalf("RecordValidationStage: %v", err)
	}

	results, err := repo.ListValidationResults(ctx, "img_vr_01")
	if err != nil {
		t.Fatalf("ListValidationResults: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Stage != db.ValidationStageBoot {
		t.Errorf("want stage=%q, got %q", db.ValidationStageBoot, results[0].Stage)
	}
	if results[0].Result != db.ValidationResultPass {
		t.Errorf("want result=%q, got %q", db.ValidationResultPass, results[0].Result)
	}
}

func TestRecordValidationStage_MultipleStagesStoredSeparately(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	stages := []string{
		db.ValidationStageBoot,
		db.ValidationStageHealth,
		db.ValidationStageSecurity,
		db.ValidationStageIntegrity,
	}
	for _, stage := range stages {
		if err := repo.RecordValidationStage(ctx, &db.ImageValidationResultRow{
			ID:      "ivr_multi_" + stage,
			ImageID: "img_multi_01",
			JobID:   "job_multi",
			Stage:   stage,
			Result:  db.ValidationResultPass,
		}); err != nil {
			t.Fatalf("RecordValidationStage stage=%s: %v", stage, err)
		}
	}

	results, err := repo.ListValidationResults(ctx, "img_multi_01")
	if err != nil {
		t.Fatalf("ListValidationResults: %v", err)
	}
	if len(results) != len(stages) {
		t.Errorf("want %d results, got %d", len(stages), len(results))
	}
}

func TestRecordValidationStage_AppendsDuplicateStage(t *testing.T) {
	// Re-recording the same stage must append, not overwrite.
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.RecordValidationStage(ctx, &db.ImageValidationResultRow{
		ID: "ivr_dup_1", ImageID: "img_dup_01", JobID: "job_dup",
		Stage: db.ValidationStageBoot, Result: db.ValidationResultFail,
	}); err != nil {
		t.Fatalf("RecordValidationStage (fail): %v", err)
	}
	if err := repo.RecordValidationStage(ctx, &db.ImageValidationResultRow{
		ID: "ivr_dup_2", ImageID: "img_dup_01", JobID: "job_dup",
		Stage: db.ValidationStageBoot, Result: db.ValidationResultPass,
		RecordedAt: time.Now().Add(time.Millisecond),
	}); err != nil {
		t.Fatalf("RecordValidationStage (pass): %v", err)
	}

	results, err := repo.ListValidationResults(ctx, "img_dup_01")
	if err != nil {
		t.Fatalf("ListValidationResults: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("want 2 rows (append, not overwrite), got %d", len(results))
	}
}

// ── ALL-STAGES-PASSED GATE tests ─────────────────────────────────────────────

func TestAllStagesPassed_AllPassReturnsTrue(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	recordAllStagesPass(t, repo, ctx, "img_allpass_01", "job_allpass")

	passed, err := repo.AllStagesPassed(ctx, "img_allpass_01")
	if err != nil {
		t.Fatalf("AllStagesPassed: %v", err)
	}
	if !passed {
		t.Error("want AllStagesPassed=true when all required stages have passed")
	}
}

func TestAllStagesPassed_MissingStageReturnsFalse(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	// Record only 3 of 4 required stages.
	for _, stage := range []string{db.ValidationStageBoot, db.ValidationStageHealth, db.ValidationStageSecurity} {
		if err := repo.RecordValidationStage(ctx, &db.ImageValidationResultRow{
			ID: "ivr_miss_" + stage, ImageID: "img_missing_01", JobID: "job_miss",
			Stage: stage, Result: db.ValidationResultPass,
		}); err != nil {
			t.Fatalf("RecordValidationStage: %v", err)
		}
	}
	// integrity is missing.

	passed, err := repo.AllStagesPassed(ctx, "img_missing_01")
	if err != nil {
		t.Fatalf("AllStagesPassed: %v", err)
	}
	if passed {
		t.Error("want AllStagesPassed=false when integrity stage is missing")
	}
}

func TestAllStagesPassed_FailedStageReturnsFalse(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	// All stages pass except security.
	for _, stage := range db.RequiredValidationStages {
		result := db.ValidationResultPass
		if stage == db.ValidationStageSecurity {
			result = db.ValidationResultFail
		}
		if err := repo.RecordValidationStage(ctx, &db.ImageValidationResultRow{
			ID: "ivr_fail_" + stage, ImageID: "img_failstage_01", JobID: "job_failstage",
			Stage: stage, Result: result,
		}); err != nil {
			t.Fatalf("RecordValidationStage: %v", err)
		}
	}

	passed, err := repo.AllStagesPassed(ctx, "img_failstage_01")
	if err != nil {
		t.Fatalf("AllStagesPassed: %v", err)
	}
	if passed {
		t.Error("want AllStagesPassed=false when security stage failed")
	}
}

func TestAllStagesPassed_RerunPassOverridesPriorFail(t *testing.T) {
	// Most-recent result per stage wins. A re-run pass must override an earlier fail.
	// Source: image_validation_repo.go AllStagesPassed (DISTINCT ON recorded_at DESC).
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	// Seed a fail result for boot directly into the pool store (recorded earlier).
	earlyTime := time.Now().Add(-time.Second)
	pool.validationResults["img_rerun_01"] = append(
		pool.validationResults["img_rerun_01"],
		&db.ImageValidationResultRow{
			ID: "ivr_rerun_fail", ImageID: "img_rerun_01", JobID: "job_rerun",
			Stage: db.ValidationStageBoot, Result: db.ValidationResultFail,
			RecordedAt: earlyTime,
		},
	)

	// Record a pass for boot via the repo (recorded now — later than the seeded fail).
	if err := repo.RecordValidationStage(ctx, &db.ImageValidationResultRow{
		ID: "ivr_rerun_pass", ImageID: "img_rerun_01", JobID: "job_rerun",
		Stage: db.ValidationStageBoot, Result: db.ValidationResultPass,
	}); err != nil {
		t.Fatalf("record pass: %v", err)
	}

	// Record pass for all other required stages.
	for _, stage := range []string{db.ValidationStageHealth, db.ValidationStageSecurity, db.ValidationStageIntegrity} {
		if err := repo.RecordValidationStage(ctx, &db.ImageValidationResultRow{
			ID: "ivr_rerun_" + stage, ImageID: "img_rerun_01", JobID: "job_rerun",
			Stage: stage, Result: db.ValidationResultPass,
		}); err != nil {
			t.Fatalf("RecordValidationStage %s: %v", stage, err)
		}
	}

	passed, err := repo.AllStagesPassed(ctx, "img_rerun_01")
	if err != nil {
		t.Fatalf("AllStagesPassed: %v", err)
	}
	if !passed {
		t.Error("want AllStagesPassed=true: re-run pass must override prior fail")
	}
}

// ── IMAGE LIFECYCLE INTEGRATION tests ─────────────────────────────────────────

func TestPromoteValidatedImage_PendingToActive(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_promote_01"] = pendingImage("img_promote_01", alice)

	if err := repo.PromoteValidatedImage(ctx, "img_promote_01"); err != nil {
		t.Fatalf("PromoteValidatedImage: %v", err)
	}

	img := pool.inner.inner.images["img_promote_01"]
	if img.Status != db.ImageStatusActive {
		t.Errorf("want status=ACTIVE after promotion, got %q", img.Status)
	}
	if img.ValidationStatus != "passed" {
		t.Errorf("want validation_status=passed, got %q", img.ValidationStatus)
	}
}

func TestPromoteValidatedImage_AlreadyActiveReturnsError(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_promote_dupe"] = &db.ImageRow{
		ID: "img_promote_dupe", Status: db.ImageStatusActive,
		ValidationStatus: "passed",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "USER",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	err := repo.PromoteValidatedImage(ctx, "img_promote_dupe")
	if err == nil {
		t.Error("want error when promoting already-ACTIVE image (0 rows affected)")
	}
}

func TestFailValidatedImage_PendingToFailed(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_fail_01"] = pendingImage("img_fail_01", alice)

	if err := repo.FailValidatedImage(ctx, "img_fail_01"); err != nil {
		t.Fatalf("FailValidatedImage: %v", err)
	}

	img := pool.inner.inner.images["img_fail_01"]
	if img.Status != db.ImageStatusFailed {
		t.Errorf("want status=FAILED after failing, got %q", img.Status)
	}
	if img.ValidationStatus != "failed" {
		t.Errorf("want validation_status=failed, got %q", img.ValidationStatus)
	}
}

func TestFailValidatedImage_AlreadyActiveReturnsError(t *testing.T) {
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_active_fail"] = &db.ImageRow{
		ID: "img_active_fail", Status: db.ImageStatusActive,
		ValidationStatus: "passed",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "USER",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	err := repo.FailValidatedImage(ctx, "img_active_fail")
	if err == nil {
		t.Error("want error when failing already-ACTIVE image (WHERE status=PENDING_VALIDATION)")
	}
}

// ── ADMISSION INTEGRATION: promotion unlocks launch ──────────────────────────

func TestAdmission_PromotedImageIsLaunchable(t *testing.T) {
	// After PromoteValidatedImage, the image must pass the launch admission gate.
	s := newValidationTestSrv(t)
	ctx := context.Background()

	// Seed a PENDING_VALIDATION user image owned by alice.
	seedValidationImage(s.mem, pendingImage("img_promo_launch", alice))

	// Simulate validation worker: promote the image.
	repo := db.New(s.mem)
	if err := repo.PromoteValidatedImage(ctx, "img_promo_launch"); err != nil {
		t.Fatalf("PromoteValidatedImage: %v", err)
	}

	// Launch must now pass.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_promo_launch"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 after promotion, got %d", resp.StatusCode)
	}
}

func TestAdmission_FailedImageIsNotLaunchable(t *testing.T) {
	// After FailValidatedImage, the image must be blocked at launch.
	s := newValidationTestSrv(t)
	ctx := context.Background()

	seedValidationImage(s.mem, pendingImage("img_fail_launch", alice))

	repo := db.New(s.mem)
	if err := repo.FailValidatedImage(ctx, "img_fail_launch"); err != nil {
		t.Fatalf("FailValidatedImage: %v", err)
	}

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_fail_launch"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for FAILED image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want code %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

func TestAdmission_PendingValidationBlockedBeforePromotion(t *testing.T) {
	// PENDING_VALIDATION image must be blocked before any promotion step.
	s := newValidationTestSrv(t)

	seedValidationImage(s.mem, pendingImage("img_pv_block", alice))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_pv_block"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for PENDING_VALIDATION image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want code %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

// ── PLATFORM TRUST INTEGRATION tests ─────────────────────────────────────────

func TestTrust_UpdateImageSignature_RecordsTrustFields(t *testing.T) {
	// UpdateImageSignature must write provenance_hash and signature_valid.
	pool := newValidationPool()
	repo := db.New(pool)
	ctx := context.Background()

	// Seed a PLATFORM image (using PENDING_VALIDATION; signature may be applied
	// before or after status transition in the real pipeline).
	platImg := pendingPlatformImage("img_sig_write")
	pool.inner.inner.images["img_sig_write"] = platImg

	if err := repo.UpdateImageSignature(ctx, "img_sig_write", "sha256:provenance", true); err != nil {
		t.Fatalf("UpdateImageSignature: %v", err)
	}

	img := pool.inner.inner.images["img_sig_write"]
	if img.ProvenanceHash == nil || *img.ProvenanceHash != "sha256:provenance" {
		t.Errorf("want provenance_hash=%q, got %v", "sha256:provenance", img.ProvenanceHash)
	}
	if img.SignatureValid == nil || !*img.SignatureValid {
		t.Error("want signature_valid=true after UpdateImageSignature")
	}
}

func TestTrust_PromotedPlatformImageWithValidSignaturePasses(t *testing.T) {
	// A PLATFORM image promoted to ACTIVE with signature_valid=true must
	// pass both the lifecycle gate and the trust gate.
	s := newValidationTestSrv(t)
	ctx := context.Background()

	// Seed PLATFORM image in PENDING_VALIDATION.
	platImg := pendingPlatformImage("img_plat_promote")
	seedValidationImage(s.mem, platImg)

	repo := db.New(s.mem)

	// Simulate signing worker: record provenance + valid signature.
	if err := repo.UpdateImageSignature(ctx, "img_plat_promote", "sha256:prov-ok", true); err != nil {
		t.Fatalf("UpdateImageSignature: %v", err)
	}

	// Simulate validation worker: promote image.
	if err := repo.PromoteValidatedImage(ctx, "img_plat_promote"); err != nil {
		t.Fatalf("PromoteValidatedImage: %v", err)
	}

	// Launch must pass both gates.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_plat_promote"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for promoted+trusted PLATFORM image, got %d", resp.StatusCode)
	}
}

func TestTrust_PromotedPlatformImageWithFailedSignatureBlocked(t *testing.T) {
	// A PLATFORM image promoted to ACTIVE with signature_valid=false must be
	// blocked by the trust gate even after status promotion.
	s := newValidationTestSrv(t)
	ctx := context.Background()

	platImg := pendingPlatformImage("img_plat_badsig")
	seedValidationImage(s.mem, platImg)

	repo := db.New(s.mem)

	// Record failed signature.
	if err := repo.UpdateImageSignature(ctx, "img_plat_badsig", "sha256:prov-fail", false); err != nil {
		t.Fatalf("UpdateImageSignature (false): %v", err)
	}

	// Promote the image (status becomes ACTIVE).
	if err := repo.PromoteValidatedImage(ctx, "img_plat_badsig"); err != nil {
		t.Fatalf("PromoteValidatedImage: %v", err)
	}

	// Launch must be blocked by trust gate (not lifecycle gate).
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_plat_badsig"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for PLATFORM image with failed signature, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageTrustViolation {
		t.Errorf("want code %q, got %q", errImageTrustViolation, env.Error.Code)
	}
}

func TestTrust_PlatformImageNoProvenanceIsBackwardCompat(t *testing.T) {
	// PLATFORM image without provenance_hash (nil) must pass the trust check.
	// This is the backward-compatibility seam for pre-factory images.
	// Source: image_repo.go ImageIsTrusted: "no provenance attached yet — skip check".
	s := newValidationTestSrv(t)
	ctx := context.Background()

	// Build a PLATFORM image with no provenance (pre-factory state).
	img := &db.ImageRow{
		ID: "img_plat_noprov", Name: "plat-noprov",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/noprov.qcow2", MinDiskGB: 10,
		Status: db.ImageStatusActive, ValidationStatus: "passed",
		ProvenanceHash: nil, SignatureValid: nil, // no factory provenance
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	seedValidationImage(s.mem, img)

	_ = ctx // no repo call needed; image is already ACTIVE

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_plat_noprov"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for PLATFORM image without provenance (backward compat), got %d", resp.StatusCode)
	}
}

// ── SHARED-IMAGE LAUNCH non-regression ───────────────────────────────────────

func TestSharedImage_GranteeLaunchAfterPromotion(t *testing.T) {
	// Grantee must be able to launch a PRIVATE user image that has been promoted
	// from PENDING_VALIDATION to ACTIVE. This confirms image-sharing + validation
	// integration works correctly together.
	//
	// Uses sharePool (wrapping memPool) to handle grant SQL.
	// Source: VM-P3B Job 1 (image sharing contract).
	sPool := newSharePool()
	repo := db.New(sPool)
	ctx := context.Background()
	t.Helper()

	// Build server with sharePool so grant SQL is handled.
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
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	// Seed default platform image for test infrastructure.
	now := time.Now()
	sPool.inner.images["00000000-0000-0000-0000-000000000010"] = &db.ImageRow{
		ID: "00000000-0000-0000-0000-000000000010", Name: "ubuntu-22.04-lts",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/ubuntu-22.04.qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed", CreatedAt: now, UpdatedAt: now,
	}

	// Seed a PRIVATE user image as ACTIVE (already promoted).
	sPool.inner.images["img_shared_promo"] = &db.ImageRow{
		ID: "img_shared_promo", Name: "shared-promo",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PRIVATE", SourceType: "USER",
		StorageURL: "nfs://images/shared.qcow2", MinDiskGB: 10,
		Status: db.ImageStatusActive, ValidationStatus: "passed",
		CreatedAt: now, UpdatedAt: now,
	}

	// Grant bob access.
	seedShareGrant(sPool, "img_shared_promo", alice, bob)

	// Bob launches the shared image.
	resp := doReq(t, ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_shared_promo"), authHdr(bob))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for grantee launch of promoted shared image, got %d", resp.StatusCode)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// recordAllStagesPass records a pass result for all required validation stages.
func recordAllStagesPass(t *testing.T, repo *db.Repo, ctx context.Context, imageID, jobID string) {
	t.Helper()
	for i, stage := range db.RequiredValidationStages {
		if err := repo.RecordValidationStage(ctx, &db.ImageValidationResultRow{
			ID:         "ivr_allpass_" + stage,
			ImageID:    imageID,
			JobID:      jobID,
			Stage:      stage,
			Result:     db.ValidationResultPass,
			RecordedAt: time.Now().Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("RecordValidationStage stage=%s: %v", stage, err)
		}
	}
}
