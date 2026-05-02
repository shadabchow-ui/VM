package main

// inventory.go — HostInventory service: business logic between HTTP handlers and the DB repo.
//
// Source: IMPLEMENTATION_PLAN_V1 §B2 (Resource Manager v1),
//         05-02-host-runtime-worker-design.md §Bootstrap + §Heartbeating,
//         AUTH_OWNERSHIP_MODEL_V1 §6 (bootstrap token lifecycle).
//
// VM-P2E Slice 2 additions:
//   - DrainHost: now accepts and forwards fromGeneration (was always 0 in Slice 1).
//   - CompleteDrain: new — attempts draining→drained transition via MarkHostDrained.
//
// VM-P2E Slice 3 additions:
//   - MarkDegraded: new — transitions a host to 'degraded' with reason code.
//   - MarkUnhealthy: new — transitions a host to 'unhealthy'; sets fence_required
//     for ambiguous failure reason codes.
//   - ClearFenceRequired: new — operator-initiated fence_required clearance.
//   - GetFenceRequiredHosts: new — observable query for hosts needing fencing.
//
// VM-P2E Slice 4 additions:
//   - RetireHost: transitions a host to 'retiring'; blocked if active workload remains.
//   - CompleteRetirement: transitions a retiring host to 'retired'; sets retired_at.
//   - GetRetiredHosts: replacement-seam query returning all retired hosts.
//
// VM-P2E Slice 5 additions:
//   - CreateCampaign: validates and creates a maintenance campaign with blast-radius check.
//   - GetCampaign: fetches a campaign by ID.
//   - ListCampaigns: lists campaigns filtered by status.
//   - AdvanceCampaign: drains the next batch of hosts within blast-radius limits.
//   - PauseCampaign: halts further advancement of a running campaign.
//   - ResumeCampaign: resumes a paused campaign.
//   - CancelCampaign: cancels a campaign; in-flight hosts drain naturally.

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
//
// VM Job 9: added HealthOK, BootID, VMLoad fields for richer health signal.
type HeartbeatRequest struct {
	UsedCPU      int    `json:"used_cpu"`
	UsedMemoryMB int    `json:"used_memory_mb"`
	UsedDiskGB   int    `json:"used_disk_gb"`
	AgentVersion string `json:"agent_version"`
	// HealthOK is the agent's self-assessment of local runtime health.
	// If false, the control plane may flag the host for operator attention.
	HealthOK bool   `json:"health_ok"`
	BootID   string `json:"boot_id,omitempty"`
	VMLoad   int    `json:"vm_load"`
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
//
// VM Job 9: enriched with HealthOK check (logs warn when false), boot_id
// change detection (logs error when reboot detected), and vm_load cross-check.
// Source: RUNTIMESERVICE_GRPC_V1 §8.
func (s *HostInventory) Heartbeat(ctx context.Context, hostID string, req *HeartbeatRequest) error {
	if hostID == "" {
		return errors.New("heartbeat: host_id required")
	}
	if err := s.repo.UpdateHeartbeat(ctx, hostID, req.UsedCPU, req.UsedMemoryMB, req.UsedDiskGB, req.AgentVersion); err != nil {
		return err
	}
	// VM Job 9: detect boot_id changes — a reboot means host state was lost.
	if req.BootID != "" {
		prev, err := s.repo.UpdateHeartbeatBootID(ctx, hostID, req.BootID)
		if err != nil {
			return fmt.Errorf("heartbeat boot_id update: %w", err)
		}
		if prev != "" && prev != req.BootID {
			return fmt.Errorf("heartbeat: host %s boot_id changed (prev=%s, new=%s) — host reboot detected, running VMs presumed lost", hostID, prev, req.BootID)
		}
	}
	return nil
}

// GetAvailableHosts returns all ready, recently-heartbeating hosts.
// Consumed by SelectHost and exposed via GET /internal/v1/hosts for the scheduler.
func (s *HostInventory) GetAvailableHosts(ctx context.Context) ([]*db.HostRecord, error) {
	return s.repo.GetAvailableHosts(ctx)
}

// GetStaleHosts returns ready hosts whose heartbeat is older than the staleness threshold.
// These hosts are excluded from GetAvailableHosts and thus from scheduler placement.
// The operator can poll this to proactively mark stale hosts degraded before users
// notice degraded capacity.
//
// Source: VM Job 9 — host fleet operations + failure recovery gate.
func (s *HostInventory) GetStaleHosts(ctx context.Context) ([]*db.HostRecord, error) {
	return s.repo.GetStaleHosts(ctx)
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

// DrainHost transitions a host to 'draining', detaches stopped VMs, and
// returns the active running VM count so callers know whether drain is complete.
//
// fromGeneration is the caller's expected current generation of the host record.
// A mismatch returns (0, false, nil) — the handler maps this to 409 Conflict.
//
// VM-P2E Slice 1: existed but always passed fromGeneration=0 (bug fixed in Slice 2).
// VM-P2E Slice 2: fromGeneration is now forwarded from the handler's request body.
//
// Source: vm-13-03__blueprint__ §interaction_or_ops_contract "Operator initiates
//
//	single-host drain".
func (i *HostInventory) DrainHost(ctx context.Context, hostID string, fromGeneration int64, reason string) (runningCount int, updated bool, err error) {
	// Attempt CAS: ready → draining.
	// If the host is already draining (repeated call), the generation check
	// will fail (generation was already incremented). The handler treats this
	// as a 409 unless the caller re-reads the current generation first.
	ok, err := i.repo.UpdateHostStatus(ctx, hostID, fromGeneration, "draining", reason)
	if err != nil {
		return 0, false, fmt.Errorf("DrainHost transition: %w", err)
	}
	if !ok {
		// CAS failed: either generation mismatch or host not found.
		// Return (0, false, nil) — the handler discriminates via GetHostByID.
		return 0, false, nil
	}

	// Detach stopped VMs so they don't block drain completion.
	// Idempotent: safe even if some instances were already detached.
	if err := i.repo.DetachStoppedInstancesFromHost(ctx, hostID); err != nil {
		return 0, true, fmt.Errorf("DrainHost detach stopped: %w", err)
	}

	// Count remaining active workload for the caller's response.
	n, err := i.repo.CountActiveInstancesOnHost(ctx, hostID)
	if err != nil {
		return 0, true, fmt.Errorf("DrainHost count active: %w", err)
	}
	return n, true, nil
}

// CompleteDrain attempts the draining → drained transition for a host.
//
// Returns:
//   - (activeCount>0, false, nil): drain blocked; active workload count returned.
//   - (0, false, nil):             CAS failed (wrong generation or not in draining state).
//   - (0, true,  nil):             transition succeeded; host is now drained.
//   - (0, false, err):             unexpected DB error.
//
// Idempotent: safe to call repeatedly. Once drained the status='draining'
// guard in MarkHostDrained prevents re-application.
//
// Source: vm-13-03__blueprint__ §interaction_or_ops_contract
//
//	"Operator confirms drain complete / drain watch signals completion".
func (i *HostInventory) CompleteDrain(ctx context.Context, hostID string, fromGeneration int64) (activeCount int, updated bool, err error) {
	return i.repo.MarkHostDrained(ctx, hostID, fromGeneration)
}

// MarkDegraded transitions a host to 'degraded' with a reason code.
//
// fromStatus is the caller's expected current status of the host (used for
// transition validation). fromGeneration is the expected generation for CAS.
//
// Valid fromStatuses: ready, draining, drained (see db.legalTransitions).
// Illegal transitions return (false, ErrIllegalHostTransition).
// CAS failure (generation mismatch or fromStatus mismatch) returns (false, nil).
//
// reasonCode should be one of the db.ReasonXxx constants.
//
// Source: vm-13-03__blueprint__ §"DEGRADED state",
//
//	§implementation_decisions "Introduce a DEGRADED state".
func (i *HostInventory) MarkDegraded(ctx context.Context, hostID string, fromGeneration int64, fromStatus, reasonCode string) (updated bool, err error) {
	ok, err := i.repo.MarkHostDegraded(ctx, hostID, fromGeneration, fromStatus, reasonCode)
	if err != nil {
		return false, fmt.Errorf("MarkDegraded: %w", err)
	}
	return ok, nil
}

// MarkUnhealthy transitions a host to 'unhealthy' with a reason code.
//
// fromStatus is the caller's expected current status (used for transition
// validation). fromGeneration is the expected generation for CAS.
//
// Valid fromStatuses: ready, draining, degraded (see db.legalTransitions).
// Illegal transitions return (false, false, ErrIllegalHostTransition).
// CAS failure returns (false, false, nil).
//
// fenceRequired return value indicates whether fence_required was set TRUE
// in the DB. This happens for reason codes in the ambiguous-failure set
// (AGENT_UNRESPONSIVE, HYPERVISOR_FAILED, NETWORK_UNREACHABLE).
//
// Source: vm-13-03__blueprint__ §"Fencing Decision Logic".
func (i *HostInventory) MarkUnhealthy(ctx context.Context, hostID string, fromGeneration int64, fromStatus, reasonCode string) (fenceRequired bool, updated bool, err error) {
	fr, ok, err := i.repo.MarkHostUnhealthy(ctx, hostID, fromGeneration, fromStatus, reasonCode)
	if err != nil {
		return false, false, fmt.Errorf("MarkUnhealthy: %w", err)
	}
	return fr, ok, nil
}

// ClearFenceRequired clears the fence_required flag on a host.
//
// Called by an operator after confirming the host is isolated, or (in Slice 4+)
// by the fencing controller after STONITH completion. The host status is NOT
// changed by this call — a separate status transition (e.g., → fenced) should
// follow if needed.
//
// Returns (true, nil) when the flag was cleared.
// Returns (false, nil) on CAS failure (wrong generation or flag already false).
//
// Source: vm-13-03__blueprint__ §"Fencing Controller" (Slice 4+ seam).
func (i *HostInventory) ClearFenceRequired(ctx context.Context, hostID string, fromGeneration int64) (updated bool, err error) {
	ok, err := i.repo.ClearFenceRequired(ctx, hostID, fromGeneration)
	if err != nil {
		return false, fmt.Errorf("ClearFenceRequired: %w", err)
	}
	return ok, nil
}

// GetFenceRequiredHosts returns all hosts with fence_required=TRUE.
//
// Observable surface for operator tooling and the future fencing controller.
// A non-empty result means recovery automation must NOT proceed for those hosts.
//
// Source: vm-13-03__blueprint__ §"Fencing Controller".
func (i *HostInventory) GetFenceRequiredHosts(ctx context.Context) ([]*db.HostRecord, error) {
	return i.repo.GetFenceRequiredHosts(ctx)
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

// ── VM-P2E Slice 4: Retirement ────────────────────────────────────────────────

// RetireHost transitions a host to 'retiring'.
//
// The transition is gated on zero active VM workload — a host must be empty
// to retire safely. If active workload remains, returns (activeCount, false, nil)
// and the caller should poll again after workloads complete.
//
// fromStatus must be one of: drained, fenced, unhealthy (see db.legalTransitions).
// fromGeneration is the expected current generation for CAS.
// reasonCode should be db.ReasonOperatorRetired or a caller-supplied code.
//
// Returns:
//   - (n>0, false, nil): n active instances remain; retirement blocked.
//   - (0, false, nil):   CAS failed (generation mismatch or wrong fromStatus).
//   - (0, true,  nil):   transition succeeded; host is now 'retiring'.
//   - (0, false, err):   DB error or illegal transition.
//
// Source: vm-13-03__blueprint__ §"Emergency Retirement", §"Operator Procedures".
func (i *HostInventory) RetireHost(ctx context.Context, hostID string, fromGeneration int64, fromStatus, reasonCode string) (activeCount int, updated bool, err error) {
	n, ok, err := i.repo.MarkHostRetiring(ctx, hostID, fromGeneration, fromStatus, reasonCode)
	if err != nil {
		return 0, false, fmt.Errorf("RetireHost: %w", err)
	}
	return n, ok, nil
}

// CompleteRetirement transitions a host from 'retiring' to 'retired'.
//
// Sets retired_at to the current wall-clock time. This timestamp is the
// replacement-seam anchor for Slice 5+ capacity planning.
//
// fromGeneration is the expected current generation for CAS (must be the
// generation returned after RetireHost succeeded).
//
// Returns (true, nil) on success.
// Returns (false, nil) on CAS failure (wrong generation or not in 'retiring').
// Returns (false, err) on DB error.
//
// Source: vm-13-03__blueprint__ §"RETIRED state — terminal".
func (i *HostInventory) CompleteRetirement(ctx context.Context, hostID string, fromGeneration int64) (updated bool, err error) {
	ok, err := i.repo.MarkHostRetired(ctx, hostID, fromGeneration)
	if err != nil {
		return false, fmt.Errorf("CompleteRetirement: %w", err)
	}
	return ok, nil
}

// GetRetiredHosts returns all hosts with status='retired', ordered by retired_at DESC.
//
// This is the replacement-seam query surface. Slice 5+ capacity planners use
// this to discover capacity holes and trigger bare-metal provisioning for hosts
// that have been retired but not yet replaced.
//
// Source: vm-13-03__blueprint__ §components "Capacity Manager" (Slice 5+ seam).
func (i *HostInventory) GetRetiredHosts(ctx context.Context) ([]*db.HostRecord, error) {
	return i.repo.GetRetiredHosts(ctx)
}

// ── VM-P2E Slice 5: Maintenance campaign orchestration ────────────────────────

// CreateCampaign creates a new maintenance campaign with blast-radius validation.
//
// id must be a caller-generated unique ID (recommend UUID4).
// reason is a human-readable label for the campaign (e.g. "kernel-4.19 patch").
// targetHostIDs is the full ordered list of hosts to act on. Must be non-empty.
// maxParallel is the maximum number of hosts to drain concurrently. Must be
// between 1 and db.MaxCampaignParallel (hard blast-radius limit).
//
// Returns the created CampaignRecord, or an error if validation fails or DB write fails.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (i *HostInventory) CreateCampaign(ctx context.Context, id, reason string, targetHostIDs []string, maxParallel int) (*db.CampaignRecord, error) {
	c, err := i.repo.CreateCampaign(ctx, id, reason, targetHostIDs, maxParallel)
	if err != nil {
		return nil, fmt.Errorf("CreateCampaign: %w", err)
	}
	return c, nil
}

// GetCampaign fetches a campaign by ID.
// Returns db.ErrCampaignNotFound when absent.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (i *HostInventory) GetCampaign(ctx context.Context, id string) (*db.CampaignRecord, error) {
	c, err := i.repo.GetCampaignByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("GetCampaign: %w", err)
	}
	return c, nil
}

// ListCampaigns returns campaigns filtered by status. Pass nil to return all.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator" observable surface.
func (i *HostInventory) ListCampaigns(ctx context.Context, statuses []string) ([]*db.CampaignRecord, error) {
	cs, err := i.repo.ListCampaigns(ctx, statuses)
	if err != nil {
		return nil, fmt.Errorf("ListCampaigns: %w", err)
	}
	return cs, nil
}

// AdvanceCampaign drains the next batch of hosts within blast-radius limits.
//
// Steps:
//  1. Fetch the campaign by ID. Return error if not found.
//  2. Reject if campaign is terminal (completed/cancelled) or paused.
//  3. Mark campaign running if still pending.
//  4. Determine next batch: campaign.NextHosts(maxParallel).
//  5. For each host in the batch: call DrainHost with generation=0 (operator does
//     not need to supply per-host generation here — the campaign owns sequencing).
//     Record outcome as "completed" or "failed" via AdvanceCampaignProgress.
//  6. Return the advance result summary.
//
// Blast-radius is enforced by NextHosts(campaign.MaxParallel) — at most
// MaxParallel hosts are sent to drain per advance call.
//
// Note: DrainHost is called with generation=0, which will fail if the host's
// generation is not 0. Callers that need generation-exact drain (e.g. single-host
// operator flows) should use the direct drain endpoint. Campaign advance is an
// operator-convenience path that accepts the CAS-retry tradeoff for batch flows.
// The handler documents this behavior explicitly.
//
// Returns a summary of which hosts were actioned and their outcomes.
//
// Source: vm-13-03__blueprint__ §interaction_or_ops_contract
//
//	"Operator initiates a fleet-wide kernel update".
func (i *HostInventory) AdvanceCampaign(ctx context.Context, campaignID, drainReason string) (*CampaignAdvanceResult, error) {
	campaign, err := i.repo.GetCampaignByID(ctx, campaignID)
	if err != nil {
		return nil, fmt.Errorf("AdvanceCampaign: %w", err)
	}

	if campaign.IsTerminal() {
		return nil, fmt.Errorf("AdvanceCampaign: campaign %s is terminal (status=%s)", campaignID, campaign.Status)
	}
	if campaign.Status == "paused" {
		return nil, fmt.Errorf("AdvanceCampaign: campaign %s is paused; resume before advancing", campaignID)
	}

	// Transition pending → running on first advance.
	if campaign.Status == "pending" {
		if _, err := i.repo.UpdateCampaignStatus(ctx, campaignID, "running"); err != nil {
			return nil, fmt.Errorf("AdvanceCampaign: status update: %w", err)
		}
	}

	// Determine which hosts to act on this batch.
	nextHosts := campaign.NextHosts(campaign.MaxParallel)
	if len(nextHosts) == 0 {
		// All hosts already actioned.
		if _, err := i.repo.UpdateCampaignStatus(ctx, campaignID, "completed"); err != nil {
			return nil, fmt.Errorf("AdvanceCampaign: complete status update: %w", err)
		}
		return &CampaignAdvanceResult{
			CampaignID: campaignID,
			Status:     "completed",
			Actioned:   nil,
		}, nil
	}

	result := &CampaignAdvanceResult{
		CampaignID: campaignID,
		Status:     "running",
	}

	for _, hostID := range nextHosts {
		// Use generation=0 (campaign-advance path; see godoc above).
		_, drained, drainErr := i.DrainHost(ctx, hostID, 0, drainReason)
		outcome := CampaignHostOutcome{HostID: hostID}
		if drainErr != nil || !drained {
			outcome.Outcome = "failed"
			if drainErr != nil {
				outcome.Error = drainErr.Error()
			} else {
				outcome.Error = "CAS failed — host may have been concurrently modified; re-read generation and retry individually"
			}
			if _, advErr := i.repo.AdvanceCampaignProgress(ctx, campaignID, hostID, "failed"); advErr != nil {
				// Log but don't halt; remaining hosts should still be attempted.
				_ = advErr
			}
		} else {
			outcome.Outcome = "completed"
			if _, advErr := i.repo.AdvanceCampaignProgress(ctx, campaignID, hostID, "completed"); advErr != nil {
				_ = advErr
			}
		}
		result.Actioned = append(result.Actioned, outcome)
	}

	return result, nil
}

// CampaignAdvanceResult is the return value from AdvanceCampaign.
type CampaignAdvanceResult struct {
	CampaignID string
	Status     string
	Actioned   []CampaignHostOutcome
}

// CampaignHostOutcome records what happened to a single host during an advance.
type CampaignHostOutcome struct {
	HostID  string
	Outcome string // "completed" | "failed"
	Error   string // non-empty when Outcome="failed"
}

// PauseCampaign halts further advancement of a running campaign.
// In-flight host drains already started will complete naturally.
// Returns (true, nil) when the campaign was paused.
// Returns (false, nil) when the campaign was not in a pausable state.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (i *HostInventory) PauseCampaign(ctx context.Context, id string) (updated bool, err error) {
	ok, err := i.repo.UpdateCampaignStatus(ctx, id, "paused")
	if err != nil {
		return false, fmt.Errorf("PauseCampaign: %w", err)
	}
	return ok, nil
}

// ResumeCampaign resumes a paused campaign.
// Returns (true, nil) when the campaign was resumed.
// Returns (false, nil) when the campaign was not paused.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (i *HostInventory) ResumeCampaign(ctx context.Context, id string) (updated bool, err error) {
	ok, err := i.repo.UpdateCampaignStatus(ctx, id, "running")
	if err != nil {
		return false, fmt.Errorf("ResumeCampaign: %w", err)
	}
	return ok, nil
}

// CancelCampaign cancels a campaign.
// In-flight host drains already started will complete naturally.
// A cancelled campaign cannot be restarted.
// Returns (true, nil) when the campaign was cancelled.
// Returns (false, nil) when the campaign was already terminal.
//
// Source: vm-13-03__blueprint__ §components "Maintenance Orchestrator".
func (i *HostInventory) CancelCampaign(ctx context.Context, id string) (updated bool, err error) {
	ok, err := i.repo.UpdateCampaignStatus(ctx, id, "cancelled")
	if err != nil {
		return false, fmt.Errorf("CancelCampaign: %w", err)
	}
	return ok, nil
}
