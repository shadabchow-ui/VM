package main

// main.go — Host Agent service entrypoint.
//
// Source: 05-02-host-runtime-worker-design.md §Startup sequence,
//         AUTH_OWNERSHIP_MODEL_V1 §6 (mTLS bootstrap),
//         IMPLEMENTATION_PLAN_V1 §B1, §27-35 (M2: VM lifecycle + metadata service).
//
// Startup sequence:
//   1. Load config from environment.
//   2. If mTLS cert does not exist: run bootstrap (token → CSR → signed cert → persist).
//   3. Build mTLS HTTP client for Resource Manager calls.
//   4. POST /internal/v1/hosts/register with inventory.
//   5. Start HeartbeatLoop in goroutine (30s interval).
//   6. Start RuntimeService HTTP server — CreateInstance/Stop/Delete/List (M2).
//   7. Start metadata service at METADATA_ADDR (default 169.254.169.254:80) (M2).
//   8. Block on SIGTERM/SIGINT → graceful shutdown.
//
// M2 environment additions:
//   RUNTIME_ADDR      RuntimeService HTTP listen addr (default :50051)
//   METADATA_ADDR     Metadata service listen addr   (default 169.254.169.254:80)
//                     Production: 169.254.169.254:80 — requires the link-local
//                       interface alias to be present before agent start.
//                     Local dev:  set to 127.0.0.1:8181 to avoid bind failure.
//   NFS_ROOT          NFS mount path for qcow2 overlays (default /mnt/nfs/vols)
//                     Local dev:  set to any writable directory (e.g. /tmp/dev-vols)
//   KERNEL_PATH       Firecracker kernel image path (default /opt/firecracker/vmlinux)
//
// Local-dev overrides (never set in production):
//   FIRECRACKER_DRY_RUN=true  Skip the firecracker binary launch entirely.
//                             Writes a fake PID file. VM will NOT actually run.
//                             Required on macOS / any non-KVM host.
//   NETWORK_DRY_RUN=true      Skip ip(8) and iptables(8) calls. TAP devices and
//                             NAT rules are not created. Required on macOS where
//                             these Linux utilities are unavailable.
//   IMAGE_CATALOG             Comma-separated "object://url=local/path" pairs.
//                             Resolves object:// image URLs to local file paths
//                             so rootfs materialisation works without object storage.
//                             Example: IMAGE_CATALOG="object://images/ubuntu-22.04-base.qcow2=/opt/images/ubuntu.qcow2"

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	runtimev1 "github.com/compute-platform/compute-platform/packages/contracts/runtimev1"
	"github.com/compute-platform/compute-platform/services/host-agent/metadata"
	"github.com/compute-platform/compute-platform/services/host-agent/runtime"
	"google.golang.org/grpc"
)

// agentConfig holds all Host Agent configuration from environment.
type agentConfig struct {
	HostID             string
	AvailabilityZone   string
	ResourceManagerURL string
	AgentVersion       string
	RuntimeAddr        string // HTTP listen addr for RuntimeService
	MetadataAddr       string // HTTP listen addr for metadata service
	NFSRoot            string // NFS mount root for qcow2 overlays
	KernelPath         string // Firecracker kernel image path
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg := loadAgentConfig()
	log.Info("host agent starting",
		"host_id", cfg.HostID,
		"az", cfg.AvailabilityZone,
		"version", cfg.AgentVersion,
		"runtime_addr", cfg.RuntimeAddr,
		"metadata_addr", cfg.MetadataAddr,
	)

	// ── Bootstrap + Registration ──────────────────────────────────────────────
	registrar := newRegistrar(cfg, log)
	mtlsClient, err := registrar.EnsureRegistered()
	if err != nil {
		log.Error("registration failed — cannot start", "error", err)
		os.Exit(1)
	}
	log.Info("host agent registered", "host_id", cfg.HostID)

	// ── Graceful shutdown context ─────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// ── Build runtime managers ────────────────────────────────────────────────
	rtCfg := runtime.DefaultRuntimeConfig()
	rfs := runtime.NewRootfsManager(cfg.NFSRoot, log)
	netMgr := runtime.NewNetworkManager(log)

	var vm runtime.VMRuntime
	switch rtCfg.Backend {
	case runtime.RuntimeQEMU:
		vm = runtime.NewQemuManager(rtCfg.DataRoot, log)
	case runtime.RuntimeFake:
		vm = runtime.NewFakeRuntime()
		log.Warn("using FakeRuntime backend — VMs will NOT actually run")
	default:
		vm = runtime.NewFirecrackerManager("", "", cfg.KernelPath, log)
	}
	svc := runtime.NewRuntimeService(vm, rfs, netMgr, log)

	// ── Build metadata store ──────────────────────────────────────────────────
	// instanceStore tracks IP → metadata for all VMs on this host.
	// It is populated by RuntimeService.CreateInstance and cleared by DeleteInstance.
	store := newInMemoryInstanceStore()

	// Wrap the RuntimeService to populate the metadata store on Create/Delete.
	wrappedSvc := &instrumentedRuntimeService{svc: svc, store: store, log: log}

	// ── Start goroutines ──────────────────────────────────────────────────────
	var wg sync.WaitGroup

	// Heartbeat loop — wires VMRuntime for real vm_load reporting.
	vmLoadFn := func() int {
		infos, err := vm.List(context.Background())
		if err != nil {
			return 0
		}
		count := 0
		for _, info := range infos {
			if info.IsRunning() {
				count++
			}
		}
		return count
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		HeartbeatLoop(ctx, cfg, mtlsClient, log, vmLoadFn)
	}()

	// RuntimeService HTTP server (dev fallback; not the production transport).
	// Production: gRPC server below is the canonical transport.
	// Set HOST_AGENT_TRANSPORT=http to use HTTP server only.
	transportMode := getEnvDefault("HOST_AGENT_TRANSPORT", "grpc")
	if transportMode == "http" || transportMode == "both" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			startRuntimeHTTPServer(ctx, cfg.RuntimeAddr, wrappedSvc, log)
		}()
	}

	// RuntimeService gRPC server (production transport).
	// The gRPC server is bound to the same RUNTIME_ADDR.
	// Set HOST_AGENT_TRANSPORT=http to skip gRPC and use HTTP only.
	if transportMode == "grpc" || transportMode == "both" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			startRuntimeGRPCServer(ctx, cfg.RuntimeAddr, svc, log)
		}()
	}

	// Metadata service.
	// Production: METADATA_ADDR must be 169.254.169.254:80 (the link-local address
	// that VMs reach via DNAT). The interface alias must be configured before the
	// agent starts (see ops runbook §metadata-interface-setup).
	// Local dev: set METADATA_ADDR=127.0.0.1:8181 to avoid the bind failure that
	// occurs when 169.254.169.254 is not assigned to any interface.
	wg.Add(1)
	go func() {
		defer wg.Done()
		metaSrv := metadata.NewServer(cfg.MetadataAddr, store, log)
		log.Info("metadata service starting", "addr", cfg.MetadataAddr)
		if err := metaSrv.Start(); err != nil {
			// A bind failure on 169.254.169.254 means the link-local interface
			// alias is not configured. In local dev, set METADATA_ADDR=127.0.0.1:8181.
			log.Error("metadata service exited",
				"error", err,
				"hint", "if bind failed on 169.254.169.254, set METADATA_ADDR=127.0.0.1:8181 for local dev or configure the interface alias for production",
			)
		}
	}()

	// ── Block until shutdown ──────────────────────────────────────────────────
	<-ctx.Done()
	log.Info("shutdown signal received — stopping host agent")
	wg.Wait()
	log.Info("host agent stopped")
}

func loadAgentConfig() agentConfig {
	return agentConfig{
		HostID:             mustEnv("AGENT_HOST_ID"),
		AvailabilityZone:   mustEnv("AGENT_AZ"),
		ResourceManagerURL: mustEnv("RESOURCE_MANAGER_URL"),
		AgentVersion:       getEnvDefault("AGENT_VERSION", "v0.1.0-m2"),
		RuntimeAddr:        getEnvDefault("RUNTIME_ADDR", ":50051"),
		// MetadataAddr default: 169.254.169.254:80 in production (requires the
		// link-local alias to be configured on the host interface before the agent
		// starts — see ops runbook §metadata-interface-setup).
		// Override with METADATA_ADDR=127.0.0.1:8181 for local dev.
		MetadataAddr: getEnvDefault("METADATA_ADDR", "169.254.169.254:80"),
		NFSRoot:      getEnvDefault("NFS_ROOT", "/mnt/nfs/vols"),
		KernelPath:   getEnvDefault("KERNEL_PATH", "/opt/firecracker/vmlinux"),
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required environment variable not set", "key", key)
		os.Exit(1)
	}
	return v
}

func getEnvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ── RuntimeService HTTP server ────────────────────────────────────────────────
// Exposes RuntimeService operations as HTTP/JSON endpoints.
// Dev-only transport. Production uses the gRPC server below.
// The runtime-client/client.go calls these endpoints from the worker when
// RUNTIME_CLIENT_MODE=http is set on the worker side.

func startRuntimeHTTPServer(ctx context.Context, addr string, svc *instrumentedRuntimeService, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("/runtime/v1/instances", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handleCreateInstance(w, r, svc, log)
		case http.MethodGet:
			handleListInstances(w, r, svc, log)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/runtime/v1/instances/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleStopInstance(w, r, svc, log)
	})
	mux.HandleFunc("/runtime/v1/instances/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleDeleteInstance(w, r, svc, log)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	log.Info("RuntimeService HTTP server starting", "addr", addr)

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background()) //nolint:contextcheck
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("RuntimeService HTTP server error", "error", err)
	}
}

// startRuntimeGRPCServer starts the gRPC RuntimeService server on the given address.
// This is the production transport. The worker connects to this server using the
// gRPC client (packages/runtime-client/grpc_client.go) with mTLS.
//
// The gRPC server uses the raw RuntimeService (not the instrumented wrapper) because
// metadata store population is handled by the gRPC server implementation layer.
// Source: RUNTIMESERVICE_GRPC_V1 §7.
func startRuntimeGRPCServer(ctx context.Context, addr string, svc *runtime.RuntimeService, log *slog.Logger) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("gRPC server: failed to listen", "addr", addr, "error", err)
		return
	}

	grpcSrv := grpc.NewServer()
	runtimev1.RegisterRuntimeServiceServer(grpcSrv, runtime.NewGRPCServer(svc, log))

	log.Info("RuntimeService gRPC server starting", "addr", addr)

	go func() {
		<-ctx.Done()
		grpcSrv.GracefulStop()
	}()

	if err := grpcSrv.Serve(lis); err != nil {
		log.Error("gRPC server error", "error", err)
	}
}

func handleCreateInstance(w http.ResponseWriter, r *http.Request, svc *instrumentedRuntimeService, log *slog.Logger) {
	var req runtime.CreateInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := svc.CreateInstance(r.Context(), &req)
	if err != nil {
		log.Error("CreateInstance failed", "instance_id", req.InstanceID, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func handleStopInstance(w http.ResponseWriter, r *http.Request, svc *instrumentedRuntimeService, log *slog.Logger) {
	var req runtime.StopInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := svc.StopInstance(r.Context(), &req)
	if err != nil {
		log.Error("StopInstance failed", "instance_id", req.InstanceID, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func handleDeleteInstance(w http.ResponseWriter, r *http.Request, svc *instrumentedRuntimeService, log *slog.Logger) {
	var req runtime.DeleteInstanceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	resp, err := svc.DeleteInstance(r.Context(), &req)
	if err != nil {
		log.Error("DeleteInstance failed", "instance_id", req.InstanceID, "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func handleListInstances(w http.ResponseWriter, r *http.Request, svc *instrumentedRuntimeService, log *slog.Logger) {
	resp, err := svc.ListInstances(r.Context(), &runtime.ListInstancesRequest{})
	if err != nil {
		log.Error("ListInstances failed", "error", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ── instrumentedRuntimeService ────────────────────────────────────────────────
// Wraps RuntimeService to keep the metadata store in sync on Create/Delete.

type instrumentedRuntimeService struct {
	svc   *runtime.RuntimeService
	store *inMemoryInstanceStore
	log   *slog.Logger
}

func (s *instrumentedRuntimeService) CreateInstance(ctx context.Context, req *runtime.CreateInstanceRequest) (*runtime.CreateInstanceResponse, error) {
	resp, err := s.svc.CreateInstance(ctx, req)
	if err == nil {
		// Register in metadata store so the VM can reach its own metadata.
		s.store.Set(req.Network.PrivateIP, &metadata.InstanceMetadata{
			InstanceID:   req.InstanceID,
			SSHPublicKey: req.SSHPublicKey,
		})
		s.log.Info("metadata store: registered instance", "instance_id", req.InstanceID, "ip", req.Network.PrivateIP)
	}
	return resp, err
}

func (s *instrumentedRuntimeService) StopInstance(ctx context.Context, req *runtime.StopInstanceRequest) (*runtime.StopInstanceResponse, error) {
	return s.svc.StopInstance(ctx, req)
}

func (s *instrumentedRuntimeService) DeleteInstance(ctx context.Context, req *runtime.DeleteInstanceRequest) (*runtime.DeleteInstanceResponse, error) {
	resp, err := s.svc.DeleteInstance(ctx, req)
	if err == nil {
		s.store.DeleteByInstanceID(req.InstanceID)
	}
	return resp, err
}

func (s *instrumentedRuntimeService) ListInstances(ctx context.Context, req *runtime.ListInstancesRequest) (*runtime.ListInstancesResponse, error) {
	return s.svc.ListInstances(ctx, req)
}

// ── inMemoryInstanceStore ─────────────────────────────────────────────────────
// Thread-safe in-memory store: privateIP → InstanceMetadata.
// Implements metadata.InstanceStore.

type inMemoryInstanceStore struct {
	mu     sync.RWMutex
	byIP   map[string]*metadata.InstanceMetadata // privateIP → metadata
	byInst map[string]string                     // instanceID → privateIP
}

func newInMemoryInstanceStore() *inMemoryInstanceStore {
	return &inMemoryInstanceStore{
		byIP:   make(map[string]*metadata.InstanceMetadata),
		byInst: make(map[string]string),
	}
}

func (s *inMemoryInstanceStore) Set(privateIP string, m *metadata.InstanceMetadata) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byIP[privateIP] = m
	s.byInst[m.InstanceID] = privateIP
}

func (s *inMemoryInstanceStore) GetByIP(privateIP string) (*metadata.InstanceMetadata, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.byIP[privateIP]
	return m, ok
}

func (s *inMemoryInstanceStore) DeleteByInstanceID(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ip, ok := s.byInst[instanceID]; ok {
		delete(s.byIP, ip)
		delete(s.byInst, instanceID)
	}
}
