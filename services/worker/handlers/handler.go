package handlers

// handler.go — Handler interface, shared deps, and repo/network abstractions.
//
// Source: IMPLEMENTATION_PLAN_V1 §21 (worker base: route → execute).
//
// InstanceStore and NetworkController are interfaces so tests can inject fakes
// without touching the real database or network controller.
//
// M10 Slice 3: InstanceStore extended with root disk methods required by
// lifecycle wiring (create → CreateRootDisk/UpdateRootDiskStatus,
// delete → GetRootDiskByInstanceID + DeleteRootDisk/DetachRootDisk).
// *db.Repo satisfies all methods (CRUD added in Slice 2).

import (
	"context"
	"net/http"

	"github.com/compute-platform/compute-platform/internal/db"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// Handler is implemented by every job type handler.
type Handler interface {
	Execute(ctx context.Context, job *db.JobRow) error
}

// ── InstanceStore ─────────────────────────────────────────────────────────────
// Subset of *db.Repo used by create, delete, stop, start, and reboot handlers.
// *db.Repo satisfies this interface. Tests inject a fake.

type InstanceStore interface {
	// Instance operations
	GetInstanceByID(ctx context.Context, id string) (*db.InstanceRow, error)
	UpdateInstanceState(ctx context.Context, id, expectedState, newState string, version int) error
	AssignHost(ctx context.Context, instanceID, hostID string, version int) error
	SoftDeleteInstance(ctx context.Context, id string, version int) error
	GetAvailableHosts(ctx context.Context) ([]*db.HostRecord, error)
	InsertEvent(ctx context.Context, row *db.EventRow) error
	GetIPByInstance(ctx context.Context, instanceID string) (string, error)

	// Root disk operations (M10 Slice 3)
	// Source: 06-01-root-disk-model-and-persistence-semantics.md,
	//         P2_VOLUME_MODEL.md §1.
	CreateRootDisk(ctx context.Context, row *db.RootDiskRow) error
	GetRootDiskByInstanceID(ctx context.Context, instanceID string) (*db.RootDiskRow, error)
	UpdateRootDiskStatus(ctx context.Context, diskID, status string) error
	DeleteRootDisk(ctx context.Context, diskID string) error
	DetachRootDisk(ctx context.Context, diskID string) error
}

// ── NetworkController ─────────────────────────────────────────────────────────
// Interface for IP allocation and release.
// *NetworkControllerClient satisfies this. Tests inject a fake.

type NetworkController interface {
	AllocateIP(ctx context.Context, vpcID, instanceID string) (string, error)
	ReleaseIP(ctx context.Context, ip, vpcID, instanceID string) error
}

// ── RuntimeClient ─────────────────────────────────────────────────────────────
// Interface for Host Agent VM operations.
// *runtimeclient.Client satisfies this. Tests inject a fake.

type RuntimeClient interface {
	CreateInstance(ctx context.Context, req *runtimeclient.CreateInstanceRequest) (*runtimeclient.CreateInstanceResponse, error)
	StopInstance(ctx context.Context, req *runtimeclient.StopInstanceRequest) (*runtimeclient.StopInstanceResponse, error)
	DeleteInstance(ctx context.Context, req *runtimeclient.DeleteInstanceRequest) (*runtimeclient.DeleteInstanceResponse, error)
}

// ── Deps ──────────────────────────────────────────────────────────────────────

// Deps holds all shared dependencies for job handlers.
// Construct once in worker/main.go; pass to each handler constructor.
type Deps struct {
	Store        InstanceStore
	Network      NetworkController
	Runtime      func(hostID, address string) *runtimeclient.Client // factory (production)
	DefaultVPCID string                                             // Phase 1: all instances share one VPC
}

// ── NetworkControllerClient ───────────────────────────────────────────────────
// HTTP client for the network controller service.
// Satisfies NetworkController.

type NetworkControllerClient struct {
	baseURL string
	http    *http.Client
}

// NewNetworkControllerClient constructs a client for the network controller service.
// baseURL e.g. "http://network-controller.internal:8083"
func NewNetworkControllerClient(baseURL string) *NetworkControllerClient {
	return &NetworkControllerClient{baseURL: baseURL, http: &http.Client{}}
}

// AllocateIP calls POST /internal/v1/ip/allocate.
func (n *NetworkControllerClient) AllocateIP(ctx context.Context, vpcID, instanceID string) (string, error) {
	var resp struct {
		IP string `json:"ip"`
	}
	body := map[string]string{"vpc_id": vpcID, "instance_id": instanceID}
	if err := jsonPost(ctx, n.http, n.baseURL+"/internal/v1/ip/allocate", body, &resp); err != nil {
		return "", err
	}
	return resp.IP, nil
}

// ReleaseIP calls POST /internal/v1/ip/release.
func (n *NetworkControllerClient) ReleaseIP(ctx context.Context, ip, vpcID, instanceID string) error {
	body := map[string]string{"ip": ip, "vpc_id": vpcID, "instance_id": instanceID}
	return jsonPost(ctx, n.http, n.baseURL+"/internal/v1/ip/release", body, nil)
}
