package main

// instance_handlers_test.go — PASS 1 tests for the public instance API.
//
// Covers:
//   - POST /v1/instances: happy path response shape
//   - POST /v1/instances: each required-field missing → correct error code + target
//   - POST /v1/instances: invalid instance_type, image_id, availability_zone
//   - POST /v1/instances: malformed JSON
//   - POST /v1/instances: empty body produces details array with all missing fields
//   - GET  /v1/instances/{id}: happy path shape
//   - GET  /v1/instances/{id}: not found → 404 with correct code
//   - GET  /v1/instances: returns list scoped to X-Principal-ID header
//   - Error envelope: request_id always present, even on 400 and 404
//   - Wrong HTTP method: 405
//
// No auth middleware tests (PASS 2).
// No idempotency tests (PASS 3).
// No lifecycle action tests (PASS 2).
//
// Test strategy: in-process httptest.Server backed by a memPool (fake db.Pool).
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

// memPool is a minimal fake db.Pool for PASS 1 handler tests.
// It stores InstanceRows in a map and satisfies exactly the queries called by
// the PASS 1 handlers: InsertInstance, GetInstanceByID, ListInstancesByOwner.
type memPool struct {
	instances map[string]*db.InstanceRow
}

func newMemPool() *memPool {
	return &memPool{instances: make(map[string]*db.InstanceRow)}
}

// seed adds an instance directly — used by test setup.
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

// Exec handles INSERT INTO instances (InsertInstance).
// arg order: $1=id, $2=name, $3=owner_principal_id, $4=instance_type_id,
//            $5=image_id, $6=availability_zone
func (p *memPool) Exec(_ context.Context, sql string, args ...any) (db.CommandTag, error) {
	if strings.Contains(sql, "INSERT INTO instances") {
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
	}
	return &fakeTag{0}, nil
}

// Query handles ListInstancesByOwner.
// Matches: SELECT ... FROM instances WHERE owner_principal_id = $1
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
// Matches: SELECT ... FROM instances WHERE id = $1
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
// Column order must match GetInstanceByID SELECT in instance_repo.go:
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
// Column order must match ListInstancesByOwner SELECT in instance_repo.go.
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

// testSrv bundles an httptest.Server with the backing memPool for direct seeding.
type testSrv struct {
	ts  *httptest.Server
	mem *memPool
}

// newTestSrv builds an in-process server wired to memPool.
// Only /v1/instances routes are registered — no mTLS, no CA needed.
func newTestSrv(t *testing.T) *testSrv {
	t.Helper()
	mem := newMemPool()
	repo := db.New(mem)
	srv := &server{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		repo:   repo,
		region: "us-east-1",
		// inventory and ca intentionally nil — not used by PASS 1 handlers.
	}
	mux := http.NewServeMux()
	srv.registerInstanceRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return &testSrv{ts: ts, mem: mem}
}

// ── HTTP helpers ──────────────────────────────────────────────────────────────

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

// ── Tests: POST /v1/instances ─────────────────────────────────────────────────

// TestCreate_Happy verifies 202, response shape, and id/status field values.
// Source: INSTANCE_MODEL_V1 §2, 08-01 §CreateInstance response.
func TestCreate_Happy(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		validCreateBody(),
		map[string]string{"X-Principal-ID": "princ_alice"})

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
	if out.Instance.ImageID != "img_ubuntu2204" {
		t.Errorf("want image_id=img_ubuntu2204, got %q", out.Instance.ImageID)
	}
	if out.Instance.AvailabilityZone != "us-east-1a" {
		t.Errorf("want availability_zone=us-east-1a, got %q", out.Instance.AvailabilityZone)
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

// TestCreate_AllFieldsMissing verifies 400 with a populated details array.
// All five required fields must appear in details.
// Source: API_ERROR_CONTRACT_V1 §1 (details array), §4 (missing_field code).
func TestCreate_AllFieldsMissing(t *testing.T) {
	s := newTestSrv(t)
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances",
		CreateInstanceRequest{}, // all fields zero-value
		nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)

	if env.Error.Code != errInvalidRequest {
		t.Errorf("top-level code: want %q, got %q", errInvalidRequest, env.Error.Code)
	}
	if env.Error.RequestID == "" {
		t.Error("request_id must be present")
	}
	if len(env.Error.Details) == 0 {
		t.Fatal("want non-empty details array for multi-field failure")
	}

	// Build target set from details.
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

// TestCreate_MissingName verifies missing_field on name only.
func TestCreate_MissingName(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.Name = ""
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "name", errMissingField)
}

// TestCreate_InvalidName verifies invalid_name when name fails the regex.
// Source: INSTANCE_MODEL_V1 §2.
func TestCreate_InvalidName(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.Name = "UPPERCASE" // fails ^[a-z]...
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "name", errInvalidName)
}

// TestCreate_InvalidInstanceType verifies invalid_instance_type.
// Source: INSTANCE_MODEL_V1 §6, API_ERROR_CONTRACT_V1 §4.
func TestCreate_InvalidInstanceType(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.InstanceType = "gp99.enormous"
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "instance_type", errInvalidInstanceType)
}

// TestCreate_InvalidImageID verifies invalid_image_id.
// Source: INSTANCE_MODEL_V1 §7, API_ERROR_CONTRACT_V1 §4.
func TestCreate_InvalidImageID(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.ImageID = "img_unknown"
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "image_id", errInvalidImageID)
}

// TestCreate_InvalidAZ verifies invalid_availability_zone.
// Source: 07-01 §Phase 1 AZ list, API_ERROR_CONTRACT_V1 §4.
func TestCreate_InvalidAZ(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.AvailabilityZone = "us-west-9z"
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "availability_zone", errInvalidAZ)
}

// TestCreate_MissingSSHKey verifies missing_field for ssh_key_name.
func TestCreate_MissingSSHKey(t *testing.T) {
	s := newTestSrv(t)
	body := validCreateBody()
	body.SSHKeyName = ""
	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances", body, nil)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	var env apiError
	decodeBody(t, resp, &env)
	assertDetailCode(t, env, "ssh_key_name", errMissingField)
}

// ── Tests: GET /v1/instances/{id} ─────────────────────────────────────────────

// TestGetInstance_Happy verifies 200 and correct field values.
// Source: 08-01 §GetInstance.
func TestGetInstance_Happy(t *testing.T) {
	s := newTestSrv(t)
	s.mem.seed(&db.InstanceRow{
		ID:               "inst_abc123",
		Name:             "test-inst",
		OwnerPrincipalID: "princ_alice",
		VMState:          "running",
		InstanceTypeID:   "gp1.medium",
		ImageID:          "img_debian12",
		AvailabilityZone: "us-east-1b",
	})

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_abc123", nil,
		map[string]string{"X-Principal-ID": "princ_alice"})

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
	if out.InstanceType != "gp1.medium" {
		t.Errorf("want instance_type=gp1.medium, got %q", out.InstanceType)
	}
	if out.Region != "us-east-1" {
		t.Errorf("want region=us-east-1, got %q", out.Region)
	}
}

// TestGetInstance_NotFound verifies 404 with correct error code.
// Source: API_ERROR_CONTRACT_V1 §4 instance_not_found.
func TestGetInstance_NotFound(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_doesnotexist", nil, nil)

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

// TestGetInstance_WrongMethod verifies 405 for POST on /{id}.
func TestGetInstance_WrongMethod(t *testing.T) {
	s := newTestSrv(t)
	s.mem.seed(&db.InstanceRow{
		ID: "inst_abc", VMState: "running",
		OwnerPrincipalID: "princ_alice", Name: "x",
		InstanceTypeID: "gp1.small", ImageID: "img_ubuntu2204",
		AvailabilityZone: "us-east-1a",
	})

	resp := doReq(t, s.ts, http.MethodPost, "/v1/instances/inst_abc", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", resp.StatusCode)
	}
}

// ── Tests: GET /v1/instances ──────────────────────────────────────────────────

// TestListInstances_Empty verifies 200 with empty list when no instances exist.
// Source: 09-02-empty-loading-and-error-states.md.
func TestListInstances_Empty(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil,
		map[string]string{"X-Principal-ID": "princ_alice"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var out ListInstancesResponse
	decodeBody(t, resp, &out)
	if out.Total != 0 {
		t.Errorf("want total=0, got %d", out.Total)
	}
	if out.Instances == nil {
		t.Error("want non-nil instances array (empty, not null)")
	}
}

// TestListInstances_ScopedToHeader verifies list only returns instances matching
// the X-Principal-ID header value. This is the PASS 1 placeholder for ownership
// scoping — full auth enforcement is added in PASS 2.
// Source: AUTH_OWNERSHIP_MODEL_V1 §4.
func TestListInstances_ScopedToHeader(t *testing.T) {
	s := newTestSrv(t)
	s.mem.seed(&db.InstanceRow{
		ID: "inst_a1", Name: "alice-one", OwnerPrincipalID: "princ_alice",
		VMState: "running", InstanceTypeID: "gp1.small",
		ImageID: "img_ubuntu2204", AvailabilityZone: "us-east-1a",
	})
	s.mem.seed(&db.InstanceRow{
		ID: "inst_a2", Name: "alice-two", OwnerPrincipalID: "princ_alice",
		VMState: "stopped", InstanceTypeID: "gp1.medium",
		ImageID: "img_ubuntu2204", AvailabilityZone: "us-east-1b",
	})
	s.mem.seed(&db.InstanceRow{
		ID: "inst_b1", Name: "bob-one", OwnerPrincipalID: "princ_bob",
		VMState: "running", InstanceTypeID: "gp1.small",
		ImageID: "img_ubuntu2204", AvailabilityZone: "us-east-1a",
	})

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil,
		map[string]string{"X-Principal-ID": "princ_alice"})

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

// TestListInstances_ResponseShape verifies each item has required fields.
// Source: INSTANCE_MODEL_V1 §2.
func TestListInstances_ResponseShape(t *testing.T) {
	s := newTestSrv(t)
	s.mem.seed(&db.InstanceRow{
		ID: "inst_shape", Name: "shape-test", OwnerPrincipalID: "princ_alice",
		VMState: "requested", InstanceTypeID: "gp1.large",
		ImageID: "img_debian12", AvailabilityZone: "us-east-1a",
	})

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances", nil,
		map[string]string{"X-Principal-ID": "princ_alice"})

	var out ListInstancesResponse
	decodeBody(t, resp, &out)
	if out.Total != 1 {
		t.Fatalf("want 1 instance, got %d", out.Total)
	}

	inst := out.Instances[0]
	if inst.ID == "" {
		t.Error("id must be present")
	}
	if inst.Name == "" {
		t.Error("name must be present")
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

// ── Tests: error envelope invariants ─────────────────────────────────────────

// TestErrorEnvelope_RequestIDAlwaysPresent checks the §7 invariant:
// every error response includes request_id regardless of status code.
// Source: API_ERROR_CONTRACT_V1 §7.
func TestErrorEnvelope_RequestIDAlwaysPresent(t *testing.T) {
	s := newTestSrv(t)

	cases := []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"400 bad json", http.MethodPost, "/v1/instances", nil},
		{"400 validation", http.MethodPost, "/v1/instances", CreateInstanceRequest{}},
		{"404 not found", http.MethodGet, "/v1/instances/inst_nope", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp *http.Response
			if tc.body == nil && tc.method == http.MethodPost {
				// Malformed JSON case: send non-JSON body.
				req, _ := http.NewRequest(tc.method, s.ts.URL+tc.path, strings.NewReader("{bad"))
				req.Header.Set("Content-Type", "application/json")
				resp, _ = s.ts.Client().Do(req)
			} else {
				resp = doReq(t, s.ts, tc.method, tc.path, tc.body, nil)
			}
			defer resp.Body.Close()

			var env apiError
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error.RequestID == "" {
				t.Errorf("[%s] request_id must always be present in error envelope", tc.name)
			}
			if !strings.HasPrefix(env.Error.RequestID, "req_") {
				t.Errorf("[%s] request_id must have req_ prefix, got %q", tc.name, env.Error.RequestID)
			}
		})
	}
}

// TestErrorEnvelope_DetailsEmptyNotNull verifies that single-error responses
// include an empty details array, not null.
// Source: API_ERROR_CONTRACT_V1 §1 ("details is always an array").
func TestErrorEnvelope_DetailsEmptyNotNull(t *testing.T) {
	s := newTestSrv(t)

	resp := doReq(t, s.ts, http.MethodGet, "/v1/instances/inst_nope", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", resp.StatusCode)
	}

	// Decode as raw map to check that "details" is [] not null.
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj, ok := raw["error"].(map[string]any)
	if !ok {
		t.Fatal("want error object in response")
	}
	details, ok := errObj["details"]
	if !ok {
		t.Fatal("want details key present in error object")
	}
	if details == nil {
		t.Error("details must not be null — must be an empty array")
	}
}

// ── Tests: validation unit ────────────────────────────────────────────────────

// TestValidation_NameRegexp exercises the name regex directly.
// Source: INSTANCE_MODEL_V1 §2.
func TestValidation_NameRegexp(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"my-instance", true},
		{"ab", true},           // minimum: 2 chars matching [a-z][a-z0-9]
		{"a1", true},
		{"a", false},           // single char — doesn't satisfy [a-z][...][a-z0-9]
		{"1starts-digit", false},
		{"has_underscore", false},
		{"has space", false},
		{"UPPERCASE", false},
		{strings.Repeat("a", 64), false}, // >63 chars total
	}
	for _, tc := range cases {
		got := nameRE.MatchString(tc.name)
		if got != tc.valid {
			t.Errorf("nameRE.MatchString(%q) = %v, want %v", tc.name, got, tc.valid)
		}
	}
}

// ── Helper ────────────────────────────────────────────────────────────────────

// assertDetailCode checks that the details array contains at least one entry
// with the given target and code.
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
	t.Errorf("no detail entry found with target=%q (codes present: %v)",
		wantTarget, detailCodes(env))
}

func detailCodes(env apiError) []string {
	var out []string
	for _, d := range env.Error.Details {
		out = append(out, fmt.Sprintf("%s:%s", d.Target, d.Code))
	}
	return out
}
