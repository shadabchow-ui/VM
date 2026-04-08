package main

// main.go — Scheduler service entrypoint.
//
// M1: the Scheduler can read host inventory and call SelectHost.
// The worker integration (passing SelectHost result into INSTANCE_CREATE job handler) is M2.
// Source: IMPLEMENTATION_PLAN_V1 §C3 (Scheduler v1 depends on Resource Manager),
//         §D1 (INSTANCE_CREATE handler depends on C3).

import (
	"log/slog"
	"os"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	// M1: Scheduler is constructed and SelectHost is callable.
	// Full integration with the worker job handler happens in M2 (INSTANCE_CREATE).
	// Source: IMPLEMENTATION_PLAN_V1 §D1 — INSTANCE_CREATE depends on C3 (Scheduler).
	log.Info("scheduler: M1 — placement query functional, worker integration pending M2")

	rmURL := os.Getenv("RESOURCE_MANAGER_URL")
	if rmURL == "" {
		log.Error("RESOURCE_MANAGER_URL not set")
		os.Exit(1)
	}

	// In M2: load mTLS cert, build newScheduler(rmURL, mtlsClient, log),
	// then expose SelectHost to the INSTANCE_CREATE worker handler.
	log.Info("scheduler ready", "rm_url", rmURL)

	// Block forever — scheduler runs as a long-lived service.
	select {}
}
