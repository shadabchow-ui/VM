package main

// instance_types.go — Public API request/response DTOs.
//
// PASS 1 scope: CreateInstanceRequest, InstanceResponse, ListInstancesResponse.
// Job fields and lifecycle action responses are added in later passes.
//
// Source: INSTANCE_MODEL_V1 §2 (canonical API resource shape),
//         08-01-api-resource-model-and-endpoint-design.md §3.

import "time"

// ── Instance resource ─────────────────────────────────────────────────────────

// InstanceResponse is the canonical JSON shape returned by instance endpoints.
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
