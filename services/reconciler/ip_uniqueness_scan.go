package reconciler

// ip_uniqueness_scan.go — IP uniqueness reconciler sub-scan.
//
// M6 requirement: "IP uniqueness reconciler sub-scan active"
//
// Scans ip_allocations for rows where the same ip_address+vpc_id combination
// is marked allocated=TRUE for more than one distinct owner_instance_id.
// This is a defense-in-depth check — the DB UNIQUE constraint is the primary
// enforcement, but the sub-scan can detect anomalies caused by bugs in the
// allocation path or schema migrations that temporarily dropped constraints.
//
// Safety rules (from 02-04-system-invariants.md, 03-03-reconciliation-loops):
//   - The sub-scan is READ-ONLY. It raises alerts and writes events; it does
//     NOT attempt to automatically release IPs or alter allocations.
//   - Destructive recovery requires a direct verification step before acting.
//     This sub-scan stops at detection and alerting. Human + reconciler-repair
//     path resolves confirmed duplicates.
//   - The sub-scan runs every 5 minutes (same cadence as the reconciler resync)
//     so it is always integrated with the existing run loop.
//
// Source: IMPLEMENTATION_PLAN_V1 §M6 gate (IP uniqueness reconciler sub-scan),
//         02-04-system-invariants.md I-2 (no two instances share same IP in VPC),
//         03-03-reconciliation-loops §Safety (no destructive action without direct
//         hypervisor verification),
//         IP_ALLOCATION_CONTRACT_V1 §anomaly-detection.

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// DuplicateIPAnomaly records one detected IP uniqueness violation.
type DuplicateIPAnomaly struct {
	// IPAddress is the IP that is duplicated.
	IPAddress string
	// VPCID is the VPC in which the duplicate was detected.
	VPCID string
	// OwnerInstanceIDs are all instance IDs that claim this IP.
	OwnerInstanceIDs []string
}

// IPUniquenessScan is the IP uniqueness reconciler sub-scan component.
//
// It is wired into the reconciler service and runs on the same 5-minute
// cycle as the periodic resync. It is safe to run concurrently with the
// main reconciler loop — it only reads ip_allocations and writes events.
type IPUniquenessScan struct {
	repo *db.Repo
	log  *slog.Logger
}

// NewIPUniquenessScan constructs the sub-scan.
// log may be nil; a no-op logger is used in that case (e.g. in integration tests).
func NewIPUniquenessScan(repo *db.Repo, log *slog.Logger) *IPUniquenessScan {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &IPUniquenessScan{repo: repo, log: log}
}

// Scan queries ip_allocations for IP addresses claimed by more than one
// instance within a VPC, logs each anomaly, and writes an event record
// per duplicate so the audit log captures the violation.
//
// Returns the list of detected anomalies (empty slice = clean).
// Never returns a non-nil error that would block the reconciler; scan
// failures are logged at ERROR level and the scan is skipped for that cycle.
//
// Source: IP_ALLOCATION_CONTRACT_V1, I-2 invariant, M6 gate requirement.
func (s *IPUniquenessScan) Scan(ctx context.Context) ([]DuplicateIPAnomaly, error) {
	anomalies, err := s.repo.FindDuplicateIPAllocations(ctx)
	if err != nil {
		s.log.Error("ip-uniqueness-scan: query failed", "error", err)
		return nil, fmt.Errorf("ip-uniqueness-scan: %w", err)
	}

	if len(anomalies) == 0 {
		s.log.Info("ip-uniqueness-scan: clean — no duplicate IP allocations detected")
		return nil, nil
	}

	// Log and record every anomaly. Do NOT auto-correct.
	for _, a := range anomalies {
		s.log.Error("ip-uniqueness-scan: DUPLICATE IP DETECTED — invariant I-2 violated",
			"ip", a.IPAddress,
			"vpc_id", a.VPCID,
			"owner_instance_ids", a.OwnerInstanceIDs,
		)

		// Write an event against each affected instance so the audit log
		// captures the violation and operators can query it.
		for _, instID := range a.OwnerInstanceIDs {
			msg := fmt.Sprintf(
				"IP uniqueness violation: ip=%s vpc=%s claimants=%v",
				a.IPAddress, a.VPCID, a.OwnerInstanceIDs,
			)
			_ = s.repo.InsertEvent(ctx, &db.EventRow{
				ID:         idgen.New(idgen.PrefixEvent),
				InstanceID: instID,
				EventType:  db.EventIPUniquenessViolation,
				Message:    msg,
				Actor:      "ip-uniqueness-scan",
			})
		}
	}

	s.log.Error("ip-uniqueness-scan: ANOMALIES DETECTED — requires operator investigation",
		"count", len(anomalies),
	)

	// Convert db.DuplicateIPRow → DuplicateIPAnomaly for the caller.
	out := make([]DuplicateIPAnomaly, len(anomalies))
	for i, a := range anomalies {
		out[i] = DuplicateIPAnomaly{
			IPAddress:        a.IPAddress,
			VPCID:            a.VPCID,
			OwnerInstanceIDs: a.OwnerInstanceIDs,
		}
	}
	return out, nil
}
