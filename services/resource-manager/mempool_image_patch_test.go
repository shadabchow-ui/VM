package main

// mempool_image_patch_test.go — VM-P2C-P1/P4: image row types and family
// resolution dispatch for the memPool test harness.
//
// imageRow  — db.Row implementation that scans one db.ImageRow.
// imageRows — db.Rows implementation that iterates []*db.ImageRow
//             (used by ListImagesByPrincipal in memPool.Query).
//
// familyQueryRowDispatch (VM-P2C-P4) — memPool.QueryRow branch for
//             ResolveFamilyLatest and ResolveFamilyByVersion
//             (SQL contains "family_name = $1").
//
// Column scan order must exactly match selectImageCols in image_repo.go:
//
//	 0  id                  *string
//	 1  name                *string
//	 2  os_family           *string
//	 3  os_version          *string
//	 4  architecture        *string
//	 5  owner_id            *string
//	 6  visibility          *string
//	 7  source_type         *string
//	 8  storage_url         *string
//	 9  min_disk_gb         *int
//	10  status              *string
//	11  validation_status   *string
//	12  deprecated_at       **time.Time
//	13  obsoleted_at        **time.Time
//	14  source_snapshot_id  **string
//	15  import_url          **string   (VM-P2C-P2)
//	16  family_name         **string   (VM-P2C-P2)
//	17  family_version      **int      (VM-P2C-P2)
//	18  created_at          *time.Time
//	19  updated_at          *time.Time
//
// Total: 20 columns.
// Source: image_repo.go selectImageCols, scanImage.

import (
	"fmt"
	"strings"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── imageRow ──────────────────────────────────────────────────────────────────

// imageRow implements db.Row for a single db.ImageRow.
// Used by QueryRow branches: GetImageByID / GetImageForAdmission,
// ResolveFamilyLatest, ResolveFamilyByVersion.
//
// Scan order must match selectImageCols in image_repo.go (20 columns).
type imageRow struct{ r *db.ImageRow }

func (row *imageRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 20 {
		return fmt.Errorf("imageRow.Scan: need 20 dest, got %d", len(dest))
	}
	*dest[0].(*string)     = r.ID
	*dest[1].(*string)     = r.Name
	*dest[2].(*string)     = r.OSFamily
	*dest[3].(*string)     = r.OSVersion
	*dest[4].(*string)     = r.Architecture
	*dest[5].(*string)     = r.OwnerID
	*dest[6].(*string)     = r.Visibility
	*dest[7].(*string)     = r.SourceType
	*dest[8].(*string)     = r.StorageURL
	*dest[9].(*int)        = r.MinDiskGB
	*dest[10].(*string)    = r.Status
	*dest[11].(*string)    = r.ValidationStatus
	*dest[12].(**time.Time) = r.DeprecatedAt
	*dest[13].(**time.Time) = r.ObsoletedAt
	*dest[14].(**string)   = r.SourceSnapshotID
	*dest[15].(**string)   = r.ImportURL
	*dest[16].(**string)   = r.FamilyName
	*dest[17].(**int)      = r.FamilyVersion
	*dest[18].(*time.Time) = r.CreatedAt
	*dest[19].(*time.Time) = r.UpdatedAt
	return nil
}

// ── imageRows ─────────────────────────────────────────────────────────────────

// imageRows implements db.Rows for []*db.ImageRow.
// Used by the Query branch handling ListImagesByPrincipal.
//
// Scan order must match selectImageCols in image_repo.go (20 columns).
type imageRows struct {
	rows []*db.ImageRow
	pos  int
}

func (r *imageRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *imageRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 20 {
		return fmt.Errorf("imageRows.Scan: need 20 dest, got %d", len(dest))
	}
	*dest[0].(*string)     = row.ID
	*dest[1].(*string)     = row.Name
	*dest[2].(*string)     = row.OSFamily
	*dest[3].(*string)     = row.OSVersion
	*dest[4].(*string)     = row.Architecture
	*dest[5].(*string)     = row.OwnerID
	*dest[6].(*string)     = row.Visibility
	*dest[7].(*string)     = row.SourceType
	*dest[8].(*string)     = row.StorageURL
	*dest[9].(*int)        = row.MinDiskGB
	*dest[10].(*string)    = row.Status
	*dest[11].(*string)    = row.ValidationStatus
	*dest[12].(**time.Time) = row.DeprecatedAt
	*dest[13].(**time.Time) = row.ObsoletedAt
	*dest[14].(**string)   = row.SourceSnapshotID
	*dest[15].(**string)   = row.ImportURL
	*dest[16].(**string)   = row.FamilyName
	*dest[17].(**int)      = row.FamilyVersion
	*dest[18].(*time.Time) = row.CreatedAt
	*dest[19].(*time.Time) = row.UpdatedAt
	return nil
}

func (r *imageRows) Close()     {}
func (r *imageRows) Err() error { return nil }

// ── familyQueryRowDispatch (VM-P2C-P4) ───────────────────────────────────────

// familyQueryRowDispatch handles QueryRow calls where the SQL contains
// "family_name = $1". Called from memPool.QueryRow; mirrors the selection
// semantics of ResolveFamilyLatest and ResolveFamilyByVersion in image_repo.go.
//
// Dispatch:
//
//	SQL contains "family_version = $2"
//	    → ResolveFamilyByVersion  args: (familyName string, version int, principalID string)
//	otherwise
//	    → ResolveFamilyLatest     args: (familyName string, principalID string)
//
// Visibility rules (both paths):
//   - PUBLIC images: accessible to any principal.
//   - PRIVATE images: only accessible to owning principal (owner_id == principalID).
//   - Source: AUTH_OWNERSHIP_MODEL_V1 §3.
//
// Status filter (both paths):
//   - ACTIVE and DEPRECATED are eligible (db.ImageIsLaunchable).
//   - OBSOLETE, FAILED, PENDING_VALIDATION are excluded.
//   - Source: vm-13-01__blueprint__ §core_contracts, P2_IMAGE_SNAPSHOT_MODEL §3.8.
//
// ResolveFamilyLatest ordering mirrors SQL:
//
//	ORDER BY family_version DESC NULLS LAST, created_at DESC
//
// Versioned images (FamilyVersion != nil) always rank above unversioned (nil).
// Among equal versions, newest CreatedAt wins.
//
// Returns &errRow{"no rows in result set"} when no candidate found —
// the isNoRowsErr signal that repo converts to (nil, nil).
//
// Source: image_repo.go ResolveFamilyLatest, ResolveFamilyByVersion,
//
//	vm-13-01__blueprint__ §family_seam,
//	AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account).
func (p *memPool) familyQueryRowDispatch(sql string, args []any) db.Row {
	if len(args) < 2 {
		return &errRow{fmt.Errorf(
			"familyQueryRowDispatch: need at least 2 args, got %d", len(args))}
	}

	familyName := asStr(args[0])

	// ── ResolveFamilyByVersion ────────────────────────────────────────────
	if strings.Contains(sql, "family_version = $2") {
		if len(args) < 3 {
			return &errRow{fmt.Errorf(
				"familyQueryRowDispatch: ResolveFamilyByVersion needs 3 args, got %d",
				len(args))}
		}
		version, ok := args[1].(int)
		if !ok {
			return &errRow{fmt.Errorf(
				"familyQueryRowDispatch: family_version arg is not int (got %T)", args[1])}
		}
		principalID := asStr(args[2])

		for _, img := range p.images {
			if img.FamilyName == nil || *img.FamilyName != familyName {
				continue
			}
			if img.FamilyVersion == nil || *img.FamilyVersion != version {
				continue
			}
			if img.Visibility == db.ImageVisibilityPrivate && img.OwnerID != principalID {
				continue
			}
			if !db.ImageIsLaunchable(img.Status) {
				continue
			}
			return &imageRow{r: img}
		}
		return &errRow{fmt.Errorf(
			"ResolveFamilyByVersion %q v%d: no rows in result set", familyName, version)}
	}

	// ── ResolveFamilyLatest ───────────────────────────────────────────────
	principalID := asStr(args[1])

	var best *db.ImageRow
	for _, img := range p.images {
		if img.FamilyName == nil || *img.FamilyName != familyName {
			continue
		}
		if img.Visibility == db.ImageVisibilityPrivate && img.OwnerID != principalID {
			continue
		}
		if !db.ImageIsLaunchable(img.Status) {
			continue
		}
		if best == nil {
			best = img
			continue
		}
		// Mirror ORDER BY family_version DESC NULLS LAST, created_at DESC.
		// Unversioned images use sentinel -1 so they sort below any versioned image.
		bestVer := -1
		if best.FamilyVersion != nil {
			bestVer = *best.FamilyVersion
		}
		imgVer := -1
		if img.FamilyVersion != nil {
			imgVer = *img.FamilyVersion
		}
		switch {
		case imgVer > bestVer:
			best = img
		case imgVer == bestVer && img.CreatedAt.After(best.CreatedAt):
			best = img
		}
	}
	if best == nil {
		return &errRow{fmt.Errorf(
			"ResolveFamilyLatest %q: no rows in result set", familyName)}
	}
	return &imageRow{r: best}
}
