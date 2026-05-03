package main

// placement.go — Scheduler host selection for VM placement.
//
// M1 scope: read host inventory from Resource Manager HTTP API; return best-fit host.
// This is PLACEMENT INPUT only. Full scheduling (AZ spread, anti-affinity) is M3.
// The worker does not call SelectHost in M1 — wired in M2.
//
// Source: IMPLEMENTATION_PLAN_V1 §C3 (Scheduler v1: depends on Resource Manager),
//         12-02-implementation-sequence §M3 (dynamic VM placement gate).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// ErrNoCapacity is returned when no available host can satisfy the resource request.
var ErrNoCapacity = errors.New("no host with sufficient capacity available")

// HostSummary mirrors the JSON shape returned by GET /internal/v1/hosts.
type HostSummary struct {
	ID               string `json:"id"`
	AvailabilityZone string `json:"availability_zone"`
	Status           string `json:"status"`
	FenceRequired    bool   `json:"fence_required"`
	TotalCPU         int    `json:"total_cpu"`
	TotalMemoryMB    int    `json:"total_memory_mb"`
	TotalDiskGB      int    `json:"total_disk_gb"`
	UsedCPU          int    `json:"used_cpu"`
	UsedMemoryMB     int    `json:"used_memory_mb"`
	UsedDiskGB       int    `json:"used_disk_gb"`
}

func (h *HostSummary) AvailableCPU() int      { return h.TotalCPU - h.UsedCPU }
func (h *HostSummary) AvailableMemoryMB() int { return h.TotalMemoryMB - h.UsedMemoryMB }
func (h *HostSummary) AvailableDiskGB() int   { return h.TotalDiskGB - h.UsedDiskGB }

// CanFit reports whether the host has enough free resources AND matches the
// requested availability zone (if az is non-empty).
//
// VM-P2E Slice 1: draining, drained, degraded, unhealthy, fenced, retired, offline,
// and maintenance hosts are ALL excluded from placement. Only status=ready qualifies.
//
// VM Job 9: added fence_required defense-in-depth check. Even if a host somehow
// appears with status=ready AND fence_required=true, placement is denied.
// The DB query (GetAvailableHosts) already excludes non-ready hosts, but the
// scheduler double-checks here so no single layer failure can bypass the gate.
//
// VM-ADMISSION-SCHEDULER-RBAC-PHASE-G-H: added az parameter for AZ-filtered placement.
// When az is non-empty, only hosts in that AZ are considered.
//
// Source: vm-13-03__blueprint__ §core_contracts "Host State Atomicity" (drain must be
//
//	immediately visible to scheduler), 05-02 §Placement.
func (h *HostSummary) CanFit(cpuCores, memoryMB, diskGB int, az string) bool {
	// Only status=ready hosts receive new placements.
	// Draining hosts that were ready before the drain command must stop receiving
	// new VMs immediately — the status change is the admission gate.
	if h.Status != "ready" {
		return false
	}
	// Defense-in-depth: fence_required=TRUE hosts must never receive placements.
	// The DB GetAvailableHosts query only returns status=ready hosts; fence_required
	// should be FALSE for ready hosts, but this check protects against any edge case
	// where a host could be ready AND fence_required simultaneously.
	if h.FenceRequired {
		return false
	}
	// AZ filtering: when az is specified, only match hosts in that zone.
	if az != "" && h.AvailabilityZone != az {
		return false
	}
	return h.AvailableCPU() >= cpuCores &&
		h.AvailableMemoryMB() >= memoryMB &&
		h.AvailableDiskGB() >= diskGB
}

// Scheduler provides host placement decisions by querying the Resource Manager.
type Scheduler struct {
	rmURL  string
	client *http.Client // mTLS client authenticated to Resource Manager
	log    *slog.Logger
}

func newScheduler(rmURL string, client *http.Client, log *slog.Logger) *Scheduler {
	return &Scheduler{rmURL: rmURL, client: client, log: log}
}

// SelectHost returns the best available host for the given resource requirements.
// Strategy (Phase 1): first-fit descending by free CPU.
// Resource Manager returns hosts pre-sorted by (total_cpu - used_cpu) DESC so
// the first host satisfying CanFit is the correct selection.
//
// az: availability zone filter. Pass empty string to accept any AZ.
//
// Returns ErrNoCapacity if no ready host satisfies the request.
// Source: IMPLEMENTATION_PLAN_V1 §C3.
func (s *Scheduler) SelectHost(ctx context.Context, cpuCores, memoryMB, diskGB int, az string) (*HostSummary, error) {
	hosts, err := s.fetchAvailableHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("SelectHost: %w", err)
	}
	for _, h := range hosts {
		if h.CanFit(cpuCores, memoryMB, diskGB, az) {
			s.log.Info("host selected",
				"host_id", h.ID,
				"az", h.AvailabilityZone,
				"free_cpu", h.AvailableCPU(),
				"free_mem_mb", h.AvailableMemoryMB(),
				"req_cpu", cpuCores,
				"req_mem_mb", memoryMB,
			)
			return h, nil
		}
	}
	s.log.Warn("no capacity",
		"req_cpu", cpuCores, "req_mem_mb", memoryMB, "req_disk_gb", diskGB,
		"req_az", az,
		"candidates", len(hosts),
	)
	return nil, ErrNoCapacity
}

// fetchAvailableHosts calls GET /internal/v1/hosts on the Resource Manager.
// The Resource Manager applies the 90-second heartbeat staleness filter and
// returns only status=ready hosts.
func (s *Scheduler) fetchAvailableHosts(ctx context.Context) ([]*HostSummary, error) {
	url := s.rmURL + "/internal/v1/hosts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(body))
	}
	var out struct {
		Hosts []*HostSummary `json:"hosts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return out.Hosts, nil
}
