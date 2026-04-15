package main

// mempool_image_patch_test.go — VM-P2C-P1: image Row/Rows scan types for the test harness.
//
// The memPool struct field (images), its initialization in newMemPool(),
// and all Query/QueryRow dispatcher cases for image operations live in
// instance_handlers_test.go. That is the shared seam and the single source of
// truth for the in-memory fake pool.
//
// This file provides only:
//   1. Scan types for image rows (imageRow, imageRows).
//   2. Compile-time interface guards.
//
// Column order for imageRow.Scan matches the SELECT in image_repo.go
// (GetImageByID, GetImageForAdmission, ListImagesByPrincipal):
//   id, name, os_family, os_version, architecture,
//   owner_id, visibility, source_type, storage_url, min_disk_gb,
//   status, validation_status, deprecated_at, obsoleted_at,
//   source_snapshot_id, created_at, updated_at  (17 columns)
//
// Source: 11-02-phase-1-test-strategy.md §unit test approach,
//         internal/db/image_repo.go.

import (
	"fmt"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── imageRow — scans a single ImageRow ───────────────────────────────────────

type imageRow struct{ r *db.ImageRow }

func (row *imageRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 17 {
		return fmt.Errorf("imageRow.Scan: need 17 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.Name
	*dest[2].(*string) = r.OSFamily
	*dest[3].(*string) = r.OSVersion
	*dest[4].(*string) = r.Architecture
	*dest[5].(*string) = r.OwnerID
	*dest[6].(*string) = r.Visibility
	*dest[7].(*string) = r.SourceType
	*dest[8].(*string) = r.StorageURL
	*dest[9].(*int) = r.MinDiskGB
	*dest[10].(*string) = r.Status
	*dest[11].(*string) = r.ValidationStatus
	*dest[12].(**time.Time) = r.DeprecatedAt
	*dest[13].(**time.Time) = r.ObsoletedAt
	*dest[14].(**string) = r.SourceSnapshotID
	*dest[15].(*time.Time) = r.CreatedAt
	*dest[16].(*time.Time) = r.UpdatedAt
	return nil
}

// ── imageRows — iterates a slice for ListImagesByPrincipal ───────────────────

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
	if len(dest) < 17 {
		return fmt.Errorf("imageRows.Scan: need 17 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.ID
	*dest[1].(*string) = row.Name
	*dest[2].(*string) = row.OSFamily
	*dest[3].(*string) = row.OSVersion
	*dest[4].(*string) = row.Architecture
	*dest[5].(*string) = row.OwnerID
	*dest[6].(*string) = row.Visibility
	*dest[7].(*string) = row.SourceType
	*dest[8].(*string) = row.StorageURL
	*dest[9].(*int) = row.MinDiskGB
	*dest[10].(*string) = row.Status
	*dest[11].(*string) = row.ValidationStatus
	*dest[12].(**time.Time) = row.DeprecatedAt
	*dest[13].(**time.Time) = row.ObsoletedAt
	*dest[14].(**string) = row.SourceSnapshotID
	*dest[15].(*time.Time) = row.CreatedAt
	*dest[16].(*time.Time) = row.UpdatedAt
	return nil
}

func (r *imageRows) Close()     {}
func (r *imageRows) Err() error { return nil }

// ── Compile-time interface guards ─────────────────────────────────────────────

var _ db.Row = (*imageRow)(nil)
var _ db.Rows = (*imageRows)(nil)
