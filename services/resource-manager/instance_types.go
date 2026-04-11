package main

// instance_types.go — Public API request/response DTOs.
//
// PASS 1: CreateInstanceRequest, InstanceResponse, ListInstancesResponse.
// PASS 2: LifecycleResponse.
// PASS 3: JobResponse for GET /v1/instances/{id}/jobs/{job_id}.
// M7:     Added PublicIP, PrivateIP to InstanceResponse.
//         Added SSHKeyResponse, CreateSSHKeyRequest, ListSSHKeysResponse.
//         Added EventResponse, ListEventsResponse.
//
// Source: INSTANCE_MODEL_V1 §2, JOB_MODEL_V1 §1, 08-01 §3,
//         09-01 §detail card, 10-02 §SSH key handling.

import "time"

// ── Instance resource ─────────────────────────────────────────────────────────

// InstanceResponse is the canonical JSON shape returned by instance endpoints.
// M7: added PublicIP, PrivateIP from ip_allocations join.
// Source: INSTANCE_MODEL_V1 §2.
type InstanceResponse struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	InstanceType     string            `json:"instance_type"`
	ImageID          string            `json:"image_id"`
	AvailabilityZone string            `json:"availability_zone"`
	Region           string            `json:"region"`
	Labels           map[string]string `json:"labels"`
	Networking       *InstanceNetworkingResponse `json:"networking,omitempty"`
	PublicIP         *string           `json:"public_ip"`
	PrivateIP        *string           `json:"private_ip"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

// ── Create ────────────────────────────────────────────────────────────────────

// CreateInstanceRequest is the payload for POST /v1/instances.
// Source: 08-01 §CreateInstance, INSTANCE_MODEL_V1 §2, 08-02 §validation.
type CreateInstanceRequest struct {
	Name             string            `json:"name"`
	InstanceType     string            `json:"instance_type"`
	ImageID          string            `json:"image_id"`
	AvailabilityZone string            `json:"availability_zone"`
	SSHKeyName       string            `json:"ssh_key_name"`
	Labels           map[string]string `json:"labels"`
	Networking       *NetworkingConfig `json:"networking,omitempty"`
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
