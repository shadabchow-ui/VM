# VM Compute Platform

VM is an experimental cloud compute platform for managing virtual machines through an API-driven control plane.

The project is designed around the same core problems real cloud compute systems need to solve: durable instance identity, asynchronous lifecycle operations, host coordination, scheduling, networking, storage, images, reconciliation, repair, and operational visibility.

This is not a toy create/delete demo. It is an early but serious EC2-style VM platform foundation.

## Current status

This repository is an active infrastructure platform build.

It contains a real Go module, a multi-service control-plane structure, shared contracts, database/repository seams, scheduler and host-agent boundaries, networking foundations, volume and image workflows, reconciliation loops, and milestone/status documentation.

The honest current posture is:

```text
Real VM platform foundation + active Phase 2 expansion + not yet production cloud infrastructure.
```

The project is already beyond prototype stage, but it should not be described as a finished cloud provider. The biggest remaining gaps are image maturity, tenancy/quotas/RBAC, host fleet operations, deeper validation, and product/operator polish.

## What this project is building

VM is aimed at becoming a managed virtual machine platform with:

- API-first instance lifecycle management.
- Durable instance identity.
- Persistent root disk semantics.
- Asynchronous lifecycle jobs.
- Scheduler-driven host placement.
- Host-agent runtime execution seams.
- Reconciliation and repair loops.
- Public IP and network lifecycle handling.
- VPC-style networking foundations.
- Detached block storage foundations.
- Snapshot and restore workflow foundations.
- Custom image lifecycle foundations.
- Structured errors and idempotent mutation behavior.
- Future console, CLI, and developer workflows.

## Core principles

### Control plane first

The API layer should admit, validate, and record intent. Actual host/runtime operations are separated into worker, scheduler, host-agent, network-controller, and reconciler seams.

### Async by default

VM lifecycle operations are long-running infrastructure actions. The platform models them as jobs instead of pretending every action is immediately complete.

### Durable resource ownership

Instances, networks, volumes, images, IPs, jobs, projects, and events need persistent state and ownership rules. Cross-account or cross-project access should not leak resources.

### Reconciliation over wishful state

Cloud systems fail. The platform includes reconciler and janitor concepts so stuck jobs, drift, duplicate IPs, and inconsistent states can be detected and repaired.

### Clear infrastructure boundaries

Resource-manager, scheduler, worker, host-agent, network-controller, reconciler, runtime client, and repository layers should remain separate so the platform can grow without turning into a single tangled service.

## Repository identity

```text
Repository: shadabchow-ui/VM
Module:     github.com/compute-platform/compute-platform
Language:   Go
```

The project currently uses Go 1.24 and dependencies including PostgreSQL support, gRPC, and protobuf.

## High-level architecture

The codebase is organized around a cloud-control-plane model with these major service areas:

| Area | Purpose |
|---|---|
| Resource manager | Public/API-facing resource admission, lifecycle requests, ownership checks, and job creation |
| Worker | Executes asynchronous lifecycle and resource operations |
| Scheduler | Places instances onto eligible hosts |
| Host agent | Host/runtime seam for VM execution |
| Network controller | Network, IP, NAT, VPC, subnet, security group, and routing control-plane seams |
| Reconciler | Detects drift and dispatches repair work |
| Janitor | Finds stuck or timed-out jobs and moves the system toward a safe state |
| Shared contracts | Domain models, job contracts, API shapes, and runtime interfaces |
| Repository layer | Persistence seam for instances, jobs, IPs, networks, projects, events, volumes, images, and related resources |

## Major capabilities

### Instance lifecycle

The core compute foundation includes work around:

- Create instance.
- List instances.
- Get instance details.
- Start instance.
- Stop instance.
- Reboot instance.
- Delete instance.
- Lifecycle admission rules.
- Idempotency for mutating endpoints.
- Job status tracking.
- Ownership-aware access behavior.
- Persistent-instance-style state modeling.

This is the strongest and most mature part of the project.

### Async jobs

The platform treats infrastructure changes as durable jobs. This allows lifecycle operations to be admitted by the API, processed by workers, observed by users/operators, and repaired if something gets stuck.

Important concepts include:

- Job creation from API actions.
- Job status retrieval.
- Timeout handling.
- Stuck-job scans.
- Repair dispatch.
- Reconciliation loops.

### Scheduler and host-agent seams

The project includes clear seams for separating placement decisions from host execution.

This matters because a serious VM platform cannot directly tie API handlers to local VM runtime calls. It needs scheduler decisions, host capacity awareness, and host-agent execution boundaries.

### Networking foundation

Networking work includes foundations for:

- Public IP lifecycle.
- Duplicate IP prevention.
- IP release on stop/delete paths.
- SSH readiness gating before marking instances as running.
- DNAT/SNAT lifecycle correctness.
- VPC resources.
- Subnets.
- Security groups.
- Security group rules.
- Route tables.
- Route entries.
- Network reconciliation scans.

This is a meaningful control-plane slice, but it is not yet a full AWS-quality VPC implementation.

### Block storage foundation

The platform includes work toward EBS-style block storage, including:

- Volumes.
- Volume create/list/get/delete flows.
- Instance volume listing.
- Attach volume workflows.
- Detach volume workflows.
- Availability-zone affinity concepts.
- Attachment limits.
- Delete-on-termination behavior.
- Snapshot-aware constraints.
- Async volume jobs.

This is an active implementation area and should be viewed as a serious foundation rather than a complete production storage service.

### Snapshots and restore direction

The storage direction includes snapshot and restore workflows. The long-term target is to support safe volume snapshots, restores, cloning, image creation from snapshots, and protection against destructive actions while snapshots are active.

### Image system foundation

The image subsystem has moved beyond a basic catalog direction. The project includes foundations for:

- Image listing.
- Image detail retrieval.
- Custom image creation.
- Image import workflows.
- Deprecating images.
- Obsoleting images.
- Snapshot-backed image creation seams.
- Import-backed image creation seams.
- Image lifecycle status.
- Image family and version concepts.
- Private ownership model foundations.
- Async image jobs.

This is still one of the most important remaining product gaps. Long-term image maturity needs stronger family/alias resolution, trusted-vs-custom image admission rules, validation pipeline integration, provenance/signing, and hardened import failure states.

### Reconciliation and repair

The platform includes reconciliation and repair concepts that make it more than a CRUD API.

Key ideas include:

- Job timeout janitor.
- Reconciler skeleton.
- Drift classification.
- Repair job dispatch.
- Rate limiting.
- Stuck-job scans.
- Active-instance scans.
- Duplicate IP detection.

Real infrastructure systems become reliable through convergence and repair. This repo is already built with that direction in mind.

## Phase-level maturity

| Phase area | Status | Notes |
|---|---|---|
| Core VM lifecycle | Strong foundation | Create/list/get/start/stop/reboot/delete, jobs, ownership, idempotency, scheduler/host seams |
| Networking expansion | Partial but real | VPC/subnet/security group/route table foundations and IP/NAT hardening |
| Volumes and snapshots | Substantial partial | Detached volume API and attach/detach semantics are underway |
| Images | Active subsystem, still a top gap | Catalog/lifecycle/import/create flows exist as foundations; trust/validation maturity still needed |
| Tenancy, quotas, RBAC | Early | Ownership exists, but full project/account quota and RBAC model needs deeper work |
| Fleet reliability | Early foundation | Reconciler/host-agent/scheduler seams exist; drain, fencing, maintenance, and replacement workflows remain |
| Console/developer UX | Early | Backend/control-plane progress is ahead of product polish |

## What is strongest today

- Core lifecycle control-plane structure.
- Async job-driven infrastructure actions.
- Scheduler and host-agent boundaries.
- Reconciler and janitor foundation.
- Ownership-aware resource behavior.
- Public IP and SSH readiness hardening.
- Early VPC/networking control-plane breadth.
- Detached volume workflows.
- Image lifecycle and import/create foundations.

## What still needs the most work

### Image maturity

The platform needs stronger trusted image handling, custom image validation, family/alias resolution, lifecycle enforcement, provenance, and import safety.

### Tenancy and quotas

A serious cloud platform needs robust project/account scoping, quota accounting, RBAC, policy-driven admission, and clean capacity-vs-quota error semantics.

### Fleet operations

The next major operational layer should include host draining, maintenance workflows, failure fencing, replacement/retirement automation, and richer host health lifecycle management.

### Product and operator experience

The backend is ahead of the user-facing and operator-facing experience. A stronger console, CLI, documentation, debugging tools, and operational dashboards will make the platform easier to use and trust.

## Technology stack

- Go 1.24
- PostgreSQL driver: `github.com/lib/pq`
- gRPC
- Protocol Buffers
- Standard Go test/build tooling

## Development

### Prerequisites

- Go 1.24 or compatible local toolchain
- PostgreSQL for database-backed integration paths
- Linux/KVM-capable host environment for deeper runtime validation, depending on the test or service being exercised

### Build

```bash
go build ./...
```

### Test

```bash
go test ./...
```

### Vet

```bash
go vet ./...
```

Some integration tests may require external services such as a database, host runtime environment, or Linux-specific networking capabilities.

## Suggested local validation flow

```bash
go build ./...
go test ./...
go vet ./...
```

For infrastructure/runtime changes, also validate the specific service, repository layer, migration, and host/runtime path touched by the change.

## Roadmap

### Near term

- Finish image lifecycle maturity.
- Strengthen image family/version/alias resolution.
- Harden trusted vs custom image admission.
- Complete snapshot-to-volume and snapshot-to-image paths.
- Tighten storage worker execution and recovery.
- Improve networking invariants and data-plane reconciliation.

### Mid term

- Build stronger project/account tenancy.
- Add quotas and quota accounting.
- Expand RBAC and admission controls.
- Improve host health tracking.
- Add host drain and maintenance workflows.
- Improve operator visibility and debugging.

### Longer term

- Mature the web/console and developer experience.
- Add production-grade fleet operations.
- Add stronger image signing/provenance.
- Add richer networking and storage policy models.
- Add deployment, upgrade, backup, and disaster-recovery runbooks.
- Move from experimental platform foundation toward serious managed compute product readiness.

## What this repo is not

This repo is not a finished AWS, Azure, or Google Cloud replacement.

It does not yet provide production-grade guarantees around multi-tenant isolation, quota enforcement, image trust, fleet operations, compliance, billing, global availability, or full hardware-backed validation.

It is best understood as a serious cloud compute platform foundation in active development.

## Good short description

Use this when describing the project publicly:

> An EC2-style VM platform in active development: API-driven instance lifecycle, async jobs, scheduler and host-agent seams, reconciliation loops, VPC networking foundations, detached block storage, and a developing custom image system.

## Contributing and development notes

When working on this repo:

- Keep API admission separate from host execution.
- Prefer durable jobs for long-running infrastructure operations.
- Preserve ownership checks and cross-tenant hiding behavior.
- Do not bypass scheduler, worker, host-agent, or reconciler boundaries for convenience.
- Add tests for lifecycle invariants, idempotency, ownership, and failure states.
- Keep implementation claims tied to code, tests, and milestone evidence.
- Be clear about what is implemented, partial, planned, or experimental.

## License

License information has not been finalized yet.

## Disclaimer

VM is an independent experimental infrastructure project. It is not affiliated with any cloud provider and should not be treated as production infrastructure without further hardening, validation, security review, and operational testing.
