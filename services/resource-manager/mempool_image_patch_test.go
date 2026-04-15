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
