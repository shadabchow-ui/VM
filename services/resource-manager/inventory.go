package main

// inventory.go — HostInventory service: business logic between HTTP handlers and the DB repo.
//
// Source: IMPLEMENTATION_PLAN_V1 §B2 (Resource Manager v1),
//         05-02-host-runtime-worker-design.md §Bootstrap + §Heartbeating,
//         AUTH_OWNERSHIP_MODEL_V1 §6 (bootstrap token lifecycle).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/compute-platform/compute-platform/internal/db"
)

// ErrNoCapacity is returned by SelectHost when no ready host fits the requested shape.
var ErrNoCapacity = errors.New("no host with sufficient capacity available")

// RegisterRequest is the payload sent by the Host Agent on startup registration.
type RegisterRequest struct {
	HostID           string `json:"host_id"`
	AvailabilityZone string `json:"availability_zone"`
	TotalCPU         int    `json:"total_cpu"`
	TotalMemoryMB    int    `json:"total_memory_mb"`
	TotalDiskGB      int    `json:"total_disk_gb"`
	AgentVersion     string `json:"agent_version"`
}

// HeartbeatRequest is the periodic utilization update from the Host Agent.
// Source: RUNTIMESERVICE_GRPC_V1 §8 (30s interval).
type HeartbeatRequest struct {
	UsedCPU      int    `json:"used_cpu"`
	UsedMemoryMB int    `json:"used_memory_mb"`
	UsedDiskGB   int    `json:"used_disk_gb"`
	AgentVersion string `json:"agent_version"`
}

// HostInventory manages host registration and inventory tracking.
type HostInventory struct {
	repo *db.Repo
}

func newHostInventory(repo *db.Repo) *HostInventory {
	return &HostInventory{repo: repo}
}

// Register handles Host Agent startup registration.
// certHostID is extracted from the mTLS cert CN — already verified by middleware.
// Idempotent: re-registration after crash sets status=ready again.
// Source: 05-02-host-runtime-worker-design.md §Bootstrap step 8.
func (s *HostInventory) Register(ctx context.Context, certHostID string, req *RegisterRequest) error {
	if certHostID != req.HostID {
		return fmt.Errorf("cert host_id %q does not match payload host_id %q", certHostID, req.HostID)
	}
	if err := validateRegisterRequest(req); err != nil {
		return err
	}
	rec := &db.HostRecord{
		ID:               req.HostID,
		AvailabilityZone: req.AvailabilityZone,
		TotalCPU:         req.TotalCPU,
		TotalMemoryMB:    req.TotalMemoryMB,
		TotalDiskGB:      req.TotalDiskGB,
		AgentVersion:     req.AgentVersion,
	}
	return s.repo.UpsertHost(ctx, rec)
}

// Heartbeat applies a periodic utilization update from an authenticated host.
// Source: RUNTIMESERVICE_GRPC_V1 §8.
func (s *HostInventory) Heartbeat(ctx context.Context, hostID string, req *HeartbeatRequest) error {
	if hostID == "" {
		return errors.New("heartbeat: host_id required")
	}
	return s.repo.UpdateHeartbeat(ctx, hostID, req.UsedCPU, req.UsedMemoryMB, req.UsedDiskGB, req.AgentVersion)
}

// GetAvailableHosts returns all ready, recently-heartbeating hosts.
// Consumed by SelectHost and exposed via GET /internal/v1/hosts for the scheduler.
func (s *HostInventory) GetAvailableHosts(ctx context.Context) ([]*db.HostRecord, error) {
	return s.repo.GetAvailableHosts(ctx)
}

// SelectHost returns the best available host for the given resource requirements.
// Phase 1 strategy: first-fit descending by free CPU (hosts pre-sorted by query).
// Full AZ-aware scheduling is M3. Source: IMPLEMENTATION_PLAN_V1 §C3.
func (s *HostInventory) SelectHost(ctx context.Context, cpuCores, memoryMB, diskGB int) (*db.HostRecord, error) {
	hosts, err := s.repo.GetAvailableHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("SelectHost: %w", err)
	}
	for _, h := range hosts {
		if h.CanFit(cpuCores, memoryMB, diskGB) {
			return h, nil
		}
	}
	return nil, ErrNoCapacity
}

// ConsumeBootstrapToken validates and atomically consumes a one-time token.
// tokenRaw is plaintext from the Host Agent; hashed here before DB lookup.
// Source: AUTH_OWNERSHIP_MODEL_V1 §6.
func (s *HostInventory) ConsumeBootstrapToken(ctx context.Context, tokenRaw string) (string, error) {
	return s.repo.ConsumeBootstrapToken(ctx, sha256hex(tokenRaw))
}

// IssueBootstrapToken creates a new 1-hour token for a host being provisioned.
// Called by the internal CLI. Returns the raw token — shown once, never stored.
func (s *HostInventory) IssueBootstrapToken(ctx context.Context, hostID string) (string, error) {
	rawToken, err := generateRawToken()
	if err != nil {
		return "", fmt.Errorf("IssueBootstrapToken: %w", err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	if err := s.repo.InsertBootstrapToken(ctx, sha256hex(rawToken), hostID, expiresAt); err != nil {
		return "", fmt.Errorf("IssueBootstrapToken: %w", err)
	}
	return rawToken, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func validateRegisterRequest(req *RegisterRequest) error {
	if req.HostID == "" {
		return errors.New("host_id required")
	}
	if req.AvailabilityZone == "" {
		return errors.New("availability_zone required")
	}
	if req.TotalCPU <= 0 {
		return errors.New("total_cpu must be > 0")
	}
	if req.TotalMemoryMB <= 0 {
		return errors.New("total_memory_mb must be > 0")
	}
	if req.TotalDiskGB <= 0 {
		return errors.New("total_disk_gb must be > 0")
	}
	return nil
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func generateRawToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
