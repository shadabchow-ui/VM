package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"testing"

	runtimev1 "github.com/compute-platform/compute-platform/packages/contracts/runtimev1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func qemuImgAvailable() bool {
	_, err := exec.LookPath("qemu-img")
	return err == nil
}

func newBufconnGRPCServer(t *testing.T, svc *RuntimeService) (runtimev1.RuntimeServiceClient, func()) {
	t.Helper()

	log := slog.New(slog.DiscardHandler)
	srv := NewGRPCServer(svc, log)

	lis := bufconn.Listen(1024 * 1024)
	gs := grpc.NewServer()
	runtimev1.RegisterRuntimeServiceServer(gs, srv)

	go func() {
		_ = gs.Serve(lis)
	}()

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.Dial()
	}

	conn, err := grpc.DialContext(context.Background(), "bufnet",
		grpc.WithContextDialer(dialer),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	client := runtimev1.NewRuntimeServiceClient(conn)
	return client, func() {
		conn.Close()
		gs.Stop()
	}
}

func TestGRPCServer_CreateInstance_HappyPath(t *testing.T) {
	if !qemuImgAvailable() {
		t.Skip("qemu-img not found — skipping rootfs materialization test")
	}

	// Create a temp dir for NFS root and a valid base image backing file.
	nfsRoot := t.TempDir()
	baseImage := filepath.Join(nfsRoot, "ubuntu-22.04-base.qcow2")

	// Create a valid qcow2 backing file so qemu-img create succeeds.
	if err := createEmptyQcow2(baseImage, 1); err != nil {
		t.Fatalf("create base qcow2: %v", err)
	}

	t.Setenv("IMAGE_CATALOG", "object://images/ubuntu-22.04-base.qcow2="+baseImage)
	t.Setenv("NETWORK_DRY_RUN", "true")

	vm := NewFakeRuntime()
	rfs := NewRootfsManager(nfsRoot, slog.New(slog.DiscardHandler))
	netMgr := NewNetworkManager(slog.New(slog.DiscardHandler))
	svc := NewRuntimeService(vm, rfs, netMgr, slog.New(slog.DiscardHandler))

	client, cleanup := newBufconnGRPCServer(t, svc)
	defer cleanup()

	req := &runtimev1.CreateInstanceRequest{
		InstanceId:     "inst-test-001",
		ImageUrl:       "object://images/ubuntu-22.04-base.qcow2",
		InstanceTypeId: "c1.small",
		CpuCores:       2,
		MemoryMb:       4096,
		DiskGb:         50,
		RootfsPath:     filepath.Join(nfsRoot, "inst-test-001.qcow2"),
		Network: &runtimev1.NetworkConfig{
			PrivateIp:  "10.0.0.5",
			PublicIp:   "",
			TapDevice:  "tap-test001",
			MacAddress: "02:00:00:00:00:01",
		},
		SshPublicKey: "ssh-ed25519 AAAA...",
	}

	resp, err := client.CreateInstance(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if resp.InstanceId != "inst-test-001" {
		t.Errorf("InstanceId = %q, want inst-test-001", resp.InstanceId)
	}
	if resp.State != "RUNNING" {
		t.Errorf("State = %q, want RUNNING", resp.State)
	}

	// Verify the fake VM recorded the call.
	if vm.CallCount() == 0 {
		t.Error("expected at least one VM call, got 0")
	}
}

// createEmptyQcow2 creates a minimal valid qcow2 image of the given size in GB.
func createEmptyQcow2(path string, sizeGB int) error {
	cmd := exec.Command("qemu-img", "create", "-f", "qcow2", path, fmt.Sprintf("%dG", sizeGB))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img create %s: %w\noutput: %s", path, err, string(out))
	}
	return nil
}

// TestGRPCServer_TypeConversion verifies that the gRPC server correctly converts
// between runtimev1 proto types and internal runtime types without requiring
// rootfs materialization (qemu-img).
func TestGRPCServer_TypeConversion(t *testing.T) {
	vm := NewFakeRuntime()
	rfs := NewRootfsManager("/tmp", slog.New(slog.DiscardHandler))
	netMgr := NewNetworkManager(slog.New(slog.DiscardHandler))
	svc := NewRuntimeService(vm, rfs, netMgr, slog.New(slog.DiscardHandler))

	// Test StopInstance type conversion (no rootfs dependency).
	// Pre-register an instance.
	_, _ = vm.Create(context.Background(), InstanceSpec{
		InstanceID: "inst-conv-001",
		CPUCores:   2,
		MemoryMB:   4096,
		RootfsPath: "/tmp/a",
	})

	client, cleanup := newBufconnGRPCServer(t, svc)
	defer cleanup()

	// Verify Stop response fields are converted correctly.
	resp, err := client.StopInstance(context.Background(), &runtimev1.StopInstanceRequest{
		InstanceId:     "inst-conv-001",
		TimeoutSeconds: 45,
	})
	if err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if resp.InstanceId != "inst-conv-001" {
		t.Errorf("InstanceId = %q, want inst-conv-001", resp.InstanceId)
	}
	if resp.State != "STOPPED" {
		t.Errorf("State = %q, want STOPPED", resp.State)
	}
}

func TestGRPCServer_StopInstance_HappyPath(t *testing.T) {
	vm := NewFakeRuntime()
	// Pre-register an instance so Stop has something to stop.
	spec := InstanceSpec{
		InstanceID: "inst-test-001",
		CPUCores:   2,
		MemoryMB:   4096,
		RootfsPath: "/mnt/nfs/vols/inst-test-001.qcow2",
	}
	_, _ = vm.Create(context.Background(), spec)

	svc := NewRuntimeService(vm,
		NewRootfsManager("/tmp", slog.New(slog.DiscardHandler)),
		NewNetworkManager(slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler),
	)

	client, cleanup := newBufconnGRPCServer(t, svc)
	defer cleanup()

	resp, err := client.StopInstance(context.Background(), &runtimev1.StopInstanceRequest{
		InstanceId:     "inst-test-001",
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if resp.State != "STOPPED" {
		t.Errorf("State = %q, want STOPPED", resp.State)
	}
}

func TestGRPCServer_DeleteInstance_HappyPath(t *testing.T) {
	vm := NewFakeRuntime()
	svc := NewRuntimeService(vm,
		NewRootfsManager("/tmp", slog.New(slog.DiscardHandler)),
		NewNetworkManager(slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler),
	)

	client, cleanup := newBufconnGRPCServer(t, svc)
	defer cleanup()

	resp, err := client.DeleteInstance(context.Background(), &runtimev1.DeleteInstanceRequest{
		InstanceId:     "inst-test-001",
		DeleteRootDisk: true,
	})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if resp.State != "DELETED" {
		t.Errorf("State = %q, want DELETED", resp.State)
	}
}

func TestGRPCServer_ListInstances_HappyPath(t *testing.T) {
	vm := NewFakeRuntime()
	// Pre-register some instances.
	_, _ = vm.Create(context.Background(), InstanceSpec{InstanceID: "inst-a", CPUCores: 1, MemoryMB: 1024, RootfsPath: "/tmp/a"})
	_, _ = vm.Create(context.Background(), InstanceSpec{InstanceID: "inst-b", CPUCores: 2, MemoryMB: 2048, RootfsPath: "/tmp/b"})

	svc := NewRuntimeService(vm,
		NewRootfsManager("/tmp", slog.New(slog.DiscardHandler)),
		NewNetworkManager(slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler),
	)

	client, cleanup := newBufconnGRPCServer(t, svc)
	defer cleanup()

	resp, err := client.ListInstances(context.Background(), &runtimev1.ListInstancesRequest{})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(resp.Instances) < 2 {
		t.Errorf("Instances count = %d, want >= 2", len(resp.Instances))
	}
}

func TestGRPCServer_StartInstance_Stub(t *testing.T) {
	vm := NewFakeRuntime()
	svc := NewRuntimeService(vm,
		NewRootfsManager("/tmp", slog.New(slog.DiscardHandler)),
		NewNetworkManager(slog.New(slog.DiscardHandler)),
		slog.New(slog.DiscardHandler),
	)

	client, cleanup := newBufconnGRPCServer(t, svc)
	defer cleanup()

	resp, err := client.StartInstance(context.Background(), &runtimev1.StartInstanceRequest{
		InstanceId: "inst-test-001",
	})
	if err != nil {
		t.Fatalf("StartInstance: %v", err)
	}
	if resp.State != "RUNNING" {
		t.Errorf("State = %q, want RUNNING", resp.State)
	}
}
