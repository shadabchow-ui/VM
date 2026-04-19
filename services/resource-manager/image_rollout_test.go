package main

// image_rollout_test.go — VM-P3C Job 2: rollout, promotion, and family
// progression tests.
//
// Coverage:
//
//   ROLLOUT STATE MACHINE WRITES (direct repo):
//     - CreateRollout persists record in 'pending' status
//     - StartCanary transitions pending → canary, records canary_percent
//     - AdvanceCanary updates canary_percent while staying in canary
//     - BeginPromotion transitions canary → promoting
//     - CompleteRollout transitions promoting → completed
//     - BeginRollback transitions canary → rolling_back, records failure_reason
//     - BeginRollback transitions promoting → rolling_back
//     - CompleteRollback transitions rolling_back → rolled_back
//     - StartCanary on non-pending rollout returns error (0 rows)
//     - BeginPromotion on non-canary rollout returns error (0 rows)
//
//   FAMILY PROGRESSION (direct repo + HTTP):
//     - CompleteRollout atomically updates family_version (MAX+1)
//     - After CompleteRollout, ResolveFamilyLatest returns promoted image
//     - After CompleteRollout, launch via family alias resolves to new image
//     - CompleteRollout on non-promoting/canary rollout returns error
//
//   ROLLBACK / FAILURE BEHAVIOUR:
//     - CompleteRollback marks image FAILED
//     - CompleteRollback leaves family alias at previous version
//     - After CompleteRollback, FAILED image is blocked at admission
//     - After CompleteRollback, family alias still resolves to prior image
//     - CompleteRollback on non-rolling_back rollout returns error
//
//   PROMOTION ELIGIBILITY:
//     - Only ACTIVE images can have their family alias updated (UpdateFamilyAlias)
//     - CompleteRollback step 2 accepts ACTIVE and PENDING_VALIDATION (late failure)
//     - CompleteRollback step 2 on already-FAILED image is a no-op (idempotent)
//
//   CVE WAIVERS:
//     - IsCVEWaived returns false when no waiver exists
//     - IsCVEWaived returns true for active family-specific waiver
//     - IsCVEWaived returns true for active global waiver (nil image_family)
//     - IsCVEWaived returns false after RevokeCVEWaiver
//     - IsCVEWaived returns false for expired waiver
//
//   SHARED HELPERS:
//     - familyLaunchBodyP3C: used by image_catalog_test.go TestPlatformTrust tests
//
// Test strategy: direct db.Repo calls via rolloutPool for state-machine tests;
// HTTP round-trips via rolloutTestSrv for family-resolution admission tests.
//
// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator,
//         vm-13-01__blueprint__ §core_contracts "Image Family Atomicity",
//         internal/db/image_rollout_repo.go,
//         11-02-phase-1-test-strategy.md §unit test approach.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── familyLaunchBodyP3C ──────────────────────────────────────────────────────────

// familyLaunchBodyP3C returns a CreateInstanceRequest using image_family alias
// instead of a direct image_id. version may be nil (resolves latest).
//
// Used by image_catalog_test.go TestPlatformTrust_FamilyResolved* tests and
// rollout promotion integration tests.
//
// Source: vm-13-01__blueprint__ §family_seam.
func familyLaunchBodyP3C(familyName string, version *int) CreateInstanceRequest {
	return CreateInstanceRequest{
		Name:             "test-inst-family",
		InstanceType:     "c1.small",
		AvailabilityZone: "us-east-1a",
		SSHKeyName:       "my-key",
		ImageFamily: &ImageFamilyRef{
			FamilyName:    familyName,
			FamilyVersion: version,
		},
	}
}

// ── rolloutTestSrv helpers ────────────────────────────────────────────────────

// newRolloutTestSrv creates a full HTTP test server backed by rolloutPool.
// Used for family-resolution admission integration tests.
func newRolloutTestSrv(t *testing.T) *rolloutTestSrv {
	t.Helper()
	pool := newRolloutPool()
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

	// Seed default PLATFORM image (no provenance_hash → backward-compat trusted)
	// so standard create-instance tests pass.
	now := time.Now()
	pool.inner.inner.images["00000000-0000-0000-0000-000000000010"] = &db.ImageRow{
		ID: "00000000-0000-0000-0000-000000000010", Name: "ubuntu-22.04-lts",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/ubuntu-22.04.qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed",
		CreatedAt: now, UpdatedAt: now,
	}
	return &rolloutTestSrv{ts: ts, mem: pool}
}

// seedRolloutImage adds an image to rolloutPool's inner memPool.
func seedRolloutImage(pool *rolloutPool, img *db.ImageRow) {
	if img.CreatedAt.IsZero() {
		img.CreatedAt = time.Now()
	}
	if img.UpdatedAt.IsZero() {
		img.UpdatedAt = time.Now()
	}
	pool.inner.inner.images[img.ID] = img
}

// platformFamilyImage returns a PUBLIC PLATFORM ACTIVE image in a named family.
func platformFamilyImage(id, familyName string, familyVersion *int) *db.ImageRow {
	sigOK := true
	hash := "sha256:prov-" + id
	return &db.ImageRow{
		ID: id, Name: "plat-" + id,
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/" + id + ".qcow2", MinDiskGB: 10,
		Status: db.ImageStatusActive, ValidationStatus: "passed",
		ProvenanceHash: &hash, SignatureValid: &sigOK,
		FamilyName:    &familyName,
		FamilyVersion: familyVersion,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
}

// intPtr returns a pointer to an int.
func intPtr(n int) *int { return &n }

// ── ROLLOUT STATE MACHINE tests ───────────────────────────────────────────────

func TestRollout_CreateRollout_PendingStatus(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_01", ImageID: "img_ro_01", JobID: "job_01", FamilyName: "ubuntu-lts",
	})
	if err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}

	r, ok := pool.rollouts["img_ro_01"]
	if !ok {
		t.Fatal("want rollout persisted, got none")
	}
	if r.Status != db.RolloutStatusPending {
		t.Errorf("want status=%q, got %q", db.RolloutStatusPending, r.Status)
	}
	if r.CanaryPercent != 0 {
		t.Errorf("want canary_percent=0, got %d", r.CanaryPercent)
	}
}

func TestRollout_GetRolloutByImageID_ReturnsRecord(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_get_01", ImageID: "img_get_01", JobID: "job_get", FamilyName: "fam-get",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}

	got, err := repo.GetRolloutByImageID(ctx, "img_get_01")
	if err != nil {
		t.Fatalf("GetRolloutByImageID: %v", err)
	}
	if got == nil {
		t.Fatal("want rollout, got nil")
	}
	if got.FamilyName != "fam-get" {
		t.Errorf("want family_name=%q, got %q", "fam-get", got.FamilyName)
	}
}

func TestRollout_GetRolloutByImageID_NilWhenAbsent(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	got, err := repo.GetRolloutByImageID(ctx, "img_nonexistent")
	if err != nil {
		t.Fatalf("GetRolloutByImageID for missing image: %v", err)
	}
	if got != nil {
		t.Error("want nil for missing rollout, got non-nil")
	}
}

func TestRollout_StartCanary_PendingToCanary(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_sc_01", ImageID: "img_sc_01", JobID: "job_sc", FamilyName: "fam-sc",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}

	if err := repo.StartCanary(ctx, "rol_sc_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}

	r := pool.rolloutsByID["rol_sc_01"]
	if r.Status != db.RolloutStatusCanary {
		t.Errorf("want status=canary, got %q", r.Status)
	}
	if r.CanaryPercent != 5 {
		t.Errorf("want canary_percent=5, got %d", r.CanaryPercent)
	}
}

func TestRollout_StartCanary_NonPendingReturnsError(t *testing.T) {
	// StartCanary on a non-pending rollout must return an error.
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_sc_e", ImageID: "img_sc_e", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	// Advance to canary first.
	if err := repo.StartCanary(ctx, "rol_sc_e", 5); err != nil {
		t.Fatalf("StartCanary (first): %v", err)
	}
	// Try to start canary again — must fail (already canary, not pending).
	if err := repo.StartCanary(ctx, "rol_sc_e", 10); err == nil {
		t.Error("want error calling StartCanary on non-pending rollout, got nil")
	}
}

func TestRollout_AdvanceCanary_UpdatesPercent(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_ac_01", ImageID: "img_ac_01", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_ac_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.AdvanceCanary(ctx, "rol_ac_01", 25); err != nil {
		t.Fatalf("AdvanceCanary to 25: %v", err)
	}
	if err := repo.AdvanceCanary(ctx, "rol_ac_01", 50); err != nil {
		t.Fatalf("AdvanceCanary to 50: %v", err)
	}

	r := pool.rolloutsByID["rol_ac_01"]
	if r.CanaryPercent != 50 {
		t.Errorf("want canary_percent=50, got %d", r.CanaryPercent)
	}
	if r.Status != db.RolloutStatusCanary {
		t.Errorf("want status=canary, got %q", r.Status)
	}
}

func TestRollout_BeginPromotion_CanaryToPromoting(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_bp_01", ImageID: "img_bp_01", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_bp_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginPromotion(ctx, "rol_bp_01"); err != nil {
		t.Fatalf("BeginPromotion: %v", err)
	}

	r := pool.rolloutsByID["rol_bp_01"]
	if r.Status != db.RolloutStatusPromoting {
		t.Errorf("want status=promoting, got %q", r.Status)
	}
}

func TestRollout_BeginPromotion_NonCanaryReturnsError(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_bp_e", ImageID: "img_bp_e", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	// Still in pending — BeginPromotion must fail.
	if err := repo.BeginPromotion(ctx, "rol_bp_e"); err == nil {
		t.Error("want error calling BeginPromotion on pending rollout, got nil")
	}
}

func TestRollout_BeginRollback_CanaryToRollingBack(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_rb_01", ImageID: "img_rb_01", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_rb_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_rb_01", "canary error rate too high"); err != nil {
		t.Fatalf("BeginRollback: %v", err)
	}

	r := pool.rolloutsByID["rol_rb_01"]
	if r.Status != db.RolloutStatusRollingBack {
		t.Errorf("want status=rolling_back, got %q", r.Status)
	}
	if r.FailureReason == nil || *r.FailureReason != "canary error rate too high" {
		t.Errorf("want failure_reason set, got %v", r.FailureReason)
	}
}

func TestRollout_BeginRollback_PromotingToRollingBack(t *testing.T) {
	// BeginRollback also accepts status=promoting (rare mid-promotion failure).
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_rbp_01", ImageID: "img_rbp_01", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_rbp_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginPromotion(ctx, "rol_rbp_01"); err != nil {
		t.Fatalf("BeginPromotion: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_rbp_01", "promotion failure"); err != nil {
		t.Fatalf("BeginRollback from promoting: %v", err)
	}

	r := pool.rolloutsByID["rol_rbp_01"]
	if r.Status != db.RolloutStatusRollingBack {
		t.Errorf("want status=rolling_back after promoting rollback, got %q", r.Status)
	}
}

func TestRollout_CompleteRollback_RollingBackToRolledBack(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	// Seed an ACTIVE image so step 2 can mark it FAILED.
	pool.inner.inner.images["img_cr_01"] = &db.ImageRow{
		ID: "img_cr_01", Status: db.ImageStatusActive,
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_cr_01", ImageID: "img_cr_01", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_cr_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_cr_01", "boot failure"); err != nil {
		t.Fatalf("BeginRollback: %v", err)
	}
	if err := repo.CompleteRollback(ctx, "rol_cr_01", "img_cr_01"); err != nil {
		t.Fatalf("CompleteRollback: %v", err)
	}

	r := pool.rolloutsByID["rol_cr_01"]
	if r.Status != db.RolloutStatusRolledBack {
		t.Errorf("want status=rolled_back, got %q", r.Status)
	}
	if r.CompletedAt == nil {
		t.Error("want completed_at set after CompleteRollback")
	}
}

func TestRollout_CompleteRollback_NonRollingBackReturnsError(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_cr_e", ImageID: "img_cr_e", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	// Still pending — CompleteRollback must fail.
	if err := repo.CompleteRollback(ctx, "rol_cr_e", "img_cr_e"); err == nil {
		t.Error("want error calling CompleteRollback on pending rollout, got nil")
	}
}

// ── FAMILY PROGRESSION tests ──────────────────────────────────────────────────

func TestRollout_CompleteRollout_UpdatesFamilyAlias(t *testing.T) {
	// CompleteRollout must atomically set family_version = MAX+1.
	// Source: vm-13-01__blueprint__ §core_contracts "Image Family Atomicity".
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	// Seed existing family member at version 3.
	pool.inner.inner.images["img_fam_old"] = platformFamilyImage("img_fam_old", "test-family", intPtr(3))

	// Seed the new candidate (no version yet, ACTIVE after validation).
	pool.inner.inner.images["img_fam_new"] = platformFamilyImage("img_fam_new", "test-family", nil)

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_fam_01", ImageID: "img_fam_new", JobID: "job", FamilyName: "test-family",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_fam_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginPromotion(ctx, "rol_fam_01"); err != nil {
		t.Fatalf("BeginPromotion: %v", err)
	}
	if err := repo.CompleteRollout(ctx, "rol_fam_01", "img_fam_new", "test-family"); err != nil {
		t.Fatalf("CompleteRollout: %v", err)
	}

	// Rollout must be completed.
	r := pool.rolloutsByID["rol_fam_01"]
	if r.Status != db.RolloutStatusCompleted {
		t.Errorf("want status=completed, got %q", r.Status)
	}
	if r.CompletedAt == nil {
		t.Error("want completed_at set")
	}

	// Image must have family_version = max(3)+1 = 4.
	newImg := pool.inner.inner.images["img_fam_new"]
	if newImg.FamilyVersion == nil {
		t.Fatal("want family_version set on promoted image, got nil")
	}
	if *newImg.FamilyVersion != 4 {
		t.Errorf("want family_version=4 (max+1), got %d", *newImg.FamilyVersion)
	}
}

func TestRollout_CompleteRollout_PromotedImageBecomesLatest(t *testing.T) {
	// After CompleteRollout, ResolveFamilyLatest must return the promoted image.
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_prev_01"] = platformFamilyImage("img_prev_01", "rollout-family", intPtr(1))
	pool.inner.inner.images["img_next_01"] = platformFamilyImage("img_next_01", "rollout-family", nil)

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_latest_01", ImageID: "img_next_01", JobID: "job", FamilyName: "rollout-family",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_latest_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginPromotion(ctx, "rol_latest_01"); err != nil {
		t.Fatalf("BeginPromotion: %v", err)
	}
	if err := repo.CompleteRollout(ctx, "rol_latest_01", "img_next_01", "rollout-family"); err != nil {
		t.Fatalf("CompleteRollout: %v", err)
	}

	resolved, err := repo.ResolveFamilyLatest(ctx, "rollout-family", "any-principal")
	if err != nil {
		t.Fatalf("ResolveFamilyLatest: %v", err)
	}
	if resolved == nil {
		t.Fatal("want resolved image, got nil")
	}
	if resolved.ID != "img_next_01" {
		t.Errorf("want img_next_01 as latest after promotion, got %q", resolved.ID)
	}
}

func TestRollout_CompleteRollout_FamilyLaunchResolvesToPromoted(t *testing.T) {
	// After CompleteRollout, launching via family alias must hit the new image.
	s := newRolloutTestSrv(t)
	ctx := context.Background()

	seedRolloutImage(s.mem, platformFamilyImage("img_prev_http", "http-family", intPtr(2)))
	seedRolloutImage(s.mem, platformFamilyImage("img_next_http", "http-family", nil))

	repo := db.New(s.mem)

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_http_01", ImageID: "img_next_http", JobID: "job", FamilyName: "http-family",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_http_01", 10); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginPromotion(ctx, "rol_http_01"); err != nil {
		t.Fatalf("BeginPromotion: %v", err)
	}
	if err := repo.CompleteRollout(ctx, "rol_http_01", "img_next_http", "http-family"); err != nil {
		t.Fatalf("CompleteRollout: %v", err)
	}

	// Launch via family alias must succeed (202).
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBodyP3C("http-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for family launch after promotion, got %d", resp.StatusCode)
	}
}

func TestRollout_CompleteRollout_NonPromotingReturnsError(t *testing.T) {
	// CompleteRollout on a rollout not in promoting or canary state must error.
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_cr2_01"] = platformFamilyImage("img_cr2_01", "fam-err", intPtr(1))

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_err_01", ImageID: "img_cr2_01", JobID: "job", FamilyName: "fam-err",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	// Still in pending — CompleteRollout must fail.
	if err := repo.CompleteRollout(ctx, "rol_err_01", "img_cr2_01", "fam-err"); err == nil {
		t.Error("want error calling CompleteRollout on pending rollout, got nil")
	}
}

// ── ROLLBACK / FAILURE tests ─────────────────────────────────────────────────

func TestRollback_CompleteRollback_MarksImageFailed(t *testing.T) {
	// CompleteRollback must transition the image to FAILED.
	// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator:
	//   "On failure, invokes the Image Catalog API to mark the image FAILED".
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_fail_rb"] = platformFamilyImage("img_fail_rb", "fail-family", nil)

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_fail_rb", ImageID: "img_fail_rb", JobID: "job", FamilyName: "fail-family",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_fail_rb", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_fail_rb", "high error rate"); err != nil {
		t.Fatalf("BeginRollback: %v", err)
	}
	if err := repo.CompleteRollback(ctx, "rol_fail_rb", "img_fail_rb"); err != nil {
		t.Fatalf("CompleteRollback: %v", err)
	}

	img := pool.inner.inner.images["img_fail_rb"]
	if img.Status != db.ImageStatusFailed {
		t.Errorf("want image status=FAILED after rollback, got %q", img.Status)
	}
}

func TestRollback_FamilyAliasUnchangedAfterRollback(t *testing.T) {
	// CompleteRollback must NOT update the family alias.
	// The previously promoted image must remain the latest.
	// Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator:
	//   "the 'Image Family' alias remains pointed at the last known-good version".
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	// Existing stable image at version 5.
	pool.inner.inner.images["img_stable"] = platformFamilyImage("img_stable", "stable-family", intPtr(5))

	// Candidate that will be rolled back (no version assigned yet).
	pool.inner.inner.images["img_candidate"] = platformFamilyImage("img_candidate", "stable-family", nil)

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_stable", ImageID: "img_candidate", JobID: "job", FamilyName: "stable-family",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_stable", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_stable", "canary failure"); err != nil {
		t.Fatalf("BeginRollback: %v", err)
	}
	if err := repo.CompleteRollback(ctx, "rol_stable", "img_candidate"); err != nil {
		t.Fatalf("CompleteRollback: %v", err)
	}

	// Candidate must be FAILED.
	candidate := pool.inner.inner.images["img_candidate"]
	if candidate.Status != db.ImageStatusFailed {
		t.Errorf("want candidate FAILED, got %q", candidate.Status)
	}

	// Candidate must NOT have a family_version assigned (alias was never updated).
	if candidate.FamilyVersion != nil {
		t.Errorf("want candidate family_version=nil (alias not promoted), got %d", *candidate.FamilyVersion)
	}

	// Family alias must still resolve to the stable image.
	resolved, err := repo.ResolveFamilyLatest(ctx, "stable-family", "any")
	if err != nil {
		t.Fatalf("ResolveFamilyLatest: %v", err)
	}
	if resolved == nil {
		t.Fatal("want stable image still resolvable after rollback, got nil")
	}
	if resolved.ID != "img_stable" {
		t.Errorf("want img_stable as latest after rollback, got %q", resolved.ID)
	}
}

func TestRollback_FailedImageBlockedAtAdmission(t *testing.T) {
	// After CompleteRollback, the FAILED candidate must be blocked at launch.
	s := newRolloutTestSrv(t)
	ctx := context.Background()

	seedRolloutImage(s.mem, platformFamilyImage("img_rb_adm", "adm-family", nil))

	repo := db.New(s.mem)

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_adm_rb", ImageID: "img_rb_adm", JobID: "job", FamilyName: "adm-family",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_adm_rb", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_adm_rb", "failure"); err != nil {
		t.Fatalf("BeginRollback: %v", err)
	}
	if err := repo.CompleteRollback(ctx, "rol_adm_rb", "img_rb_adm"); err != nil {
		t.Fatalf("CompleteRollback: %v", err)
	}

	// Direct launch of the FAILED image must be blocked.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_rb_adm"), authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for FAILED image after rollback, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotLaunchable {
		t.Errorf("want code %q, got %q", errImageNotLaunchable, env.Error.Code)
	}
}

func TestRollback_FamilyStillLaunchableAfterRollback(t *testing.T) {
	// After CompleteRollback, the prior stable image must still be launchable
	// via the family alias.
	s := newRolloutTestSrv(t)
	ctx := context.Background()

	// Stable image at version 2 — already in the family.
	seedRolloutImage(s.mem, platformFamilyImage("img_stable_http", "stable-http-family", intPtr(2)))
	// Candidate (to be rolled back).
	seedRolloutImage(s.mem, platformFamilyImage("img_cand_http", "stable-http-family", nil))

	repo := db.New(s.mem)

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_sh_01", ImageID: "img_cand_http", JobID: "job", FamilyName: "stable-http-family",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_sh_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_sh_01", "regression"); err != nil {
		t.Fatalf("BeginRollback: %v", err)
	}
	if err := repo.CompleteRollback(ctx, "rol_sh_01", "img_cand_http"); err != nil {
		t.Fatalf("CompleteRollback: %v", err)
	}

	// Family launch must still work via the stable image.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBodyP3C("stable-http-family", nil), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for family launch after rollback (stable img still active), got %d", resp.StatusCode)
	}
}

// ── PROMOTION ELIGIBILITY tests ───────────────────────────────────────────────

func TestPromotion_OnlyActiveImageGetsAliasUpdate(t *testing.T) {
	// UpdateFamilyAlias (called by CompleteRollout step 2) only accepts
	// images in ACTIVE status. A PENDING_VALIDATION image must be rejected.
	// Source: image_repo.go UpdateFamilyAlias WHERE status = 'ACTIVE'.
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	fn := "elig-family"
	// Image in PENDING_VALIDATION — not yet promoted.
	pool.inner.inner.images["img_pv_elig"] = &db.ImageRow{
		ID: "img_pv_elig", Status: db.ImageStatusPendingValidation,
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		FamilyName: &fn, FamilyVersion: nil,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	// UpdateFamilyAlias must return 0 rows affected → error.
	if err := repo.UpdateFamilyAlias(ctx, "elig-family", "img_pv_elig"); err == nil {
		t.Error("want error when promoting PENDING_VALIDATION image, got nil")
	}

	// Family version must remain nil.
	img := pool.inner.inner.images["img_pv_elig"]
	if img.FamilyVersion != nil {
		t.Error("want family_version unchanged (nil) for non-ACTIVE image")
	}
}

func TestPromotion_RollbackMarksPendingValidationFailed(t *testing.T) {
	// CompleteRollback step 2 accepts PENDING_VALIDATION images
	// (early failure before image was activated).
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_pv_rb"] = &db.ImageRow{
		ID: "img_pv_rb", Status: db.ImageStatusPendingValidation,
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_pv_rb", ImageID: "img_pv_rb", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_pv_rb", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_pv_rb", "early failure"); err != nil {
		t.Fatalf("BeginRollback: %v", err)
	}
	if err := repo.CompleteRollback(ctx, "rol_pv_rb", "img_pv_rb"); err != nil {
		t.Fatalf("CompleteRollback: %v", err)
	}

	img := pool.inner.inner.images["img_pv_rb"]
	if img.Status != db.ImageStatusFailed {
		t.Errorf("want FAILED for PENDING_VALIDATION image after rollback, got %q", img.Status)
	}
}

func TestPromotion_RollbackAlreadyFailedIsNoOp(t *testing.T) {
	// CompleteRollback step 2 on an already-FAILED image is idempotent (0 rows OK).
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	pool.inner.inner.images["img_already_failed"] = &db.ImageRow{
		ID: "img_already_failed", Status: db.ImageStatusFailed,
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}

	if err := repo.CreateRollout(ctx, &db.ImageRolloutRow{
		ID: "rol_af_01", ImageID: "img_already_failed", JobID: "job", FamilyName: "fam",
	}); err != nil {
		t.Fatalf("CreateRollout: %v", err)
	}
	if err := repo.StartCanary(ctx, "rol_af_01", 5); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if err := repo.BeginRollback(ctx, "rol_af_01", "reason"); err != nil {
		t.Fatalf("BeginRollback: %v", err)
	}
	// CompleteRollback step 2 returns 0 rows for already-FAILED — must not error.
	if err := repo.CompleteRollback(ctx, "rol_af_01", "img_already_failed"); err != nil {
		t.Fatalf("CompleteRollback on already-FAILED image must be no-op, got: %v", err)
	}

	// Image must remain FAILED.
	img := pool.inner.inner.images["img_already_failed"]
	if img.Status != db.ImageStatusFailed {
		t.Errorf("want image still FAILED, got %q", img.Status)
	}
}

// ── CVE WAIVER tests ──────────────────────────────────────────────────────────

func TestCVEWaiver_NotWaivedWhenAbsent(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	waived, err := repo.IsCVEWaived(ctx, "CVE-2024-0001", "ubuntu-lts")
	if err != nil {
		t.Fatalf("IsCVEWaived: %v", err)
	}
	if waived {
		t.Error("want IsCVEWaived=false when no waiver exists")
	}
}

func TestCVEWaiver_FamilySpecificWaiverApplies(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	fam := "ubuntu-lts"
	if err := repo.CreateCVEWaiver(ctx, &db.ImageCVEWaiverRow{
		ID: "w_01", CVEID: "CVE-2024-0001", ImageFamily: &fam,
		GrantedBy: "ops", Reason: "mitigated upstream",
	}); err != nil {
		t.Fatalf("CreateCVEWaiver: %v", err)
	}

	waived, err := repo.IsCVEWaived(ctx, "CVE-2024-0001", "ubuntu-lts")
	if err != nil {
		t.Fatalf("IsCVEWaived: %v", err)
	}
	if !waived {
		t.Error("want IsCVEWaived=true for active family-specific waiver")
	}
}

func TestCVEWaiver_GlobalWaiverApplies(t *testing.T) {
	// Global waiver (image_family IS NULL) must apply to any family.
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	if err := repo.CreateCVEWaiver(ctx, &db.ImageCVEWaiverRow{
		ID: "w_global", CVEID: "CVE-2024-9999", ImageFamily: nil,
		GrantedBy: "ops", Reason: "global mitigation",
	}); err != nil {
		t.Fatalf("CreateCVEWaiver (global): %v", err)
	}

	for _, fam := range []string{"ubuntu-lts", "debian-12", "any-other-family"} {
		waived, err := repo.IsCVEWaived(ctx, "CVE-2024-9999", fam)
		if err != nil {
			t.Fatalf("IsCVEWaived for %s: %v", fam, err)
		}
		if !waived {
			t.Errorf("want global waiver to apply to family %q, but IsCVEWaived=false", fam)
		}
	}
}

func TestCVEWaiver_FamilyWaiverDoesNotApplyToOtherFamily(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	fam := "ubuntu-lts"
	if err := repo.CreateCVEWaiver(ctx, &db.ImageCVEWaiverRow{
		ID: "w_fam_specific", CVEID: "CVE-2024-0002", ImageFamily: &fam,
		GrantedBy: "ops", Reason: "mitigated",
	}); err != nil {
		t.Fatalf("CreateCVEWaiver: %v", err)
	}

	// Waiver is for ubuntu-lts; debian-12 must NOT be waived.
	waived, err := repo.IsCVEWaived(ctx, "CVE-2024-0002", "debian-12")
	if err != nil {
		t.Fatalf("IsCVEWaived: %v", err)
	}
	if waived {
		t.Error("want IsCVEWaived=false for different family")
	}
}

func TestCVEWaiver_RevokedWaiverNotApplied(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	fam := "ubuntu-lts"
	if err := repo.CreateCVEWaiver(ctx, &db.ImageCVEWaiverRow{
		ID: "w_revoke", CVEID: "CVE-2024-0003", ImageFamily: &fam,
		GrantedBy: "ops", Reason: "mitigated",
	}); err != nil {
		t.Fatalf("CreateCVEWaiver: %v", err)
	}

	// Revoke it.
	if err := repo.RevokeCVEWaiver(ctx, "w_revoke"); err != nil {
		t.Fatalf("RevokeCVEWaiver: %v", err)
	}

	waived, err := repo.IsCVEWaived(ctx, "CVE-2024-0003", "ubuntu-lts")
	if err != nil {
		t.Fatalf("IsCVEWaived after revoke: %v", err)
	}
	if waived {
		t.Error("want IsCVEWaived=false after waiver is revoked")
	}
}

func TestCVEWaiver_ExpiredWaiverNotApplied(t *testing.T) {
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	fam := "ubuntu-lts"
	// Create a waiver that expired 1 second ago.
	expired := time.Now().Add(-time.Second)
	if err := repo.CreateCVEWaiver(ctx, &db.ImageCVEWaiverRow{
		ID: "w_expired", CVEID: "CVE-2024-0004", ImageFamily: &fam,
		GrantedBy: "ops", Reason: "mitigated", ExpiresAt: &expired,
	}); err != nil {
		t.Fatalf("CreateCVEWaiver: %v", err)
	}

	waived, err := repo.IsCVEWaived(ctx, "CVE-2024-0004", "ubuntu-lts")
	if err != nil {
		t.Fatalf("IsCVEWaived with expired waiver: %v", err)
	}
	if waived {
		t.Error("want IsCVEWaived=false for expired waiver")
	}
}

func TestCVEWaiver_RevokeCVEWaiver_Idempotent(t *testing.T) {
	// Revoking an already-revoked waiver must be a no-op (no error).
	pool := newRolloutPool()
	repo := db.New(pool)
	ctx := context.Background()

	fam := "ubuntu-lts"
	if err := repo.CreateCVEWaiver(ctx, &db.ImageCVEWaiverRow{
		ID: "w_idem", CVEID: "CVE-2024-0005", ImageFamily: &fam,
		GrantedBy: "ops", Reason: "mitigated",
	}); err != nil {
		t.Fatalf("CreateCVEWaiver: %v", err)
	}
	if err := repo.RevokeCVEWaiver(ctx, "w_idem"); err != nil {
		t.Fatalf("first RevokeCVEWaiver: %v", err)
	}
	// Second revoke must not error.
	if err := repo.RevokeCVEWaiver(ctx, "w_idem"); err != nil {
		t.Fatalf("second RevokeCVEWaiver (idempotent): %v", err)
	}
}

// ── SQL dispatch self-tests ───────────────────────────────────────────────────

func TestDispatch_RollbackFailImageSQL_IsDistinct(t *testing.T) {
	// isRollbackFailImageSQL must match only the CompleteRollback step-2 shape.
	rollbackSQL := "UPDATE images SET status = 'FAILED', updated_at = NOW() WHERE id = $1 AND status IN ('ACTIVE', 'PENDING_VALIDATION')"
	failValidSQL := "UPDATE images SET status = 'FAILED', validation_status = 'failed', updated_at = NOW() WHERE id = $1 AND status = 'PENDING_VALIDATION'"

	if !isRollbackFailImageSQL(rollbackSQL) {
		t.Error("want isRollbackFailImageSQL=true for CompleteRollback step-2 SQL")
	}
	if isRollbackFailImageSQL(failValidSQL) {
		t.Error("want isRollbackFailImageSQL=false for FailValidatedImage SQL")
	}
}
