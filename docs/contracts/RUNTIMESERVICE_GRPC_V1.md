# RUNTIMESERVICE_GRPC_V1

## RuntimeService gRPC Interface — Phase 1 Contract

**Status:** FROZEN — changes require formal review with downstream dependency analysis.

---

## 1. Purpose

The RuntimeService gRPC interface is the sole communication contract between the control plane worker and the Host Agent. The control plane never invokes hypervisor tooling (Firecracker, libvirt, QEMU) directly. All hypervisor interaction is mediated through this interface.

This abstraction enables Phase 2 hypervisor replacement (Firecracker → QEMU, Cloud Hypervisor) with zero control plane code changes.

---

## 2. Service Definition

```protobuf
syntax = "proto3";

package compute.runtime.v1;

option go_package = "github.com/<org>/compute-platform/packages/contracts/runtimev1";

service RuntimeService {
  // Creates and starts a VM from an InstanceConfig. Idempotent.
  // If VM already exists for instance_id, returns current state without modification.
  rpc CreateInstance(CreateInstanceRequest) returns (CreateInstanceResponse);

  // Starts a previously stopped VM. Idempotent.
  // If VM is already running, returns success without modification.
  rpc StartInstance(StartInstanceRequest) returns (StartInstanceResponse);

  // Stops a running VM. Idempotent.
  // If VM is already stopped or absent, returns success.
  rpc StopInstance(StopInstanceRequest) returns (StopInstanceResponse);

  // Deletes all resources for a VM (process, rootfs, TAP, iptables). Idempotent.
  // If resources are already absent, returns success.
  rpc DeleteInstance(DeleteInstanceRequest) returns (DeleteInstanceResponse);

  // Returns the state of all VMs on this host.
  rpc ListInstances(ListInstancesRequest) returns (ListInstancesResponse);
}
```

---

## 3. Message Definitions

### CreateInstanceRequest

```protobuf
message CreateInstanceRequest {
  string instance_id = 1;         // inst_ prefixed KSUID
  InstanceConfig config = 2;
}

message InstanceConfig {
  string instance_id = 1;
  int32 vcpus = 2;
  int32 memory_mb = 3;

  KernelConfig kernel = 4;
  RootFilesystemConfig root_filesystem = 5;
  repeated NetworkInterfaceConfig network_interfaces = 6;

  map<string, string> metadata = 7;  // key-value pairs for metadata service
}

message KernelConfig {
  string source_url = 1;           // object store URL, resolved by control plane
  string boot_args = 2;            // e.g., "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda"
}

message RootFilesystemConfig {
  string source_url = 1;           // URL to the qcow2 base image or overlay on NFS
  int32 size_gb = 2;
  bool is_read_only = 3;           // false for root disk
}

message NetworkInterfaceConfig {
  string device_id = 1;            // vNIC identifier
  string guest_mac_address = 2;    // assigned MAC for guest
  string host_tap_device = 3;      // TAP device name on host (optional, host agent may assign)
  string private_ip = 4;           // assigned private IP
  string public_ip = 5;            // assigned public IP (empty if none)
}
```

### CreateInstanceResponse

```protobuf
message CreateInstanceResponse {
  string instance_id = 1;
  InstanceState state = 2;
  string error_message = 3;        // populated only on failure
}
```

### StartInstanceRequest / Response

```protobuf
message StartInstanceRequest {
  string instance_id = 1;
  InstanceConfig config = 2;       // full config for re-provisioning on potentially new host
}

message StartInstanceResponse {
  string instance_id = 1;
  InstanceState state = 2;
  string error_message = 3;
}
```

### StopInstanceRequest / Response

```protobuf
message StopInstanceRequest {
  string instance_id = 1;
  bool force = 1;                  // Phase 1: false = ACPI shutdown with timeout then force
                                   // Phase 2: true = immediate force stop
  int32 timeout_seconds = 2;       // grace period before force. Default 90.
}

message StopInstanceResponse {
  string instance_id = 1;
  InstanceState state = 2;
  string error_message = 3;
}
```

### DeleteInstanceRequest / Response

```protobuf
message DeleteInstanceRequest {
  string instance_id = 1;
  bool delete_root_disk = 2;       // Phase 1: always true
}

message DeleteInstanceResponse {
  string instance_id = 1;
  InstanceState state = 2;
  string error_message = 3;
}
```

### ListInstancesRequest / Response

```protobuf
message ListInstancesRequest {
  // No filters in Phase 1. Returns all instances on this host.
}

message ListInstancesResponse {
  repeated InstanceInfo instances = 1;
}

message InstanceInfo {
  string instance_id = 1;
  InstanceState state = 2;
  int32 vcpus = 3;
  int32 memory_mb = 4;
  int64 uptime_seconds = 5;
  string host_pid = 6;             // VM process PID on host (for diagnostics)
}
```

### InstanceState Enum

```protobuf
enum InstanceState {
  INSTANCE_STATE_UNKNOWN = 0;
  INSTANCE_STATE_CREATING = 1;
  INSTANCE_STATE_RUNNING = 2;
  INSTANCE_STATE_STOPPED = 3;
  INSTANCE_STATE_STOPPING = 4;
  INSTANCE_STATE_DELETED = 5;
  INSTANCE_STATE_ERROR = 6;
}
```

---

## 4. Idempotency Contract

All methods are idempotent. This is non-negotiable.

| Method | Idempotency Behavior |
|--------|---------------------|
| `CreateInstance` | If VM already exists for `instance_id`: return current state, do not create second VM. |
| `StartInstance` | If VM is already running: return `RUNNING`, no-op. |
| `StopInstance` | If VM is already stopped or absent: return `STOPPED`, no-op. |
| `DeleteInstance` | If VM and resources are already absent: return `DELETED`, no-op. |
| `ListInstances` | Read-only. Always safe. |

**Why:** Workers may retry operations due to queue re-delivery, crash recovery, or reconciler repair actions. Non-idempotent operations cause duplicate VMs, orphan resources, or data corruption.

---

## 5. Error Handling

gRPC status codes used by the Host Agent:

| gRPC Code | Meaning | Worker Behavior |
|-----------|---------|-----------------|
| `OK` | Operation succeeded. | Mark job step complete. |
| `ALREADY_EXISTS` | Resource already exists (idempotent case). | Treat as success. |
| `NOT_FOUND` | Resource not found (idempotent delete case). | Treat as success for delete operations. |
| `FAILED_PRECONDITION` | Invalid state for operation (e.g., start on a creating VM). | Retry after backoff. |
| `RESOURCE_EXHAUSTED` | Host out of capacity (CPU, RAM, disk). | Fail job. Scheduler should select different host. |
| `INTERNAL` | Unexpected host agent error. | Retry up to `max_attempts`. Log full error. |
| `UNAVAILABLE` | Host Agent temporarily unreachable. | Retry with exponential backoff. |
| `DEADLINE_EXCEEDED` | Operation timed out. | Retry. Check if operation partially completed (idempotency). |

**Timeout per RPC call:** All RPC calls from the worker to the Host Agent are wrapped in a 60-second deadline by default. `CreateInstance` may use a longer deadline (up to 300 seconds) to accommodate rootfs materialization.

---

## 6. Authentication

Host Agent ↔ Control Plane communication uses mTLS with host-specific certificates:

- Certificate CN: `host-{host_id}`
- Bootstrap flow: short-lived one-time token → CSR → signed mTLS certificate. Token invalidated after first use.
- All gRPC calls must be over mTLS. Unauthenticated calls are rejected.

---

## 7. Host Agent Operations Detail

The Host Agent translates RuntimeService calls into the following physical operations:

### CreateInstance Sequence

```
1. Materialize rootfs: create qcow2 CoW overlay on NFS pointing to base image
2. Create TAP device, attach to host bridge (br0)
3. Program iptables DNAT/SNAT rules (if public IP assigned)
4. Configure DHCP relay for private IP
5. Populate metadata service entry for this instance
6. Launch Firecracker microVM with kernel binary, rootfs, and network config
7. Wait for VM process to be confirmed running (hypervisor-level check)
8. Return RUNNING state
```

### StopInstance Sequence

```
1. Send ACPI shutdown signal to guest OS
2. Wait up to timeout_seconds (default 90s) for clean shutdown
3. If timeout: force kill VM process (virsh destroy equivalent)
4. Remove iptables rules
5. Release TAP device (do not delete — delete is on DeleteInstance)
6. Return STOPPED state
```

### DeleteInstance Sequence

```
1. If running: force stop VM process
2. Delete TAP device
3. Remove iptables rules
4. Delete metadata service entry
5. If delete_root_disk: delete qcow2 overlay from NFS
6. Return DELETED state
```

---

## 8. State Reporting

The Host Agent pushes state reports to the control plane asynchronously:

**Instance state updates:** `POST /api/v1/host/{host_id}/instance_status`

```json
{
  "instance_id": "inst_...",
  "state": "RUNNING",
  "timestamp": "2026-04-05T12:00:00Z",
  "host_pid": "12345"
}
```

**Heartbeat:** `POST /api/v1/host/{host_id}/heartbeat` every 30 seconds with full instance inventory.

**Control plane polling:** Worker polls `GET /api/v1/host/{host_id}/desired_instances` every 5 seconds. Tolerates control plane unavailability — worker continues running existing instances on last known state.

---

## 9. Open Implementation Decisions

| ID | Question | Impact |
|----|----------|--------|
| OID-RT-1 | Should `StartInstance` require a full `InstanceConfig` (for re-provisioning on new host) or just `instance_id` (for resuming on same host)? Master Blueprint implies full config for start since host may change. | Request message shape. |
| OID-RT-2 | Exact Firecracker API version and kernel binary sourcing strategy? | Host Agent implementation detail. |
| OID-RT-3 | Should `StopInstance.force` be exposed in Phase 1 or always false with internal force-after-timeout? | API surface for Phase 2 hard reboot. |
