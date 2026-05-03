package runtimeclient

import (
	"context"
	"fmt"
	"time"

	runtimev1 "github.com/compute-platform/compute-platform/packages/contracts/runtimev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// GRPCClient is the production gRPC client for the Host Agent RuntimeService.
// It uses the generated gRPC stub and converts between the worker-facing
// runtimeclient types and the proto-generated runtimev1 types.
//
// One GRPCClient per host. Safe for concurrent use.
//
// Source: RUNTIMESERVICE_GRPC_V1 §2 (service definition),
//
//	IMPLEMENTATION_PLAN_V1 §C1 (Host Agent VM lifecycle primitives).
type GRPCClient struct {
	hostID string
	conn   *grpc.ClientConn
	stub   runtimev1.RuntimeServiceClient
}

// NewGRPCClient constructs a GRPCClient targeting the Host Agent at the given address.
// address format: "host-{id}:50051".
// If dialOpts is nil, uses insecure credentials (dev-only).
func NewGRPCClient(hostID, address string, dialOpts ...grpc.DialOption) (*GRPCClient, error) {
	if len(dialOpts) == 0 {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, address, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial host=%s addr=%s: %w", hostID, address, err)
	}

	return &GRPCClient{
		hostID: hostID,
		conn:   conn,
		stub:   runtimev1.NewRuntimeServiceClient(conn),
	}, nil
}

// Close closes the underlying gRPC connection.
func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

// CreateInstance materialises a VM on the host and boots it.
// Uses a 300-second timeout to accommodate rootfs materialisation.
func (c *GRPCClient) CreateInstance(ctx context.Context, req *CreateInstanceRequest) (*CreateInstanceResponse, error) {
	createCtx, cancel := context.WithTimeout(ctx, createInstanceTimeout)
	defer cancel()

	rpcReq := toProtoCreateRequest(req)
	rpcResp, err := c.stub.CreateInstance(createCtx, rpcReq)
	if err != nil {
		return nil, fmt.Errorf("CreateInstance host=%s instance=%s: %w", c.hostID, req.InstanceID, err)
	}
	return fromProtoCreateResponse(rpcResp), nil
}

// StopInstance stops a running VM via ACPI, then force-kills on timeout.
func (c *GRPCClient) StopInstance(ctx context.Context, req *StopInstanceRequest) (*StopInstanceResponse, error) {
	rpcReq := toProtoStopRequest(req)
	rpcResp, err := c.stub.StopInstance(ctx, rpcReq)
	if err != nil {
		return nil, fmt.Errorf("StopInstance host=%s instance=%s: %w", c.hostID, req.InstanceID, err)
	}
	return fromProtoStopResponse(rpcResp), nil
}

// DeleteInstance destroys all VM resources on the host.
func (c *GRPCClient) DeleteInstance(ctx context.Context, req *DeleteInstanceRequest) (*DeleteInstanceResponse, error) {
	rpcReq := toProtoDeleteRequest(req)
	rpcResp, err := c.stub.DeleteInstance(ctx, rpcReq)
	if err != nil {
		return nil, fmt.Errorf("DeleteInstance host=%s instance=%s: %w", c.hostID, req.InstanceID, err)
	}
	return fromProtoDeleteResponse(rpcResp), nil
}

// StartInstance starts a previously stopped VM.
func (c *GRPCClient) StartInstance(ctx context.Context, req *StartInstanceRequest) (*StartInstanceResponse, error) {
	rpcReq := toProtoStartRequest(req)
	rpcResp, err := c.stub.StartInstance(ctx, rpcReq)
	if err != nil {
		return nil, fmt.Errorf("StartInstance host=%s instance=%s: %w", c.hostID, req.InstanceID, err)
	}
	return fromProtoStartResponse(rpcResp), nil
}

// ListInstances returns the state of all VMs currently on this host.
func (c *GRPCClient) ListInstances(ctx context.Context) (*ListInstancesResponse, error) {
	rpcResp, err := c.stub.ListInstances(ctx, &runtimev1.ListInstancesRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListInstances host=%s: %w", c.hostID, err)
	}
	return fromProtoListResponse(rpcResp), nil
}

// StartInstanceRequest matches the proto StartInstanceRequest for public API.
type StartInstanceRequest struct {
	InstanceID string `json:"instance_id"`
}

// StartInstanceResponse matches the proto StartInstanceResponse for public API.
type StartInstanceResponse struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
}

// ── Proto conversion helpers ──────────────────────────────────────────────────

func toProtoCreateRequest(req *CreateInstanceRequest) *runtimev1.CreateInstanceRequest {
	return &runtimev1.CreateInstanceRequest{
		InstanceId:     req.InstanceID,
		ImageUrl:       req.ImageURL,
		InstanceTypeId: req.InstanceTypeID,
		CpuCores:       req.CPUCores,
		MemoryMb:       req.MemoryMB,
		DiskGb:         req.DiskGB,
		RootfsPath:     req.RootfsPath,
		Network: &runtimev1.NetworkConfig{
			PrivateIp:  req.Network.PrivateIP,
			PublicIp:   req.Network.PublicIP,
			TapDevice:  req.Network.TapDevice,
			MacAddress: req.Network.MacAddress,
		},
		SshPublicKey: req.SSHPublicKey,
	}
}

func fromProtoCreateResponse(resp *runtimev1.CreateInstanceResponse) *CreateInstanceResponse {
	return &CreateInstanceResponse{
		InstanceID: resp.InstanceId,
		State:      resp.State,
	}
}

func toProtoStopRequest(req *StopInstanceRequest) *runtimev1.StopInstanceRequest {
	return &runtimev1.StopInstanceRequest{
		InstanceId:     req.InstanceID,
		TimeoutSeconds: req.TimeoutSeconds,
	}
}

func fromProtoStopResponse(resp *runtimev1.StopInstanceResponse) *StopInstanceResponse {
	return &StopInstanceResponse{
		InstanceID: resp.InstanceId,
		State:      resp.State,
	}
}

func toProtoDeleteRequest(req *DeleteInstanceRequest) *runtimev1.DeleteInstanceRequest {
	return &runtimev1.DeleteInstanceRequest{
		InstanceId:     req.InstanceID,
		DeleteRootDisk: req.DeleteRootDisk,
	}
}

func fromProtoDeleteResponse(resp *runtimev1.DeleteInstanceResponse) *DeleteInstanceResponse {
	return &DeleteInstanceResponse{
		InstanceID: resp.InstanceId,
		State:      resp.State,
	}
}

func toProtoStartRequest(req *StartInstanceRequest) *runtimev1.StartInstanceRequest {
	return &runtimev1.StartInstanceRequest{
		InstanceId: req.InstanceID,
	}
}

func fromProtoStartResponse(resp *runtimev1.StartInstanceResponse) *StartInstanceResponse {
	return &StartInstanceResponse{
		InstanceID: resp.InstanceId,
		State:      resp.State,
	}
}

func fromProtoListResponse(resp *runtimev1.ListInstancesResponse) *ListInstancesResponse {
	var instances []InstanceStatus
	for _, s := range resp.Instances {
		instances = append(instances, InstanceStatus{
			InstanceID: s.InstanceId,
			State:      s.State,
			HostPID:    s.HostPid,
		})
	}
	return &ListInstancesResponse{Instances: instances}
}
