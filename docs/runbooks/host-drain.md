# Host Drain Runbook

## Overview

Host drain safely evacuates active workloads from a physical hypervisor before maintenance or decommissioning. Once draining is initiated, the host no longer receives new VM placements, and existing VMs are moved or stopped.

## Host Status States

| Status | Schedulable | Meaning |
|---|---|---|
| `ready` | Yes | Accepting new VM placements |
| `draining` | No | Evacuating active workloads; no new placements |
| `drained` | No | All workloads gone; safe for maintenance |
| `degraded` | No | Health signals degraded; drain-then-recover |
| `unhealthy` | No | Health signals failed; fence may be required |
| `fenced` | No | Isolated; recovery may proceed (future) |
| `retiring` | No | Marked for permanent removal |
| `retired` | No | Terminal — permanently out of service |

## Drain Procedure

### 1. Initiate Drain

Set the host to `draining` status via the internal API:

```
POST /internal/v1/hosts/{host_id}/drain
X-Principal-ID: operator
Content-Type: application/json

{"reason": "Scheduled maintenance — chassis fan replacement"}
```

The host immediately becomes unschedulable. The drain_reason is persisted for operator visibility.

### 2. Monitor Drain Progress

Check the host status:
```
GET /internal/v1/hosts/{host_id}
```

Monitor active instance count on the host:
```
SELECT COUNT(*) FROM instances WHERE host_id = '{host_id}' AND vm_state NOT IN ('deleted', 'failed', 'stopped');
```

### 3. Complete Drain (Mark Drained)

When the host has zero active instances:
```
POST /internal/v1/hosts/{host_id}/drain-complete
X-Principal-ID: operator
```

The host transitions from `draining` to `drained`. The CAS gate rejects the transition if active instances remain.

### 4. Return to Service

When maintenance is complete:
```
POST /internal/v1/hosts/{host_id}/reactivate
X-Principal-ID: operator
```

The host transitions from `drained` back to `ready` and resumes accepting placements.

## Safety Gates

### CAS (Compare-And-Swap)
All host status transitions use `generation`-based optimistic concurrency control. If the host's generation has changed since you last read it, the transition is rejected and you must re-read before retrying.

### Scheduler Exclusions
The scheduler's `CanFit` check excludes hosts in any non-ready status (draining, drained, degraded, unhealthy, fenced, retired, retiring, maintenance, provisioning, offline). A host marked draining immediately stops receiving new placements.

### Fence Required Gate
Hosts with `fence_required=TRUE` must not receive recovery actions until fencing is confirmed. Recovery automation (`GetRecoveryEligibleHosts`) explicitly excludes these hosts.

### Workload Gate
`MarkHostDrained` and `MarkHostRetiring` gate on zero active workload. The transition is atomically rejected if any instances remain on the host.

## Degraded / Unhealthy Handling

### Degraded
A host marked `degraded` (via `MarkHostDegraded`) has soft health degradation. It is unschedulable but the host agent is still reachable. Drain the host and re-activate:
1. Drain → drained
2. Fix underlying issue
3. Reactivate → ready

### Unhealthy
A host marked `unhealthy` (via `MarkHostUnhealthy`) has severe health failure. If the reason code is in the ambiguous failure set (`AGENT_UNRESPONSIVE`, `HYPERVISOR_FAILED`, `NETWORK_UNREACHABLE`), `fence_required` is set to `TRUE`. Recovery MUST NOT proceed until fencing is confirmed.

### Fencing Protocol
1. Confirm host isolation (fencing controller or operator verification)
2. `ClearFenceRequired` to clear the flag
3. Recovery automation may then proceed (transition to drained → stop VMs → fail over → reactivate)

## Recovery Automation Safety

The reconciler classifies instances on degraded/unhealthy hosts as `host_unhealthy_with_live_instance`. The dispatcher writes an event but does NOT auto-repair. No automatic recovery occurs while `fence_required=TRUE`.

The `GetRecoveryEligibleHosts` query includes only hosts with `fence_required=FALSE` and `status IN ('drained', 'degraded', 'unhealthy')`.

## Common Scenarios

### Draining Doesn't Complete
If instances remain on the host after expected drain time:
1. Verify no long-running jobs are stuck
2. Check for instances stuck in transitional states (stopping, rebooting)
3. The reconciler will detect stuck instances and mark them failed after timeout thresholds
4. Manually stop any remaining VMs if the host is deteriorating

### Heartbeat Staleness
The Resource Manager reads `last_heartbeat_at` and the heartbeat staleness window (90s). A host with a heartbeat older than 90s is classified as stale and excluded from `GetAvailableHosts`. If the staleness exceeds the degraded threshold, the heartbeat monitoring transitions the host to `degraded`.

### Boot ID Change
If the host agent reports a different `boot_id`, the host has rebooted. All previously-running VMs on that host are presumed lost. The reconciler detects this via the boot_id change signal and classifies affected instances as failed.
