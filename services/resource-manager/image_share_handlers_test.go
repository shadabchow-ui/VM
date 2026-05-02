package main

// image_share_handlers_test.go — VM-P3B Job 1: tests for image share grant endpoints.
//
// Coverage:
//
//   GRANT (POST /v1/images/{id}/grants):
//     - Owner can grant access to a grantee                        → 200
//     - Re-granting same grantee is idempotent                     → 200 (same grant returned)
//     - Non-owner cannot grant (get 404)                           → 404
//     - Cannot grant on PUBLIC image                               → 422
//     - Cannot grant self (owner granting themselves)              → 422
//     - Missing grantee_principal_id                               → 400
//     - Missing auth                                               → 401
//
//   REVOKE (DELETE /v1/images/{id}/grants/{grantee_id}):
//     - Owner can revoke an existing grant                         → 200 revoked=true
//     - Revoking non-existent grant is idempotent                  → 200 revoked=false
//     - Non-owner cannot revoke (gets 404)                         → 404
//     - Missing auth                                               → 401
//
//   LIST GRANTS (GET /v1/images/{id}/grants):
//     - Owner sees all grants                                      → 200 + grant list
//     - Empty grant list                                           → 200 + empty array
//     - Non-owner cannot list (gets 404)                           → 404
//     - Missing auth                                               → 401
//
//   GRANTEE VISIBILITY (GET /v1/images, GET /v1/images/{id}):
//     - Grantee sees shared image in list                          → 200 includes image
//     - Non-grantee does not see private image in list             → 200 excludes image
//     - Grantee can GET shared image by ID                         → 200
//     - Non-grantee gets 404 on private image by ID                → 404
//
//   GRANTEE LAUNCH (POST /v1/instances):
//     - Grantee can launch from shared private image               → 202
//     - Revoked grantee cannot launch after revoke                 → 422 invalid_image_id
//     - Non-grantee cannot launch private image (still 422)        → 422 invalid_image_id
//
//   RESPONSE SHAPE:
//     - owner_principal_id not leaked in grant response
//     - required fields present in grant response
//
// Test strategy: in-process httptest.Server backed by sharePool — a fake db.Pool
// that implements db.Pool, wraps *memPool, and adds image_share_grants storage.
// All helpers (doReq, decodeBody, authHdr, alice, bob, imageAdmissionBody, etc.)
// are defined in instance_handlers_test.go and image_handlers_test.go in the same
// package. This file adds only grant-specific pool, row types, and test functions.
//
// ASSUMPTION: imageRow and imageRows (returned by memPool.QueryRow / memPool.Query
// for images) are defined in an unpackaged test helper file that compiles in the
// same `package main` test build. They are not redefined here.
//
// Source: 11-02-phase-1-test-strategy.md §unit test approach, VM-P3B Job 1.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── sharePool ─────────────────────────────────────────────────────────────────
//
// sharePool wraps *memPool and extends it with image_share_grants support.
// It implements db.Pool so it can be passed to db.New() directly.

type sharePool struct {
	inner       *memPool
	imageGrants map[string]*db.ImageGrantRow // key: imageID+"|"+granteeID
}

func newSharePool() *sharePool {
	return &sharePool{
		inner:       newMemPool(),
		imageGrants: make(map[string]*db.ImageGrantRow),
	}
}

func shareGrantKey(imageID, granteeID string) string {
	return imageID + "|" + granteeID
}

func (p *sharePool) Close() {}

// Exec handles image_share_grants writes; delegates everything else to inner.
func (p *sharePool) Exec(ctx context.Context, sql string, args ...any) (db.CommandTag, error) {
	switch {
	// CreateImageGrant — INSERT INTO image_share_grants
	// $1=id, $2=image_id, $3=owner_principal_id, $4=grantee_principal_id
	case strings.Contains(sql, "INSERT INTO image_share_grants"):
		id := asStr(args[0])
		imageID := asStr(args[1])
		ownerID := asStr(args[2])
		granteeID := asStr(args[3])
		key := shareGrantKey(imageID, granteeID)
		if _, exists := p.imageGrants[key]; exists {
			return &fakeTag{0}, nil // ON CONFLICT DO NOTHING
		}
		p.imageGrants[key] = &db.ImageGrantRow{
			ID:                 id,
			ImageID:            imageID,
			OwnerPrincipalID:   ownerID,
			GranteePrincipalID: granteeID,
			CreatedAt:          time.Now(),
		}
		return &fakeTag{1}, nil

	// RevokeImageGrant — DELETE FROM image_share_grants WHERE image_id=$1 AND grantee_principal_id=$2
	case strings.Contains(sql, "DELETE FROM image_share_grants"):
		imageID := asStr(args[0])
		granteeID := asStr(args[1])
		key := shareGrantKey(imageID, granteeID)
		if _, exists := p.imageGrants[key]; exists {
			delete(p.imageGrants, key)
			return &fakeTag{1}, nil
		}
		return &fakeTag{0}, nil
	}
	return p.inner.Exec(ctx, sql, args...)
}

// ── Grant row scan types ──────────────────────────────────────────────────────

// imageGrantRow scans a single ImageGrantRow (5 columns).
// Column order matches image_grant_repo.go GetImageGrant SELECT list:
//
//	id, image_id, owner_principal_id, grantee_principal_id, created_at
type imageGrantRow struct{ r *db.ImageGrantRow }

func (row *imageGrantRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 5 {
		return fmt.Errorf("imageGrantRow.Scan: need 5 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.ImageID
	*dest[2].(*string) = r.OwnerPrincipalID
	*dest[3].(*string) = r.GranteePrincipalID
	*dest[4].(*time.Time) = r.CreatedAt
	return nil
}

// imageGrantRows iterates a slice of ImageGrantRow for ListImageGrants.
type imageGrantRows struct {
	rows []*db.ImageGrantRow
	pos  int
}

func (r *imageGrantRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *imageGrantRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 5 {
		return fmt.Errorf("imageGrantRows.Scan: need 5 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.ID
	*dest[1].(*string) = row.ImageID
	*dest[2].(*string) = row.OwnerPrincipalID
	*dest[3].(*string) = row.GranteePrincipalID
	*dest[4].(*time.Time) = row.CreatedAt
	return nil
}

func (r *imageGrantRows) Close()     {}
func (r *imageGrantRows) Err() error { return nil }

// Query handles image_share_grants list and grant-aware image list; delegates rest.
func (p *sharePool) Query(ctx context.Context, sql string, args ...any) (db.Rows, error) {
	switch {
	// ListImageGrants — SELECT ... FROM image_share_grants WHERE image_id = $1
	case strings.Contains(sql, "FROM image_share_grants") && strings.Contains(sql, "image_id = $1"):
		imageID := asStr(args[0])
		var out []*db.ImageGrantRow
		for _, g := range p.imageGrants {
			if g.ImageID == imageID {
				out = append(out, g)
			}
		}
		return &imageGrantRows{rows: out}, nil

	// ListImagesByPrincipalWithGrants — the query contains "grantee_principal_id"
	// in the subquery, distinguishing it from the plain ListImagesByPrincipal query.
	// Source: image_grant_repo.go ListImagesByPrincipalWithGrants.
	case strings.Contains(sql, "FROM images") && strings.Contains(sql, "grantee_principal_id"):
		principalID := asStr(args[0])
		var out []*db.ImageRow
		for _, img := range p.inner.images {
			if img.Visibility == "PUBLIC" {
				out = append(out, img)
				continue
			}
			if img.Visibility == "PRIVATE" && img.OwnerID == principalID {
				out = append(out, img)
				continue
			}
			if img.Visibility == "PRIVATE" {
				key := shareGrantKey(img.ID, principalID)
				if _, hasGrant := p.imageGrants[key]; hasGrant {
					out = append(out, img)
				}
			}
		}
		return &imageRows{rows: out}, nil
	}
	return p.inner.Query(ctx, sql, args...)
}

// QueryRow handles image_share_grants point lookups; delegates rest.
func (p *sharePool) QueryRow(ctx context.Context, sql string, args ...any) db.Row {
	// GetImageGrant — SELECT ... FROM image_share_grants WHERE image_id=$1 AND grantee_principal_id=$2
	if strings.Contains(sql, "FROM image_share_grants") {
		imageID := asStr(args[0])
		granteeID := asStr(args[1])
		key := shareGrantKey(imageID, granteeID)
		g, ok := p.imageGrants[key]
		if !ok {
			return &errRow{fmt.Errorf("GetImageGrant: no rows in result set")}
		}
		return &imageGrantRow{r: g}
	}
	return p.inner.QueryRow(ctx, sql, args...)
}

// ── Test server ───────────────────────────────────────────────────────────────

type shareTestSrv struct {
	ts  *httptest.Server
	mem *sharePool
}

func newShareTestSrv(t *testing.T) *shareTestSrv {
	t.Helper()
	pool := newSharePool()
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
	ts := startTestServer(t, mux)

	// Seed platform images so handleCreateInstance admission passes.
	now := time.Now()
	pool.inner.images["00000000-0000-0000-0000-000000000010"] = &db.ImageRow{
		ID: "00000000-0000-0000-0000-000000000010", Name: "ubuntu-22.04-lts",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: "PUBLIC", SourceType: "PLATFORM",
		StorageURL: "nfs://images/ubuntu-22.04.qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed", CreatedAt: now, UpdatedAt: now,
	}

	return &shareTestSrv{ts: ts, mem: pool}
}

// seedPrivateImage adds a PRIVATE ACTIVE image owned by ownerID.
func seedPrivateImage(pool *sharePool, id, ownerID string) {
	now := time.Now()
	pool.inner.images[id] = &db.ImageRow{
		ID: id, Name: "img-" + id,
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: ownerID, Visibility: "PRIVATE", SourceType: "USER",
		StorageURL: "nfs://images/" + id + ".qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed",
		CreatedAt: now, UpdatedAt: now,
	}
}

// seedShareGrant adds a grant row directly to the pool (bypasses handler).
func seedShareGrant(pool *sharePool, imageID, ownerID, granteeID string) {
	key := shareGrantKey(imageID, granteeID)
	pool.imageGrants[key] = &db.ImageGrantRow{
		ID:                 "igrant_" + imageID + "_" + granteeID,
		ImageID:            imageID,
		OwnerPrincipalID:   ownerID,
		GranteePrincipalID: granteeID,
		CreatedAt:          time.Now(),
	}
}

// ── GRANT tests ───────────────────────────────────────────────────────────────

func TestGrantImage_OwnerCanGrant(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_grant_01", alice)

	body := GrantImageAccessRequest{GranteePrincipalID: bob}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_grant_01/grants", body, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out GrantImageAccessResponse
	decodeBody(t, resp, &out)
	if out.Grant.GranteePrincipalID != bob {
		t.Errorf("want grantee=%q, got %q", bob, out.Grant.GranteePrincipalID)
	}
	if out.Grant.ImageID != "img_grant_01" {
		t.Errorf("want image_id=%q, got %q", "img_grant_01", out.Grant.ImageID)
	}
	if out.Grant.ID == "" {
		t.Error("grant id must be non-empty")
	}
}

func TestGrantImage_ReGrantIsIdempotent(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_grant_idem", alice)
	seedShareGrant(s.mem, "img_grant_idem", alice, bob)

	body := GrantImageAccessRequest{GranteePrincipalID: bob}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_grant_idem/grants", body, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for idempotent re-grant, got %d", resp.StatusCode)
	}
	var out GrantImageAccessResponse
	decodeBody(t, resp, &out)
	if out.Grant.GranteePrincipalID != bob {
		t.Errorf("want grantee=%q, got %q", bob, out.Grant.GranteePrincipalID)
	}
}

func TestGrantImage_NonOwnerCannotGrant(t *testing.T) {
	// Non-owner must get 404 (not 403). Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_grant_nonowner", alice)

	body := GrantImageAccessRequest{GranteePrincipalID: "princ_charlie"}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_grant_nonowner/grants", body, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for non-owner grant attempt, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

func TestGrantImage_PublicImageRejected(t *testing.T) {
	// Cannot add share grants to a PUBLIC image.
	s := newShareTestSrv(t)
	now := time.Now()
	s.mem.inner.images["img_public_alice"] = &db.ImageRow{
		ID: "img_public_alice", Name: "alice-public",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: alice, Visibility: "PUBLIC", SourceType: "USER",
		StorageURL: "nfs://images/alice-public.qcow2", MinDiskGB: 10,
		Status: "ACTIVE", ValidationStatus: "passed",
		CreatedAt: now, UpdatedAt: now,
	}

	body := GrantImageAccessRequest{GranteePrincipalID: bob}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_public_alice/grants", body, authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for grant on PUBLIC image, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageGrantPublicImage {
		t.Errorf("want code %q, got %q", errImageGrantPublicImage, env.Error.Code)
	}
}

func TestGrantImage_SelfGrantRejected(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_selfgrant", alice)

	body := GrantImageAccessRequest{GranteePrincipalID: alice}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_selfgrant/grants", body, authHdr(alice))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for self-grant, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageGrantSelfGrant {
		t.Errorf("want code %q, got %q", errImageGrantSelfGrant, env.Error.Code)
	}
}

func TestGrantImage_MissingGrantee(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_nograntee", alice)

	body := GrantImageAccessRequest{GranteePrincipalID: ""}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_nograntee/grants", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for missing grantee, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageGrantGranteeRequired {
		t.Errorf("want code %q, got %q", errImageGrantGranteeRequired, env.Error.Code)
	}
}

func TestGrantImage_MissingAuth(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_grant_noauth", alice)

	body := GrantImageAccessRequest{GranteePrincipalID: bob}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_grant_noauth/grants", body, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// ── REVOKE tests ──────────────────────────────────────────────────────────────

func TestRevokeImage_OwnerCanRevoke(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_revoke_01", alice)
	seedShareGrant(s.mem, "img_revoke_01", alice, bob)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/images/img_revoke_01/grants/"+bob, nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out RevokeImageAccessResponse
	decodeBody(t, resp, &out)
	if !out.Revoked {
		t.Error("want revoked=true")
	}
	if _, exists := s.mem.imageGrants[shareGrantKey("img_revoke_01", bob)]; exists {
		t.Error("grant must be removed from store after revoke")
	}
}

func TestRevokeImage_NonExistentGrantIsIdempotent(t *testing.T) {
	// Source: VM-P3B Job 1 §8.
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_revoke_noop", alice)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/images/img_revoke_noop/grants/"+bob, nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for idempotent revoke, got %d", resp.StatusCode)
	}
	var out RevokeImageAccessResponse
	decodeBody(t, resp, &out)
	if out.Revoked {
		t.Error("want revoked=false for non-existent grant")
	}
}

func TestRevokeImage_NonOwnerCannotRevoke(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_revoke_nonowner", alice)
	seedShareGrant(s.mem, "img_revoke_nonowner", alice, bob)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/images/img_revoke_nonowner/grants/"+bob, nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for non-owner revoke, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

func TestRevokeImage_MissingAuth(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_revoke_noauth", alice)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/images/img_revoke_noauth/grants/"+bob, nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// ── LIST GRANTS tests ─────────────────────────────────────────────────────────

func TestListImageGrants_OwnerSeesGrants(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_list_grants", alice)
	seedShareGrant(s.mem, "img_list_grants", alice, bob)
	seedShareGrant(s.mem, "img_list_grants", alice, "princ_charlie")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_list_grants/grants", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImageGrantsResponse
	decodeBody(t, resp, &out)
	if out.Total != 2 {
		t.Errorf("want total=2, got %d", out.Total)
	}
}

func TestListImageGrants_EmptyList(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_list_empty", alice)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_list_empty/grants", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImageGrantsResponse
	decodeBody(t, resp, &out)
	if out.Total != 0 {
		t.Errorf("want total=0, got %d", out.Total)
	}
	if out.Grants == nil {
		t.Error("grants field must be a non-nil empty slice, not null")
	}
}

func TestListImageGrants_NonOwnerCannotList(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_list_nonowner", alice)
	seedShareGrant(s.mem, "img_list_nonowner", alice, bob)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_list_nonowner/grants", nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for non-owner list, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

func TestListImageGrants_MissingAuth(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_list_noauth", alice)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_list_noauth/grants", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
}

// ── GRANTEE VISIBILITY tests ──────────────────────────────────────────────────

func TestListImages_GranteeSeesSharedImage(t *testing.T) {
	// Source: VM-P3B Job 1 §4.
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_visible_to_bob", alice)
	seedShareGrant(s.mem, "img_visible_to_bob", alice, bob)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, authHdr(bob))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImagesResponse
	decodeBody(t, resp, &out)

	found := false
	for _, img := range out.Images {
		if img.ID == "img_visible_to_bob" {
			found = true
		}
	}
	if !found {
		t.Error("grantee must see shared PRIVATE image in GET /v1/images")
	}
}

func TestListImages_NonGranteeDoesNotSeePrivateImage(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_hidden_from_bob", alice)
	// No grant for bob.

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, authHdr(bob))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImagesResponse
	decodeBody(t, resp, &out)

	for _, img := range out.Images {
		if img.ID == "img_hidden_from_bob" {
			t.Error("non-grantee must not see owner's PRIVATE image in list")
		}
	}
}

func TestGetImage_GranteeCanGetSharedImage(t *testing.T) {
	// Source: VM-P3B Job 1 §5.
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_get_by_bob", alice)
	seedShareGrant(s.mem, "img_get_by_bob", alice, bob)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_get_by_bob", nil, authHdr(bob))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for grantee GET /v1/images/{id}, got %d", resp.StatusCode)
	}
	var out ImageResponse
	decodeBody(t, resp, &out)
	if out.ID != "img_get_by_bob" {
		t.Errorf("want id=%q, got %q", "img_get_by_bob", out.ID)
	}
}

func TestGetImage_NonGranteeGets404OnPrivateImage(t *testing.T) {
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3, VM-P3B Job 1 §5.
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_404_for_bob", alice)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_404_for_bob", nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for non-grantee GET, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

// ── GRANTEE LAUNCH tests ──────────────────────────────────────────────────────

func TestCreateInstance_GranteeCanLaunchSharedPrivateImage(t *testing.T) {
	// Source: VM-P3B Job 1 §6, §7.
	// Instance must be owned by the grantee (bob), not the image owner (alice).
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_launch_shared", alice)
	seedShareGrant(s.mem, "img_launch_shared", alice, bob)

	body := imageAdmissionBody("img_launch_shared")
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(bob))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for grantee launch, got %d", resp.StatusCode)
	}

	var created *db.InstanceRow
	for _, inst := range s.mem.inner.instances {
		created = inst
		break
	}
	if created == nil {
		t.Fatal("no instance found in store after create")
	}
	if created.OwnerPrincipalID != bob {
		t.Errorf("instance must be owned by grantee %q, got %q", bob, created.OwnerPrincipalID)
	}
}

func TestCreateInstance_RevokedGranteeCannotLaunch(t *testing.T) {
	// Source: VM-P3B Job 1 §8 — revoke blocks future launches only.
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_revoked_launch", alice)
	seedShareGrant(s.mem, "img_revoked_launch", alice, bob)
	delete(s.mem.imageGrants, shareGrantKey("img_revoked_launch", bob)) // simulate revoke

	body := imageAdmissionBody("img_revoked_launch")
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 after revoke, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidImageID {
		t.Errorf("want code %q, got %q", errInvalidImageID, env.Error.Code)
	}
}

func TestCreateInstance_NonGranteeCannotLaunchPrivateImage(t *testing.T) {
	// Unchanged from pre-P3B behavior.
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_no_grant_launch", alice)

	body := imageAdmissionBody("img_no_grant_launch")
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for non-grantee launch attempt, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidImageID {
		t.Errorf("want code %q, got %q", errInvalidImageID, env.Error.Code)
	}
}

// ── Response shape ────────────────────────────────────────────────────────────

func TestGrantImageResponse_Shape(t *testing.T) {
	// owner_principal_id must not appear in grant response.
	// Required: id, image_id, grantee_principal_id, created_at.
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_resp_shape", alice)

	body := GrantImageAccessRequest{GranteePrincipalID: bob}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_resp_shape/grants", body, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	var raw map[string]any
	decodeBody(t, resp, &raw)
	grant, _ := raw["grant"].(map[string]any)
	if grant == nil {
		t.Fatal("grant field missing from response")
	}
	if _, ok := grant["owner_principal_id"]; ok {
		t.Error("owner_principal_id must not be present in grant response")
	}
	for _, field := range []string{"id", "image_id", "grantee_principal_id", "created_at"} {
		if _, ok := grant[field]; !ok {
			t.Errorf("required field %q missing from grant response", field)
		}
	}
}
