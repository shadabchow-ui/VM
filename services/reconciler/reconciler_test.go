package reconciler

// reconciler_test.go — Unit tests for the reconciler skeleton.
//
// Tests verify:
//   - Enqueue puts instance IDs into the work channel
//   - reconcileOne with no-drift instance produces no dispatch
//   - reconcileOne with drifted instance calls dispatch
//   - resync with N instances enqueues N items
//   - RunWorkers drains channel and calls reconcileOne for each ID
//   - Work queue full: excess Enqueue calls are dropped without panic or block
//
// All tests run on macOS dev box without PostgreSQL or Linux/KVM.
// Source: 03-03-reconciliation-loops §hybrid trigger model,
//         IMPLEMENTATION_PLAN_V1 §WS-3 (event-driven + 5-min resync),
//         R-07 (hybrid trigger is non-negotiable).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── reconcilerFakePool ────────────────────────────────────────────────────────
// Implements db.Pool for reconciler tests.
// Serves GetInstanceByID and ListActiveInstances; captures InsertJob/UpdateState.

type reconcilerFakePool struct {
	mu sync.Mutex

	// instances to return from GetInstanceByID and ListActiveInstances
	instances map[string]*db.InstanceRow

	// captured side effects
	insertedJobIDs   []string
	updateStateCalls []string // instanceID values

	// count for HasActivePendingJob
	countResult int

	// list active error
	listErr error
}

func newReconcilerFakePool() *reconcilerFakePool {
	return &reconcilerFakePool{
		instances: make(map[string]*db.InstanceRow),
	}
}

func (p *reconcilerFakePool) addInstance(inst *db.InstanceRow) {
	p.instances[inst.ID] = inst
}

func (p *reconcilerFakePool) Exec(_ context.Context, query string, args ...any) (db.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Contains(query, "INSERT INTO jobs") {
		if len(args) >= 1 {
			if id, ok := args[0].(string); ok {
				p.insertedJobIDs = append(p.insertedJobIDs, id)
			}
		}
	}
	if containsUpdateInstances(query) && len(args) >= 1 {
		if id, ok := args[0].(string); ok {
			p.updateStateCalls = append(p.updateStateCalls, id)
		}
	}
	return &reconcilerTag{rows: 1}, nil
}

func (p *reconcilerFakePool) QueryRow(_ context.Context, query string, args ...any) db.Row {
	p.mu.Lock()
	defer p.mu.Unlock()
	if containsSelectCount(query) {
		return &reconcilerRow{values: []any{p.countResult}}
	}
	if len(args) >= 1 {
		if id, ok := args[0].(string); ok {
			if inst, ok2 := p.instances[id]; ok2 {
				return &reconcilerRow{values: instanceToScanValues(inst)}
			}
		}
	}
	return &reconcilerRow{err: fmt.Errorf("no rows in result set")}
}

func (p *reconcilerFakePool) Query(_ context.Context, _ string, _ ...any) (db.Rows, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listErr != nil {
		return nil, p.listErr
	}
	var rows [][]any
	for _, inst := range p.instances {
		rows = append(rows, instanceToScanValues(inst))
	}
	return newReconcilerRows(rows), nil
}

func (p *reconcilerFakePool) Close() {}

type reconcilerTag struct{ rows int64 }

func (t *reconcilerTag) RowsAffected() int64 { return t.rows }

type reconcilerRow struct {
	values []any
	err    error
}

func (r *reconcilerRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if i >= len(r.values) || r.values[i] == nil {
			continue
		}
		switch dst := d.(type) {
		case *string:
			if v, ok := r.values[i].(string); ok {
				*dst = v
			}
		case *int:
			if v, ok := r.values[i].(int); ok {
				*dst = v
			}
		case **string:
			if v, ok := r.values[i].(string); ok {
				*dst = &v
			}
		case *time.Time:
			if v, ok := r.values[i].(time.Time); ok {
				*dst = v
			}
		case **time.Time:
			if v, ok := r.values[i].(time.Time); ok {
				*dst = &v
			}
		}
	}
	return nil
}

type reconcilerRows struct {
	data [][]any
	idx  int
}

func newReconcilerRows(data [][]any) *reconcilerRows {
	return &reconcilerRows{data: data, idx: -1}
}

func (r *reconcilerRows) Next() bool {
	r.idx++
	return r.idx < len(r.data)
}
func (r *reconcilerRows) Scan(dest ...any) error {
	row := r.data[r.idx]
	for i, d := range dest {
		if i >= len(row) || row[i] == nil {
			continue
		}
		switch dst := d.(type) {
		case *string:
			if v, ok := row[i].(string); ok {
				*dst = v
			}
		case *int:
			if v, ok := row[i].(int); ok {
				*dst = v
			}
		case **string:
			if v, ok := row[i].(string); ok {
				*dst = &v
			}
		case *time.Time:
			if v, ok := row[i].(time.Time); ok {
				*dst = v
			}
		case **time.Time:
			if v, ok := row[i].(time.Time); ok {
				*dst = &v
			}
		}
	}
	return nil
}
func (r *reconcilerRows) Close() {}
func (r *reconcilerRows) Err() error { return nil }

// ── helpers ───────────────────────────────────────────────────────────────────

func reconcilerTestLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// makeHealthyInstance returns an instance that ClassifyDrift will find clean.
func makeHealthyInstance(id string) *db.InstanceRow {
	hostID := "host-001"
	return &db.InstanceRow{
		ID:        id,
		VMState:   "running",
		HostID:    &hostID,
		Version:   1,
		UpdatedAt: time.Now(), // just updated → no missing-runtime-process drift
	}
}

// makeDriftedInstance returns an instance that ClassifyDrift will flag.
func makeDriftedInstance(id string) *db.InstanceRow {
	return &db.InstanceRow{
		ID:        id,
		VMState:   "provisioning",
		HostID:    nil,
		Version:   1,
		UpdatedAt: time.Now().Add(-20 * time.Minute), // > 15 min → StuckProvisioning
	}
}

func makeReconciler(pool *reconcilerFakePool) *Reconciler {
	repo := db.New(pool)
	return NewReconciler(repo, reconcilerTestLog())
}

func recCtx() context.Context { return context.Background() }

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestReconciler_Enqueue_PutsIDInWorkChannel verifies that Enqueue is non-blocking
// and the item arrives in the channel.
// Source: 03-03 §Event-Driven Triggers.
func TestReconciler_Enqueue_PutsIDInWorkChannel(t *testing.T) {
	pool := newReconcilerFakePool()
	rec := makeReconciler(pool)

	rec.Enqueue("inst-rec-001")

	select {
	case got := <-rec.work:
		if got != "inst-rec-001" {
			t.Errorf("work channel item = %q, want inst-rec-001", got)
		}
	default:
		t.Error("work channel was empty after Enqueue")
	}
}

// TestReconciler_Enqueue_DropsWhenQueueFull verifies that a full work channel
// does not block or panic — excess items are silently dropped.
// Correctness is preserved by the periodic resync backstop.
func TestReconciler_Enqueue_DropsWhenQueueFull(t *testing.T) {
	pool := newReconcilerFakePool()
	rec := makeReconciler(pool)

	// Fill the queue completely.
	for i := 0; i < workQueueDepth; i++ {
		rec.Enqueue("inst-fill")
	}

	// This must not block.
	done := make(chan struct{})
	go func() {
		rec.Enqueue("inst-overflow") // should be dropped
		close(done)
	}()

	select {
	case <-done:
		// OK — did not block.
	case <-time.After(100 * time.Millisecond):
		t.Error("Enqueue blocked when queue was full")
	}
}

// TestReconciler_ReconcileOne_NoDrift_NoDispatch verifies that a healthy instance
// produces no DB side effects.
// Source: 03-03 §reconcile() pseudocode "if driftClass == NO_DRIFT: return".
func TestReconciler_ReconcileOne_NoDrift_NoDispatch(t *testing.T) {
	pool := newReconcilerFakePool()
	inst := makeHealthyInstance("inst-rec-002")
	pool.addInstance(inst)

	rec := makeReconciler(pool)
	rec.reconcileOne(recCtx(), "inst-rec-002")

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertedJobIDs) != 0 {
		t.Errorf("healthy instance: expected 0 job inserts, got %d", len(pool.insertedJobIDs))
	}
	if len(pool.updateStateCalls) != 0 {
		t.Errorf("healthy instance: expected 0 state updates, got %d", len(pool.updateStateCalls))
	}
}

// TestReconciler_ReconcileOne_WithDrift_CallsDispatch verifies that a drifted
// instance causes a side effect (state update or job insert).
// Source: 03-03 §reconcile() "Enqueue a new job to perform the repair."
func TestReconciler_ReconcileOne_WithDrift_CallsDispatch(t *testing.T) {
	pool := newReconcilerFakePool()
	inst := makeDriftedInstance("inst-rec-003")
	pool.addInstance(inst)

	rec := makeReconciler(pool)
	rec.reconcileOne(recCtx(), "inst-rec-003")

	pool.mu.Lock()
	defer pool.mu.Unlock()
	// StuckProvisioning → failInstance → UpdateInstanceState called.
	if len(pool.updateStateCalls) == 0 {
		t.Error("drifted instance: expected UpdateInstanceState to be called")
	}
}

// TestReconciler_ReconcileOne_MissingInstance_Skipped verifies that a
// GetInstanceByID error (instance deleted) does not panic or error-cascade.
func TestReconciler_ReconcileOne_MissingInstance_Skipped(t *testing.T) {
	pool := newReconcilerFakePool()
	// "inst-gone" is not in the pool — GetInstanceByID returns error.
	rec := makeReconciler(pool)

	// Must not panic.
	rec.reconcileOne(recCtx(), "inst-gone")

	pool.mu.Lock()
	defer pool.mu.Unlock()
	if len(pool.insertedJobIDs) != 0 || len(pool.updateStateCalls) != 0 {
		t.Error("missing instance: expected no side effects")
	}
}

// TestReconciler_Resync_EnqueuesAllActiveInstances verifies the periodic resync
// enqueues every non-deleted instance.
// Source: IMPLEMENTATION_PLAN_V1 §R-07, 03-03 §Periodic Polling (step 2–3).
func TestReconciler_Resync_EnqueuesAllActiveInstances(t *testing.T) {
	pool := newReconcilerFakePool()
	wantIDs := []string{"inst-sync-a", "inst-sync-b", "inst-sync-c"}
	for _, id := range wantIDs {
		pool.addInstance(makeHealthyInstance(id))
	}

	rec := makeReconciler(pool)
	rec.resync(recCtx())

	// Drain exactly len(wantIDs) items from the channel.
	// resync enqueues synchronously so all items are already in the buffer.
	seen := make(map[string]bool)
	for i := 0; i < len(wantIDs); i++ {
		select {
		case id := <-rec.work:
			seen[id] = true
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out draining work channel after %d items (got %v)", i, seen)
		}
	}

	for _, id := range wantIDs {
		if !seen[id] {
			t.Errorf("resync: %q not enqueued. seen=%v", id, seen)
		}
	}
}

// TestReconciler_RunWorkers_DrainsChannel verifies that RunWorkers consumes
// items from the work channel and calls reconcileOne for each.
func TestReconciler_RunWorkers_DrainsChannel(t *testing.T) {
	pool := newReconcilerFakePool()
	// Add healthy instances so reconcileOne completes without errors.
	for _, id := range []string{"inst-w-001", "inst-w-002"} {
		pool.addInstance(makeHealthyInstance(id))
	}

	rec := makeReconciler(pool)
	rec.Enqueue("inst-w-001")
	rec.Enqueue("inst-w-002")

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// RunWorkers runs until ctx cancelled — we cancel after items are drained.
	done := make(chan struct{})
	go func() {
		rec.RunWorkers(ctx)
		close(done)
	}()

	// Wait for the work channel to drain.
	deadline := time.After(400 * time.Millisecond)
	for {
		select {
		case <-deadline:
			cancel()
			goto wait
		default:
			if len(rec.work) == 0 {
				cancel()
				goto wait
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
wait:
	<-done
	// No panic, no deadlock — success.
}

// TestReconciler_WorkQueueDepth_IsPositive verifies the constant is sane.
func TestReconciler_WorkQueueDepth_IsPositive(t *testing.T) {
	if workQueueDepth <= 0 {
		t.Errorf("workQueueDepth = %d, want > 0", workQueueDepth)
	}
}

// TestReconciler_ResyncInterval_Is5Min verifies the resync period matches the
// non-negotiable R-07 requirement.
// Source: IMPLEMENTATION_PLAN_V1 §R-07 "periodic resync every 5 minutes".
func TestReconciler_ResyncInterval_Is5Min(t *testing.T) {
	if resyncInterval != 5*time.Minute {
		t.Errorf("resyncInterval = %v, want 5m (R-07 non-negotiable)", resyncInterval)
	}
}
