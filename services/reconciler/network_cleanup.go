package reconciler

// network_cleanup.go — Detects and reports stale network state for deleted/stopped
// instances that may have leftover TAP devices, NAT rules, or SG chains on the host.
//
// This is a read-only detection scan (never mutates host state). It queries the DB
// for NICs whose owning instance is deleted but the NIC record remains active.
//
// The scan writes reconciliation events so operators can manually clean up.
// Full host-side cleanup requires the host-agent to be running on the target host,
// which is outside the scope of this detection-only loop.
//
// Uses the existing db.Repo.ListStaleNetworkInterfaces method (VM Job 5).
//
// Source: VM Job 3 — network cleanup reconciliation helper.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
)

// NetworkCleanupScan detects stale network resources that need operator attention.
// It runs as a sub-scan alongside the reconciler's periodic resync loop.
type NetworkCleanupScan struct {
	repo *db.Repo
	log  *slog.Logger
}

// NewNetworkCleanupScan constructs the scan helper.
func NewNetworkCleanupScan(repo *db.Repo, log *slog.Logger) *NetworkCleanupScan {
	return &NetworkCleanupScan{repo: repo, log: log}
}

// NetworkCleanupResult holds the findings of a single scan cycle.
type NetworkCleanupResult struct {
	StaleNICs         int
	ErrorsEncountered int
}

// Scan performs one detection cycle. It does NOT mutate any DB or host state.
// Returns the scan result and any error from the DB queries.
func (s *NetworkCleanupScan) Scan(ctx context.Context) (*NetworkCleanupResult, error) {
	result := &NetworkCleanupResult{}

	stale, err := s.repo.ListStaleNetworkInterfaces(ctx)
	if err != nil {
		s.log.Error("network_cleanup: ListStaleNetworkInterfaces failed", "error", err)
		result.ErrorsEncountered++
		return result, fmt.Errorf("ListStaleNetworkInterfaces: %w", err)
	}

	result.StaleNICs = len(stale)
	for _, nic := range stale {
		s.log.Warn("network_cleanup: stale NIC detected on deleted instance",
			"nic_id", nic.NICID,
			"instance_id", nic.InstanceID,
			"private_ip", nic.PrivateIP,
			"nic_status", nic.NICStatus,
			"instance_state", nic.InstanceState,
			"vpc_id", nic.VPCID,
		)
		_ = s.repo.InsertEvent(ctx, &db.EventRow{
			ID:         idgen.New(idgen.PrefixEvent),
			InstanceID: nic.InstanceID,
			EventType:  "network.stale_nic",
			Message: fmt.Sprintf("Stale NIC %s (IP %s) for deleted instance %s (state %s) — needs cleanup",
				nic.NICID, nic.PrivateIP, nic.InstanceID, nic.InstanceState),
			Actor: "reconciler.network_cleanup",
		})
	}

	s.log.Info("network_cleanup: scan complete",
		"stale_nics", result.StaleNICs,
	)

	return result, nil
}

// The RunNetworkCleanupScanLoop entrypoint is defined in service.go alongside
// the other sub-scan loop runners. It calls scan.Scan() on each resync cycle
// and logs results.
