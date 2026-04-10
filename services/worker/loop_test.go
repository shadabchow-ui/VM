package main

// loop_test.go — Unit tests for WorkerLoop execute, route, and finalise logic.
//
// Source: IMPLEMENTATION_PLAN_V1 §21, JOB_MODEL_V1 §2.
//
// Tests use a real *db.Repo backed by the existing lib/pq driver pointed at
// a fake in-memory state via a package-level intercept table.
// The key insight: we test loop.execute() and loop.finaliseJob() directly
// (they are unexported but in the same package) without needing a real DB
// for the job-status update path — we capture the UpdateJobStatus call via
// a thin repo stub.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/services/worker/handlers"
)

// ── Stub repo for loop tests ──────────────────────────────────────────────────
// Satisfies the db.Repo-shaped interface needed by WorkerLoop.
// We achieve this by providing a fakePool that captures UpdateJobStatus and
// RequeueFailedAttempt calls.

type loopTestPool struct {
	mu           sync.Mutex
	statusUpdate map[string]string // jobID → status set
	errMsgUpdate map[string]string // jobID → errMsg set
	requeuedJobs map[string]bool   // jobID → true if RequeueFailedAttempt was called
}

func newLoopTestPool() *loopTestPool {
	return &loopTestPool{
		statusUpdate: make(map[string]string),
		errMsgUpdate: make(map[string]string),
		requeuedJobs: make(map[string]bool),
	}
}

// Exec captures UPDATE jobs SET ... calls.
// Distinguishes between:
//   - UpdateJobStatus:      args = (id, status, errMsg, completedAt, updatedAt)
//   - RequeueFailedAttempt: args = (id, errMsg) with SQL containing "status = 'pending'"
func (p *loopTestPool) Exec(_ context.Context, query string, args ...any) (db.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Detect RequeueFailedAttempt by its SQL pattern: status hardcoded as 'pending'
	// and claimed_at = NULL in the query itself.
	if strings.Contains(query, "status") && strings.Contains(query, "'pending'") &&
		strings.Contains(query, "claimed_at") && strings.Contains(query, "NULL") {
		// RequeueFailedAttempt signature: (id, errMsg)
		if len(args) >= 1 {
			if id, ok := args[0].(string); ok {
				p.requeuedJobs[id] = true
				p.statusUpdate[id] = "pending"
				if len(args) >= 2 {
					if msg, ok := args[1].(*string); ok && msg != nil {
						p.errMsgUpdate[id] = *msg
					}
				}
			}
		}
		return &stubTag{1}, nil
	}

	// UpdateJobStatus signature: (id, status, errMsg, completedAt, updatedAt)
	if len(args) >= 2 {
		if id, ok := args[0].(string); ok {
			if status, ok := args[1].(string); ok {
				p.statusUpdate[id] = status
				if len(args) >= 3 {
					if msg, ok := args[2].(*string); ok && msg != nil {
						p.errMsgUpdate[id] = *msg
					}
				}
			}
		}
	}
	return &stubTag{1}, nil
}

func (p *loopTestPool) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	return &emptyRows{}, nil
}
func (p *loopTestPool) QueryRow(_ context.Context, _ string, _ ...any) db.Row {
	return &noRow{}
}
func (p *loopTestPool) Close() {}

type stubTag struct{ n int64 }

func (t *stubTag) RowsAffected() int64 { return t.n }

type emptyRows struct{}

func (r *emptyRows) Next() bool        { return false }
func (r *emptyRows) Scan(...any) error { return nil }
func (r *emptyRows) Close()            {}
func (r *emptyRows) Err() error        { return nil }

type noRow struct{}

func (r *noRow) Scan(...any) error { return fmt.Errorf("no rows") }

// ── fakeHandler ───────────────────────────────────────────────────────────────

type loopFakeHandler struct {
	mu       sync.Mutex
	called   []string
	failWith error
}

func (h *loopFakeHandler) Execute(_ context.Context, job *db.JobRow) error {
	h.mu.Lock()
	h.called = append(h.called, job.ID)
	h.mu.Unlock()
	return h.failWith
}

// ── helpers ───────────────────────────────────────────────────────────────────

func loopTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func makeLoop(t *testing.T, pool *loopTestPool, dispatch map[string]handlers.Handler) *WorkerLoop {
	t.Helper()
	repo := db.New(pool)
	// WorkerLoop needs *sql.DB only for claimNext transactions.
	// For execute/finaliseJob tests we pass nil — these methods don't use sqlDB directly.
	return NewWorkerLoop(repo, nil, dispatch, loopTestLog())
}

func makeJob(id, instanceID, jobType string, attempt, maxAttempts int) *db.JobRow {
	return &db.JobRow{
		ID: id, InstanceID: instanceID, JobType: jobType,
		AttemptCount: attempt, MaxAttempts: maxAttempts,
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestWorkerLoop_Execute_RoutesToHandler(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_CREATE": h})

	job := makeJob("job-route-001", "inst-x", "INSTANCE_CREATE", 1, 3)
	loop.execute(context.Background(), job)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.called) == 0 || h.called[0] != "job-route-001" {
		t.Errorf("handler.called = %v, want [job-route-001]", h.called)
	}
}

func TestWorkerLoop_Execute_MarksCompleted_OnSuccess(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{} // returns nil → success
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_CREATE": h})

	job := makeJob("job-ok-001", "inst-ok", "INSTANCE_CREATE", 1, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	status := pool.statusUpdate["job-ok-001"]
	pool.mu.Unlock()
	if status != "completed" {
		t.Errorf("job status = %q, want completed", status)
	}
}

func TestWorkerLoop_Execute_RequeuesPending_WhenAttemptsRemain(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{failWith: errors.New("transient error")}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_CREATE": h})

	// attempt=1, max=3 → still retriable → requeue to "pending"
	job := makeJob("job-retry-001", "inst-retry", "INSTANCE_CREATE", 1, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	status := pool.statusUpdate["job-retry-001"]
	requeued := pool.requeuedJobs["job-retry-001"]
	pool.mu.Unlock()
	if status != "pending" {
		t.Errorf("job status = %q, want pending (requeued for retry)", status)
	}
	if !requeued {
		t.Error("RequeueFailedAttempt was not called")
	}
}

func TestWorkerLoop_Execute_RequeuesPending_WhenAttempt2Of3(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{failWith: errors.New("transient error")}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_CREATE": h})

	// attempt=2, max=3 → still retriable → requeue to "pending"
	job := makeJob("job-retry-002", "inst-retry", "INSTANCE_CREATE", 2, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	status := pool.statusUpdate["job-retry-002"]
	requeued := pool.requeuedJobs["job-retry-002"]
	pool.mu.Unlock()
	if status != "pending" {
		t.Errorf("job status = %q, want pending (requeued for retry)", status)
	}
	if !requeued {
		t.Error("RequeueFailedAttempt was not called for attempt 2")
	}
}

func TestWorkerLoop_Execute_MarksDeadWhenMaxAttemptsExhausted(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{failWith: errors.New("permanent error")}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_CREATE": h})

	// attempt == max → dead
	job := makeJob("job-dead-001", "inst-dead", "INSTANCE_CREATE", 3, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	status := pool.statusUpdate["job-dead-001"]
	requeued := pool.requeuedJobs["job-dead-001"]
	pool.mu.Unlock()
	if status != "dead" {
		t.Errorf("job status = %q, want dead", status)
	}
	if requeued {
		t.Error("RequeueFailedAttempt should not be called when attempts exhausted")
	}
}

func TestWorkerLoop_Execute_MarksDeadForUnknownJobType(t *testing.T) {
	pool := newLoopTestPool()
	loop := makeLoop(t, pool, map[string]handlers.Handler{}) // no handlers

	job := makeJob("job-unk-001", "inst-u", "INSTANCE_STOP", 1, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	status := pool.statusUpdate["job-unk-001"]
	pool.mu.Unlock()
	if status != "dead" {
		t.Errorf("unknown job type status = %q, want dead", status)
	}
}

func TestWorkerLoop_Execute_ErrorMessageRecorded_OnRequeue(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{failWith: errors.New("handler blew up")}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_DELETE": h})

	// attempt=1, max=3 → requeue with error message preserved
	job := makeJob("job-errmsg-001", "inst-e", "INSTANCE_DELETE", 1, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	msg := pool.errMsgUpdate["job-errmsg-001"]
	status := pool.statusUpdate["job-errmsg-001"]
	pool.mu.Unlock()
	if msg == "" {
		t.Error("error message not recorded in job update")
	}
	if status != "pending" {
		t.Errorf("job status = %q, want pending (requeued)", status)
	}
}

func TestWorkerLoop_Execute_ErrorMessageRecorded_OnDead(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{failWith: errors.New("final failure")}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_DELETE": h})

	// attempt=3, max=3 → dead with error message
	job := makeJob("job-errmsg-dead-001", "inst-e", "INSTANCE_DELETE", 3, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	msg := pool.errMsgUpdate["job-errmsg-dead-001"]
	status := pool.statusUpdate["job-errmsg-dead-001"]
	pool.mu.Unlock()
	if msg == "" {
		t.Error("error message not recorded for dead job")
	}
	if status != "dead" {
		t.Errorf("job status = %q, want dead", status)
	}
}

func TestWorkerLoop_Execute_DeleteHandlerRouted(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_DELETE": h})

	job := makeJob("job-del-route-001", "inst-del", "INSTANCE_DELETE", 1, 3)
	loop.execute(context.Background(), job)

	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.called) == 0 {
		t.Error("DELETE handler was not called")
	}
}

func TestWorkerLoop_Execute_MaxAttempts1_FailsImmediatelyToDead(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{failWith: errors.New("one-shot failure")}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_CREATE": h})

	// max=1, attempt=1 → immediately dead (no retry possible)
	job := makeJob("job-oneshot-001", "inst-oneshot", "INSTANCE_CREATE", 1, 1)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	status := pool.statusUpdate["job-oneshot-001"]
	pool.mu.Unlock()
	if status != "dead" {
		t.Errorf("job status = %q, want dead (max_attempts=1)", status)
	}
}
