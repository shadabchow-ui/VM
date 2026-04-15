package main

// mempool_family_dispatch_test.go — VM-P2C-P3: memPool QueryRow dispatch for
// family alias resolution methods.
//
// ResolveFamilyLatest and ResolveFamilyByVersion both call pool.QueryRow with
// SQL containing "family_name = $1". The existing memPool.QueryRow dispatcher
// in instance_handlers_test.go does not cover this SQL shape, so it falls
// through to the default errRow. This file adds the missing dispatch cases.
//
// Dispatch strategy (matches SQL fragments in image_repo.go):
//   - ResolveFamilyLatest:   "family_name = $1" AND "ORDER BY family_version"
//                            args: familyName (string), callerPrincipalID (string)
//   - ResolveFamilyByVersion: "family_name = $1" AND "family_version = $2"
//                            args: familyName (string), version (int), callerPrincipalID (string)
//
// Selection rules implemented in-process (mirror image_repo.go exactly):
//   - Candidate must have matching family_name (exact string).
//   - Candidate must be visible: visibility=PUBLIC OR (visibility=PRIVATE AND owner_id=caller).
//   - Candidate status must be ACTIVE or DEPRECATED.
//   - ResolveFamilyLatest: pick highest family_version (nil last), then newest created_at.
//   - ResolveFamilyByVersion: pick exact family_version match.
//
// This file must NOT be imported or referenced outside of test builds.
// Source: 11-02-phase-1-test-strategy.md §unit test approach,
//         internal/db/image_repo.go ResolveFamilyLatest / ResolveFamilyByVersion.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/compute-platform/compute-platform/internal/db"
)

// familyQueryRow is the memPool Row implementation returned by family resolution
// dispatches. It wraps an *db.ImageRow and uses the same imageRow.Scan logic.
// Returns errRow when img is nil (no candidate found — "no rows in result set").
func familyQueryRow(img *db.ImageRow) db.Row {
	if img == nil {
		return &errRow{fmt.Errorf("no rows in result set")}
	}
	return &imageRow{r: img}
}

// resolveFamilyLatestFromMem implements the in-process equivalent of
// image_repo.go ResolveFamilyLatest for the memPool test harness.
//
// Selection rules (identical to SQL in image_repo.go):
//  1. family_name == familyName (exact, case-sensitive)
//  2. visibility == PUBLIC OR (visibility == PRIVATE AND owner_id == callerID)
//  3. status IN (ACTIVE, DEPRECATED)
//  4. Order: family_version DESC NULLS LAST, created_at DESC; pick first.
func resolveFamilyLatestFromMem(images map[string]*db.ImageRow, familyName, callerID string) *db.ImageRow {
	var candidates []*db.ImageRow
	for _, img := range images {
		if img.FamilyName == nil || *img.FamilyName != familyName {
			continue
		}
		// Visibility check.
		if img.Visibility == db.ImageVisibilityPrivate && img.OwnerID != callerID {
			continue
		}
		// Lifecycle check.
		if !db.ImageIsLaunchable(img.Status) {
			continue
		}
		candidates = append(candidates, img)
	}
	if len(candidates) == 0 {
		return nil
	}
	// Sort: family_version DESC NULLS LAST, then created_at DESC.
	sort.Slice(candidates, func(i, j int) bool {
		vi := candidates[i].FamilyVersion
		vj := candidates[j].FamilyVersion
		// Versioned images rank before unversioned.
		if vi == nil && vj != nil {
			return false // i after j
		}
		if vi != nil && vj == nil {
			return true // i before j
		}
		if vi != nil && vj != nil && *vi != *vj {
			return *vi > *vj // higher version first
		}
		// Tie-break by created_at DESC.
		return candidates[i].CreatedAt.After(candidates[j].CreatedAt)
	})
	return candidates[0]
}

// resolveFamilyByVersionFromMem implements the in-process equivalent of
// image_repo.go ResolveFamilyByVersion for the memPool test harness.
func resolveFamilyByVersionFromMem(images map[string]*db.ImageRow, familyName string, version int, callerID string) *db.ImageRow {
	for _, img := range images {
		if img.FamilyName == nil || *img.FamilyName != familyName {
			continue
		}
		if img.FamilyVersion == nil || *img.FamilyVersion != version {
			continue
		}
		// Visibility check.
		if img.Visibility == db.ImageVisibilityPrivate && img.OwnerID != callerID {
			continue
		}
		// Lifecycle check.
		if !db.ImageIsLaunchable(img.Status) {
			continue
		}
		return img
	}
	return nil
}

// familyQueryRowDispatch is called from memPool.QueryRow when the SQL matches
// a family resolution query. It distinguishes ResolveFamilyLatest from
// ResolveFamilyByVersion by inspecting whether the SQL contains "family_version = $2".
//
// SQL shapes (from image_repo.go):
//
//	ResolveFamilyLatest:
//	  WHERE family_name = $1
//	  AND (visibility = 'PUBLIC' OR (visibility = 'PRIVATE' AND owner_id = $2))
//	  AND status IN ('ACTIVE', 'DEPRECATED')
//	  ORDER BY family_version DESC NULLS LAST, created_at DESC
//	  LIMIT 1
//	  args: familyName, callerPrincipalID
//
//	ResolveFamilyByVersion:
//	  WHERE family_name = $1
//	  AND family_version = $2
//	  AND (visibility = 'PUBLIC' OR (visibility = 'PRIVATE' AND owner_id = $3))
//	  AND status IN ('ACTIVE', 'DEPRECATED')
//	  LIMIT 1
//	  args: familyName, version(int), callerPrincipalID
func (p *memPool) familyQueryRowDispatch(sql string, args []any) db.Row {
	if strings.Contains(sql, "family_version = $2") {
		// ResolveFamilyByVersion: args[0]=familyName, args[1]=version(int), args[2]=callerID
		if len(args) < 3 {
			return &errRow{fmt.Errorf("ResolveFamilyByVersion: expected 3 args, got %d", len(args))}
		}
		familyName := asStr(args[0])
		version, ok := args[1].(int)
		if !ok {
			return &errRow{fmt.Errorf("ResolveFamilyByVersion: args[1] not int: %T", args[1])}
		}
		callerID := asStr(args[2])
		return familyQueryRow(resolveFamilyByVersionFromMem(p.images, familyName, version, callerID))
	}
	// ResolveFamilyLatest: args[0]=familyName, args[1]=callerID
	if len(args) < 2 {
		return &errRow{fmt.Errorf("ResolveFamilyLatest: expected 2 args, got %d", len(args))}
	}
	familyName := asStr(args[0])
	callerID := asStr(args[1])
	return familyQueryRow(resolveFamilyLatestFromMem(p.images, familyName, callerID))
}
