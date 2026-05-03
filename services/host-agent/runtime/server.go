package runtime

import (
	"context"
	"log/slog"

	runtimev1 "github.com/compute-platform/compute-platform/packages/contracts/runtimev1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Ensure gRPCRuntimeServer implements the generated RuntimeServiceServer interface.
var _ runtimev1.RuntimeServiceServer = (*gRPCRuntimeServer)(nil)

// gRPCRuntimeServer wraps the existing RuntimeService and adapts it to the
// generated proto gRPC server interface.
//
// It converts between runtimev1 proto types and the internal runtime package types
// defined in service.go, then delegates to the existing RuntimeService.
type gRPCRuntimeServer struct {
	runtimev1.UnimplementedRuntimeServiceServer
	svc *RuntimeService
	log *slog.Logger
}

// NewGRPCServer creates a gRPC server implementation backed by the RuntimeService.
// This implements runtimev1.RuntimeServiceServer and can be registered with a
// gRPC server via runtimev1.RegisterRuntimeServiceServer(grpcServer, gRPCServer).
func NewGRPCServer(svc *RuntimeService, log *slog.Logger) runtimev1.RuntimeServiceServer {
	return &gRPCRuntimeServer{svc: svc, log: log}
}

// CreateInstance implements runtimev1.RuntimeServiceServer.
func (s *gRPCRuntimeServer) CreateInstance(ctx context.Context, req *runtimev1.CreateInstanceRequest) (*runtimev1.CreateInstanceResponse, error) {
	s.log.Info("gRPC CreateInstance", "instance_id", req.InstanceId)

	nc := req.GetNetwork()
	if nc == nil {
		nc = &runtimev1.NetworkConfig{}
	}

	internalReq := &CreateInstanceRequest{
		InstanceID:     req.InstanceId,
		ImageURL:       req.ImageUrl,
		InstanceTypeID: req.InstanceTypeId,
		CPUCores:       req.CpuCores,
		MemoryMB:       req.MemoryMb,
		DiskGB:         req.DiskGb,
		RootfsPath:     req.RootfsPath,
		Network: NetworkConfig{
			PrivateIP:  nc.PrivateIp,
			PublicIP:   nc.PublicIp,
			TapDevice:  nc.TapDevice,
			MacAddress: nc.MacAddress,
		},
		SSHPublicKey: req.SshPublicKey,
	}

	resp, err := s.svc.CreateInstance(ctx, internalReq)
	if err != nil {
		return nil, toGRPCError(err)
	}

	return &runtimev1.CreateInstanceResponse{
		InstanceId: resp.InstanceID,
		State:      resp.State,
	}, nil
}

// StartInstance implements runtimev1.RuntimeServiceServer.
func (s *gRPCRuntimeServer) StartInstance(ctx context.Context, req *runtimev1.StartInstanceRequest) (*runtimev1.StartInstanceResponse, error) {
	s.log.Info("gRPC StartInstance", "instance_id", req.InstanceId)

	resp, err := s.svc.StartInstance(ctx, &StartInstanceRequest{InstanceID: req.InstanceId})
	if err != nil {
		return nil, toGRPCError(err)
	}

	return &runtimev1.StartInstanceResponse{
		InstanceId: resp.InstanceID,
		State:      resp.State,
	}, nil
}

// StopInstance implements runtimev1.RuntimeServiceServer.
func (s *gRPCRuntimeServer) StopInstance(ctx context.Context, req *runtimev1.StopInstanceRequest) (*runtimev1.StopInstanceResponse, error) {
	s.log.Info("gRPC StopInstance", "instance_id", req.InstanceId)

	resp, err := s.svc.StopInstance(ctx, &StopInstanceRequest{
		InstanceID:     req.InstanceId,
		TimeoutSeconds: req.TimeoutSeconds,
	})
	if err != nil {
		return nil, toGRPCError(err)
	}

	return &runtimev1.StopInstanceResponse{
		InstanceId: resp.InstanceID,
		State:      resp.State,
	}, nil
}

// DeleteInstance implements runtimev1.RuntimeServiceServer.
func (s *gRPCRuntimeServer) DeleteInstance(ctx context.Context, req *runtimev1.DeleteInstanceRequest) (*runtimev1.DeleteInstanceResponse, error) {
	s.log.Info("gRPC DeleteInstance", "instance_id", req.InstanceId)

	resp, err := s.svc.DeleteInstance(ctx, &DeleteInstanceRequest{
		InstanceID:     req.InstanceId,
		DeleteRootDisk: req.DeleteRootDisk,
	})
	if err != nil {
		return nil, toGRPCError(err)
	}

	return &runtimev1.DeleteInstanceResponse{
		InstanceId: resp.InstanceID,
		State:      resp.State,
	}, nil
}

// ListInstances implements runtimev1.RuntimeServiceServer.
func (s *gRPCRuntimeServer) ListInstances(ctx context.Context, req *runtimev1.ListInstancesRequest) (*runtimev1.ListInstancesResponse, error) {
	s.log.Info("gRPC ListInstances")

	resp, err := s.svc.ListInstances(ctx, &ListInstancesRequest{})
	if err != nil {
		return nil, toGRPCError(err)
	}

	var instances []*runtimev1.InstanceStatus
	for _, inst := range resp.Instances {
		instances = append(instances, &runtimev1.InstanceStatus{
			InstanceId: inst.InstanceID,
			State:      inst.State,
			HostPid:    inst.HostPID,
		})
	}

	return &runtimev1.ListInstancesResponse{Instances: instances}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func toGRPCError(err error) error {
	return status.Errorf(codes.Internal, "%v", err)
}
