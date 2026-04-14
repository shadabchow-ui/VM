package main

// mempool_snapshot_patch_test.go — VM-P2B-S2: snapshot Row/Rows scan types for the test harness.
//
// The memPool struct field (snapshots), its initialization in newMemPool(),
// and all Exec/Query/QueryRow dispatcher cases for snapshot operations live in
// instance_handlers_test.go. That is the shared seam and the single source of
// truth for the in-memory fake pool.
//
// This file provides only:
//   1. Scan types for snapshot rows (snapshotRow, snapshotRows).
//   2. Compile-time interface guards.
//
// Column order for snapshotRow.Scan matches GetSnapshotByID SELECT in snapshot_repo.go:
//   id, owner_principal_id, display_name, region,
//   source_volume_id, source_instance_id,
//   size_gb, status, progress_percent,
//   storage_path, storage_pool_id, encrypted,
//   version, locked_by, created_at, completed_at, updated_at, deleted_at  (18 columns)
//
// Source: 11-02-phase-1-test-strategy.md §unit test approach.

import (
	"fmt"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── snapshotRow — scans a single SnapshotRow ──────────────────────────────────

type snapshotRow struct{ r *db.SnapshotRow }

func (row *snapshotRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 18 {
		return fmt.Errorf("snapshotRow.Scan: need 18 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.OwnerPrincipalID
	*dest[2].(*string) = r.DisplayName
	*dest[3].(*string) = r.Region
	*dest[4].(**string) = r.SourceVolumeID
	*dest[5].(**string) = r.SourceInstanceID
	*dest[6].(*int) = r.SizeGB
	*dest[7].(*string) = r.Status
	*dest[8].(*int) = r.ProgressPercent
	*dest[9].(**string) = r.StoragePath
	*dest[10].(**string) = r.StoragePoolID
	*dest[11].(*bool) = r.Encrypted
	*dest[12].(*int) = r.Version
	*dest[13].(**string) = r.LockedBy
	*dest[14].(*time.Time) = r.CreatedAt
	*dest[15].(**time.Time) = r.CompletedAt
	*dest[16].(*time.Time) = r.UpdatedAt
	*dest[17].(**time.Time) = r.DeletedAt
	return nil
}

// ── snapshotRows — iterates a slice for ListSnapshotsByOwner ─────────────────

type snapshotRows struct {
	rows []*db.SnapshotRow
	pos  int
}

func (r *snapshotRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *snapshotRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 18 {
		return fmt.Errorf("snapshotRows.Scan: need 18 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.ID
	*dest[1].(*string) = row.OwnerPrincipalID
	*dest[2].(*string) = row.DisplayName
	*dest[3].(*string) = row.Region
	*dest[4].(**string) = row.SourceVolumeID
	*dest[5].(**string) = row.SourceInstanceID
	*dest[6].(*int) = row.SizeGB
	*dest[7].(*string) = row.Status
	*dest[8].(*int) = row.ProgressPercent
	*dest[9].(**string) = row.StoragePath
	*dest[10].(**string) = row.StoragePoolID
	*dest[11].(*bool) = row.Encrypted
	*dest[12].(*int) = row.Version
	*dest[13].(**string) = row.LockedBy
	*dest[14].(*time.Time) = row.CreatedAt
	*dest[15].(**time.Time) = row.CompletedAt
	*dest[16].(*time.Time) = row.UpdatedAt
	*dest[17].(**time.Time) = row.DeletedAt
	return nil
}

func (r *snapshotRows) Close()     {}
func (r *snapshotRows) Err() error { return nil }

// ── Compile-time interface guards ─────────────────────────────────────────────

var _ db.Row = (*snapshotRow)(nil)
var _ db.Rows = (*snapshotRows)(nil)
