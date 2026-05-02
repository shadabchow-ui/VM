package main

// heartbeat.go — periodic Host Agent inventory heartbeat to the Resource Manager.
//
// Source: RUNTIMESERVICE_GRPC_V1 §8 (heartbeat every 30s),
//         05-02-host-runtime-worker-design.md §Worker Heartbeating.
//
// Interval: 30s. Staleness window in Resource Manager: 90s (3 × interval).
// On failure: log warn, continue — never crash.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const heartbeatInterval = 30 * time.Second

type heartbeatPayload struct {
	UsedCPU      int    `json:"used_cpu"`
	UsedMemoryMB int    `json:"used_memory_mb"`
	UsedDiskGB   int    `json:"used_disk_gb"`
	AgentVersion string `json:"agent_version"`
}

// HeartbeatLoop sends a heartbeat every 30 seconds.
// Stops cleanly on ctx cancellation (SIGTERM path).
func HeartbeatLoop(ctx context.Context, cfg agentConfig, client *http.Client, log *slog.Logger) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	log.Info("heartbeat loop started", "interval", heartbeatInterval)

	// Send immediately so the host appears ready without waiting 30s.
	sendHeartbeat(ctx, cfg, client, log)

	for {
		select {
		case <-ctx.Done():
			log.Info("heartbeat loop stopped")
			return
		case <-ticker.C:
			sendHeartbeat(ctx, cfg, client, log)
		}
	}
}

func sendHeartbeat(ctx context.Context, cfg agentConfig, client *http.Client, log *slog.Logger) {
	payload := heartbeatPayload{
		UsedCPU:      measureUsedCPU(),
		UsedMemoryMB: measureUsedMemoryMB(),
		UsedDiskGB:   measureUsedDiskGB(),
		AgentVersion: cfg.AgentVersion,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		log.Warn("heartbeat: marshal error", "error", err)
		return
	}

	url := fmt.Sprintf("%s/internal/v1/hosts/%s/heartbeat", cfg.ResourceManagerURL, cfg.HostID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		log.Warn("heartbeat: build request error", "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		log.Warn("heartbeat: send error", "host_id", cfg.HostID, "error", err)
		return
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if resp.StatusCode == http.StatusNoContent {
		log.Debug("heartbeat ok", "host_id", cfg.HostID)
		return
	}
	log.Warn("heartbeat: unexpected status", "host_id", cfg.HostID, "status", resp.StatusCode)
}

// measureUsedCPU returns current CPU core usage.
// Phase 1: return 0 (utilization tracking is M4). Phase 2: read /proc/stat delta.
func measureUsedCPU() int { return 0 }

// measureUsedMemoryMB reads MemTotal - MemAvailable from /proc/meminfo.
func measureUsedMemoryMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var total, available int
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			fmt.Sscanf(strings.TrimPrefix(line, "MemTotal:"), "%d", &total)
		case strings.HasPrefix(line, "MemAvailable:"):
			fmt.Sscanf(strings.TrimPrefix(line, "MemAvailable:"), "%d", &available)
		}
	}
	if total == 0 {
		return 0
	}
	return (total - available) / 1024 // kB → MB
}

// measureUsedDiskGB returns current disk usage.
// Phase 1: return 0. Phase 2: syscall.Statfs("/").
func measureUsedDiskGB() int { return 0 }
