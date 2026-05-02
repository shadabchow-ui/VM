package runtimeclient

// client.go — HTTP client for the Host Agent's RuntimeService.
//
// Source: RUNTIMESERVICE_GRPC_V1 §2 (service definition),
//         IMPLEMENTATION_PLAN_V1 §C1 (Host Agent VM lifecycle primitives).
//
// M2 implementation note:
// The proto contract specifies gRPC transport. For M2, we implement an HTTP/JSON
// client that matches the RuntimeService interface exactly. This avoids a protoc
// dependency while keeping the contract boundary clean. The worker calls this
// client the same way it would call a gRPC stub. Replacing with gRPC is a pure
// transport swap — no caller changes required.
//
// The client communicates with the Host Agent's RuntimeService HTTP server
// (services/host-agent/runtime/service.go) over an authenticated internal channel.
//
// Timeouts (from RUNTIMESERVICE_GRPC_V1 §timeout):
//   Default RPC timeout:         60 seconds
//   CreateInstance timeout:      300 seconds (rootfs materialisation + VM boot)
//
// Authentication: in production, the HTTP client uses a mTLS certificate
// issued to the worker service identity. For M2, the internal gateway handles
// authentication at the network edge — the client sends requests plaintext
// on the internal network.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultTimeout        = 60 * time.Second
	createInstanceTimeout = 300 * time.Second
)

// ── Request / response types (mirror runtime/service.go) ─────────────────────

// NetworkConfig matches the NetworkConfig message in runtime.proto.
type NetworkConfig struct {
	PrivateIP  string `json:"private_ip"`
	PublicIP   string `json:"public_ip"`
	TapDevice  string `json:"tap_device"`
	MacAddress string `json:"mac_address"`
}

// ExtraDiskConfig describes an additional block device to attach to a VM.
type ExtraDiskConfig struct {
	DiskID     string `json:"disk_id"`
	HostPath   string `json:"host_path"`
	DeviceName string `json:"device_name"`
}

// CreateInstanceRequest matches the CreateInstanceRequest proto message.
type CreateInstanceRequest struct {
	InstanceID     string            `json:"instance_id"`
	ImageURL       string            `json:"image_url"`
	InstanceTypeID string            `json:"instance_type_id"`
	CPUCores       int32             `json:"cpu_cores"`
	MemoryMB       int32             `json:"memory_mb"`
	DiskGB         int32             `json:"disk_gb"`
	RootfsPath     string            `json:"rootfs_path"`
	Network        NetworkConfig     `json:"network"`
	SSHPublicKey   string            `json:"ssh_public_key"`
	ExtraDisks     []ExtraDiskConfig `json:"extra_disks,omitempty"`
}

// CreateInstanceResponse matches the CreateInstanceResponse proto message.
type CreateInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"` // "RUNNING"
}

// StopInstanceRequest matches the StopInstanceRequest proto message.
type StopInstanceRequest struct {
	InstanceID     string `json:"instance_id"`
	TimeoutSeconds int32  `json:"timeout_seconds"`
}

// StopInstanceResponse matches the StopInstanceResponse proto message.
type StopInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"` // "STOPPED"
}

// DeleteInstanceRequest matches the DeleteInstanceRequest proto message.
type DeleteInstanceRequest struct {
	InstanceID     string `json:"instance_id"`
	DeleteRootDisk bool   `json:"delete_root_disk"`
}

// DeleteInstanceResponse matches the DeleteInstanceResponse proto message.
type DeleteInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"` // "DELETED"
}

// InstanceStatus matches the InstanceStatus proto message.
type InstanceStatus struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
	HostPID    int32  `json:"host_pid"`
}

// ListInstancesResponse matches the ListInstancesResponse proto message.
type ListInstancesResponse struct {
	Instances []InstanceStatus `json:"instances"`
}

// ── Client ────────────────────────────────────────────────────────────────────

// Client is the runtime client for a single Host Agent.
// One Client instance per host. Safe for concurrent use.
type Client struct {
	hostID  string
	baseURL string // e.g. "http://host-abc123:50051"
	http    *http.Client
}

// NewClient constructs a Client targeting the Host Agent at the given address.
// address format: "host-{id}:50051" — the client prepends "http://".
// Inject a custom http.Client for mTLS or testing.
func NewClient(hostID, address string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		hostID:  hostID,
		baseURL: "http://" + address,
		http:    httpClient,
	}
}

// CreateInstance materialises a VM on the host and boots it.
// Uses a 300-second timeout to accommodate rootfs materialisation.
// Source: RUNTIMESERVICE_GRPC_V1 §CreateInstance.
func (c *Client) CreateInstance(ctx context.Context, req *CreateInstanceRequest) (*CreateInstanceResponse, error) {
	// Override context deadline for the long create operation.
	createCtx, cancel := context.WithTimeout(ctx, createInstanceTimeout)
	defer cancel()

	var resp CreateInstanceResponse
	if err := c.post(createCtx, "/runtime/v1/instances", req, &resp); err != nil {
		return nil, fmt.Errorf("CreateInstance host=%s instance=%s: %w", c.hostID, req.InstanceID, err)
	}
	return &resp, nil
}

// StopInstance stops a running VM via ACPI, then force-kills on timeout.
// Source: RUNTIMESERVICE_GRPC_V1 §StopInstance.
func (c *Client) StopInstance(ctx context.Context, req *StopInstanceRequest) (*StopInstanceResponse, error) {
	var resp StopInstanceResponse
	if err := c.post(ctx, "/runtime/v1/instances/stop", req, &resp); err != nil {
		return nil, fmt.Errorf("StopInstance host=%s instance=%s: %w", c.hostID, req.InstanceID, err)
	}
	return &resp, nil
}

// DeleteInstance destroys all VM resources on the host.
// Source: RUNTIMESERVICE_GRPC_V1 §DeleteInstance.
func (c *Client) DeleteInstance(ctx context.Context, req *DeleteInstanceRequest) (*DeleteInstanceResponse, error) {
	var resp DeleteInstanceResponse
	if err := c.post(ctx, "/runtime/v1/instances/delete", req, &resp); err != nil {
		return nil, fmt.Errorf("DeleteInstance host=%s instance=%s: %w", c.hostID, req.InstanceID, err)
	}
	return &resp, nil
}

// ListInstances returns the state of all VMs currently on this host.
// Used by the reconciler to detect drift.
// Source: RUNTIMESERVICE_GRPC_V1 §ListInstances.
func (c *Client) ListInstances(ctx context.Context) (*ListInstancesResponse, error) {
	var resp ListInstancesResponse
	if err := c.get(ctx, "/runtime/v1/instances", &resp); err != nil {
		return nil, fmt.Errorf("ListInstances host=%s: %w", c.hostID, err)
	}
	return &resp, nil
}

// ── HTTP transport ────────────────────────────────────────────────────────────

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, out)
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
