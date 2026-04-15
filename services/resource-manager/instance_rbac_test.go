package main

// instance_rbac_test.go — VM-P2D Slice 2: project-aware RBAC tests.
//
// Test matrix (4 required tests per slice spec):
//   1. Direct account owner can GET and LIST their own instance (Phase 1 preserved).
//   2. Project member with EDITOR role can GET and LIST a project-owned instance.
//   3. Cross-account non-member gets 404 on GET (ownership hiding preserved).
//   4. LIST excludes instances the caller has no access to; VIEWER can read.
//
// All tests use the shared memPool + newTestSrv infrastructure from
// instance_handlers_test.go. memPool is extended with projectMembers in
// that file (VM-P2D Slice 2 additions).
//
// Source: P2_PROJECT_RBAC_MODEL.md §4.2 (permission matrix),
//         §4.3 (evaluation order),
//         §7 (error rules — 404 for cross-account, 403 for insufficient role),
//         AUTH_OWNERSHIP_MODEL_V1 §3 (ownership hiding).

import (
	"net/http"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// seedProjectMember adds an active membership row to memPool.
// Role must be one of "OWNER", "EDITOR", "VIEWER".
func seedProjectMember(mem *memPool, memberID, projectID, accountPrincipalID, role string) {
	mem.projectMembers[membershipKey(projectID, accountPrincipalID)] = &membershipEntry{
		id:                 memberID,
		projectID:          projectID,
		accountPrincipalID: accountPrincipalID,
		role:               role,
	}
}

// seedProjectWithPrincipal seeds a project record and its PROJECT principal.
// projectPrincipalID is the value stored in instances.owner_principal_id
// when an instance is assigned to this project.
func seedProjectWithPrincipal(mem *memPool, projectID, projectPrincipalID, createdBy string) {
	now := time.Now()
	mem.projects[projectID] = &db.ProjectRow{
		ID:          projectID,
		PrincipalID: projectPrincipalID,
		CreatedBy:   createdBy,
		Name:        "proj-" + projectID,
		DisplayName: "Project " + projectID,
		Status:      "active",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	mem.principals[projectPrincipalID] = "PROJECT"
}

// ── test 1: direct account owner ─────────────────────────────────────────────

// TestRBAC_DirectOwner_GetAndList verifies that Phase 1 direct-ownership
// behavior is fully preserved after the Phase 2 RBAC extension.
func TestRBAC_DirectOwner_GetAndList(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_owner_direct", "direct-own", alice, "running")

	// GET — must return 200.
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_owner_direct", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct owner GET: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// LIST — must include the instance.
	resp = doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("direct owner LIST: want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)
	if out.Total != 1 {
		t.Errorf("direct owner LIST: want total=1, got %d", out.Total)
	}
	if len(out.Instances) < 1 || out.Instances[0].ID != "inst_owner_direct" {
		t.Errorf("direct owner LIST: wrong instance returned")
	}
}

// ── test 2: project member with EDITOR role ───────────────────────────────────

// TestRBAC_ProjectMember_Editor_CanGetAndList verifies that a project EDITOR
// can read project-owned instances via both GET and LIST.
func TestRBAC_ProjectMember_Editor_CanGetAndList(t *testing.T) {
	s := newTestSrv(t)

	const carol = "princ_carol"
	const projID = "proj_rbac_editor"
	const projPrincipalID = "prin_proj_editor"

	// Carol owns the project; alice is an EDITOR member.
	seedProjectWithPrincipal(s.mem, projID, projPrincipalID, carol)
	seedProjectMember(s.mem, "mem_alice_editor", projID, alice, "EDITOR")

	// Instance owned by the project's principal (not alice directly).
	seedInstance(s.mem, "inst_proj_editor", "proj-inst-editor", projPrincipalID, "running")

	// GET by alice (EDITOR) — must succeed.
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_proj_editor", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("project EDITOR GET: want 200, got %d", resp.StatusCode)
	}
	var inst InstanceResponse
	decodeBody(t, resp, &inst)
	if inst.ID != "inst_proj_editor" {
		t.Errorf("project EDITOR GET: wrong instance ID %q", inst.ID)
	}

	// LIST by alice — project-owned instance must appear.
	resp = doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("project EDITOR LIST: want 200, got %d", resp.StatusCode)
	}
	var listOut ListInstancesResponse
	decodeBody(t, resp, &listOut)
	found := false
	for _, i := range listOut.Instances {
		if i.ID == "inst_proj_editor" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("project EDITOR LIST: inst_proj_editor must be visible (got %d instances)", listOut.Total)
	}
}

// ── test 3: cross-account non-member gets 404 ─────────────────────────────────

// TestRBAC_CrossAccount_NonMember_Gets404 verifies that ownership hiding is
// preserved: a caller with no project membership sees 404, not 403.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §3, P2_PROJECT_RBAC_MODEL.md §7.
func TestRBAC_CrossAccount_NonMember_Gets404(t *testing.T) {
	s := newTestSrv(t)

	const carol = "princ_carol"
	const projID = "proj_rbac_hidden"
	const projPrincipalID = "prin_proj_hidden"

	// Carol's project — alice is NOT a member.
	seedProjectWithPrincipal(s.mem, projID, projPrincipalID, carol)
	seedInstance(s.mem, "inst_hidden", "hidden-inst", projPrincipalID, "running")

	// Alice tries GET — must receive 404 (not 403, not 200).
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_hidden", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("non-member GET: want 404, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInstanceNotFound {
		t.Errorf("non-member GET: want code %q, got %q", errInstanceNotFound, env.Error.Code)
	}
}

// ── test 4: list scoping ──────────────────────────────────────────────────────

// TestRBAC_List_ProjectAwareScoping verifies that LIST:
//   - includes the caller's own instances
//   - includes instances from projects where the caller has VIEWER membership
//   - excludes instances from projects where the caller has no membership
//   - excludes instances directly owned by other accounts
func TestRBAC_List_ProjectAwareScoping(t *testing.T) {
	s := newTestSrv(t)

	// Alice's direct instance.
	seedInstance(s.mem, "inst_alice_own", "alice-own", alice, "running")

	// Bob's direct instance — alice has no membership.
	seedInstance(s.mem, "inst_bob_own", "bob-own", bob, "running")

	// Project A: alice is a VIEWER — read allowed.
	const projA = "proj_viewer_a"
	const projAPrincipal = "prin_proj_a"
	const carol = "princ_carol"
	seedProjectWithPrincipal(s.mem, projA, projAPrincipal, carol)
	seedProjectMember(s.mem, "mem_alice_viewer", projA, alice, "VIEWER")
	seedInstance(s.mem, "inst_proj_viewer", "proj-viewer-inst", projAPrincipal, "stopped")

	// Project B: alice has NO membership — must be invisible.
	const projB = "proj_no_access_b"
	const projBPrincipal = "prin_proj_b"
	seedProjectWithPrincipal(s.mem, projB, projBPrincipal, carol)
	seedInstance(s.mem, "inst_proj_noaccess", "proj-noaccess-inst", projBPrincipal, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("project-aware LIST: want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)

	ids := make(map[string]bool, len(out.Instances))
	for _, i := range out.Instances {
		ids[i.ID] = true
	}

	if !ids["inst_alice_own"] {
		t.Error("LIST must include alice's own instance")
	}
	if !ids["inst_proj_viewer"] {
		t.Error("LIST must include project instance alice has VIEWER membership for")
	}
	if ids["inst_bob_own"] {
		t.Error("LIST must NOT include bob's directly-owned instance")
	}
	if ids["inst_proj_noaccess"] {
		t.Error("LIST must NOT include instance from project alice has no membership in")
	}
}

// ── test 5: VIEWER cannot perform lifecycle actions ───────────────────────────

// TestRBAC_Viewer_CannotWrite verifies that a project VIEWER receives 403
// when attempting a lifecycle action (stop), not 404 or 200.
//
// This is the only 403 case in the RBAC contract.
// Source: P2_PROJECT_RBAC_MODEL.md §4.2, §7.
func TestRBAC_Viewer_CannotWrite(t *testing.T) {
	s := newTestSrv(t)

	const carol = "princ_carol"
	const projID = "proj_viewer_write"
	const projPrincipalID = "prin_proj_vw"

	seedProjectWithPrincipal(s.mem, projID, projPrincipalID, carol)
	seedProjectMember(s.mem, "mem_alice_viewer_w", projID, alice, "VIEWER")
	seedInstance(s.mem, "inst_viewer_write", "viewer-write-inst", projPrincipalID, "running")

	// VIEWER alice attempts stop — must get 403.
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_viewer_write/stop", nil, authHdr(alice))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("VIEWER stop: want 403, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errForbidden {
		t.Errorf("VIEWER stop: want code %q, got %q", errForbidden, env.Error.Code)
	}
}
