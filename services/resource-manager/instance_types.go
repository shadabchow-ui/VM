package main

// instance_types.go — Public API request/response DTOs.
//
// PASS 1: CreateInstanceRequest, InstanceResponse, ListInstancesResponse.
// PASS 2: LifecycleResponse.
// PASS 3: JobResponse for GET /v1/instances/{id}/jobs/{job_id}.
// M7:     Added PublicIP, PrivateIP to InstanceResponse.
//         Added SSHKeyResponse, CreateSSHKeyRequest, ListSSHKeysResponse.
//         Added EventResponse, ListEventsResponse.
// M10 Slice 4: Added BlockDeviceMapping, BlockDevices field on create request
//         and InstanceResponse. Source: INSTANCE_MODEL_V1 §2 (block_devices),
//         execution_blueprint §7.7 (block_devices: [{image_id, size_gb, delete_on_termination}]),
//         P2_VOLUME_MODEL §1, P2_MIGRATION_COMPATIBILITY_RULES §7.2.
// VM-P2D Slice 4: Added ProjectID to CreateInstanceRequest and InstanceResponse.
//         When project_id is set in the request, the instance is created in project
//         scope: owner_principal_id is set to the project's principal_id, and quota
//         is scoped to that project's principal_id.
//         Source: vm-16-01__blueprint__ §quota_enforcement_point,
//                 AUTH_OWNERSHIP_MODEL_V1 §3 (project-scoped ownership),
//                 P2_PROJECT_RBAC_MODEL.md §2.4.
//
// Source: INSTANCE_MODEL_V1 §2, JOB_MODEL_V1 §1, 08-01 §3,
//         09-01 §detail card, 10-02 §SSH key handling.

import "time"

// ── Instance resource ─────────────────────────────────────────────────────────

// InstanceResponse is the canonical JSON shape returned by instance endpoints.
// M7: added PublicIP, PrivateIP from ip_allocations join.
// M10 Slice 4: added BlockDevices.
// VM-P2D Slice 4: added ProjectID (omitempty — nil for classic/no-project instances).
// Source: INSTANCE_MODEL_V1 §2.
type InstanceResponse struct {
	ID               string                     `json:"id"`
	Name             string                     `json:"name"`
	Status           string                     `json:"status"`
	InstanceType     string                     `json:"instance_type"`
	ImageID          string                     `json:"image_id"`
	ImageFamily      string                     `json:"image_family,omitempty"`
	ImageVersion     *string                    `json:"image_version,omitempty"`
	AvailabilityZone string                     `json:"availability_zone"`
	Region           string                     `json:"region"`
	// ProjectID is the project this instance belongs to.
	// Nil for classic (no-project) instances created without a project_id.
	// When set, owner_principal_id in DB equals the project's principal_id.
	// Source: VM-P2D Slice 4, AUTH_OWNERSHIP_MODEL_V1 §3.
	ProjectID        *string                    `json:"project_id,omitempty"`
	Labels           map[string]string          `json:"labels"`
	BlockDevices     []BlockDeviceMapping       `json:"block_devices"`
	Networking       *InstanceNetworkingResponse `json:"networking,omitempty"`
	PublicIP         *string                    `json:"public_ip"`
	PrivateIP        *string                    `json:"private_ip"`
	CreatedAt        time.Time                  `json:"created_at"`
	UpdatedAt        time.Time                  `json:"updated_at"`
}

// ── Block device mapping (M10 Slice 4) ──────────────────────────────────────

// BlockDeviceMapping represents a single block device in the create request
// and instance response. Phase 1: exactly one entry (root disk).
// Phase 1 constraint: delete_on_termination must be true.
// Source: INSTANCE_MODEL_V1 §2 (block_devices item shape),
//         execution_blueprint §7.7, 12-03-risks-and-phase-2-expansion.md.
type BlockDeviceMapping struct {
	ImageID             string `json:"image_id"`
	SizeGB              int    `json:"size_gb"`
	DeleteOnTermination bool   `json:"delete_on_termination"`
}

// ── Create ────────────────────────────────────────────────────────────────────

// CreateInstanceRequest is the payload for POST /v1/instances.
// M10 Slice 4: added BlockDevices. When omitted, the handler synthesizes
// a default entry from image_id + shape disk size + delete_on_termination=true.
// VM-P2D Slice 4: added ProjectID. When set, the instance is created in project
// scope. The project must exist and be owned by the calling principal.
// Source: 08-01 §CreateInstance, INSTANCE_MODEL_V1 §2, 08-02 §validation,
//         execution_blueprint §7.7, AUTH_OWNERSHIP_MODEL_V1 §3.
type CreateInstanceRequest struct {
	Name             string               `json:"name"`
	InstanceType     string               `json:"instance_type"`
	ImageID          string               `json:"image_id,omitempty"`
	ImageFamily      *ImageFamilyRef      `json:"image_family,omitempty"`
	AvailabilityZone string               `json:"availability_zone"`
	SSHKeyName       string               `json:"ssh_key_name"`
	Labels           map[string]string    `json:"labels"`
	Networking       *NetworkingConfig    `json:"networking,omitempty"`
	BlockDevices     []BlockDeviceMapping `json:"block_devices,omitempty"`
	// ProjectID scopes the instance to a project. When present the project must
	// exist and be owned by the calling principal; owner_principal_id is set to
	// the project's principal_id. When absent, classic (user-principal) scope is used.
	// Source: VM-P2D Slice 4, AUTH_OWNERSHIP_MODEL_V1 §3.
	ProjectID        *string              `json:"project_id,omitempty"`
}


// CreateInstanceResponse is returned from POST /v1/instances with 202 Accepted.
// Source: 08-01 §CreateInstance response.
type CreateInstanceResponse struct {
	Instance InstanceResponse `json:"instance"`
}

// ── List ──────────────────────────────────────────────────────────────────────

// ListInstancesResponse wraps the instance list.
// Source: 08-01 §ListInstances.
type ListInstancesResponse struct {
	Instances []InstanceResponse `json:"instances"`
	Total     int                `json:"total"`
}

// ── Lifecycle action response ─────────────────────────────────────────────────

// LifecycleResponse is returned by delete/stop/start/reboot endpoints.
// Contains the enqueued job_id so the caller can poll for completion.
// Source: JOB_MODEL_V1 §1, 08-01 §lifecycle endpoints.
type LifecycleResponse struct {
	InstanceID string `json:"instance_id"`
	JobID      string `json:"job_id"`
	Action     string `json:"action"`
}

// ── Job status response ───────────────────────────────────────────────────────

// JobResponse is the canonical JSON shape for GET /v1/instances/{id}/jobs/{job_id}.
// Only exposes fields appropriate for external clients per JOB_MODEL_V1 §1.
// Internal-only fields (idempotency_key, claimed_at) are not exposed.
// Source: JOB_MODEL_V1 §1, 08-01 §job status endpoint.
type JobResponse struct {
	ID           string     `json:"id"`
	InstanceID   string     `json:"instance_id"`
	JobType      string     `json:"job_type"`
	Status       string     `json:"status"`
	AttemptCount int        `json:"attempt_count"`
	MaxAttempts  int        `json:"max_attempts"`
	ErrorMessage *string    `json:"error_message,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// ── SSH Key responses (M7) ────────────────────────────────────────────────────

// SSHKeyResponse is the external shape for an SSH key.
// Never returns the full public_key text after creation.
// Source: 10-02 §Connection Credential Display.
type SSHKeyResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	KeyType     string    `json:"key_type"`
	CreatedAt   time.Time `json:"created_at"`
}

// CreateSSHKeyRequest is the payload for POST /v1/ssh-keys.
// Source: 10-02 §SSH Public Key Intake and Validation.
type CreateSSHKeyRequest struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

// ListSSHKeysResponse wraps the SSH key list.
type ListSSHKeysResponse struct {
	SSHKeys []SSHKeyResponse `json:"ssh_keys"`
	Total   int              `json:"total"`
}

// ── Event responses (M7) ──────────────────────────────────────────────────────

// EventResponse is the external shape for an instance event.
// Source: EVENTS_SCHEMA_V1 §2, 09-01 §Event History Card.
type EventResponse struct {
	ID        string    `json:"id"`
	EventType string    `json:"event_type"`
	Message   *string   `json:"message,omitempty"`
	Actor     *string   `json:"actor,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ListEventsResponse wraps the event list.
type ListEventsResponse struct {
	Events []EventResponse `json:"events"`
	Total  int             `json:"total"`
}

// ── M9 Slice 4: Networking types ──────────────────────────────────────────────

// NetworkingConfig holds optional networking config for instance creation.
type NetworkingConfig struct {
	SubnetID         string   `json:"subnet_id,omitempty"`
	SecurityGroupIDs []string `json:"security_group_ids,omitempty"`
}

// InstanceNetworkingResponse holds networking info in responses.
type InstanceNetworkingResponse struct {
	VPCID            string                    `json:"vpc_id"`
	SubnetID         string                    `json:"subnet_id"`
	PrimaryInterface *NetworkInterfaceResponse `json:"primary_interface,omitempty"`
}

// NetworkInterfaceResponse represents a NIC in API responses.
type NetworkInterfaceResponse struct {
	ID               string   `json:"id"`
	PrivateIP        string   `json:"private_ip"`
	MACAddress       string   `json:"mac_address"`
	Status           string   `json:"status"`
	SecurityGroupIDs []string `json:"security_group_ids,omitempty"`
}
