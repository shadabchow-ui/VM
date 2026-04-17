package main

// rollout_handlers_test.go — Unit tests for VM-P3C rollout control endpoints.
//
// Tests cover:
//   handleRolloutPause:
//     - 200 + paused:true when valid reason supplied
//     - reason echoed in response
//     - 400 missing_field when reason empty
//     - 400 invalid_value on bad JSON
//     - 405 on non-POST
//     - gate.IsPaused() true after call
//
//   handleRolloutResume:
//     - 200 + paused:false after paused gate
//     - 200 + paused:false idempotent when already resumed
//     - 405 on non-POST
//     - gate.IsPaused() false after call
//
//   handleRolloutStatus:
//     - 200 + paused:false when resumed
//     - 200 + paused:true + reason when paused
//     - 405 on non-GET
//     - does not mutate gate state
//
//   registerRolloutRoutes:
//     - no-op when rolloutGate is nil (backward compat)
//
// Style: httptest.ResponseRecorder + fakeRolloutGate.
// Source: VM_PHASE_ROADMAP §9 "bounded rollout controls",
//         11-02-phase-1-test-strategy §Unit.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ── Fake gate ─────────────────────────────────────────────────────────────────

// fakeRolloutGate satisfies RolloutGateInterface for handler tests.
type fakeRolloutGate struct {
	mu          sync.Mutex
	paused      bool
	pausedAt    *time.Time
	reason      string
	pauseCalls  []string
	resumeCalls int
}

func newFakeRolloutGate() *fakeRolloutGate { return &fakeRolloutGate{} }

func (g *fakeRolloutGate) Pause(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused = true
	now := time.Now()
	g.pausedAt = &now
	g.reason = reason
	g.pauseCalls = append(g.pauseCalls, reason)
}

func (g *fakeRolloutGate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused = false
	g.pausedAt = nil
	g.reason = ""
	g.resumeCalls++
}

func (g *fakeRolloutGate) IsPaused() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paused
}

func (g *fakeRolloutGate) GateStatus() RolloutGateStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	return RolloutGateStatus{
		Paused:   g.paused,
		PausedAt: g.pausedAt,
		Reason:   g.reason,
	}
}

// ── Test helpers ──────────────────────────────────────────────────────────────

func newRolloutTestServer(gate *fakeRolloutGate) *server {
	return &server{
		log:         newDiscardLogger(),
		rolloutGate: gate,
	}
}

func doRolloutRequest(t *testing.T, handler http.HandlerFunc, method string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var b []byte
	if body != nil {
		var err error
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
	}
	req := httptest.NewRequest(method, "/internal/v1/rollout/test", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func decodeRolloutStatusResp(t *testing.T, w *httptest.ResponseRecorder) rolloutStatusResponse {
	t.Helper()
	var resp rolloutStatusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode rolloutStatusResponse: %v — body: %s", err, w.Body.String())
	}
	return resp
}

// ── handleRolloutPause tests ──────────────────────────────────────────────────

func TestHandleRolloutPause_ValidReason_Returns200(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	w := doRolloutRequest(t, s.handleRolloutPause, http.MethodPost,
		map[string]string{"reason": "upgrading worker v1.4.2"})

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeRolloutStatusResp(t, w)
	if !resp.Paused {
		t.Error("response.paused must be true after pause")
	}
	if resp.Reason != "upgrading worker v1.4.2" {
		t.Errorf("response.reason = %q, want %q", resp.Reason, "upgrading worker v1.4.2")
	}
	if resp.Message == "" {
		t.Error("response.message must not be empty")
	}
}

func TestHandleRolloutPause_CallsGatePause(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	doRolloutRequest(t, s.handleRolloutPause, http.MethodPost,
		map[string]string{"reason": "schema migration"})

	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.pauseCalls) != 1 {
		t.Fatalf("expected 1 Pause call, got %d", len(gate.pauseCalls))
	}
	if gate.pauseCalls[0] != "schema migration" {
		t.Errorf("Pause reason = %q, want schema migration", gate.pauseCalls[0])
	}
}

func TestHandleRolloutPause_EmptyReason_Returns400MissingField(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	w := doRolloutRequest(t, s.handleRolloutPause, http.MethodPost,
		map[string]string{"reason": ""})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	var env apiError
	if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != errMissingField {
		t.Errorf("code = %q, want %q", env.Error.Code, errMissingField)
	}
	if gate.IsPaused() {
		t.Error("gate must not be paused when request is rejected")
	}
}

func TestHandleRolloutPause_BadJSON_Returns400(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/rollout/pause",
		bytes.NewReader([]byte("not json")))
	w := httptest.NewRecorder()
	s.handleRolloutPause(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHandleRolloutPause_WrongMethod_Returns405(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/rollout/pause", nil)
	w := httptest.NewRecorder()
	s.handleRolloutPause(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandleRolloutPause_GateIsPausedAfterCall(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	doRolloutRequest(t, s.handleRolloutPause, http.MethodPost,
		map[string]string{"reason": "test"})

	if !gate.IsPaused() {
		t.Error("gate.IsPaused() must be true after handleRolloutPause")
	}
}

// ── handleRolloutResume tests ─────────────────────────────────────────────────

func TestHandleRolloutResume_WhenPaused_Returns200PausedFalse(t *testing.T) {
	gate := newFakeRolloutGate()
	gate.Pause("setup pause")
	s := newRolloutTestServer(gate)

	w := doRolloutRequest(t, s.handleRolloutResume, http.MethodPost, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body=%s", w.Code, w.Body.String())
	}
	resp := decodeRolloutStatusResp(t, w)
	if resp.Paused {
		t.Error("response.paused must be false after resume")
	}
}

func TestHandleRolloutResume_Idempotent_WhenAlreadyResumed(t *testing.T) {
	gate := newFakeRolloutGate()
	// Gate starts resumed (fresh).
	s := newRolloutTestServer(gate)

	w := doRolloutRequest(t, s.handleRolloutResume, http.MethodPost, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeRolloutStatusResp(t, w)
	if resp.Paused {
		t.Error("idempotent resume: paused must be false")
	}
}

func TestHandleRolloutResume_CallsGateResume(t *testing.T) {
	gate := newFakeRolloutGate()
	gate.Pause("setup")
	s := newRolloutTestServer(gate)

	doRolloutRequest(t, s.handleRolloutResume, http.MethodPost, nil)

	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.resumeCalls != 1 {
		t.Errorf("expected 1 Resume call, got %d", gate.resumeCalls)
	}
}

func TestHandleRolloutResume_WrongMethod_Returns405(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/rollout/resume", nil)
	w := httptest.NewRecorder()
	s.handleRolloutResume(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandleRolloutResume_GateIsResumedAfterCall(t *testing.T) {
	gate := newFakeRolloutGate()
	gate.Pause("test")
	s := newRolloutTestServer(gate)

	doRolloutRequest(t, s.handleRolloutResume, http.MethodPost, nil)

	if gate.IsPaused() {
		t.Error("gate.IsPaused() must be false after handleRolloutResume")
	}
}

// ── handleRolloutStatus tests ─────────────────────────────────────────────────

func TestHandleRolloutStatus_WhenResumed_ReturnsPausedFalse(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/rollout/status", nil)
	w := httptest.NewRecorder()
	s.handleRolloutStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeRolloutStatusResp(t, w)
	if resp.Paused {
		t.Error("status.paused must be false when gate is resumed")
	}
	if resp.Message == "" {
		t.Error("status.message must not be empty")
	}
}

func TestHandleRolloutStatus_WhenPaused_ReturnsPausedTrueWithReason(t *testing.T) {
	gate := newFakeRolloutGate()
	gate.Pause("host-agent patch rollout")
	s := newRolloutTestServer(gate)

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/rollout/status", nil)
	w := httptest.NewRecorder()
	s.handleRolloutStatus(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	resp := decodeRolloutStatusResp(t, w)
	if !resp.Paused {
		t.Error("status.paused must be true when gate is paused")
	}
	if resp.Reason != "host-agent patch rollout" {
		t.Errorf("status.reason = %q, want %q", resp.Reason, "host-agent patch rollout")
	}
	if resp.PausedAt == nil {
		t.Error("status.paused_at must not be nil when paused")
	}
}

func TestHandleRolloutStatus_WrongMethod_Returns405(t *testing.T) {
	gate := newFakeRolloutGate()
	s := newRolloutTestServer(gate)

	req := httptest.NewRequest(http.MethodPost, "/internal/v1/rollout/status", nil)
	w := httptest.NewRecorder()
	s.handleRolloutStatus(w, req)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", w.Code)
	}
}

func TestHandleRolloutStatus_IsNonMutating(t *testing.T) {
	gate := newFakeRolloutGate()
	gate.Pause("non-mutating test")
	s := newRolloutTestServer(gate)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/rollout/status", nil)
		w := httptest.NewRecorder()
		s.handleRolloutStatus(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("call %d: want 200, got %d", i+1, w.Code)
		}
	}
	if !gate.IsPaused() {
		t.Error("handleRolloutStatus must not modify gate state")
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.resumeCalls != 0 {
		t.Errorf("handleRolloutStatus must not call Resume(), got %d calls", gate.resumeCalls)
	}
}

// ── registerRolloutRoutes nil-gate test ───────────────────────────────────────

// TestRegisterRolloutRoutes_NilGate_IsNoop verifies backward compat:
// when rolloutGate is nil, routes are not registered and endpoints 404.
func TestRegisterRolloutRoutes_NilGate_IsNoop(t *testing.T) {
	s := &server{log: newDiscardLogger(), rolloutGate: nil}
	mux := http.NewServeMux()
	s.registerRolloutRoutes(mux) // must not panic

	for _, path := range []string{
		"/internal/v1/rollout/pause",
		"/internal/v1/rollout/resume",
		"/internal/v1/rollout/status",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("nil gate: path %s want 404, got %d", path, w.Code)
		}
	}
}
