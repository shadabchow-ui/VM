package main

// instance_handlers_test.go — PASS 1 + PASS 2 tests for the public instance API.
//
// PASS 1 coverage (unchanged behaviour, headers fixed for auth middleware):
//   - POST /v1/instances: happy path, validation failures, error envelope shape
//   - GET  /v1/instances/{id}: happy path, not found
//   - GET  /v1/instances: list scoped to principal, empty list, response shape
//   - Error envelope: request_id always present, details never null
//   - Validation unit: name regexp
//
// PASS 2 coverage (new):
//   AUTH:
//     - missing X-Principal-ID → 401 authentication_required
//     - all endpoints enforce auth
//   OWNERSHIP:
//     - access own instance → 200
//     - access other user's instance → 404 (not 403)
//     - delete/stop/start/reboot on other user's instance → 404
//   LIFECYCLE:
//     - stop/start/reboot/delete happy paths → 202 + job_id
//     - illegal state transitions → 409 illegal_state_transition
//     - lifecycle on non-existent instance → 404
//
// Test strategy: in-process httptest.Server backed by memPool (fake db.Pool).
// No DB, no Linux/KVM, no network required.
// Source: 11-02-phase-1-test-strategy.md §unit test approach.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── in-memory Pool ────────────────────────────────────────────────────────────

// memPool is a fake db.Pool for handler tests.
// Covers: InsertInstance, GetInstanceByID, ListInstancesByOwner, InsertJob.
type memPool struct {
	instances map[string]*db.InstanceRow
	jobs      map[string]*db.JobRow
}

func newMemPool() *memPool {
	return &memPool{
		instances: make(map[string]*db.InstanceRow),
		jobs:      make(map[string]*db.JobRow),
	}
}

// seed adds an instance directly.
func (p *memPool) seed(row *db.InstanceRow) {
	now := time.Now()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = now
	}
	if row.VMState == "" {
		row.VMState = "requested"
	}
	p.instances[row.ID] = row
}

// Exec handles INSERT INTO instances and INSERT INTO jobs.
// InsertInstance arg order (instance_repo.go):
//   $1=id, $2=name, $3=owner_principal_id, $4=instance_type_id, $5=image_id, $6=availability_zone
// InsertJob arg order (job_repo.go):
//   $1=id, $2=instance_id, $3=job_type, $4=idempotency_key, $5=max_attempts
func (p *memPool) Exec(_ context.Context, sql string, args ...any) (db.CommandTag, error) {
	switch {
	case strings.Contains(sql, "INSERT INTO instances"):
		id := asStr(args[0])
		now := time.Now()
		p.instances[id] = &db.InstanceRow{
			ID:               id,
			Name:             asStr(args[1]),
			OwnerPrincipalID: asStr(args[2]),
			VMState:          "requested",
			InstanceTypeID:   asStr(args[3]),
			ImageID:          asStr(args[4]),
			AvailabilityZone: asStr(args[5]),
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		return &fakeTag{1}, nil

	case strings.Contains(sql, "INSERT INTO jobs"):
		// $1=id, $2=instance_id, $3=job_type, $4=idempotency_key, $5=max_attempts
		id := asStr(args[0])
		now := time.Now()
		p.jobs[id] = &db.JobRow{
			ID:         id,
			InstanceID: asStr(args[1]),
			JobType:    asStr(args[2]),
			Status:     "pending",
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		return &fakeTag{1}, nil
	}
	return &fakeTag{0}, nil
}

// Query handles ListInstancesByOwner.
func (p *memPool) Query(_ context.Context, sql string, args ...any) (db.Rows, error) {
	if strings.Contains(sql, "FROM instances") && strings.Contains(sql, "owner_principal_id") {
		ownerID := asStr(args[0])
		var out []*db.InstanceRow
		for _, r := range p.instances {
			if r.OwnerPrincipalID == ownerID && r.DeletedAt == nil {
				out = append(out, r)
			}
		}
		return &instRows{rows: out}, nil
	}
	return &instRows{}, nil
}

// QueryRow handles GetInstanceByID.
func (p *memPool) QueryRow(_ context.Context, sql string, args ...any) db.Row {
	if strings.Contains(sql, "FROM instances") && strings.Contains(sql, "id = $1") {
		id := asStr(args[0])
		r, ok := p.instances[id]
		if !ok || r.DeletedAt != nil {
			return &errRow{fmt.Errorf("GetInstanceByID %s: no rows in result set", id)}
		}
		return &instRow{r: r}
	}
	return &errRow{fmt.Errorf("no rows in result set")}
}

func (p *memPool) Close() {}

// ── Row types ─────────────────────────────────────────────────────────────────

type fakeTag struct{ n int64 }

func (t *fakeTag) RowsAffected() int64 { return t.n }

type errRow struct{ err error }

func (r *errRow) Scan(...any) error { return r.err }

// instRow scans a single InstanceRow.
// Column order matches GetInstanceByID SELECT in instance_repo.go:
// id, name, owner_principal_id, vm_state, instance_type_id, image_id,
// host_id, availability_zone, version, created_at, updated_at, deleted_at
type instRow struct{ r *db.InstanceRow }

func (row *instRow) Scan(dest ...any) error {
	r := row.r
	if len(dest) < 12 {
		return fmt.Errorf("instRow.Scan: need 12 dest, got %d", len(dest))
	}
	*dest[0].(*string) = r.ID
	*dest[1].(*string) = r.Name
	*dest[2].(*string) = r.OwnerPrincipalID
	*dest[3].(*string) = r.VMState
	*dest[4].(*string) = r.InstanceTypeID
	*dest[5].(*string) = r.ImageID
	*dest[6].(**string) = r.HostID
	*dest[7].(*string) = r.AvailabilityZone
	*dest[8].(*int) = r.Version
	*dest[9].(*time.Time) = r.CreatedAt
	*dest[10].(*time.Time) = r.UpdatedAt
	*dest[11].(**time.Time) = r.DeletedAt
	return nil
}

// instRows iterates a slice for ListInstancesByOwner.
// Column order matches ListInstancesByOwner SELECT in instance_repo.go.
type instRows struct {
	rows []*db.InstanceRow
	pos  int
}

func (r *instRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *instRows) Scan(dest ...any) error {
	row := r.rows[r.pos-1]
	if len(dest) < 12 {
		return fmt.Errorf("instRows.Scan: need 12 dest, got %d", len(dest))
	}
	*dest[0].(*string) = row.ID
	*dest[1].(*string) = row.Name
	*dest[2].(*string) = row.OwnerPrincipalID
	*dest[3].(*string) = row.VMState
	*dest[4].(*string) = row.InstanceTypeID
	*dest[5].(*string) = row.ImageID
	*dest[6].(**string) = row.HostID
	*dest[7].(*string) = row.AvailabilityZone
	*dest[8].(*int) = row.Version
	*dest[9].(*time.Time) = row.CreatedAt
	*dest[10].(*time.Time) = row.UpdatedAt
	*dest[11].(**time.Time) = row.DeletedAt
	return nil
}

func (r *instRows) Close() {}
func (r *instRows) Err() error { return nil }

// ── Test server ───────────────────────────────────────────────────────────────

type testSrv struct {
	ts  *httptest.Server
	mem *memPool
}

func newTestSrv(t *testing.T) *testSrv {
	t.Helper()
	mem := newMemPool()
	repo := db.New(mem)
	srv := &server{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
		region: "us-east-1",
	}
	mux := http.NewServeMux()
	srv.registerInstanceRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &testSrv{ts: ts, mem: mem}
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

// alice is the default test principal.
const alice = "princ_alice"
const bob = "princ_bob"

// authHdr returns a header map with X-Principal-ID set to principal.
func authHdr(principal string) map[string]string {
	return map[string]string{"X-Principal-ID": principal}
}

func doReq(t *testing.T, ts *httptest.Server, method, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	} else {
		r = strings.NewReader("")
	}
	req, err := http.NewRequest(method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func decodeBody(t *testing.T, resp *http.Response, out any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

func validCreateBody() CreateInstanceRequest {
	return CreateInstanceRequest{
		Name:             "my-instance",
		InstanceType:     "gp1.small",
		ImageID:          "img_ubuntu2204",
		AvailabilityZone: "us-east-1a",
		SSHKeyName:       "my-key",
	}
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

// seedInstance adds a ready-to-use instance owned by principal into memPool.
func seedInstance(mem *memPool, id, name, owner, vmState string) {
	mem.seed(&db.InstanceRow{
		ID:               id,
		Name:             name,
		OwnerPrincipalID: owner,
		VMState:          vmState,
		InstanceTypeID:   "gp1.small",
		ImageID:          "img_ubuntu2204",
		AvailabilityZone: "us-east-1a",
	})
}

// ── PASS 2: Auth tests ────────────────────────────────────────────────────────

// TestAuth_MissingHeader verifies 401 when X-Principal-ID is absent.
// Source: AUTH_OWNERSHIP_MODEL_V1 §1, API_ERROR_CONTRACT_V1 §4.
func TestAuth_MissingHeader(t *testing.T) {
	s := newTestSrv(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/instances"},
		{http.MethodPost, "/v1/instances"},
		{http.MethodGet, "/v1/instances/inst_any"},
		{http.MethodDelete, "/v1/instances/inst_any"},
		{http.MethodPost, "/v1/instances/inst_any/stop"},
		{http.MethodPost, "/v1/instances/inst_any/start"},
		{http.MethodPost, "/v1/instances/inst_any/reboot"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			resp := doReq(t, s.ts, ep.method, ep.path, nil, nil) // no header
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d", resp.StatusCode)
			}
			var env apiError
			decodeBody(t, resp, &env)
			if env.Error.Code != errAuthRequired {
				t.Errorf("want code %q, got %q", errAuthRequired, env.Error.Code)
			}
			if env.Error.RequestID == "" {
				t.Error("request_id must be present even on 401")
			}
		})
	}
}

// TestAuth_ValidHeader verifies that a valid X-Principal-ID is accepted.
// Source: AUTH_OWNERSHIP_MODEL_V1 §1.
func TestAuth_ValidHeader(t *testing.T) {
	s := newTestSrv(t)
	// GET /v1/instances with a valid header must not return 401.
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("valid X-Principal-ID must not produce 401")
	}
	resp.Body.Close()
}

// ── PASS 2: Ownership tests ───────────────────────────────────────────────────

// TestOwnership_OwnInstance verifies that a principal can access their own instance.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func TestOwnership_OwnInstance(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_mine", "mine", alice, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_mine", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200 for own instance, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestOwnership_OtherUsersInstance verifies 404 (not 403) when accessing another
// user's instance. Resource existence must not be leaked.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3, API_ERROR_CONTRACT_V1 §3.
func TestOwnership_OtherUsersInstance(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_bobs", "bobs-inst", bob, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_bobs", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 (not 403) for cross-account access, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInstanceNotFound {
		t.Errorf("want code %q, got %q", errInstanceNotFound, env.Error.Code)
	}
}

// TestOwnership_LifecycleOnOtherUsersInstance verifies 404 on lifecycle actions
// targeting another user's instance, across all action types.
// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
func TestOwnership_LifecycleOnOtherUsersInstance(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_bobs2", "bobs-inst2", bob, "running")

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodDelete, "/v1/instances/inst_bobs2"},
		{http.MethodPost, "/v1/instances/inst_bobs2/stop"},
		{http.MethodPost, "/v1/instances/inst_bobs2/start"},
		{http.MethodPost, "/v1/instances/inst_bobs2/reboot"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			resp := doReq(t, s.ts, ep.method, ep.path, nil, authHdr(alice))
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("want 404 for cross-account lifecycle, got %d", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

// ── PASS 2: Lifecycle happy path tests ───────────────────────────────────────

// TestStop_Happy verifies POST /stop returns 202 + job_id from running state.
// Source: LIFECYCLE_STATE_MACHINE_V1 §running→stopping, JOB_MODEL_V1.
func TestStop_Happy(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_run", "run-me", alice, "running")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_run/stop", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out LifecycleResponse
	decodeBody(t, resp, &out)
	if out.InstanceID != "inst_run" {
		t.Errorf("want instance_id=inst_run, got %q", out.InstanceID)
	}
	if !strings.HasPrefix(out.JobID, "job_") {
		t.Errorf("want job_id with job_ prefix, got %q", out.JobID)
	}
	if out.Action != "stop" {
		t.Errorf("want action=stop, got %q", out.Action)
	}
	// Verify job was recorded in the pool.
	if _, ok := s.mem.jobs[out.JobID]; !ok {
		t.Error("job must be recorded in the job store")
	}
}

// TestStart_Happy verifies POST /start returns 202 + job_id from stopped state.
// Source: LIFECYCLE_STATE_MACHINE_V1 §stopped→starting.
func TestStart_Happy(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_stopped", "stopped-inst", alice, "stopped")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_stopped/start", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out LifecycleResponse
	decodeBody(t, resp, &out)
	if out.Action != "start" {
		t.Errorf("want action=start, got %q", out.Action)
	}
	if !strings.HasPrefix(out.JobID, "job_") {
		t.Errorf("want job_ prefix, got %q", out.JobID)
	}
}

// TestReboot_Happy verifies POST /reboot returns 202 + job_id from running state.
// Source: LIFECYCLE_STATE_MACHINE_V1 §running→rebooting.
func TestReboot_Happy(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_reboot", "reboot-me", alice, "running")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_reboot/reboot", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out LifecycleResponse
	decodeBody(t, resp, &out)
	if out.Action != "reboot" {
		t.Errorf("want action=reboot, got %q", out.Action)
	}
}

// TestDelete_Happy verifies DELETE returns 202 + job_id from running state.
// Source: LIFECYCLE_STATE_MACHINE_V1 §running→deleting.
func TestDelete_Happy(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_del", "del-me", alice, "running")

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/instances/inst_del", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out LifecycleResponse
	decodeBody(t, resp, &out)
	if out.InstanceID != "inst_del" {
		t.Errorf("want instance_id=inst_del, got %q", out.InstanceID)
	}
	if out.Action != "delete" {
		t.Errorf("want action=delete, got %q", out.Action)
	}
}

// TestDelete_FromFailed verifies DELETE is accepted from failed state.
// Source: LIFECYCLE_STATE_MACHINE_V1 §failed→deleting.
func TestDelete_FromFailed(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_failed", "failed-inst", alice, "failed")

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/instances/inst_failed", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 from failed state, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestDelete_FromStopped verifies DELETE is accepted from stopped state.
// Source: LIFECYCLE_STATE_MACHINE_V1 §stopped→deleting.
func TestDelete_FromStopped(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_stopsed", "stopped-del", alice, "stopped")

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/instances/inst_stopsed", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 from stopped state, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ── PASS 2: Illegal state transition tests ───────────────────────────────────

// TestIllegalTransition_StopOnStopped verifies 409 when stopping an already-stopped instance.
// Source: LIFECYCLE_STATE_MACHINE_V1, API_ERROR_CONTRACT_V1 §4.
func TestIllegalTransition_StopOnStopped(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_stopped2", "already-stopped", alice, "stopped")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_stopped2/stop", nil, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errIllegalTransition {
		t.Errorf("want code %q, got %q", errIllegalTransition, env.Error.Code)
	}
	if env.Error.RequestID == "" {
		t.Error("request_id must be present on 409")
	}
}

// TestIllegalTransition_StartOnRunning verifies 409 when starting an already-running instance.
func TestIllegalTransition_StartOnRunning(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_running2", "already-running", alice, "running")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_running2/start", nil, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errIllegalTransition {
		t.Errorf("want code %q, got %q", errIllegalTransition, env.Error.Code)
	}
}

// TestIllegalTransition_RebootOnStopped verifies 409 when rebooting a stopped instance.
func TestIllegalTransition_RebootOnStopped(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_stopreboot", "stopped-reboot", alice, "stopped")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_stopreboot/reboot", nil, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errIllegalTransition {
		t.Errorf("want code %q, got %q", errIllegalTransition, env.Error.Code)
	}
}

// TestIllegalTransition_DeleteOnDeleting verifies 409 when deleting an already-deleting instance.
func TestIllegalTransition_DeleteOnDeleting(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_deleting", "being-deleted", alice, "deleting")

	resp := doReq(t, s.ts, http.MethodDelete, "/v1/instances/inst_deleting", nil, authHdr(alice))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errIllegalTransition {
		t.Errorf("want code %q, got %q", errIllegalTransition, env.Error.Code)
	}
}

// TestIllegalTransition_Matrix exercises the full set of illegal transitions
// from states where the action is not permitted.
// Source: LIFECYCLE_STATE_MACHINE_V1 §transition table.
func TestIllegalTransition_Matrix(t *testing.T) {
	cases := []struct {
		state  string
		method string
		action string
	}{
		// stop is only legal from running
		{"stopped", http.MethodPost, "stop"},
		{"requested", http.MethodPost, "stop"},
		{"provisioning", http.MethodPost, "stop"},
		{"deleting", http.MethodPost, "stop"},
		{"failed", http.MethodPost, "stop"},
		// start is only legal from stopped
		{"running", http.MethodPost, "start"},
		{"requested", http.MethodPost, "start"},
		{"provisioning", http.MethodPost, "start"},
		{"deleting", http.MethodPost, "start"},
		{"failed", http.MethodPost, "start"},
		// reboot is only legal from running
		{"stopped", http.MethodPost, "reboot"},
		{"requested", http.MethodPost, "reboot"},
		{"failed", http.MethodPost, "reboot"},
		// delete is illegal from deleting/deleted
		{"deleting", http.MethodDelete, ""},
	}

	s := newTestSrv(t)
	for i, tc := range cases {
		id := fmt.Sprintf("inst_matrix%d", i)
		seedInstance(s.mem, id, fmt.Sprintf("mat-%d", i), alice, tc.state)

		path := "/v1/instances/" + id
		if tc.action != "" {
			path += "/" + tc.action
		}

		resp := doReq(t, s.ts, tc.method, path, nil, authHdr(alice))
		name := fmt.Sprintf("state=%s action=%s", tc.state, tc.action)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("[%s] want 409, got %d", name, resp.StatusCode)
			resp.Body.Close()
			continue
		}
		var env apiError
		decodeBody(t, resp, &env)
		if env.Error.Code != errIllegalTransition {
			t.Errorf("[%s] want code %q, got %q", name, errIllegalTransition, env.Error.Code)
		}
	}
}

// TestLifecycle_NotFound verifies 404 when acting on a non-existent instance.
func TestLifecycle_NotFound(t *testing.T) {
	s := newTestSrv(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{http.MethodDelete, "/v1/instances/inst_ghost"},
		{http.MethodPost, "/v1/instances/inst_ghost/stop"},
		{http.MethodPost, "/v1/instances/inst_ghost/start"},
		{http.MethodPost, "/v1/instances/inst_ghost/reboot"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			resp := doReq(t, s.ts, ep.method, ep.path, nil, authHdr(alice))
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("want 404 for non-existent instance, got %d", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

// TestLifecycle_JobEnqueued verifies that a successful lifecycle action actually
// records a job in the job store (API enqueues, does not call runtime directly).
// Source: JOB_MODEL_V1, core-architecture-blueprint §control-plane semantics.
func TestLifecycle_JobEnqueued(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_enq", "enqueue-test", alice, "running")

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_enq/stop", nil, authHdr(alice))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out LifecycleResponse
	decodeBody(t, resp, &out)

	if len(s.mem.jobs) != 1 {
		t.Errorf("want exactly 1 job in store, got %d", len(s.mem.jobs))
	}
	job, ok := s.mem.jobs[out.JobID]
	if !ok {
		t.Fatalf("job %q not found in store", out.JobID)
	}
	if job.JobType != jobTypeStop {
		t.Errorf("want job_type=%q, got %q", jobTypeStop, job.JobType)
	}
	if job.InstanceID != "inst_enq" {
		t.Errorf("want instance_id=inst_enq, got %q", job.InstanceID)
	}
}

// ── PASS 1: POST /v1/instances ────────────────────────────────────────────────

// TestCreate_Happy verifies 202, response shape, and field values.
// Source: INSTANCE_MODEL_V1 §2, 08-01 §CreateInstance response.
func TestCreate_Happy(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		validCreateBody(), authHdr(alice))

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	var out CreateInstanceResponse
	decodeBody(t, resp, &out)

	if !strings.HasPrefix(out.Instance.ID, "inst_") {
		t.Errorf("instance ID must have inst_ prefix, got %q", out.Instance.ID)
	}
	if out.Instance.Status != "requested" {
		t.Errorf("want status=requested, got %q", out.Instance.Status)
	}
	if out.Instance.InstanceType != "gp1.small" {
		t.Errorf("want instance_type=gp1.small, got %q", out.Instance.InstanceType)
	}
	if out.Instance.Region != "us-east-1" {
		t.Errorf("want region=us-east-1, got %q", out.Instance.Region)
	}
	if out.Instance.CreatedAt.IsZero() {
		t.Error("want non-zero created_at")
	}
	if out.Instance.Labels == nil {
		t.Error("want labels to be non-nil map (even if empty)")
	}
}

// TestCreate_MalformedJSON verifies 400 invalid_request on a bad body.
// Source: API_ERROR_CONTRACT_V1 §4.
func TestCreate_MalformedJSON(t *testing.T) {
	s := newTestSrv(t)
	req, _ := http.NewRequest(http.MethodPost, s.ts.URL+"/v1/instances",
		strings.NewReader("{not valid json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Principal-ID", alice)
	resp, err := s.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidRequest {
		t.Errorf("want code %q, got %q", errInvalidRequest, env.Error.Code)
	}
	if env.Error.RequestID == "" {
		t.Error("request_id must always be present per API_ERROR_CONTRACT_V1 §7")
	}
}

// TestCreate_AllFieldsMissing verifies 400 with populated details array.
// Source: API_ERROR_CONTRACT_V1 §1 (details array).
func TestCreate_AllFieldsMissing(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		CreateInstanceRequest{}, authHdr(alice))

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInvalidRequest {
		t.Errorf("top-level code: want %q, got %q", errInvalidRequest, env.Error.Code)
	}
	if len(env.Error.Details) == 0 {
		t.Fatal("want non-empty details array for multi-field failure")
	}
	targets := make(map[string]bool)
	for _, d := range env.Error.Details {
		targets[d.Target] = true
	}
	for _, want := range []string{"name", "instance_type", "image_id", "availability_zone", "ssh_key_name"} {
		if !targets[want] {
			t.Errorf("want validation error targeting %q in details", want)
		}
	}
}

func TestCreate_MissingName(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.Name = ""
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "name", errMissingField)
}

func TestCreate_InvalidName(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.Name = "UPPERCASE"
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "name", errInvalidName)
}

func TestCreate_InvalidInstanceType(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.InstanceType = "gp99.enormous"
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "instance_type", errInvalidInstanceType)
}

func TestCreate_InvalidImageID(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.ImageID = "img_unknown"
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "image_id", errInvalidImageID)
}

func TestCreate_InvalidAZ(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.AvailabilityZone = "us-west-9z"
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "availability_zone", errInvalidAZ)
}

func TestCreate_MissingSSHKey(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.SSHKeyName = ""
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, authHdr(alice))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "ssh_key_name", errMissingField)
}

// ── PASS 1: GET /v1/instances/{id} ───────────────────────────────────────────

func TestGetInstance_Happy(t *testing.T) {
	s := newTestSrv(t)
	s.mem.seed(&db.InstanceRow{
		ID: "inst_abc123", Name: "test-inst", OwnerPrincipalID: alice,
		VMState: "running", InstanceTypeID: "gp1.medium",
		ImageID: "img_debian12", AvailabilityZone: "us-east-1b",
	})

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_abc123", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out InstanceResponse
	decodeBody(t, resp, &out)
	if out.ID != "inst_abc123" {
		t.Errorf("want ID=inst_abc123, got %q", out.ID)
	}
	if out.Status != "running" {
		t.Errorf("want status=running, got %q", out.Status)
	}
	if out.Region != "us-east-1" {
		t.Errorf("want region=us-east-1, got %q", out.Region)
	}
}

func TestGetInstance_NotFound(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_doesnotexist", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	if env.Error.Code != errInstanceNotFound {
		t.Errorf("want code %q, got %q", errInstanceNotFound, env.Error.Code)
	}
	if env.Error.RequestID == "" {
		t.Error("request_id must be present on 404")
	}
}

func TestGetInstance_WrongMethod(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_abc", "x", alice, "running")
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_abc", nil, authHdr(alice))
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ── PASS 1: GET /v1/instances ─────────────────────────────────────────────────

func TestListInstances_Empty(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)
	if out.Total != 0 {
		t.Errorf("want total=0, got %d", out.Total)
	}
	if out.Instances == nil {
		t.Error("want non-nil instances slice (empty, not null)")
	}
}

func TestListInstances_ScopedToHeader(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_a1", "alice-one", alice, "running")
	seedInstance(s.mem, "inst_a2", "alice-two", alice, "stopped")
	seedInstance(s.mem, "inst_b1", "bob-one", bob, "running")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)
	if out.Total != 2 {
		t.Errorf("want 2 instances for alice, got %d", out.Total)
	}
	for _, inst := range out.Instances {
		if inst.ID == "inst_b1" {
			t.Error("bob's instance must not appear in alice's list")
		}
	}
}

func TestListInstances_ResponseShape(t *testing.T) {
	s := newTestSrv(t)
	seedInstance(s.mem, "inst_shape", "shape-test", alice, "requested")

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil, authHdr(alice))
	var out ListInstancesResponse
	decodeBody(t, resp, &out)
	if out.Total != 1 {
		t.Fatalf("want 1 instance, got %d", out.Total)
	}
	inst := out.Instances[0]
	if inst.ID == "" {
		t.Error("id must be present")
	}
	if inst.Status == "" {
		t.Error("status must be present")
	}
	if inst.Region == "" {
		t.Error("region must be present")
	}
	if inst.CreatedAt.IsZero() {
		t.Error("created_at must be present")
	}
}

// ── PASS 1: error envelope invariants ─────────────────────────────────────────

// TestErrorEnvelope_RequestIDAlwaysPresent checks the §7 invariant.
// Source: API_ERROR_CONTRACT_V1 §7.
func TestErrorEnvelope_RequestIDAlwaysPresent(t *testing.T) {
	s := newTestSrv(t)

	cases := []struct {
		name    string
		method  string
		path    string
		body    any
		rawBody string
		headers map[string]string
	}{
		{name: "401 missing auth", method: http.MethodGet, path: "/v1/instances"},
		{name: "400 bad json", method: http.MethodPost, path: "/v1/instances", rawBody: "{bad", headers: authHdr(alice)},
		{name: "400 validation", method: http.MethodPost, path: "/v1/instances", body: CreateInstanceRequest{}, headers: authHdr(alice)},
		{name: "404 not found", method: http.MethodGet, path: "/v1/instances/inst_nope", headers: authHdr(alice)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.rawBody != "" {
				req, _ := http.NewRequest(tc.method, s.ts.URL+tc.path, strings.NewReader(tc.rawBody))
				req.Header.Set("Content-Type", "application/json")
				for k, v := range tc.headers {
					req.Header.Set(k, v)
				}
				resp, _ = s.ts.Client().Do(req)
			} else {
				resp = doReq(t, s.ts, tc.method, tc.path, tc.body, tc.headers)
			}

			var env apiError
			decodeBody(t, resp, &env)
			if env.Error.RequestID == "" {
				t.Errorf("request_id must always be present in error envelope")
			}
			if !strings.HasPrefix(env.Error.RequestID, "req_") {
				t.Errorf("request_id must have req_ prefix, got %q", env.Error.RequestID)
			}
		})
	}
}

// TestErrorEnvelope_DetailsEmptyNotNull verifies details is [] not null.
// Source: API_ERROR_CONTRACT_V1 §1.
func TestErrorEnvelope_DetailsEmptyNotNull(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_nope", nil, authHdr(alice))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, ok := raw["error"].(map[string]any)
	if !ok {
		t.Fatal("want error object in response")
	}
	details, exists := errObj["details"]
	if !exists {
		t.Fatal("want details key present in error object")
	}
	if details == nil {
		t.Error("details must not be null — must be an empty array")
	}
}

// ── PASS 1: validation unit ───────────────────────────────────────────────────

func TestValidation_NameRegexp(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"my-instance", true},
		{"ab", true},
		{"a1", true},
		{"a", false},
		{"1starts-digit", false},
		{"has_underscore", false},
		{"has space", false},
		{"UPPERCASE", false},
		{strings.Repeat("a", 64), false},
	}
	for _, tc := range cases {
		got := nameRE.MatchString(tc.name)
		if got != tc.valid {
			t.Errorf("nameRE.MatchString(%q) = %v, want %v", tc.name, got, tc.valid)
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func assertDetailCode(t *testing.T, env apiError, wantTarget, wantCode string) {
	t.Helper()
	for _, d := range env.Error.Details {
		if d.Target == wantTarget {
			if d.Code != wantCode {
				t.Errorf("detail[target=%q]: want code %q, got %q", wantTarget, wantCode, d.Code)
			}
			return
		}
	}
	t.Errorf("no detail entry with target=%q (got: %v)", wantTarget, detailCodes(env))
}

func detailCodes(env apiError) []string {
	var out []string
	for _, d := range env.Error.Details {
		out = append(out, fmt.Sprintf("%s:%s", d.Target, d.Code))
	}
	return out
}
