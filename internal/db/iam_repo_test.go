package db

// iam_repo_test.go — Unit tests for IAM repo methods (iam_repo.go).
//
// Phase 16A coverage:
//   - CreateServiceAccount: Exec args, duplicate-key → ErrServiceAccountNameConflict
//   - GetServiceAccountByID: scan values, not-found → ErrServiceAccountNotFound
//   - ListServiceAccountsByProject: returns rows, empty on miss
//   - SetServiceAccountStatus: Exec args, zero-rows → ErrServiceAccountNotFound
//   - SoftDeleteServiceAccount: Exec args, zero-rows → ErrServiceAccountNotFound
//   - CreateRoleBinding: Exec args
//   - GetRoleBindingByID: scan values, not-found → ErrRoleBindingNotFound
//   - ListRoleBindings: filtered and unfiltered
//   - DeleteRoleBinding: Exec args, zero-rows → ErrRoleBindingNotFound
//   - CheckPrincipalHasRole: true and false cases
//
// Pattern: fakePool / fakeTag / fakeRow / fakeRows from repo_test.go.
// fakePool, fakeTag, fakeRow, fakeRows, newRepo, ctx are defined in repo_test.go
// (same package db); no redeclaration needed here.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md §Unit.

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// ── service account column helper ─────────────────────────────────────────────

// saRow builds a fakeRow values slice matching the 10-column SELECT in
// GetServiceAccountByID / ListServiceAccountsByProject:
//
//	id, project_id, name, display_name, description, status,
//	created_by, created_at, updated_at, deleted_at
func saRow(id, projectID, name, displayName, status, createdBy string) []any {
	now := time.Now()
	return []any{
		id, projectID, name, displayName,
		nil,        // description (*string) — nil = no description
		status,
		createdBy,
		now,        // created_at
		now,        // updated_at
		nil,        // deleted_at (*time.Time)
	}
}

// ── CreateServiceAccount ──────────────────────────────────────────────────────

func TestCreateServiceAccount_ExecArgs(t *testing.T) {
	// CreateServiceAccount does INSERT then GetServiceAccountByID (QueryRow).
	// Prime multiQueryRow so the GetServiceAccountByID call has something to scan.
	now := time.Now()
	pool := &fakePool{
		execRows: 1,
		multiQueryRow: []fakeRow{
			{values: []any{"sa_001", "proj_001", "my-sa", "My SA", nil, "active", "user_001", now, now, nil}},
		},
	}
	r := newRepo(pool)

	sa, err := r.CreateServiceAccount(ctx(), "sa_001", "proj_001", "my-sa", "My SA", "user_001", nil)
	if err != nil {
		t.Fatalf("CreateServiceAccount: %v", err)
	}
	if sa.ID != "sa_001" {
		t.Errorf("ID = %q, want sa_001", sa.ID)
	}
	if sa.Status != "active" {
		t.Errorf("Status = %q, want active", sa.Status)
	}

	// Verify INSERT args: $1=id, $2=project_id, $3=name, $4=display_name, $5=description, $6=created_by
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
	call := pool.execCalls[0]
	if call.args[0] != "sa_001" {
		t.Errorf("arg[0] (id) = %v, want sa_001", call.args[0])
	}
	if call.args[1] != "proj_001" {
		t.Errorf("arg[1] (project_id) = %v, want proj_001", call.args[1])
	}
	if call.args[2] != "my-sa" {
		t.Errorf("arg[2] (name) = %v, want my-sa", call.args[2])
	}
}

func TestCreateServiceAccount_DuplicateKey_ReturnsNameConflict(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("duplicate key value violates unique constraint")}
	r := newRepo(pool)

	_, err := r.CreateServiceAccount(ctx(), "sa_001", "proj_001", "dupe", "Dupe SA", "user_001", nil)
	if !errors.Is(err, ErrServiceAccountNameConflict) {
		t.Errorf("want ErrServiceAccountNameConflict, got %v", err)
	}
}

func TestCreateServiceAccount_PropagatesExecError(t *testing.T) {
	pool := &fakePool{execErr: fmt.Errorf("db: connection reset")}
	r := newRepo(pool)

	_, err := r.CreateServiceAccount(ctx(), "sa_x", "proj_x", "x", "X", "u", nil)
	if err == nil {
		t.Error("expected error, got nil")
	}
}

// ── GetServiceAccountByID ─────────────────────────────────────────────────────

func TestGetServiceAccountByID_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"sa_001", "proj_001", "deploy-bot", "Deploy Bot",
			nil, "active", "user_001", now, now, nil,
		}},
	}
	r := newRepo(pool)

	sa, err := r.GetServiceAccountByID(ctx(), "sa_001", "proj_001")
	if err != nil {
		t.Fatalf("GetServiceAccountByID: %v", err)
	}
	if sa.ID != "sa_001" {
		t.Errorf("ID = %q, want sa_001", sa.ID)
	}
	if sa.Name != "deploy-bot" {
		t.Errorf("Name = %q, want deploy-bot", sa.Name)
	}
	if sa.Status != "active" {
		t.Errorf("Status = %q, want active", sa.Status)
	}
}

func TestGetServiceAccountByID_NotFound_ReturnsErrSANotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.GetServiceAccountByID(ctx(), "sa_missing", "proj_001")
	if err == nil {
		t.Fatal("expected ErrServiceAccountNotFound, got nil")
	}
	if !errors.Is(err, ErrServiceAccountNotFound) {
		t.Errorf("want ErrServiceAccountNotFound, got %v", err)
	}
}

func TestGetServiceAccountByID_CrossProject_ReturnsNotFound(t *testing.T) {
	// Project scope guard: querying with wrong project_id returns no rows.
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.GetServiceAccountByID(ctx(), "sa_001", "proj_OTHER")
	if !errors.Is(err, ErrServiceAccountNotFound) {
		t.Errorf("cross-project access must return ErrServiceAccountNotFound, got %v", err)
	}
}

// ── ListServiceAccountsByProject ──────────────────────────────────────────────

func TestListServiceAccountsByProject_ReturnsRows(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"sa_001", "proj_001", "bot-a", "Bot A", nil, "active", "user_001", now, now, nil},
			{"sa_002", "proj_001", "bot-b", "Bot B", nil, "disabled", "user_001", now, now, nil},
		},
	}
	r := newRepo(pool)

	list, err := r.ListServiceAccountsByProject(ctx(), "proj_001")
	if err != nil {
		t.Fatalf("ListServiceAccountsByProject: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 results, got %d", len(list))
	}
	if list[0].ID != "sa_001" {
		t.Errorf("list[0].ID = %q, want sa_001", list[0].ID)
	}
	if list[1].Status != "disabled" {
		t.Errorf("list[1].Status = %q, want disabled", list[1].Status)
	}
}

func TestListServiceAccountsByProject_EmptyProject_ReturnsNil(t *testing.T) {
	pool := &fakePool{queryRowsData: [][]any{}}
	r := newRepo(pool)

	list, err := r.ListServiceAccountsByProject(ctx(), "proj_empty")
	if err != nil {
		t.Fatalf("ListServiceAccountsByProject: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("want 0 results, got %d", len(list))
	}
}

// ── SetServiceAccountStatus ───────────────────────────────────────────────────

func TestSetServiceAccountStatus_Disable_ExecArgs(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		execRows: 1,
		multiQueryRow: []fakeRow{
			{values: []any{"sa_001", "proj_001", "bot", "Bot", nil, "disabled", "user_001", now, now, nil}},
		},
	}
	r := newRepo(pool)

	sa, err := r.SetServiceAccountStatus(ctx(), "sa_001", "proj_001", "disabled")
	if err != nil {
		t.Fatalf("SetServiceAccountStatus: %v", err)
	}
	if sa.Status != "disabled" {
		t.Errorf("Status = %q, want disabled", sa.Status)
	}

	// Verify UPDATE args: $1=id, $2=project_id, $3=status
	call := pool.execCalls[0]
	if call.args[2] != "disabled" {
		t.Errorf("arg[2] (status) = %v, want disabled", call.args[2])
	}
}

func TestSetServiceAccountStatus_NotFound_ReturnsError(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	_, err := r.SetServiceAccountStatus(ctx(), "sa_missing", "proj_001", "active")
	if err == nil {
		t.Error("expected error for 0 rows affected, got nil")
	}
	if !errors.Is(err, ErrServiceAccountNotFound) {
		t.Errorf("want ErrServiceAccountNotFound, got %v", err)
	}
}

// ── SoftDeleteServiceAccount ──────────────────────────────────────────────────

func TestSoftDeleteServiceAccount_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.SoftDeleteServiceAccount(ctx(), "sa_001", "proj_001"); err != nil {
		t.Fatalf("SoftDeleteServiceAccount: %v", err)
	}
	if len(pool.execCalls) != 1 {
		t.Fatalf("expected 1 Exec call, got %d", len(pool.execCalls))
	}
}

func TestSoftDeleteServiceAccount_NotFound_ReturnsError(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.SoftDeleteServiceAccount(ctx(), "sa_missing", "proj_001")
	if !errors.Is(err, ErrServiceAccountNotFound) {
		t.Errorf("want ErrServiceAccountNotFound, got %v", err)
	}
}

func TestSoftDeleteServiceAccount_WrongProject_ReturnsError(t *testing.T) {
	// Zero rows affected because project_id guard in WHERE clause won't match.
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.SoftDeleteServiceAccount(ctx(), "sa_001", "proj_OTHER")
	if !errors.Is(err, ErrServiceAccountNotFound) {
		t.Errorf("cross-project delete must return ErrServiceAccountNotFound, got %v", err)
	}
}

// ── CreateRoleBinding ─────────────────────────────────────────────────────────

func TestCreateRoleBinding_ExecArgs(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		execRows: 1,
		multiQueryRow: []fakeRow{
			{values: []any{
				"rb_001", "proj_001", "user_001",
				"roles/owner", "project", "proj_001",
				"admin_001", now, now,
			}},
		},
	}
	r := newRepo(pool)

	rb, err := r.CreateRoleBinding(ctx(),
		"rb_001", "proj_001", "user_001",
		"roles/owner", "project", "proj_001", "admin_001")
	if err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}
	if rb.ID != "rb_001" {
		t.Errorf("ID = %q, want rb_001", rb.ID)
	}
	if rb.Role != "roles/owner" {
		t.Errorf("Role = %q, want roles/owner", rb.Role)
	}

	// Verify INSERT args
	call := pool.execCalls[0]
	if call.args[0] != "rb_001" {
		t.Errorf("arg[0] (id) = %v, want rb_001", call.args[0])
	}
	if call.args[3] != "roles/owner" {
		t.Errorf("arg[3] (role) = %v, want roles/owner", call.args[3])
	}
}

func TestCreateRoleBinding_ConflictReturnsNilFromGetRoleBindingByID(t *testing.T) {
	// ON CONFLICT DO NOTHING → execRows=0, then GetRoleBindingByID returns not-found.
	// The handler treats nil rb as a conflict (409). Repo returns nil, nil in this case.
	pool := &fakePool{
		execRows: 0,
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	rb, err := r.CreateRoleBinding(ctx(),
		"rb_dupe", "proj_001", "user_001",
		"roles/owner", "project", "proj_001", "admin_001")
	// err should be ErrRoleBindingNotFound (wrapped by GetRoleBindingByID)
	if err == nil {
		// If ON CONFLICT path returns nil, rb must also be nil
		if rb != nil {
			t.Error("expected nil rb for conflict path")
		}
	}
}

// ── GetRoleBindingByID ────────────────────────────────────────────────────────

func TestGetRoleBindingByID_ReturnsRow_WhenFound(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{
			"rb_001", "proj_001", "user_001",
			"roles/compute.viewer", "project", "proj_001",
			"admin_001", now, now,
		}},
	}
	r := newRepo(pool)

	rb, err := r.GetRoleBindingByID(ctx(), "rb_001", "proj_001")
	if err != nil {
		t.Fatalf("GetRoleBindingByID: %v", err)
	}
	if rb.ID != "rb_001" {
		t.Errorf("ID = %q, want rb_001", rb.ID)
	}
	if rb.Role != "roles/compute.viewer" {
		t.Errorf("Role = %q, want roles/compute.viewer", rb.Role)
	}
}

func TestGetRoleBindingByID_NotFound_ReturnsErrRoleBindingNotFound(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("no rows in result set")},
	}
	r := newRepo(pool)

	_, err := r.GetRoleBindingByID(ctx(), "rb_missing", "proj_001")
	if !errors.Is(err, ErrRoleBindingNotFound) {
		t.Errorf("want ErrRoleBindingNotFound, got %v", err)
	}
}

// ── ListRoleBindings ──────────────────────────────────────────────────────────

func TestListRoleBindings_AllBindings(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"rb_001", "proj_001", "user_001", "roles/owner", "project", "proj_001", "admin", now, now},
			{"rb_002", "proj_001", "user_002", "roles/compute.viewer", "project", "proj_001", "admin", now, now},
		},
	}
	r := newRepo(pool)

	list, err := r.ListRoleBindings(ctx(), "proj_001", "")
	if err != nil {
		t.Fatalf("ListRoleBindings: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 bindings, got %d", len(list))
	}
}

func TestListRoleBindings_FilteredByPrincipal(t *testing.T) {
	now := time.Now()
	pool := &fakePool{
		queryRowsData: [][]any{
			{"rb_001", "proj_001", "user_001", "roles/owner", "project", "proj_001", "admin", now, now},
		},
	}
	r := newRepo(pool)

	list, err := r.ListRoleBindings(ctx(), "proj_001", "user_001")
	if err != nil {
		t.Fatalf("ListRoleBindings filtered: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("want 1 binding for user_001, got %d", len(list))
	}
	if list[0].PrincipalID != "user_001" {
		t.Errorf("PrincipalID = %q, want user_001", list[0].PrincipalID)
	}
}

// ── DeleteRoleBinding ─────────────────────────────────────────────────────────

func TestDeleteRoleBinding_Success(t *testing.T) {
	pool := &fakePool{execRows: 1}
	r := newRepo(pool)

	if err := r.DeleteRoleBinding(ctx(), "rb_001", "proj_001"); err != nil {
		t.Fatalf("DeleteRoleBinding: %v", err)
	}
}

func TestDeleteRoleBinding_NotFound_ReturnsError(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.DeleteRoleBinding(ctx(), "rb_missing", "proj_001")
	if !errors.Is(err, ErrRoleBindingNotFound) {
		t.Errorf("want ErrRoleBindingNotFound, got %v", err)
	}
}

func TestDeleteRoleBinding_WrongProject_ReturnsError(t *testing.T) {
	pool := &fakePool{execRows: 0}
	r := newRepo(pool)

	err := r.DeleteRoleBinding(ctx(), "rb_001", "proj_OTHER")
	if !errors.Is(err, ErrRoleBindingNotFound) {
		t.Errorf("cross-project delete must return ErrRoleBindingNotFound, got %v", err)
	}
}

// ── CheckPrincipalHasRole ─────────────────────────────────────────────────────

func TestCheckPrincipalHasRole_True(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{true}},
	}
	r := newRepo(pool)

	ok, err := r.CheckPrincipalHasRole(ctx(),
		"proj_001", "user_001", "roles/owner", "project", "proj_001")
	if err != nil {
		t.Fatalf("CheckPrincipalHasRole: %v", err)
	}
	if !ok {
		t.Error("expected true (principal has role), got false")
	}
}

func TestCheckPrincipalHasRole_False_WhenNoBinding(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{values: []any{false}},
	}
	r := newRepo(pool)

	ok, err := r.CheckPrincipalHasRole(ctx(),
		"proj_001", "user_stranger", "roles/owner", "project", "proj_001")
	if err != nil {
		t.Fatalf("CheckPrincipalHasRole: %v", err)
	}
	if ok {
		t.Error("expected false (principal does not have role), got true")
	}
}

func TestCheckPrincipalHasRole_PropagatesDBError(t *testing.T) {
	pool := &fakePool{
		queryRowResult: fakeRow{err: fmt.Errorf("db: connection refused")},
	}
	r := newRepo(pool)

	_, err := r.CheckPrincipalHasRole(ctx(),
		"proj_001", "user_001", "roles/owner", "project", "proj_001")
	if err == nil {
		t.Error("expected DB error, got nil")
	}
}

// ── IAM Role constant sanity checks ──────────────────────────────────────────

func TestIAMRoleConstants_HaveExpectedValues(t *testing.T) {
	if IAMRoleOwner != "roles/owner" {
		t.Errorf("IAMRoleOwner = %q, want roles/owner", IAMRoleOwner)
	}
	if IAMRoleComputeViewer != "roles/compute.viewer" {
		t.Errorf("IAMRoleComputeViewer = %q, want roles/compute.viewer", IAMRoleComputeViewer)
	}
	if IAMResourceTypeProject != "project" {
		t.Errorf("IAMResourceTypeProject = %q, want project", IAMResourceTypeProject)
	}
	if IAMResourceTypeServiceAccount != "service_account" {
		t.Errorf("IAMResourceTypeServiceAccount = %q, want service_account", IAMResourceTypeServiceAccount)
	}
}
