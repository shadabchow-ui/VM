package reconciler

// service.go — Reconciler service entrypoint.
//
// Wires janitor + reconciler + periodic resync and runs all loops.
//
// Deployment: milestone-gated. This service MUST NOT be deployed to production
// before the M4 gate passes. Once M4 passes, BLOCK 4 (API cannot go to
// production until reconciler is active) is cleared.
//
// VM Job 5: added RunNetworkCleanupScanLoop for stale NIC detection.
//
// Source: IMPLEMENTATION_PLAN_V1 §BLOCK 4,
//         12-02-implementation-sequence §M4 gate.

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/observability"
)

// Run is the service entrypoint. Call from a main() after DB setup.
// Blocks until ctx is cancelled.
// VM Job 5: wired network_cleanup sub-scan alongside existing loops.
func Run(ctx context.Context, repo *db.Repo) {
	log := observability.New("info")

	janitor := NewJanitor(repo, log)
	rec := NewReconciler(repo, log)
	ipScan := NewIPUniquenessScan(repo, log)
	netCleanupScan := NewNetworkCleanupScan(repo, log)

	go janitor.Run(ctx)
	go rec.RunPeriodicResync(ctx)
	go RunIPUniquenessScanLoop(ctx, ipScan, log)
	go RunNetworkCleanupScanLoop(ctx, netCleanupScan, log)

	rec.RunWorkers(ctx)
}

// ServiceMain is the standalone service entry for services/reconciler/cmd/main.go
// or equivalent. Reads DATABASE_URL from env.
func ServiceMain() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	log := observability.New("info")

	dbURL := db.DatabaseURL()
	pool, err := db.NewSQLPool(dbURL)
	if err != nil {
		log.Error("reconciler: failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	repo := db.New(pool)
	if err := repo.Ping(ctx); err != nil {
		log.Error("reconciler: database ping failed", "error", err)
		os.Exit(1)
	}

	log.Info("reconciler: connected to database")
	Run(ctx, repo)
}

// ── VM Job 3/5: Sub-scan loop runner ──────────────────────────────────────────

// RunNetworkCleanupScanLoop runs the stale NIC detection scan on each resync cycle.
func RunNetworkCleanupScanLoop(ctx context.Context, scan *NetworkCleanupScan, log *slog.Logger) {
	log.Info("network-cleanup-scan: loop started", "interval", resyncInterval)
	for {
		select {
		case <-ctx.Done():
			log.Info("network-cleanup-scan: loop stopped")
			return
		case <-time.After(resyncInterval):
		}
		result, err := scan.Scan(ctx)
		if err != nil {
			log.Error("network-cleanup-scan: scan cycle failed", "error", err)
		} else if result.StaleNICs > 0 {
			log.Warn("network-cleanup-scan: stale NICs found", "count", result.StaleNICs)
		}
	}
}
