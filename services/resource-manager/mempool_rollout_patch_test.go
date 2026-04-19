package main

// mempool_rollout_patch_test.go — VM-P3C Job 2: rollout-state and CVE-waiver
// SQL dispatch for the memPool test harness.
//
// Adds in-memory handling for all SQL shapes produced by:
//   - image_rollout_repo.go  (CreateRollout, StartCanary, AdvanceCanary,
//                             BeginPromotion, CompleteRollout, BeginRollback,
//                             CompleteRollback, GetRolloutByImageID)
//   - image_rollout_repo.go  (IsCVEWaived, CreateCVEWaiver, RevokeCVEWaiver)
//
// Pattern: identical to mempool_image_patch_test.go (VM-P3B Job 2).
// The rolloutPool wraps imageCatalogPool (which already wraps memPool) and
// intercepts new SQL shapes before delegating to the inner pool.
//
// SQL shape dispatch keys:
//
//   INSERT INTO image_rollouts              → CreateRollout
//   UPDATE image_rollouts … 'canary' …      → StartCanary
//   UPDATE image_rollouts … canary_percent  → AdvanceCanary
//   UPDATE image_rollouts … 'promoting'     → BeginPromotion
//   UPDATE image_rollouts … 'completed'     → CompleteRollout (step 1)
//   UPDATE images … family_version = ( …   → CompleteRollout (step 2 / UpdateFamilyAlias)
//   UPDATE image_rollouts … 'rolling_back'  → BeginRollback
//   UPDATE image_rollouts … 'rolled_back'   → CompleteRollback (step 1)
//   UPDATE images … 'FAILED' … IN ('ACTIVE' → CompleteRollback (step 2)
//   SELECT … FROM image_rollouts            → GetRolloutByImageID
//   SELECT COUNT(*) FROM image_cve_waivers  → IsCVEWaived
//   INSERT INTO image_cve_waivers           → CreateCVEWaiver
//   UPDATE image_cve_waivers                → RevokeCVEWaiver
//
// CompleteRollout step 2 reuses the existing UpdateFamilyAlias dispatcher
// (isUpdateFamilyAliasSQL / execUpdateFamilyAlias) from mempool_image_patch_test.go
// since the SQL is identical.
//
// Source: internal/db/image_rollout_repo.go,
//         vm-13-01__blueprint__ §Publication and Rollout Orchestrator,
//         vm-13-01__blueprint__ §core_contracts "Image Family Atomicity".

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── rolloutPool ───────────────────────────────────────────────────────────────

// rolloutPool wraps imageCatalogPool and handles rollout + CVE waiver SQL shapes.
type rolloutPool struct {
	inner      *imageCatalogPool
	rollouts   map[string]*db.ImageRolloutRow    // keyed by image_id (unique per image)
	rolloutsByID map[string]*db.ImageRolloutRow  // keyed by rollout id
	cveWaivers map[string]*db.ImageCVEWaiverRow  // keyed by waiver id
}

func newRolloutPool() *rolloutPool {
	return &rolloutPool{
		inner:        newImageCatalogPool(),
		rollouts:     make(map[string]*db.ImageRolloutRow),
		rolloutsByID: make(map[string]*db.ImageRolloutRow),
		cveWaivers:   make(map[string]*db.ImageCVEWaiverRow),
	}
}

func (p *rolloutPool) Close() {}

// ── Exec ──────────────────────────────────────────────────────────────────────

func (p *rolloutPool) Exec(ctx context.Context, sql string, args ...any) (db.CommandTag, error) {
	switch {
	// CreateRollout
	case strings.Contains(sql, "INSERT INTO image_rollouts"):
		return execCreateRollout(p, args)

	// StartCanary — UPDATE image_rollouts SET status = 'canary', canary_percent = $2
	// Discriminator: contains 'pending' in WHERE (and sets 'canary' in SET).
	case strings.Contains(sql, "UPDATE image_rollouts") && strings.Contains(sql, "'canary'") &&
		strings.Contains(sql, "'pending'"):
		return execStartCanary(p, args)

	// AdvanceCanary — UPDATE image_rollouts SET canary_percent = $2 (no status change)
	// Discriminator: canary_percent in SET but no status literal in WHERE for this op.
	// WHERE has status = 'canary' but SET does NOT set status.
	case strings.Contains(sql, "UPDATE image_rollouts") && strings.Contains(sql, "canary_percent = $2") &&
		!strings.Contains(sql, "'pending'") && !strings.Contains(sql, "'promoting'") &&
		!strings.Contains(sql, "'rolling_back'") && !strings.Contains(sql, "'completed'") &&
		!strings.Contains(sql, "'rolled_back'"):
		return execAdvanceCanary(p, args)

	// BeginPromotion — UPDATE image_rollouts SET status = 'promoting'
	case strings.Contains(sql, "UPDATE image_rollouts") && strings.Contains(sql, "'promoting'"):
		return execBeginPromotion(p, args)

	// CompleteRollout step 1 — UPDATE image_rollouts SET status = 'completed'
	case strings.Contains(sql, "UPDATE image_rollouts") && strings.Contains(sql, "'completed'"):
		return execCompleteRolloutStatus(p, args)

	// CompleteRollback step 1 — UPDATE image_rollouts SET status = 'rolled_back'
	// MUST come before BeginRollback since this SQL contains both 'rolled_back' and 'rolling_back'.
	case strings.Contains(sql, "UPDATE image_rollouts") && strings.Contains(sql, "'rolled_back'"):
		return execCompleteRollbackStatus(p, args)

	// BeginRollback — UPDATE image_rollouts SET status = 'rolling_back'
	case strings.Contains(sql, "UPDATE image_rollouts") && strings.Contains(sql, "'rolling_back'"):
		return execBeginRollback(p, args)

	// CompleteRollback step 2 — UPDATE images SET status = 'FAILED' WHERE … IN ('ACTIVE', 'PENDING_VALIDATION')
	case isRollbackFailImageSQL(sql):
		return execRollbackFailImage(p.inner.inner, args)

	// CompleteRollout step 2 — reuse UpdateFamilyAlias dispatcher
	case isUpdateFamilyAliasSQL(sql):
		return execUpdateFamilyAlias(p.inner.inner, args)

	// UpdateImageSignature (trust field writes, may come through this pool)
	case isUpdateImageSignatureSQL(sql):
		return execUpdateImageSignature(p.inner.inner, args)

	// RevokeCVEWaiver — UPDATE image_cve_waivers SET revoked_at
	case strings.Contains(sql, "UPDATE image_cve_waivers"):
		return execRevokeCVEWaiver(p, args)

	// CreateCVEWaiver — INSERT INTO image_cve_waivers
	case strings.Contains(sql, "INSERT INTO image_cve_waivers"):
		return execCreateCVEWaiver(p, args)
	}
	return p.inner.Exec(ctx, sql, args...)
}

// ── Query ─────────────────────────────────────────────────────────────────────

func (p *rolloutPool) Query(ctx context.Context, sql string, args ...any) (db.Rows, error) {
	return p.inner.Query(ctx, sql, args...)
}

// ── QueryRow ──────────────────────────────────────────────────────────────────

func (p *rolloutPool) QueryRow(ctx context.Context, sql string, args ...any) db.Row {
	switch {
	// GetRolloutByImageID — SELECT … FROM image_rollouts WHERE image_id = $1
	case strings.Contains(sql, "FROM image_rollouts"):
		imageID := asStr(args[0])
		r, ok := p.rollouts[imageID]
		if !ok {
			return &errRow{fmt.Errorf("GetRolloutByImageID: no rows in result set")}
		}
		return &rolloutRow{r: r}

	// IsCVEWaived — SELECT COUNT(*) FROM image_cve_waivers
	case strings.Contains(sql, "FROM image_cve_waivers"):
		cveID := asStr(args[0])
		familyName := asStr(args[1])
		now := time.Now()
		count := 0
		for _, w := range p.cveWaivers {
			if w.CVEID != cveID {
				continue
			}
			if w.RevokedAt != nil {
				continue
			}
			if w.ExpiresAt != nil && w.ExpiresAt.Before(now) {
				continue
			}
			// Match family-specific or global (nil family)
			if w.ImageFamily != nil && *w.ImageFamily != familyName {
				continue
			}
			count++
		}
		return &intRow{value: count}
	}
	return p.inner.QueryRow(ctx, sql, args...)
}

// ── Exec helpers ──────────────────────────────────────────────────────────────

// execCreateRollout handles CreateRollout.
// SQL args: $1=id, $2=image_id, $3=job_id, $4=family_name
func execCreateRollout(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 4 {
		return &fakeTag{0}, fmt.Errorf("CreateRollout: expected 4 args, got %d", len(args))
	}
	id := asStr(args[0])
	imageID := asStr(args[1])
	now := time.Now()
	row := &db.ImageRolloutRow{
		ID:            id,
		ImageID:       imageID,
		JobID:         asStr(args[2]),
		FamilyName:    asStr(args[3]),
		Status:        db.RolloutStatusPending,
		CanaryPercent: 0,
		StartedAt:     now,
		UpdatedAt:     now,
	}
	p.rollouts[imageID] = row
	p.rolloutsByID[id] = row
	return &fakeTag{1}, nil
}

// execStartCanary handles StartCanary.
// SQL args: $1=rollout_id, $2=canary_percent
func execStartCanary(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 2 {
		return &fakeTag{0}, fmt.Errorf("StartCanary: expected 2 args, got %d", len(args))
	}
	id := asStr(args[0])
	r, ok := p.rolloutsByID[id]
	if !ok || r.Status != db.RolloutStatusPending {
		return &fakeTag{0}, nil
	}
	pct, _ := args[1].(int)
	r.Status = db.RolloutStatusCanary
	r.CanaryPercent = pct
	r.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// execAdvanceCanary handles AdvanceCanary.
// SQL args: $1=rollout_id, $2=new_canary_percent
func execAdvanceCanary(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 2 {
		return &fakeTag{0}, fmt.Errorf("AdvanceCanary: expected 2 args, got %d", len(args))
	}
	id := asStr(args[0])
	r, ok := p.rolloutsByID[id]
	if !ok || r.Status != db.RolloutStatusCanary {
		return &fakeTag{0}, nil
	}
	pct, _ := args[1].(int)
	r.CanaryPercent = pct
	r.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// execBeginPromotion handles BeginPromotion.
// SQL args: $1=rollout_id
func execBeginPromotion(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 1 {
		return &fakeTag{0}, fmt.Errorf("BeginPromotion: expected 1 arg, got %d", len(args))
	}
	id := asStr(args[0])
	r, ok := p.rolloutsByID[id]
	if !ok || r.Status != db.RolloutStatusCanary {
		return &fakeTag{0}, nil
	}
	r.Status = db.RolloutStatusPromoting
	r.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// execCompleteRolloutStatus handles CompleteRollout step 1 (rollout status → completed).
// SQL args: $1=rollout_id
func execCompleteRolloutStatus(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 1 {
		return &fakeTag{0}, fmt.Errorf("CompleteRollout: expected 1 arg, got %d", len(args))
	}
	id := asStr(args[0])
	r, ok := p.rolloutsByID[id]
	if !ok {
		return &fakeTag{0}, nil
	}
	if r.Status != db.RolloutStatusPromoting && r.Status != db.RolloutStatusCanary {
		return &fakeTag{0}, nil
	}
	now := time.Now()
	r.Status = db.RolloutStatusCompleted
	r.CompletedAt = &now
	r.UpdatedAt = now
	return &fakeTag{1}, nil
}

// execBeginRollback handles BeginRollback.
// SQL args: $1=rollout_id, $2=reason
func execBeginRollback(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 2 {
		return &fakeTag{0}, fmt.Errorf("BeginRollback: expected 2 args, got %d", len(args))
	}
	id := asStr(args[0])
	r, ok := p.rolloutsByID[id]
	if !ok {
		return &fakeTag{0}, nil
	}
	if r.Status != db.RolloutStatusCanary && r.Status != db.RolloutStatusPromoting {
		return &fakeTag{0}, nil
	}
	reason := asStr(args[1])
	r.Status = db.RolloutStatusRollingBack
	r.FailureReason = &reason
	r.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// execCompleteRollbackStatus handles CompleteRollback step 1 (rollout status → rolled_back).
// SQL args: $1=rollout_id
func execCompleteRollbackStatus(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 1 {
		return &fakeTag{0}, fmt.Errorf("CompleteRollback: expected 1 arg, got %d", len(args))
	}
	id := asStr(args[0])
	r, ok := p.rolloutsByID[id]
	if !ok || r.Status != db.RolloutStatusRollingBack {
		return &fakeTag{0}, nil
	}
	now := time.Now()
	r.Status = db.RolloutStatusRolledBack
	r.CompletedAt = &now
	r.UpdatedAt = now
	return &fakeTag{1}, nil
}

// execRollbackFailImage handles CompleteRollback step 2 (mark image FAILED).
// SQL: UPDATE images SET status = 'FAILED' WHERE id = $1 AND status IN ('ACTIVE', 'PENDING_VALIDATION')
// args: $1=image_id
func execRollbackFailImage(p *memPool, args []any) (db.CommandTag, error) {
	if len(args) < 1 {
		return &fakeTag{0}, fmt.Errorf("execRollbackFailImage: expected 1 arg, got %d", len(args))
	}
	id := asStr(args[0])
	img, ok := p.images[id]
	if !ok {
		return &fakeTag{0}, nil
	}
	if img.Status != db.ImageStatusActive && img.Status != db.ImageStatusPendingValidation {
		// 0 rows affected is acceptable — image may already be in a terminal state.
		return &fakeTag{0}, nil
	}
	img.Status = db.ImageStatusFailed
	img.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// execCreateCVEWaiver handles CreateCVEWaiver.
// SQL args: $1=id, $2=cve_id, $3=image_family(*string), $4=granted_by, $5=reason, $6=expires_at(*time.Time)
func execCreateCVEWaiver(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 6 {
		return &fakeTag{0}, fmt.Errorf("CreateCVEWaiver: expected 6 args, got %d", len(args))
	}
	id := asStr(args[0])
	now := time.Now()
	w := &db.ImageCVEWaiverRow{
		ID:        id,
		CVEID:     asStr(args[1]),
		GrantedBy: asStr(args[3]),
		Reason:    asStr(args[4]),
		CreatedAt: now,
	}
	if args[2] != nil {
		if s, ok := args[2].(*string); ok && s != nil {
			w.ImageFamily = s
		} else if sv, ok2 := args[2].(string); ok2 && sv != "" {
			w.ImageFamily = &sv
		}
	}
	if args[5] != nil {
		if t, ok := args[5].(*time.Time); ok && t != nil {
			w.ExpiresAt = t
		}
	}
	p.cveWaivers[id] = w
	return &fakeTag{1}, nil
}

// execRevokeCVEWaiver handles RevokeCVEWaiver.
// SQL args: $1=waiver_id
func execRevokeCVEWaiver(p *rolloutPool, args []any) (db.CommandTag, error) {
	if len(args) < 1 {
		return &fakeTag{0}, fmt.Errorf("RevokeCVEWaiver: expected 1 arg, got %d", len(args))
	}
	id := asStr(args[0])
	w, ok := p.cveWaivers[id]
	if !ok || w.RevokedAt != nil {
		return &fakeTag{0}, nil // already revoked or not found — idempotent
	}
	now := time.Now()
	w.RevokedAt = &now
	return &fakeTag{1}, nil
}

// ── SQL shape detectors ───────────────────────────────────────────────────────

// isRollbackFailImageSQL detects the CompleteRollback step-2 UPDATE images shape.
// SQL: UPDATE images SET status = 'FAILED' WHERE id = $1 AND status IN ('ACTIVE', 'PENDING_VALIDATION')
// Distinguishes from FailValidatedImage (which only includes 'PENDING_VALIDATION' in WHERE).
func isRollbackFailImageSQL(sql string) bool {
	return strings.Contains(sql, "UPDATE images") &&
		strings.Contains(sql, "'FAILED'") &&
		strings.Contains(sql, "IN ('ACTIVE', 'PENDING_VALIDATION')")
}

// isCompleteRolloutFamilySQL is NOT needed — CompleteRollout step 2 reuses
// isUpdateFamilyAliasSQL which already matches that SQL shape exactly.

// ── Row scan types ────────────────────────────────────────────────────────────

// rolloutRow scans a single ImageRolloutRow.
// Column order matches GetRolloutByImageID SELECT list in image_rollout_repo.go:
//   id, image_id, job_id, family_name, status, canary_percent,
//   failure_reason, started_at, updated_at, completed_at
type rolloutRow struct{ r *db.ImageRolloutRow }

func (row *rolloutRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 10 {
		return fmt.Errorf("rolloutRow.Scan: need 10 dest, got %d", len(dest))
	}
	*dest[0].(*string)     = r.ID
	*dest[1].(*string)     = r.ImageID
	*dest[2].(*string)     = r.JobID
	*dest[3].(*string)     = r.FamilyName
	*dest[4].(*string)     = r.Status
	*dest[5].(*int)        = r.CanaryPercent
	*dest[6].(**string)    = r.FailureReason
	*dest[7].(*time.Time)  = r.StartedAt
	*dest[8].(*time.Time)  = r.UpdatedAt
	*dest[9].(**time.Time) = r.CompletedAt
	return nil
}

// ── rolloutTestSrv ────────────────────────────────────────────────────────────

// rolloutTestSrv is the HTTP test server backed by rolloutPool.
// Used by image_rollout_test.go for rollout/promotion/family-progression tests.
type rolloutTestSrv struct {
	ts  *httptest.Server
	mem *rolloutPool
}
