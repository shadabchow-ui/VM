package main

// mempool_volume_patch_test.go — VM-P2B: volume Row/Rows scan types for the test harness.
//
// The memPool struct fields (volumes, volumeAttachments), their initialization,
// and all Exec/Query/QueryRow dispatcher cases for volume operations live in
// instance_handlers_test.go. That is the shared seam and the single source of
// truth for the in-memory fake pool.
//
// This file provides only:
//   1. Scan types for volume rows (volumeRow, volumeRows).
//   2. Scan types for volume attachment rows (volumeAttachmentRow, attachmentRows).
//   3. Scan type for device-path list (devicePathRows) used by NextDevicePath.
//   4. Compile-time interface guards.
//
// intRow and stringValueRow are defined in instance_handlers_test.go.
//
// Source: 11-02-phase-1-test-strategy.md §unit test approach.

import (
	"fmt"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── volumeRow — scans a single VolumeRow ─────────────────────────────────────

// volumeRow scans a single VolumeRow for QueryRow calls.
// Column order matches GetVolumeByID SELECT in volume_repo.go:
//   id, owner_principal_id, display_name, region, availability_zone,
//   size_gb, origin, source_disk_id, source_snapshot_id,
//   status, storage_path, storage_pool_id,
//   version, locked_by, created_at, updated_at, deleted_at  (17 columns)
type volumeRow struct{ r *db.VolumeRow }

func (row *volumeRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 17 {
		return fmt.Errorf("volumeRow.Scan: need 17 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.OwnerPrincipalID
	*dest[2].(*string) = r.DisplayName
	*dest[3].(*string) = r.Region
	*dest[4].(*string) = r.AvailabilityZone
	*dest[5].(*int) = r.SizeGB
	*dest[6].(*string) = r.Origin
	*dest[7].(**string) = r.SourceDiskID
	*dest[8].(**string) = r.SourceSnapshotID
	*dest[9].(*string) = r.Status
	*dest[10].(**string) = r.StoragePath
	*dest[11].(**string) = r.StoragePoolID
	*dest[12].(*int) = r.Version
	*dest[13].(**string) = r.LockedBy
	*dest[14].(*time.Time) = r.CreatedAt
	*dest[15].(*time.Time) = r.UpdatedAt
	*dest[16].(**time.Time) = r.DeletedAt
	return nil
}

// ── volumeRows — iterates a slice for ListVolumesByOwner ─────────────────────

type volumeRows struct {
	rows []*db.VolumeRow
	pos  int
}

func (r *volumeRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *volumeRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 17 {
		return fmt.Errorf("volumeRows.Scan: need 17 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.ID
	*dest[1].(*string) = row.OwnerPrincipalID
	*dest[2].(*string) = row.DisplayName
	*dest[3].(*string) = row.Region
	*dest[4].(*string) = row.AvailabilityZone
	*dest[5].(*int) = row.SizeGB
	*dest[6].(*string) = row.Origin
	*dest[7].(**string) = row.SourceDiskID
	*dest[8].(**string) = row.SourceSnapshotID
	*dest[9].(*string) = row.Status
	*dest[10].(**string) = row.StoragePath
	*dest[11].(**string) = row.StoragePoolID
	*dest[12].(*int) = row.Version
	*dest[13].(**string) = row.LockedBy
	*dest[14].(*time.Time) = row.CreatedAt
	*dest[15].(*time.Time) = row.UpdatedAt
	*dest[16].(**time.Time) = row.DeletedAt
	return nil
}

func (r *volumeRows) Close() {}
func (r *volumeRows) Err() error { return nil }

// ── volumeAttachmentRow — scans a single VolumeAttachmentRow ─────────────────

// volumeAttachmentRow scans a single VolumeAttachmentRow for QueryRow calls.
// Column order matches GetActiveAttachmentByVolume SELECT in volume_repo.go:
//   id, volume_id, instance_id, device_path, delete_on_termination, attached_at, detached_at  (7 columns)
type volumeAttachmentRow struct{ r *db.VolumeAttachmentRow }

func (row *volumeAttachmentRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 7 {
		return fmt.Errorf("volumeAttachmentRow.Scan: need 7 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.VolumeID
	*dest[2].(*string) = r.InstanceID
	*dest[3].(*string) = r.DevicePath
	*dest[4].(*bool) = r.DeleteOnTermination
	*dest[5].(*time.Time) = r.AttachedAt
	*dest[6].(**time.Time) = r.DetachedAt
	return nil
}

// ── attachmentRows — iterates a slice for ListActiveAttachmentsByInstance ─────

type attachmentRows struct {
	rows []*db.VolumeAttachmentRow
	pos  int
}

func (r *attachmentRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *attachmentRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 7 {
		return fmt.Errorf("attachmentRows.Scan: need 7 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.ID
	*dest[1].(*string) = row.VolumeID
	*dest[2].(*string) = row.InstanceID
	*dest[3].(*string) = row.DevicePath
	*dest[4].(*bool) = row.DeleteOnTermination
	*dest[5].(*time.Time) = row.AttachedAt
	*dest[6].(**time.Time) = row.DetachedAt
	return nil
}

func (r *attachmentRows) Close() {}
func (r *attachmentRows) Err() error { return nil }

// ── devicePathRows — iterates device paths for NextDevicePath ─────────────────

type devicePathRows struct {
	paths []string
	pos   int
}

func (r *devicePathRows) Next() bool {
	if r.pos >= len(r.paths) {
		return false
	}
	r.pos++
	return true
}

func (r *devicePathRows) Scan(dest ...any) error {
	if len(dest) < 1 {
		return fmt.Errorf("devicePathRows.Scan: need 1 dest")
	}
	*dest[0].(*string) = r.paths[r.pos-1]
	return nil
}

func (r *devicePathRows) Close() {}
func (r *devicePathRows) Err() error { return nil }

// ── Compile-time interface guards ─────────────────────────────────────────────

var _ db.Row = (*volumeRow)(nil)
var _ db.Row = (*volumeAttachmentRow)(nil)

var _ db.Rows = (*volumeRows)(nil)
var _ db.Rows = (*attachmentRows)(nil)
var _ db.Rows = (*devicePathRows)(nil)
