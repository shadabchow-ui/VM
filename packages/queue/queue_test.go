package queue

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func zeroCommandTag() CommandTag {
	var tag CommandTag
	return tag
}

type fakeRow struct {
	values []any
	err    error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("scan arity mismatch")
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *string:
			v, ok := r.values[i].(string)
			if !ok {
				return errors.New("scan type mismatch: want string")
			}
			*d = v
		case *int:
			v, ok := r.values[i].(int)
			if !ok {
				return errors.New("scan type mismatch: want int")
			}
			*d = v
		default:
			return errors.New("unsupported scan destination")
		}
	}
	return nil
}

type fakeTx struct {
	row       Row
	execErr   error
	commitErr error

	execCalls int
	committed bool
	rolled    bool

	lastExecSQL  string
	lastExecArgs []any
}

func (tx *fakeTx) Exec(ctx context.Context, sql string, args ...any) (CommandTag, error) {
	tx.execCalls++
	tx.lastExecSQL = sql
	tx.lastExecArgs = args
	return zeroCommandTag(), tx.execErr
}

func (tx *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) Row {
	return tx.row
}

func (tx *fakeTx) Commit(ctx context.Context) error {
	tx.committed = true
	return tx.commitErr
}

func (tx *fakeTx) Rollback(ctx context.Context) error {
	tx.rolled = true
	return nil
}

type fakeTxPool struct {
	tx       *fakeTx
	beginErr error

	execCalls int

	lastExecSQL  string
	lastExecArgs []any
}

func (p *fakeTxPool) Begin(ctx context.Context) (Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return p.tx, nil
}

func (p *fakeTxPool) Exec(ctx context.Context, sql string, args ...any) (CommandTag, error) {
	p.execCalls++
	p.lastExecSQL = sql
	p.lastExecArgs = args
	return zeroCommandTag(), nil
}

func TestClaimNext_NoPendingJob_ReturnsNil(t *testing.T) {
	pool := &fakeTxPool{
		tx: &fakeTx{
			row: &fakeRow{err: errors.New("no rows in result set")},
		},
	}

	got, err := NewClaimer(pool).ClaimNext(context.Background())
	if err != nil {
		t.Fatalf("ClaimNext returned unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("ClaimNext = %#v, want nil", got)
	}
}

func TestClaimNext_ClaimsPendingJob(t *testing.T) {
	pool := &fakeTxPool{
		tx: &fakeTx{
			row: &fakeRow{
				values: []any{
					"job_123",
					"inst_456",
					"INSTANCE_CREATE",
					"idem_789",
					0,
				},
			},
		},
	}

	got, err := NewClaimer(pool).ClaimNext(context.Background())
	if err != nil {
		t.Fatalf("ClaimNext returned unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("ClaimNext returned nil job, want claimed job")
	}
	if got.ID != "job_123" || got.InstanceID != "inst_456" || got.JobType != "INSTANCE_CREATE" || got.IdempotencyKey != "idem_789" || got.AttemptCount != 0 {
		t.Fatalf("ClaimNext returned wrong job: %#v", got)
	}

	if pool.tx.execCalls != 1 {
		t.Fatalf("Exec calls = %d, want 1", pool.tx.execCalls)
	}
	if !strings.Contains(pool.tx.lastExecSQL, "SET status        = 'in_progress'") {
		t.Fatalf("claim update SQL did not set in_progress: %q", pool.tx.lastExecSQL)
	}
	if !pool.tx.committed {
		t.Fatal("transaction was not committed")
	}
	if !pool.tx.rolled {
		t.Fatal("deferred rollback was not invoked")
	}
	if len(pool.tx.lastExecArgs) != 2 {
		t.Fatalf("claim update args = %d, want 2", len(pool.tx.lastExecArgs))
	}
	if jobID, ok := pool.tx.lastExecArgs[0].(string); !ok || jobID != "job_123" {
		t.Fatalf("claim update first arg = %#v, want job_123", pool.tx.lastExecArgs[0])
	}
}

func TestClaimNext_BeginError(t *testing.T) {
	pool := &fakeTxPool{beginErr: errors.New("db unavailable")}

	got, err := NewClaimer(pool).ClaimNext(context.Background())
	if err == nil {
		t.Fatal("ClaimNext error = nil, want error")
	}
	if got != nil {
		t.Fatalf("ClaimNext returned %#v, want nil", got)
	}
	if !strings.Contains(err.Error(), "ClaimNext begin") {
		t.Fatalf("error = %q, want wrapped begin error", err.Error())
	}
}

func TestClaimNext_QueryError(t *testing.T) {
	pool := &fakeTxPool{
		tx: &fakeTx{
			row: &fakeRow{err: errors.New("query exploded")},
		},
	}

	got, err := NewClaimer(pool).ClaimNext(context.Background())
	if err == nil {
		t.Fatal("ClaimNext error = nil, want error")
	}
	if got != nil {
		t.Fatalf("ClaimNext returned %#v, want nil", got)
	}
	if !strings.Contains(err.Error(), "ClaimNext query") {
		t.Fatalf("error = %q, want wrapped query error", err.Error())
	}
}

func TestClaimNext_UpdateError(t *testing.T) {
	pool := &fakeTxPool{
		tx: &fakeTx{
			row: &fakeRow{
				values: []any{
					"job_123",
					"inst_456",
					"INSTANCE_CREATE",
					"idem_789",
					1,
				},
			},
			execErr: errors.New("update failed"),
		},
	}

	got, err := NewClaimer(pool).ClaimNext(context.Background())
	if err == nil {
		t.Fatal("ClaimNext error = nil, want error")
	}
	if got != nil {
		t.Fatalf("ClaimNext returned %#v, want nil", got)
	}
	if !strings.Contains(err.Error(), "ClaimNext update") {
		t.Fatalf("error = %q, want wrapped update error", err.Error())
	}
}

func TestClaimNext_CommitError(t *testing.T) {
	pool := &fakeTxPool{
		tx: &fakeTx{
			row: &fakeRow{
				values: []any{
					"job_123",
					"inst_456",
					"INSTANCE_CREATE",
					"idem_789",
					2,
				},
			},
			commitErr: errors.New("commit failed"),
		},
	}

	got, err := NewClaimer(pool).ClaimNext(context.Background())
	if err == nil {
		t.Fatal("ClaimNext error = nil, want error")
	}
	if got != nil {
		t.Fatalf("ClaimNext returned %#v, want nil", got)
	}
	if !strings.Contains(err.Error(), "ClaimNext commit") {
		t.Fatalf("error = %q, want wrapped commit error", err.Error())
	}
}

func TestComplete_UpdatesCompletedStatus(t *testing.T) {
	pool := &fakeTxPool{}

	if err := Complete(context.Background(), pool, "job_done"); err != nil {
		t.Fatalf("Complete returned unexpected error: %v", err)
	}

	if pool.execCalls != 1 {
		t.Fatalf("Exec calls = %d, want 1", pool.execCalls)
	}
	if !strings.Contains(pool.lastExecSQL, "SET status       = 'completed'") {
		t.Fatalf("complete SQL = %q, want completed update", pool.lastExecSQL)
	}
	if len(pool.lastExecArgs) != 2 {
		t.Fatalf("complete args = %d, want 2", len(pool.lastExecArgs))
	}
	if jobID, ok := pool.lastExecArgs[0].(string); !ok || jobID != "job_done" {
		t.Fatalf("complete first arg = %#v, want job_done", pool.lastExecArgs[0])
	}
}

func TestFail_UpdatesPendingOrDeadStatus(t *testing.T) {
	pool := &fakeTxPool{}

	if err := Fail(context.Background(), pool, "job_fail", "boom"); err != nil {
		t.Fatalf("Fail returned unexpected error: %v", err)
	}

	if pool.execCalls != 1 {
		t.Fatalf("Exec calls = %d, want 1", pool.execCalls)
	}
	if !strings.Contains(pool.lastExecSQL, "WHEN attempt_count >= max_attempts THEN 'dead'") {
		t.Fatalf("fail SQL = %q, want dead/pending transition", pool.lastExecSQL)
	}
	if len(pool.lastExecArgs) != 3 {
		t.Fatalf("fail args = %d, want 3", len(pool.lastExecArgs))
	}
	if jobID, ok := pool.lastExecArgs[0].(string); !ok || jobID != "job_fail" {
		t.Fatalf("fail first arg = %#v, want job_fail", pool.lastExecArgs[0])
	}
	if msg, ok := pool.lastExecArgs[1].(string); !ok || msg != "boom" {
		t.Fatalf("fail second arg = %#v, want boom", pool.lastExecArgs[1])
	}
}
