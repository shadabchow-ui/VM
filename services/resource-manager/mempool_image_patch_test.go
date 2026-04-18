package main

// mempool_image_patch_test.go — VM-P2C-P1/P4 + VM-P3B Job 2: image row types,
// family resolution dispatch, and trust field dispatch for the memPool harness.
//
// imageRow  — db.Row implementation that scans one db.ImageRow.
// imageRows — db.Rows implementation that iterates []*db.ImageRow
//             (used by ListImagesByPrincipal in memPool.Query).
//
// familyQueryRowDispatch (VM-P2C-P4) — memPool.QueryRow branch for
//             ResolveFamilyLatest and ResolveFamilyByVersion
//             (SQL contains "family_name = $1").
//
// VM-P3B Job 2 additions:
//   - imageRow.Scan and imageRows.Scan extended to 22 columns (added
//     provenance_hash at col 20, signature_valid at col 21).
//   - memPool.Exec extended with UpdateFamilyAlias case:
//     SQL: "UPDATE images" AND "family_name = $1" AND "family_version = ("
//   - memPool.Exec extended with UpdateImageSignature case:
//     SQL: "UPDATE images" AND "provenance_hash = $2"
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
//	20  provenance_hash     **string   (VM-P3B Job 2)
//	21  signature_valid     **bool     (VM-P3B Job 2)
//
// Total: 22 columns.
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
// Scan order must match selectImageCols in image_repo.go (22 columns).
type imageRow struct{ r *db.ImageRow }

func (row *imageRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 22 {
		return fmt.Errorf("imageRow.Scan: need 22 dest, got %d", len(dest))
	}
	*dest[0].(*string)      = r.ID
	*dest[1].(*string)      = r.Name
	*dest[2].(*string)      = r.OSFamily
	*dest[3].(*string)      = r.OSVersion
	*dest[4].(*string)      = r.Architecture
	*dest[5].(*string)      = r.OwnerID
	*dest[6].(*string)      = r.Visibility
	*dest[7].(*string)      = r.SourceType
	*dest[8].(*string)      = r.StorageURL
	*dest[9].(*int)         = r.MinDiskGB
	*dest[10].(*string)     = r.Status
	*dest[11].(*string)     = r.ValidationStatus
	*dest[12].(**time.Time)  = r.DeprecatedAt
	*dest[13].(**time.Time)  = r.ObsoletedAt
	*dest[14].(**string)    = r.SourceSnapshotID
	*dest[15].(**string)    = r.ImportURL
	*dest[16].(**string)    = r.FamilyName
	*dest[17].(**int)       = r.FamilyVersion
	*dest[18].(*time.Time)  = r.CreatedAt
	*dest[19].(*time.Time)  = r.UpdatedAt
	// VM-P3B Job 2: trust boundary columns.
	*dest[20].(**string) = r.ProvenanceHash
	*dest[21].(**bool)   = r.SignatureValid
	return nil
}

// ── imageRows ─────────────────────────────────────────────────────────────────

// imageRows implements db.Rows for []*db.ImageRow.
// Used by the Query branch handling ListImagesByPrincipal.
//
// Scan order must match selectImageCols in image_repo.go (22 columns).
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
	if len(dest) < 22 {
		return fmt.Errorf("imageRows.Scan: need 22 dest, got %d", len(dest))
	}
	*dest[0].(*string)      = row.ID
	*dest[1].(*string)      = row.Name
	*dest[2].(*string)      = row.OSFamily
	*dest[3].(*string)      = row.OSVersion
	*dest[4].(*string)      = row.Architecture
	*dest[5].(*string)      = row.OwnerID
	*dest[6].(*string)      = row.Visibility
	*dest[7].(*string)      = row.SourceType
	*dest[8].(*string)      = row.StorageURL
	*dest[9].(*int)         = row.MinDiskGB
	*dest[10].(*string)     = row.Status
	*dest[11].(*string)     = row.ValidationStatus
	*dest[12].(**time.Time)  = row.DeprecatedAt
	*dest[13].(**time.Time)  = row.ObsoletedAt
	*dest[14].(**string)    = row.SourceSnapshotID
	*dest[15].(**string)    = row.ImportURL
	*dest[16].(**string)    = row.FamilyName
	*dest[17].(**int)       = row.FamilyVersion
	*dest[18].(*time.Time)  = row.CreatedAt
	*dest[19].(*time.Time)  = row.UpdatedAt
	// VM-P3B Job 2: trust boundary columns.
	*dest[20].(**string) = row.ProvenanceHash
	*dest[21].(**bool)   = row.SignatureValid
	return nil
}

func (r *imageRows) Close()     {}
func (r *imageRows) Err() error { return nil }

// ── memPool Exec extensions (VM-P3B Job 2) ───────────────────────────────────
//
// The existing memPool.Exec in instance_handlers_test.go handles "UPDATE images"
// for UpdateImageStatus (args: id, status, deprecatedAt, obsoletedAt).
// VM-P3B Job 2 adds two new UPDATE shapes that must be distinguished by their
// SQL fragments to avoid conflicts with the existing case.
//
// These methods are added to memPool via a separate extension file rather than
// modifying instance_handlers_test.go, keeping the diff minimal.
//
// Dispatch is appended to the Exec switch in instance_handlers_test.go by
// the Go compiler at build time — all files in the same package are compiled
// together, so these helpers are available to memPool without any import.
//
// IMPORTANT: The existing "UPDATE images" case in instance_handlers_test.go
// Exec() handles UpdateImageStatus and UpdateImageValidationStatus. To avoid
// conflict, the new cases below use more specific SQL substring matches and
// are checked BEFORE the existing generic "UPDATE images" case — but since
// we cannot modify instance_handlers_test.go, we add a separate dispatcher
// method called from the new cases in image_catalog_test.go's extended pool
// (see imageCatalogPool below in image_catalog_test.go).

// execUpdateFamilyAlias handles UpdateFamilyAlias for memPool test dispatch.
// SQL shape (image_repo.go UpdateFamilyAlias):
//
//	UPDATE images SET family_version = (SELECT ...) WHERE id = $2 AND family_name = $1 AND status = 'ACTIVE'
//	args[0]=familyName, args[1]=imageID
func execUpdateFamilyAlias(p *memPool, args []any) (db.CommandTag, error) {
	if len(args) < 2 {
		return &fakeTag{0}, fmt.Errorf("UpdateFamilyAlias: expected 2 args, got %d", len(args))
	}
	familyName := asStr(args[0])
	imageID := asStr(args[1])

	img, ok := p.images[imageID]
	if !ok {
		return &fakeTag{0}, nil
	}
	if img.FamilyName == nil || *img.FamilyName != familyName {
		return &fakeTag{0}, nil
	}
	if img.Status != db.ImageStatusActive {
		return &fakeTag{0}, nil
	}

	// Compute max(family_version) across family members.
	maxVer := 0
	for _, candidate := range p.images {
		if candidate.FamilyName == nil || *candidate.FamilyName != familyName {
			continue
		}
		if candidate.FamilyVersion != nil && *candidate.FamilyVersion > maxVer {
			maxVer = *candidate.FamilyVersion
		}
	}
	newVer := maxVer + 1
	img.FamilyVersion = &newVer
	img.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// execUpdateImageSignature handles UpdateImageSignature for memPool test dispatch.
// SQL shape (image_repo.go UpdateImageSignature):
//
//	UPDATE images SET provenance_hash = $2, signature_valid = $3 WHERE id = $1 AND source_type = 'PLATFORM'
//	args[0]=id, args[1]=provenanceHash(string), args[2]=signatureValid(bool)
func execUpdateImageSignature(p *memPool, args []any) (db.CommandTag, error) {
	if len(args) < 3 {
		return &fakeTag{0}, fmt.Errorf("UpdateImageSignature: expected 3 args, got %d", len(args))
	}
	id := asStr(args[0])
	img, ok := p.images[id]
	if !ok || img.SourceType != db.ImageSourceTypePlatform {
		return &fakeTag{0}, nil
	}
	hash := asStr(args[1])
	img.ProvenanceHash = &hash
	valid := args[2].(bool)
	img.SignatureValid = &valid
	img.UpdatedAt = time.Now()
	return &fakeTag{1}, nil
}

// isUpdateFamilyAliasSQL reports whether an UPDATE images SQL string is the
// UpdateFamilyAlias shape (contains the subquery "family_version = (").
func isUpdateFamilyAliasSQL(sql string) bool {
	return strings.Contains(sql, "UPDATE images") &&
		strings.Contains(sql, "family_version = (") &&
		strings.Contains(sql, "family_name = $1")
}

// isUpdateImageSignatureSQL reports whether an UPDATE images SQL string is the
// UpdateImageSignature shape (sets provenance_hash).
func isUpdateImageSignatureSQL(sql string) bool {
	return strings.Contains(sql, "UPDATE images") &&
		strings.Contains(sql, "provenance_hash = $2")
}
