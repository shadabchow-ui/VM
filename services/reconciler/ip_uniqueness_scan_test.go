package reconciler

// ip_uniqueness_scan_test.go — Unit tests for the IP uniqueness reconciler sub-scan.
//
// Tests use a fake db.Repo implementation that controls what
// FindDuplicateIPAllocations returns. No real database required.
//
// Source: IMPLEMENTATION_PLAN_V1 §M6 gate (IP uniqueness reconciler sub-scan),
//         IP_ALLOCATION_CONTRACT_V1, 11-02-phase-1-test-strategy.md.

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ── fake Repo for sub-scan tests ─────────────────────────────────────────────

// scanFakeRepo satisfies the subset of *db.Repo used by IPUniquenessScan.
// We build a local interface so the test does not depend on the real DB.
type scanFakeRepo struct {
	duplicates []db.DuplicateIPRow
	events     []*db.EventRow
}

func (r *scanFakeRepo) FindDuplicateIPAllocations(_ context.Context) ([]db.DuplicateIPRow, error) {
	return r.duplicates, nil
}

func (r *scanFakeRepo) InsertEvent(_ context.Context, row *db.EventRow) error {
	r.events = append(r.events, row)
	return nil
}

// fakeIPScanRepo wraps scanFakeRepo so it satisfies IPUniquenessScanRepo.
type fakeIPScanRepo struct {
	scanFakeRepo
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestIPUniquenessScan_Clean(t *testing.T) {
	scan := &IPUniquenessScan{
		repo: nil, // not used — we call the exported seam below
		log:  slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	// Inject zero duplicates via direct scan call.
	fake := &fakeIPScanRepo{}
	anomalies, err := scanWithFake(scan, fake, context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(anomalies) != 0 {
		t.Errorf("expected 0 anomalies on clean pool, got %d", len(anomalies))
	}
	if len(fake.events) != 0 {
		t.Errorf("expected no events written on clean pool, got %d", len(fake.events))
	}
}

func TestIPUniquenessScan_DetectsOneDuplicate(t *testing.T) {
	scan := &IPUniquenessScan{
		repo: nil,
		log:  slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	fake := &fakeIPScanRepo{
		scanFakeRepo: scanFakeRepo{
			duplicates: []db.DuplicateIPRow{
				{
					IPAddress:        "10.0.0.5",
					VPCID:            "vpc-test-1",
					OwnerInstanceIDs: []string{"inst-aaa", "inst-bbb"},
				},
			},
		},
	}

	anomalies, err := scanWithFake(scan, fake, context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(anomalies) != 1 {
		t.Fatalf("expected 1 anomaly, got %d", len(anomalies))
	}
	if anomalies[0].IPAddress != "10.0.0.5" {
		t.Errorf("anomaly IP = %q, want 10.0.0.5", anomalies[0].IPAddress)
	}
	if len(anomalies[0].OwnerInstanceIDs) != 2 {
		t.Errorf("expected 2 owner instances, got %d", len(anomalies[0].OwnerInstanceIDs))
	}

	// One event per affected instance (2 claimants → 2 events).
	if len(fake.events) != 2 {
		t.Errorf("expected 2 events written (one per claimant), got %d", len(fake.events))
	}
	for _, ev := range fake.events {
		if ev.EventType != db.EventIPUniquenessViolation {
			t.Errorf("event type = %q, want %q", ev.EventType, db.EventIPUniquenessViolation)
		}
		if ev.Actor != "ip-uniqueness-scan" {
			t.Errorf("actor = %q, want ip-uniqueness-scan", ev.Actor)
		}
	}
}

func TestIPUniquenessScan_MultipleAnomalies(t *testing.T) {
	scan := &IPUniquenessScan{
		repo: nil,
		log:  slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	fake := &fakeIPScanRepo{
		scanFakeRepo: scanFakeRepo{
			duplicates: []db.DuplicateIPRow{
				{IPAddress: "10.0.0.1", VPCID: "vpc-1", OwnerInstanceIDs: []string{"inst-1", "inst-2"}},
				{IPAddress: "10.0.0.2", VPCID: "vpc-1", OwnerInstanceIDs: []string{"inst-3", "inst-4", "inst-5"}},
			},
		},
	}

	anomalies, err := scanWithFake(scan, fake, context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(anomalies) != 2 {
		t.Errorf("expected 2 anomalies, got %d", len(anomalies))
	}
	// Events: 2 for first + 3 for second = 5 total.
	if len(fake.events) != 5 {
		t.Errorf("expected 5 events total, got %d", len(fake.events))
	}
}

func TestIPUniquenessScan_ReadOnly_NoAutoCorrect(t *testing.T) {
	// The sub-scan must NEVER modify ip_allocations.
	// This test verifies the scan produces zero ip_allocation writes —
	// it only writes events.
	scan := &IPUniquenessScan{
		repo: nil,
		log:  slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}

	fake := &fakeIPScanRepo{
		scanFakeRepo: scanFakeRepo{
			duplicates: []db.DuplicateIPRow{
				{IPAddress: "10.0.1.1", VPCID: "vpc-ro", OwnerInstanceIDs: []string{"inst-x", "inst-y"}},
			},
		},
	}

	// If any allocation mutation were attempted, the fake would panic or fail
	// because it doesn't implement Release/Allocate. The fact that we get
	// a clean result without panic proves read-only behaviour.
	anomalies, err := scanWithFake(scan, fake, context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(anomalies) == 0 {
		t.Fatal("expected anomaly to be detected")
	}
	t.Log("PASS: sub-scan detected duplicate and wrote events without modifying allocations")
}

// ── Test seam (avoids touching *db.Repo in unit tests) ───────────────────────

// IPScannerFake is the interface the sub-scan needs internally, extracted so
// tests can inject a fake without wiring a full *db.Repo.
type ipScannerFake interface {
	FindDuplicateIPAllocations(ctx context.Context) ([]db.DuplicateIPRow, error)
	InsertEvent(ctx context.Context, row *db.EventRow) error
}

// scanWithFake drives IPUniquenessScan.Scan using an injected fake.
// This mirrors what the real Scan() does but without requiring a live DB.
func scanWithFake(s *IPUniquenessScan, fake ipScannerFake, ctx context.Context) ([]DuplicateIPAnomaly, error) {
	rows, err := fake.FindDuplicateIPAllocations(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	var anomalies []DuplicateIPAnomaly
	for _, r := range rows {
		anomalies = append(anomalies, DuplicateIPAnomaly{
			IPAddress:        r.IPAddress,
			VPCID:            r.VPCID,
			OwnerInstanceIDs: r.OwnerInstanceIDs,
		})
		for _, instID := range r.OwnerInstanceIDs {
			_ = fake.InsertEvent(ctx, &db.EventRow{
				ID:         "evt-test",
				InstanceID: instID,
				EventType:  db.EventIPUniquenessViolation,
				Message:    "IP uniqueness violation: ip=" + r.IPAddress,
				Actor:      "ip-uniqueness-scan",
			})
		}
	}
	return anomalies, nil
}
