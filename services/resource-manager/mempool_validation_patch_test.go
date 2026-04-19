package main

// mempool_validation_patch_test.go — VM-P3C Job 1: validation/manifest SQL dispatch
// for the memPool test harness.
//
// Adds in-memory handling for all new SQL shapes produced by:
//   - image_validation_repo.go  (RecordValidationStage, AllStagesPassed,
//                                PromoteValidatedImage, FailValidatedImage,
//                                InsertValidationJob, InsertPublishJob,
//                                ListValidationResults)
//   - image_build_manifest_repo.go (UpsertBuildManifest, SetManifestSignature,
//                                   IsBuildManifestSigned, GetBuildManifest)
//
// Pattern: identical to mempool_image_patch_test.go (VM-P3B Job 2) and
//          image_share_handlers_test.go (VM-P3B Job 1).
//
// The validationPool wraps *imageCatalogPool (which already wraps *memPool)
// and intercepts new SQL shapes before delegating to the inner pool.
// This avoids touching any existing test file.
//
// SQL shape dispatch keys (must match the exact SQL strings in repo files):
//
//   INSERT INTO image_validation_results  → RecordValidationStage
//   INSERT INTO image_build_manifests     → UpsertBuildManifest (ON CONFLICT)
//   UPDATE image_build_manifests          → SetManifestSignature
//   UPDATE images … 'ACTIVE' … PENDING_VALIDATION   → PromoteValidatedImage
//   UPDATE images … 'FAILED' … PENDING_VALIDATION   → FailValidatedImage
//   SELECT DISTINCT ON (stage)            → AllStagesPassed
//   SELECT COUNT(*) FROM image_build_manifests      → IsBuildManifestSigned
//   SELECT … FROM image_build_manifests  → GetBuildManifest
//   SELECT … FROM image_validation_results          → ListValidationResults
//   IMAGE_VALIDATE / IMAGE_PUBLISH job inserts      → InsertValidationJob / InsertPublishJob
//
// Source: vm-13-01__blueprint__ §Image Validation Service,
//         vm-13-01__blueprint__ §Image Signing and Provenance Service,
//         internal/db/image_validation_repo.go,
//         internal/db/image_build_manifest_repo.go.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── In-memory stores ──────────────────────────────────────────────────────────

// validationPool wraps imageCatalogPool and handles validation/manifest SQL shapes.
type validationPool struct {
	inner           *imageCatalogPool
	buildManifests  map[string]*db.ImageBuildManifestRow    // keyed by image_id
	validationResults map[string][]*db.ImageValidationResultRow // keyed by image_id
}

func newValidationPool() *validationPool {
	return &validationPool{
		inner:             newImageCatalogPool(),
		buildManifests:    make(map[string]*db.ImageBuildManifestRow),
		validationResults: make(map[string][]*db.ImageValidationResultRow),
	}
}

func (p *validationPool) Close() {}

// ── Exec ──────────────────────────────────────────────────────────────────────

func (p *validationPool) Exec(ctx context.Context, sql string, args ...any) (db.CommandTag, error) {
	switch {
	// UpsertBuildManifest — INSERT INTO image_build_manifests
	case strings.Contains(sql, "INSERT INTO image_build_manifests"):
		return execUpsertBuildManifest(p, args)

	// SetManifestSignature — UPDATE image_build_manifests
	case strings.Contains(sql, "UPDATE image_build_manifests"):
		return execSetManifestSignature(p, args)

	// RecordValidationStage — INSERT INTO image_validation_results
	case strings.Contains(sql, "INSERT INTO image_validation_results"):
		return execRecordValidationStage(p, args)

	// PromoteValidatedImage — UPDATE images SET status = 'ACTIVE' WHERE … PENDING_VALIDATION
	case isPromoteImageSQL(sql):
		return execPromoteValidatedImage(p.inner.inner, args)

	// FailValidatedImage — UPDATE images SET status = 'FAILED' WHERE … PENDING_VALIDATION
	case isFailImageSQL(sql):
		return execFailValidatedImage(p.inner.inner, args)

	// IMAGE_VALIDATE / IMAGE_PUBLISH job inserts share the same INSERT INTO jobs shape
	// as InsertImageJob; the inner pool already handles INSERT INTO jobs generically.
	}
	return p.inner.Exec(ctx, sql, args...)
}

// ── Query ─────────────────────────────────────────────────────────────────────

func (p *validationPool) Query(ctx context.Context, sql string, args ...any) (db.Rows, error) {
	switch {
	// AllStagesPassed — SELECT DISTINCT ON (stage) stage, result FROM image_validation_results
	case strings.Contains(sql, "DISTINCT ON (stage)"):
		return queryAllStagesPassed(p, args)

	// ListValidationResults — SELECT … FROM image_validation_results WHERE image_id = $1
	case strings.Contains(sql, "FROM image_validation_results"):
		imageID := asStr(args[0])
		rows := p.validationResults[imageID]
		return &validationResultRows{rows: rows}, nil
	}
	return p.inner.Query(ctx, sql, args...)
}

// ── QueryRow ──────────────────────────────────────────────────────────────────

func (p *validationPool) QueryRow(ctx context.Context, sql string, args ...any) db.Row {
	switch {
	// IsBuildManifestSigned — SELECT COUNT(*) FROM image_build_manifests WHERE … signature IS NOT NULL …
	case strings.Contains(sql, "COUNT(*)") && strings.Contains(sql, "FROM image_build_manifests"):
		imageID := asStr(args[0])
		m, ok := p.buildManifests[imageID]
		if !ok || m.Signature == nil || m.ProvenanceJSON == nil || m.SignedAt == nil {
			return &intRow{value: 0}
		}
		return &intRow{value: 1}

	// GetBuildManifest — SELECT … FROM image_build_manifests WHERE image_id = $1
	case strings.Contains(sql, "FROM image_build_manifests"):
		imageID := asStr(args[0])
		m, ok := p.buildManifests[imageID]
		if !ok {
			return &errRow{fmt.Errorf("GetBuildManifest: no rows in result set")}
		}
		return &buildManifestRow{r: m}
	}
	return p.inner.QueryRow(ctx, sql, args...)
}

// ── Exec helpers ──────────────────────────────────────────────────────────────

// execUpsertBuildManifest handles UpsertBuildManifest.
// SQL args: $1=image_id, $2=build_config_ref, $3=base_image_digest, $4=image_digest
func execUpsertBuildManifest(p *validationPool, args []any) (db.CommandTag, error) {
	if len(args) < 4 {
		return &fakeTag{0}, fmt.Errorf("UpsertBuildManifest: expected 4 args, got %d", len(args))
	}
	imageID := asStr(args[0])
	existing, ok := p.buildManifests[imageID]
	if !ok {
		p.buildManifests[imageID] = &db.ImageBuildManifestRow{
			ImageID:         imageID,
			BuildConfigRef:  asStr(args[1]),
			BaseImageDigest: asStr(args[2]),
			ImageDigest:     asStr(args[3]),
			CreatedAt:       time.Now(),
		}
	} else {
		// ON CONFLICT DO UPDATE — update build fields only
		existing.BuildConfigRef = asStr(args[1])
		existing.BaseImageDigest = asStr(args[2])
		existing.ImageDigest = asStr(args[3])
	}
	return &fakeTag{1}, nil
}

// execSetManifestSignature handles SetManifestSignature.
// SQL args: $1=image_id, $2=provenance_json, $3=signature, $4=signed_at
func execSetManifestSignature(p *validationPool, args []any) (db.CommandTag, error) {
	if len(args) < 4 {
		return &fakeTag{0}, fmt.Errorf("SetManifestSignature: expected 4 args, got %d", len(args))
	}
	imageID := asStr(args[0])
	m, ok := p.buildManifests[imageID]
	if !ok {
		return &fakeTag{0}, fmt.Errorf("SetManifestSignature: no manifest for image %s", imageID)
	}
	pj := asStr(args[1])
	sig := asStr(args[2])
	m.ProvenanceJSON = &pj
	m.Signature = &sig
	if t, ok2 := args[3].(time.Time); ok2 {
		m.SignedAt = &t
	}
	return &fakeTag{1}, nil
}

// execRecordValidationStage handles RecordValidationStage.
// SQL args: $1=id, $2=image_id, $3=job_id, $4=stage, $5=result, $6=detail_json
func execRecordValidationStage(p *validationPool, args []any) (db.CommandTag, error) {
	if len(args) < 6 {
		return &fakeTag{0}, fmt.Errorf("RecordValidationStage: expected 6 args, got %d", len(args))
	}
	row := &db.ImageValidationResultRow{
		ID:         asStr(args[0]),
		ImageID:    asStr(args[1]),
		JobID:      asStr(args[2]),
		Stage:      asStr(args[3]),
		Result:     asStr(args[4]),
		RecordedAt: time.Now(),
	}
	if args[5] != nil {
		if s, ok := args[5].(*string); ok {
			row.DetailJSON = s
		}
	}
	p.validationResults[row.ImageID] = append(p.validationResults[row.ImageID], row)
	return &fakeTag{1}, nil
}

// execPromoteValidatedImage handles PromoteValidatedImage.
// SQL shape: UPDATE images SET status='ACTIVE', validation_status='passed' WHERE id=$1 AND status='PENDING_VALIDATION'
// args[0] = image_id
func execPromoteValidatedImage(p *memPool, args []any) (db.CommandTag, error) {
	if len(args) < 1 {
		return &fakeTag{0}, fmt.Errorf("PromoteValidatedImage: expected 1 arg, got %d", len(args))
	}
	id := asStr(args[0])
	img, ok := p.images[id]
	if !ok || img.Status != db.ImageStatusPendingValidation {
		return &fakeTag{0}, nil
	}
	img.Status = db.ImageStatusActive
	img.ValidationStatus = "passed"
	img.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// execFailValidatedImage handles FailValidatedImage.
// SQL shape: UPDATE images SET status='FAILED', validation_status='failed' WHERE id=$1 AND status='PENDING_VALIDATION'
// args[0] = image_id
func execFailValidatedImage(p *memPool, args []any) (db.CommandTag, error) {
	if len(args) < 1 {
		return &fakeTag{0}, fmt.Errorf("FailValidatedImage: expected 1 arg, got %d", len(args))
	}
	id := asStr(args[0])
	img, ok := p.images[id]
	if !ok || img.Status != db.ImageStatusPendingValidation {
		return &fakeTag{0}, nil
	}
	img.Status = db.ImageStatusFailed
	img.ValidationStatus = "failed"
	img.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// ── Query helpers ─────────────────────────────────────────────────────────────

// queryAllStagesPassed handles AllStagesPassed.
// Returns (stage, result) pairs for the most recent result per required stage.
// The real query uses DISTINCT ON (stage) ORDER BY stage, recorded_at DESC.
// We simulate this: for each required stage, find the latest result.
func queryAllStagesPassed(p *validationPool, args []any) (db.Rows, error) {
	imageID := asStr(args[0])
	results := p.validationResults[imageID]

	// For each required stage, find the most recent result.
	latest := make(map[string]*db.ImageValidationResultRow)
	for _, r := range results {
		if !isRequiredStage(r.Stage) {
			continue
		}
		existing, ok := latest[r.Stage]
		if !ok || r.RecordedAt.After(existing.RecordedAt) {
			latest[r.Stage] = r
		}
	}

	// Return as (stage, result) string pairs.
	var pairs []stagePair
	for stage, r := range latest {
		pairs = append(pairs, stagePair{stage: stage, result: r.Result})
	}
	return &stagePairRows{pairs: pairs}, nil
}

// isRequiredStage reports whether a stage name is in RequiredValidationStages.
func isRequiredStage(stage string) bool {
	for _, s := range db.RequiredValidationStages {
		if s == stage {
			return true
		}
	}
	return false
}

// ── SQL shape detectors ───────────────────────────────────────────────────────

// isPromoteImageSQL detects the PromoteValidatedImage UPDATE shape.
// SQL: UPDATE images SET status = 'ACTIVE', validation_status = 'passed' WHERE … AND status = 'PENDING_VALIDATION'
func isPromoteImageSQL(sql string) bool {
	return strings.Contains(sql, "UPDATE images") &&
		strings.Contains(sql, "'ACTIVE'") &&
		strings.Contains(sql, "PENDING_VALIDATION")
}

// isFailImageSQL detects the FailValidatedImage UPDATE shape.
// SQL: UPDATE images SET status = 'FAILED', validation_status = 'failed' WHERE … AND status = 'PENDING_VALIDATION'
func isFailImageSQL(sql string) bool {
	return strings.Contains(sql, "UPDATE images") &&
		strings.Contains(sql, "'FAILED'") &&
		strings.Contains(sql, "PENDING_VALIDATION")
}

// ── Row scan types ────────────────────────────────────────────────────────────

// buildManifestRow scans a single ImageBuildManifestRow.
// Column order matches selectBuildManifestCols in image_build_manifest_repo.go:
//   image_id, build_config_ref, base_image_digest, image_digest,
//   provenance_json, signature, signed_at, created_at
type buildManifestRow struct{ r *db.ImageBuildManifestRow }

func (row *buildManifestRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 8 {
		return fmt.Errorf("buildManifestRow.Scan: need 8 dest, got %d", len(dest))
	}
	*dest[0].(*string)      = r.ImageID
	*dest[1].(*string)      = r.BuildConfigRef
	*dest[2].(*string)      = r.BaseImageDigest
	*dest[3].(*string)      = r.ImageDigest
	*dest[4].(**string)     = r.ProvenanceJSON
	*dest[5].(**string)     = r.Signature
	*dest[6].(**time.Time)  = r.SignedAt
	*dest[7].(*time.Time)   = r.CreatedAt
	return nil
}

// validationResultRows iterates []*db.ImageValidationResultRow.
// Used by ListValidationResults.
type validationResultRows struct {
	rows []*db.ImageValidationResultRow
	pos  int
}

func (r *validationResultRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *validationResultRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 7 {
		return fmt.Errorf("validationResultRows.Scan: need 7 dest, got %d", len(dest))
	}
	*dest[0].(*string)     = row.ID
	*dest[1].(*string)     = row.ImageID
	*dest[2].(*string)     = row.JobID
	*dest[3].(*string)     = row.Stage
	*dest[4].(*string)     = row.Result
	*dest[5].(**string)    = row.DetailJSON
	*dest[6].(*time.Time)  = row.RecordedAt
	return nil
}

func (r *validationResultRows) Close()     {}
func (r *validationResultRows) Err() error { return nil }

// stagePair is a (stage, result) tuple returned by AllStagesPassed query.
type stagePair struct {
	stage  string
	result string
}

// stagePairRows iterates stage+result pairs for AllStagesPassed.
// Scan must match the SELECT shape: stage, result (2 columns).
type stagePairRows struct {
	pairs []stagePair
	pos   int
}

func (r *stagePairRows) Next() bool {
	if r.pos >= len(r.pairs) {
		return false
	}
	r.pos++
	return true
}

func (r *stagePairRows) Scan(dest ...any) error {
	p := r.pairs[r.pos-1]
	if len(dest) < 2 {
		return fmt.Errorf("stagePairRows.Scan: need 2 dest, got %d", len(dest))
	}
	*dest[0].(*string) = p.stage
	*dest[1].(*string) = p.result
	return nil
}

func (r *stagePairRows) Close()     {}
func (r *stagePairRows) Err() error { return nil }

// ── validationTestSrv ─────────────────────────────────────────────────────────

// validationTestSrv is the test server backed by validationPool.
// Used by image_validation_test.go for validation/manifest write-path tests.
type validationTestSrv struct {
	ts  *httptest.Server
	mem *validationPool
}
