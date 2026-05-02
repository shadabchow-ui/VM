package main

// vm_job10_cross_tenant_test.go — Cross-tenant image resource access matrix tests.
//
// VM Job 10: Verifies that private images are not accessible across tenant boundaries
// and that image ownership enforcement works correctly for list, get, mutate, launch,
// grant, and revoke operations.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §3 (404-for-cross-account),
//         VM Job 10 implementation mandate §10 (cross-tenant tests).

import (
	"net/http"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── Cross-tenant image list/get tests ─────────────────────────────────────────

// TestCrossTenant_GetPrivateImage_NonOwnerGets404 verifies that a non-owner
// receives 404 (not 403) when trying to GET a private image owned by another principal.
func TestCrossTenant_GetPrivateImage_NonOwnerGets404(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_alice_secret", "alice-secret", alice, db.ImageStatusActive))

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_alice_secret", nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-tenant GET, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

// TestCrossTenant_ListImages_NonOwnerExcludesPrivate verifies that a non-owner's
// image list does not include private images owned by other principals.
func TestCrossTenant_ListImages_NonOwnerExcludesPrivate(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_alice_priv_list", "alice-private-list", alice, db.ImageStatusActive))
	seedImage(s.mem, privateImage("img_bob_priv_list", "bob-private-list", bob, db.ImageStatusActive))

	// Bob lists images — should see his own + public, but NOT alice's private.
	resp := doReq(t, s.ts, http.MethodGet, "/v1/images", nil, authHdr(bob))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListImagesResponse
	decodeBody(t, resp, &out)

	foundAlice := false
	foundBob := false
	for _, img := range out.Images {
		if img.ID == "img_alice_priv_list" {
			foundAlice = true
		}
		if img.ID == "img_bob_priv_list" {
			foundBob = true
		}
	}
	if foundAlice {
		t.Error("bob must not see alice's private image in list")
	}
	if !foundBob {
		t.Error("bob must see his own private image in list")
	}
}

// TestCrossTenant_DeprecateImage_NonOwnerGets404 verifies that a non-owner
// receives 404 when attempting to deprecate another principal's private image.
func TestCrossTenant_DeprecateImage_NonOwnerGets404(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_alice_dep", "alice-dep", alice, db.ImageStatusActive))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_alice_dep/deprecate", nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-tenant deprecate, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

// TestCrossTenant_ObsoleteImage_NonOwnerGets404 verifies that a non-owner
// receives 404 when attempting to obsolete another principal's private image.
func TestCrossTenant_ObsoleteImage_NonOwnerGets404(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_alice_obs", "alice-obs", alice, db.ImageStatusActive))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_alice_obs/obsolete", nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-tenant obsolete, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageNotFound {
		t.Errorf("want code %q, got %q", errImageNotFound, env.Error.Code)
	}
}

// ── Cross-tenant launch admission tests ───────────────────────────────────────

// TestCrossTenant_LaunchFromPrivateImage_NonOwnerBlocked verifies that a non-owner
// cannot launch an instance from another principal's private image (422, not 404).
func TestCrossTenant_LaunchFromPrivateImage_NonOwnerBlocked(t *testing.T) {
	s := newTestSrv(t)
	seedImage(s.mem, privateImage("img_launch_block", "launch-block", alice, db.ImageStatusActive))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_launch_block"), authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for cross-tenant launch, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidImageID {
		t.Errorf("want code %q, got %q", errInvalidImageID, env.Error.Code)
	}
}

// TestCrossTenant_LaunchAfterShare_GranteeCanLaunch verifies that after the owner
// grants access, the grantee can launch from the shared private image.
func TestCrossTenant_LaunchAfterShare_GranteeCanLaunch(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_shared_launch", alice)
	seedShareGrant(s.mem, "img_shared_launch", alice, bob)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_shared_launch"), authHdr(bob))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for grantee launch, got %d", resp.StatusCode)
	}
}

// TestCrossTenant_LaunchAfterRevoke_Blocked verifies that after revocation,
// the former grantee can no longer launch.
func TestCrossTenant_LaunchAfterRevoke_Blocked(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_revoked_block", alice)
	// Seed then delete the grant — simulates post-revoke state.
	seedShareGrant(s.mem, "img_revoked_block", alice, bob)
	delete(s.mem.imageGrants, shareGrantKey("img_revoked_block", bob))

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_revoked_block"), authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 after revoke, got %d", resp.StatusCode)
	}
}

// ── Cross-tenant share grant tests ────────────────────────────────────────────

// TestCrossTenant_Grant_NonOwnerGets404 verifies that a non-owner gets 404
// when trying to grant access to another principal's private image.
func TestCrossTenant_Grant_NonOwnerGets404(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_alice_grant_xt", alice)

	body := GrantImageAccessRequest{GranteePrincipalID: "princ_charlie"}
	resp := doReq(t, s.ts, http.MethodPost, "/v1/images/img_alice_grant_xt/grants", body, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-tenant grant, got %d", resp.StatusCode)
	}
}

// TestCrossTenant_Revoke_NonOwnerGets404 verifies that a non-owner gets 404
// when trying to revoke a grant on another principal's private image.
func TestCrossTenant_Revoke_NonOwnerGets404(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_alice_revoke_xt", alice)
	seedShareGrant(s.mem, "img_alice_revoke_xt", alice, bob)

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/images/img_alice_revoke_xt/grants/"+bob, nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-tenant revoke, got %d", resp.StatusCode)
	}
}

// TestCrossTenant_ListGrants_NonOwnerGets404 verifies that a non-owner gets 404
// when trying to list grants on another principal's private image.
func TestCrossTenant_ListGrants_NonOwnerGets404(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_alice_listg_xt", alice)
	seedShareGrant(s.mem, "img_alice_listg_xt", alice, bob)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/images/img_alice_listg_xt/grants", nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for cross-tenant list grants, got %d", resp.StatusCode)
	}
}

// ── Cross-tenant family resolution tests ──────────────────────────────────────

// TestCrossTenant_FamilyResolution_PrivateNotResolvable verifies that a private
// family image is not resolvable by a non-owner.
func TestCrossTenant_FamilyResolution_PrivateNotResolvable(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img_fam_xt_priv", "xt-private-family", familyIntPtr(1),
		db.ImageStatusActive, alice)

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("xt-private-family", nil), authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 for cross-tenant family resolution, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errImageFamilyNotFound {
		t.Errorf("want %q, got %q", errImageFamilyNotFound, env.Error.Code)
	}
}

// TestCrossTenant_FamilyResolution_PublicResolvable verifies that a public
// family image is resolvable by any principal.
func TestCrossTenant_FamilyResolution_PublicResolvable(t *testing.T) {
	s := newTestSrv(t)
	seedFamilyImage(s.mem, "img_fam_xt_pub", "xt-public-family", familyIntPtr(1),
		db.ImageStatusActive, "system")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		familyLaunchBody("xt-public-family", nil), authHdr(bob))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for public family non-owner, got %d", resp.StatusCode)
	}
}

// ── Cross-tenant image mutation matrix ────────────────────────────────────────

// TestCrossTenant_MutateMatrix verifies that all mutating image operations
// are blocked for non-owners with 404 (not 403).
func TestCrossTenant_MutateMatrix(t *testing.T) {
	type testCase struct {
		name       string
		method     string
		path       string
		body       interface{}
		wantStatus int
		wantCode   string
	}

	bcases := []testCase{
		{
			name:       "non-owner GET private image",
			method:     http.MethodGet,
			path:       "/v1/images/img_mut_matrix",
			body:       nil,
			wantStatus: http.StatusNotFound,
			wantCode:   errImageNotFound,
		},
		{
			name:       "non-owner DEPRECATE private image",
			method:     http.MethodPost,
			path:       "/v1/images/img_mut_matrix/deprecate",
			body:       nil,
			wantStatus: http.StatusNotFound,
			wantCode:   errImageNotFound,
		},
		{
			name:       "non-owner OBSOLETE private image",
			method:     http.MethodPost,
			path:       "/v1/images/img_mut_matrix/obsolete",
			body:       nil,
			wantStatus: http.StatusNotFound,
			wantCode:   errImageNotFound,
		},
	}

	for _, tc := range bcases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSrv(t)
			seedImage(s.mem, privateImage("img_mut_matrix", "mut-matrix", alice, db.ImageStatusActive))

			resp := doReq(t, s.ts, tc.method, tc.path, tc.body, authHdr(bob))
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("want %d, got %d", tc.wantStatus, resp.StatusCode)
			}
			var env apiError
			decodeBody(t, resp, &env)
			if env.Error.Code != tc.wantCode {
				t.Errorf("want code %q, got %q", tc.wantCode, env.Error.Code)
			}
		})
	}
}

// ── Cross-tenant with grant matrix ────────────────────────────────────────────

// TestCrossTenant_GrantedAccessMatrix verifies the full flow:
// grant → launch → revoke → can't launch.
func TestCrossTenant_GrantedAccessMatrix(t *testing.T) {
	s := newShareTestSrv(t)
	seedPrivateImage(s.mem, "img_grant_flow", alice)

	// Pre-grant: bob cannot launch.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_grant_flow"), authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Error("pre-grant: bob must not launch")
	}

	// Alice grants access to bob.
	body := GrantImageAccessRequest{GranteePrincipalID: bob}
	resp = doReq(t, s.ts, http.MethodPost, "/v1/images/img_grant_flow/grants", body, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("grant: want 200, got %d", resp.StatusCode)
	}

	// Post-grant: bob CAN launch.
	resp = doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_grant_flow"), authHdr(bob))
	if resp.StatusCode != http.StatusAccepted {
		t.Error("post-grant: bob must be able to launch")
	}

	// Bob can GET the shared image.
	resp = doReq(t, s.ts, http.MethodGet, "/v1/images/img_grant_flow", nil, authHdr(bob))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("post-grant GET: want 200, got %d", resp.StatusCode)
	}

	// Alice revokes the grant.
	resp = doReq(t, s.ts, http.MethodDelete, "/v1/images/img_grant_flow/grants/"+bob, nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: want 200, got %d", resp.StatusCode)
	}

	// Post-revoke: bob cannot launch again.
	resp = doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_grant_flow"), authHdr(bob))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Error("post-revoke: bob must not launch")
	}

	// Post-revoke: bob cannot GET the image anymore.
	resp = doReq(t, s.ts, http.MethodGet, "/v1/images/img_grant_flow", nil, authHdr(bob))
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("post-revoke GET: want 404, got %d", resp.StatusCode)
	}
}

// ── Deprecation warning test ──────────────────────────────────────────────────

// TestDeprecationWarning_EmittedOnLaunch verifies that launching from a DEPRECATED
// image includes a warning in the CreateInstanceResponse.
func TestDeprecationWarning_EmittedOnLaunch(t *testing.T) {
	s := newTestSrv(t)
	now := time.Now()
	depAt := now.Add(-24 * time.Hour)
	seedImage(s.mem, &db.ImageRow{
		ID: "img_dep_warn", Name: "deprecated-warn",
		OSFamily: "ubuntu", OSVersion: "22.04", Architecture: "x86_64",
		OwnerID: "system", Visibility: db.ImageVisibilityPublic,
		SourceType: db.ImageSourceTypePlatform,
		StorageURL: "nfs://images/dep-warn.qcow2", MinDiskGB: 10,
		Status: db.ImageStatusDeprecated, ValidationStatus: "passed",
		DeprecatedAt: &depAt,
		CreatedAt:    now, UpdatedAt: now,
	})

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("img_dep_warn"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("deprecated launch: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if len(out.Warnings) == 0 {
		t.Error("want at least one warning for deprecated image launch, got none")
	}
	if len(out.Warnings) > 0 && out.Warnings[0] == "" {
		t.Error("warning must not be empty string")
	}
}

// TestDeprecationWarning_NotEmittedOnActiveLaunch verifies that launching from an
// ACTIVE image produces no warnings.
func TestDeprecationWarning_NotEmittedOnActiveLaunch(t *testing.T) {
	s := newTestSrv(t)
	// Default seeded image is ACTIVE.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		imageAdmissionBody("00000000-0000-0000-0000-000000000010"), authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("active launch: want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)
	if len(out.Warnings) > 0 {
		t.Errorf("want zero warnings for ACTIVE image launch, got %d: %v",
			len(out.Warnings), out.Warnings)
	}
}
