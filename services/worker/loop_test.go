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
	"sync"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/services/worker/handlers"
)

// ── Stub repo for loop tests ──────────────────────────────────────────────────
// Satisfies the db.Repo-shaped interface needed by WorkerLoop.
// We achieve this by providing a fakePool that captures UpdateJobStatus calls.

type loopTestPool struct {
	mu           sync.Mutex
	statusUpdate map[string]string  // jobID → status set
	errMsgUpdate map[string]string  // jobID → errMsg set
}

func newLoopTestPool() *loopTestPool {
	return &loopTestPool{
		statusUpdate: make(map[string]string),
		errMsgUpdate: make(map[string]string),
	}
}

// Exec captures UPDATE jobs SET status=... WHERE id=...
// UpdateJobStatus calls: Exec(ctx, "UPDATE jobs SET status=$2 ... WHERE id=$1", id, status, ...)
func (p *loopTestPool) Exec(_ context.Context, query string, args ...any) (db.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
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
func (r *emptyRows) Next() bool            { return false }
func (r *emptyRows) Scan(...any) error     { return nil }
func (r *emptyRows) Close()               {}
func (r *emptyRows) Err() error           { return nil }

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

func TestWorkerLoop_Execute_MarksFailedOnError_WhenAttemptsRemain(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{failWith: errors.New("transient error")}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_CREATE": h})

	// attempt=1, max=3 → still retriable → "failed"
	job := makeJob("job-fail-001", "inst-fail", "INSTANCE_CREATE", 1, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	status := pool.statusUpdate["job-fail-001"]
	pool.mu.Unlock()
	if status != "failed" {
		t.Errorf("job status = %q, want failed", status)
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
	pool.mu.Unlock()
	if status != "dead" {
		t.Errorf("job status = %q, want dead", status)
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

func TestWorkerLoop_Execute_ErrorMessageRecorded(t *testing.T) {
	pool := newLoopTestPool()
	h := &loopFakeHandler{failWith: errors.New("handler blew up")}
	loop := makeLoop(t, pool, map[string]handlers.Handler{"INSTANCE_DELETE": h})

	job := makeJob("job-errmsg-001", "inst-e", "INSTANCE_DELETE", 1, 3)
	loop.execute(context.Background(), job)

	pool.mu.Lock()
	msg := pool.errMsgUpdate["job-errmsg-001"]
	pool.mu.Unlock()
	if msg == "" {
		t.Error("error message not recorded in job update")
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
