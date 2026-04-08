package domainmodel

import "time"

// HostStatus represents the operational state of a physical host. Source: 05-02-host-runtime-worker-design.md.
type HostStatus string

const (
	HostStatusProvisioning HostStatus = "provisioning"
	HostStatusReady        HostStatus = "ready"
	HostStatusDraining     HostStatus = "draining"
	HostStatusMaintenance  HostStatus = "maintenance"
	HostStatusOffline      HostStatus = "offline"
)

// Host is the inventory record for a physical hypervisor. Source: IMPLEMENTATION_PLAN_V1 §B2, 05-02.
type Host struct {
	ID               string     `db:"id"`              // CN=host-{host_id} in mTLS cert
	AvailabilityZone string     `db:"availability_zone"`
	Status           HostStatus `db:"status"`
	TotalCPU         int        `db:"total_cpu"`       // physical cores
	TotalMemoryMB    int        `db:"total_memory_mb"`
	TotalDiskGB      int        `db:"total_disk_gb"`
	UsedCPU          int        `db:"used_cpu"`
	UsedMemoryMB     int        `db:"used_memory_mb"`
	UsedDiskGB       int        `db:"used_disk_gb"`
	AgentVersion     string     `db:"agent_version"`
	LastHeartbeatAt  *time.Time `db:"last_heartbeat_at"`
	RegisteredAt     time.Time  `db:"registered_at"`
	UpdatedAt        time.Time  `db:"updated_at"`
}

// AvailableCPU returns free CPU cores.
func (h *Host) AvailableCPU() int { return h.TotalCPU - h.UsedCPU }

// AvailableMemoryMB returns free memory.
func (h *Host) AvailableMemoryMB() int { return h.TotalMemoryMB - h.UsedMemoryMB }

// AvailableDiskGB returns free disk.
func (h *Host) AvailableDiskGB() int { return h.TotalDiskGB - h.UsedDiskGB }

// CanFit returns true if the host has enough free resources for the requested shape.
func (h *Host) CanFit(cpuCores, memoryMB, diskGB int) bool {
	return h.Status == HostStatusReady &&
		h.AvailableCPU() >= cpuCores &&
		h.AvailableMemoryMB() >= memoryMB &&
		h.AvailableDiskGB() >= diskGB
}

// HostRegistrationRequest is what the Host Agent sends on startup.
// Source: 05-02-host-runtime-worker-design.md §Bootstrap sequence.
type HostRegistrationRequest struct {
	HostID           string `json:"host_id"`
	AvailabilityZone string `json:"availability_zone"`
	TotalCPU         int    `json:"total_cpu"`
	TotalMemoryMB    int    `json:"total_memory_mb"`
	TotalDiskGB      int    `json:"total_disk_gb"`
	AgentVersion     string `json:"agent_version"`
}

// HostRegistrationResponse is returned by the Resource Manager on successful registration.
type HostRegistrationResponse struct {
	HostID    string `json:"host_id"`
	Status    string `json:"status"`
	Message   string `json:"message"`
}

// HostHeartbeatRequest is the periodic inventory update. Source: RUNTIMESERVICE_GRPC_V1 §8.
type HostHeartbeatRequest struct {
	HostID        string `json:"host_id"`
	UsedCPU       int    `json:"used_cpu"`
	UsedMemoryMB  int    `json:"used_memory_mb"`
	UsedDiskGB    int    `json:"used_disk_gb"`
	AgentVersion  string `json:"agent_version"`
}
