package runtimeclient

import (
	"context"
	"net"
	"testing"
	"time"

	runtimev1 "github.com/compute-platform/compute-platform/packages/contracts/runtimev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1024 * 1024

// fakeGRPCServer implements runtimev1.RuntimeServiceServer for testing.
type fakeGRPCServer struct {
	runtimev1.UnimplementedRuntimeServiceServer

	createFunc func(ctx context.Context, req *runtimev1.CreateInstanceRequest) (*runtimev1.CreateInstanceResponse, error)
	stopFunc   func(ctx context.Context, req *runtimev1.StopInstanceRequest) (*runtimev1.StopInstanceResponse, error)
	deleteFunc func(ctx context.Context, req *runtimev1.DeleteInstanceRequest) (*runtimev1.DeleteInstanceResponse, error)
	listFunc   func(ctx context.Context, req *runtimev1.ListInstancesRequest) (*runtimev1.ListInstancesResponse, error)
	startFunc  func(ctx context.Context, req *runtimev1.StartInstanceRequest) (*runtimev1.StartInstanceResponse, error)
}

func (f *fakeGRPCServer) CreateInstance(ctx context.Context, req *runtimev1.CreateInstanceRequest) (*runtimev1.CreateInstanceResponse, error) {
	if f.createFunc != nil {
		return f.createFunc(ctx, req)
	}
	return &runtimev1.CreateInstanceResponse{InstanceId: req.InstanceId, State: "CREATED"}, nil
}

func (f *fakeGRPCServer) StopInstance(ctx context.Context, req *runtimev1.StopInstanceRequest) (*runtimev1.StopInstanceResponse, error) {
	if f.stopFunc != nil {
		return f.stopFunc(ctx, req)
	}
	return &runtimev1.StopInstanceResponse{InstanceId: req.InstanceId, State: "STOPPED"}, nil
}

func (f *fakeGRPCServer) DeleteInstance(ctx context.Context, req *runtimev1.DeleteInstanceRequest) (*runtimev1.DeleteInstanceResponse, error) {
	if f.deleteFunc != nil {
		return f.deleteFunc(ctx, req)
	}
	return &runtimev1.DeleteInstanceResponse{InstanceId: req.InstanceId, State: "DELETED"}, nil
}

func (f *fakeGRPCServer) StartInstance(ctx context.Context, req *runtimev1.StartInstanceRequest) (*runtimev1.StartInstanceResponse, error) {
	if f.startFunc != nil {
		return f.startFunc(ctx, req)
	}
	return &runtimev1.StartInstanceResponse{InstanceId: req.InstanceId, State: "RUNNING"}, nil
}

func (f *fakeGRPCServer) ListInstances(ctx context.Context, req *runtimev1.ListInstancesRequest) (*runtimev1.ListInstancesResponse, error) {
	if f.listFunc != nil {
		return f.listFunc(ctx, req)
	}
	return &runtimev1.ListInstancesResponse{}, nil
}

func newBufconnGRPCClient(t *testing.T, srv *fakeGRPCServer) *GRPCClient {
	t.Helper()

	lis := bufconn.Listen(bufSize)
	gs := grpc.NewServer()
	runtimev1.RegisterRuntimeServiceServer(gs, srv)

	go func() {
		_ = gs.Serve(lis)
	}()
	t.Cleanup(gs.Stop)

	client, err := NewGRPCClient("host-test", "bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.Dial()
	}), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewGRPCClient: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// ── CreateInstance gRPC tests ────────────────────────────────────────────────

func TestGRPCClient_CreateInstance_HappyPath(t *testing.T) {
	srv := &fakeGRPCServer{
		createFunc: func(ctx context.Context, req *runtimev1.CreateInstanceRequest) (*runtimev1.CreateInstanceResponse, error) {
			if req.InstanceId != "inst_test001" {
				t.Errorf("InstanceId = %q, want inst_test001", req.InstanceId)
			}
			if req.CpuCores != 2 {
				t.Errorf("CpuCores = %d, want 2", req.CpuCores)
			}
			return &runtimev1.CreateInstanceResponse{InstanceId: req.InstanceId, State: "CREATED"}, nil
		},
	}
	client := newBufconnGRPCClient(t, srv)

	resp, err := client.CreateInstance(context.Background(), &CreateInstanceRequest{
		InstanceID: "inst_test001",
		CPUCores:   2,
		MemoryMB:   4096,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if resp.InstanceID != "inst_test001" {
		t.Errorf("InstanceID = %q, want inst_test001", resp.InstanceID)
	}
	if resp.State != "CREATED" {
		t.Errorf("State = %q, want CREATED", resp.State)
	}
}

func TestGRPCClient_CreateInstance_NetworkFieldsPropagated(t *testing.T) {
	var captured *runtimev1.CreateInstanceRequest
	srv := &fakeGRPCServer{
		createFunc: func(ctx context.Context, req *runtimev1.CreateInstanceRequest) (*runtimev1.CreateInstanceResponse, error) {
			captured = req
			return &runtimev1.CreateInstanceResponse{InstanceId: req.InstanceId, State: "CREATED"}, nil
		},
	}
	client := newBufconnGRPCClient(t, srv)

	_, _ = client.CreateInstance(context.Background(), &CreateInstanceRequest{
		InstanceID: "inst_net001",
		CPUCores:   4,
		MemoryMB:   8192,
		Network: NetworkConfig{
			PrivateIP:  "10.0.0.5",
			PublicIP:   "203.0.113.10",
			TapDevice:  "tap-abc",
			MacAddress: "02:aa:bb:cc:dd:ee",
		},
		SSHPublicKey: "ssh-ed25519 AAAA...",
	})

	if captured == nil {
		t.Fatal("request not captured")
	}
	if captured.Network.GetPrivateIp() != "10.0.0.5" {
		t.Errorf("Network.PrivateIp = %q, want 10.0.0.5", captured.Network.GetPrivateIp())
	}
	if captured.Network.GetPublicIp() != "203.0.113.10" {
		t.Errorf("Network.PublicIp = %q, want 203.0.113.10", captured.Network.GetPublicIp())
	}
	if captured.Network.GetTapDevice() != "tap-abc" {
		t.Errorf("Network.TapDevice = %q, want tap-abc", captured.Network.GetTapDevice())
	}
	if captured.SshPublicKey != "ssh-ed25519 AAAA..." {
		t.Errorf("SshPublicKey = %q, want ssh-ed25519 AAAA...", captured.SshPublicKey)
	}
}

func TestGRPCClient_CreateInstance_PropagatesError(t *testing.T) {
	srv := &fakeGRPCServer{
		createFunc: func(ctx context.Context, req *runtimev1.CreateInstanceRequest) (*runtimev1.CreateInstanceResponse, error) {
			return nil, status.Error(codes.Internal, "host agent internal error")
		},
	}
	client := newBufconnGRPCClient(t, srv)

	_, err := client.CreateInstance(context.Background(), &CreateInstanceRequest{InstanceID: "inst_x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestGRPCClient_CreateInstance_UsesLongTimeout(t *testing.T) {
	if createInstanceTimeout < 300*time.Second {
		t.Errorf("createInstanceTimeout = %v, want >= 300s", createInstanceTimeout)
	}
}

// ── StopInstance gRPC tests ──────────────────────────────────────────────────

func TestGRPCClient_StopInstance_HappyPath(t *testing.T) {
	var captured *runtimev1.StopInstanceRequest
	srv := &fakeGRPCServer{
		stopFunc: func(ctx context.Context, req *runtimev1.StopInstanceRequest) (*runtimev1.StopInstanceResponse, error) {
			captured = req
			return &runtimev1.StopInstanceResponse{InstanceId: req.InstanceId, State: "STOPPED"}, nil
		},
	}
	client := newBufconnGRPCClient(t, srv)

	resp, err := client.StopInstance(context.Background(), &StopInstanceRequest{
		InstanceID:     "inst_stop001",
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if resp.State != "STOPPED" {
		t.Errorf("State = %q, want STOPPED", resp.State)
	}
	if captured.TimeoutSeconds != 30 {
		t.Errorf("TimeoutSeconds = %d, want 30", captured.TimeoutSeconds)
	}
}

// ── DeleteInstance gRPC tests ────────────────────────────────────────────────

func TestGRPCClient_DeleteInstance_HappyPath(t *testing.T) {
	var captured *runtimev1.DeleteInstanceRequest
	srv := &fakeGRPCServer{
		deleteFunc: func(ctx context.Context, req *runtimev1.DeleteInstanceRequest) (*runtimev1.DeleteInstanceResponse, error) {
			captured = req
			return &runtimev1.DeleteInstanceResponse{InstanceId: req.InstanceId, State: "DELETED"}, nil
		},
	}
	client := newBufconnGRPCClient(t, srv)

	resp, err := client.DeleteInstance(context.Background(), &DeleteInstanceRequest{
		InstanceID:     "inst_del001",
		DeleteRootDisk: true,
	})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if resp.State != "DELETED" {
		t.Errorf("State = %q, want DELETED", resp.State)
	}
	if !captured.DeleteRootDisk {
		t.Error("DeleteRootDisk not propagated")
	}
}

// ── ListInstances gRPC tests ─────────────────────────────────────────────────

func TestGRPCClient_ListInstances_HappyPath(t *testing.T) {
	srv := &fakeGRPCServer{
		listFunc: func(ctx context.Context, req *runtimev1.ListInstancesRequest) (*runtimev1.ListInstancesResponse, error) {
			return &runtimev1.ListInstancesResponse{
				Instances: []*runtimev1.InstanceStatus{
					{InstanceId: "inst_a", State: "RUNNING", HostPid: 12345},
					{InstanceId: "inst_b", State: "STOPPED", HostPid: 0},
				},
			}, nil
		},
	}
	client := newBufconnGRPCClient(t, srv)

	resp, err := client.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(resp.Instances) != 2 {
		t.Fatalf("Instances count = %d, want 2", len(resp.Instances))
	}
	if resp.Instances[0].State != "RUNNING" {
		t.Errorf("Instances[0].State = %q, want RUNNING", resp.Instances[0].State)
	}
	if resp.Instances[0].HostPID != 12345 {
		t.Errorf("Instances[0].HostPID = %d, want 12345", resp.Instances[0].HostPID)
	}
}

func TestGRPCClient_ListInstances_Empty(t *testing.T) {
	srv := &fakeGRPCServer{
		listFunc: func(ctx context.Context, req *runtimev1.ListInstancesRequest) (*runtimev1.ListInstancesResponse, error) {
			return &runtimev1.ListInstancesResponse{}, nil
		},
	}
	client := newBufconnGRPCClient(t, srv)

	resp, err := client.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(resp.Instances) != 0 {
		t.Errorf("Instances count = %d, want 0", len(resp.Instances))
	}
}

// ── Context cancellation ─────────────────────────────────────────────────────

func TestGRPCClient_ContextCancelled_ReturnsError(t *testing.T) {
	srv := &fakeGRPCServer{
		createFunc: func(ctx context.Context, req *runtimev1.CreateInstanceRequest) (*runtimev1.CreateInstanceResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	client := newBufconnGRPCClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.CreateInstance(ctx, &CreateInstanceRequest{InstanceID: "inst_x"})
	if err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
}
