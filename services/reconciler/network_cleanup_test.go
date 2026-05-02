package reconciler

// network_cleanup_test.go — Unit tests for the network cleanup detection scan.
//
// Uses a fake pool that returns configurable StaleNICRow slices to verify:
//   1. Clean pool → zero stale NICs.
//   2. Stale NICs detected → events written.
//   3. Error propagation from DB failures.
//
// No PostgreSQL or Linux/KVM required.
// Source: VM Job 3 — network cleanup tests.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
)

// networkCleanupFakePool implements db.Pool for network cleanup scan tests.
// Serves ListStaleNetworkInterfaces and captures InsertEvent calls.
type networkCleanupFakePool struct {
	mu sync.Mutex

	// stale NICs to return
	staleNICs []*db.StaleNICRow
	staleErr  error

	// captured events
	events []*db.EventRow
}

func newNetworkCleanupFakePool() *networkCleanupFakePool {
	return &networkCleanupFakePool{}
}

func (p *networkCleanupFakePool) Exec(_ context.Context, query string, args ...any) (db.CommandTag, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if strings.Contains(query, "INSERT INTO instance_events") {
		event := parseEventFromArgs(args)
		if event != nil {
			p.events = append(p.events, event)
		}
	}
	return &cleanupTag{rows: 1}, nil
}

func (p *networkCleanupFakePool) QueryRow(_ context.Context, query string, args ...any) db.Row {
	return &cleanupNoRow{}
}

func (p *networkCleanupFakePool) Query(_ context.Context, query string, args ...any) (db.Rows, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.staleErr != nil {
		return nil, p.staleErr
	}
	return &cleanupRows{staleNICs: p.staleNICs}, nil
}

func (p *networkCleanupFakePool) Close() {}

func parseEventFromArgs(args []any) *db.EventRow {
	if len(args) < 6 {
		return nil
	}
	event := &db.EventRow{}
	if id, ok := args[0].(string); ok {
		event.ID = id
	}
	if instanceID, ok := args[1].(string); ok {
		event.InstanceID = instanceID
	}
	if eventType, ok := args[2].(string); ok {
		event.EventType = eventType
	}
	if msg, ok := args[3].(string); ok {
		event.Message = msg
	}
	if actor, ok := args[4].(string); ok {
		event.Actor = actor
	}
	// args[5] is details (JSONB) — we ignore it here.
	return event
}

// ── fake Rows / Row / Tag implementations ──────────────────────────────────────

type cleanupRows struct {
	staleNICs []*db.StaleNICRow
	pos       int
}

func (r *cleanupRows) Next() bool {
	r.pos++
	return r.pos <= len(r.staleNICs)
}

func (r *cleanupRows) Scan(dest ...any) error {
	if r.pos-1 >= len(r.staleNICs) {
		return nil
	}
	nic := r.staleNICs[r.pos-1]
	for i, d := range dest {
		switch i {
		case 0:
			*(d.(*string)) = nic.NICID
		case 1:
			*(d.(*string)) = nic.InstanceID
		case 2:
			*(d.(*string)) = nic.SubnetID
		case 3:
			*(d.(*string)) = nic.VPCID
		case 4:
			*(d.(*string)) = nic.PrivateIP
		case 5:
			*(d.(*string)) = nic.NICStatus
		case 6:
			*(d.(*string)) = nic.InstanceState
		}
	}
	return nil
}

func (r *cleanupRows) Close()     {}
func (r *cleanupRows) Err() error { return nil }

type cleanupTag struct{ rows int64 }

func (t *cleanupTag) RowsAffected() int64 { return t.rows }

type cleanupNoRow struct{}

func (r *cleanupNoRow) Scan(...any) error { return nil }

// ── Tests ─────────────────────────────────────────────────────────────────────

func newTestNetworkCleanupScan(pool db.Pool) *NetworkCleanupScan {
	repo := db.New(pool)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return NewNetworkCleanupScan(repo, log)
}

func TestNetworkCleanup_CleanPool_NoStaleNICs(t *testing.T) {
	pool := newNetworkCleanupFakePool()
	scan := newTestNetworkCleanupScan(pool)
	ctx := context.Background()

	result, err := scan.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.StaleNICs != 0 {
		t.Errorf("StaleNICs = %d, want 0", result.StaleNICs)
	}
	if len(pool.events) != 0 {
		t.Errorf("events count = %d, want 0 for clean pool", len(pool.events))
	}
}

func TestNetworkCleanup_DetectsStaleNIC(t *testing.T) {
	pool := newNetworkCleanupFakePool()
	pool.staleNICs = []*db.StaleNICRow{
		{
			NICID:         "nic_stale01",
			InstanceID:    "inst_del01",
			SubnetID:      "subnet_001",
			VPCID:         "vpc_001",
			PrivateIP:     "10.0.1.5",
			NICStatus:     "attached",
			InstanceState: "deleted",
		},
	}
	scan := newTestNetworkCleanupScan(pool)
	ctx := context.Background()

	result, err := scan.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.StaleNICs != 1 {
		t.Errorf("StaleNICs = %d, want 1", result.StaleNICs)
	}
	if len(pool.events) != 1 {
		t.Errorf("events count = %d, want 1 (should emit event for stale NIC)", len(pool.events))
	}
	if pool.events[0].EventType != "network.stale_nic" {
		t.Errorf("event type = %q, want network.stale_nic", pool.events[0].EventType)
	}
	if pool.events[0].InstanceID != "inst_del01" {
		t.Errorf("event instance = %q, want inst_del01", pool.events[0].InstanceID)
	}
}

func TestNetworkCleanup_MultipleStaleNICs(t *testing.T) {
	pool := newNetworkCleanupFakePool()
	pool.staleNICs = []*db.StaleNICRow{
		{NICID: "nic_01", InstanceID: "inst_a", PrivateIP: "10.0.0.1", NICStatus: "attached", InstanceState: "deleted"},
		{NICID: "nic_02", InstanceID: "inst_b", PrivateIP: "10.0.0.2", NICStatus: "pending", InstanceState: "deleted"},
		{NICID: "nic_03", InstanceID: "inst_c", PrivateIP: "10.0.0.3", NICStatus: "attached", InstanceState: "deleted"},
	}
	scan := newTestNetworkCleanupScan(pool)
	ctx := context.Background()

	result, err := scan.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.StaleNICs != 3 {
		t.Errorf("StaleNICs = %d, want 3", result.StaleNICs)
	}
	if len(pool.events) != 3 {
		t.Errorf("events count = %d, want 3", len(pool.events))
	}
}

func TestNetworkCleanup_DBError(t *testing.T) {
	pool := newNetworkCleanupFakePool()
	pool.staleErr = fmt.Errorf("connection refused")

	scan := newTestNetworkCleanupScan(pool)
	ctx := context.Background()

	_, err := scan.Scan(ctx)
	if err == nil {
		t.Fatal("expected error from DB failure")
	}
}

func TestNetworkCleanup_ZeroErrorsOnSuccess(t *testing.T) {
	pool := newNetworkCleanupFakePool()
	pool.staleNICs = []*db.StaleNICRow{
		{NICID: "nic_s1", InstanceID: "inst_x", PrivateIP: "10.0.0.99", NICStatus: "attached", InstanceState: "deleted"},
	}
	scan := newTestNetworkCleanupScan(pool)
	ctx := context.Background()

	result, err := scan.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if result.ErrorsEncountered != 0 {
		t.Errorf("ErrorsEncountered = %d, want 0 on success", result.ErrorsEncountered)
	}
}
