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
//
// VM-P2B-S2: Added SnapshotStore interface + SnapshotDeps for snapshot
// job handlers (SNAPSHOT_CREATE, SNAPSHOT_DELETE, VOLUME_RESTORE).
//
// VM-P2B-S3: Added SetVolumeStoragePath + CountActiveSnapshotsByVolume to
// VolumeStore (required by VOLUME_CREATE handler and VOLUME_DELETE worker
// invariant enforcement). Added CountVolumesBySourceSnapshot to SnapshotStore
// (required by SNAPSHOT_DELETE worker to enforce SNAP-I-3).

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

	// NIC lifecycle operations (VM-P2A-S2)
	// Source: P2_VPC_NETWORK_CONTRACT §5 (NIC model), VM-P2A-S2 audit findings R1, R3.
	// GetPrimaryNetworkInterfaceByInstance returns the primary NIC for an instance.
	// Returns (nil, nil) when no NIC exists (Phase 1 classic instance — safe no-op).
	GetPrimaryNetworkInterfaceByInstance(ctx context.Context, instanceID string) (*db.NetworkInterfaceRow, error)
	// UpdateNetworkInterfaceStatus sets the status field of a NIC row.
	// Used to advance NIC through pending→attached (create/start) and attached→detached (stop).
	UpdateNetworkInterfaceStatus(ctx context.Context, nicID, status string) error
	// SoftDeleteNetworkInterface marks a NIC as deleted (status=deleted).
	// Called by the delete handler as the final NIC lifecycle step.
	SoftDeleteNetworkInterface(ctx context.Context, nicID string) error

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

// ── VolumeStore ───────────────────────────────────────────────────────────────
// Subset of *db.Repo used by volume job handlers (VOLUME_CREATE, VOLUME_ATTACH,
// VOLUME_DETACH, VOLUME_DELETE). *db.Repo satisfies this interface.
// Tests inject fakeVolumeStore.
// Source: P2_VOLUME_MODEL.md §7 VOL-I-5 (locked_by), §4 (attach/detach), §5 (delete).
// VM-P2B Slice 1.
// VM-P2B-S3: Added SetVolumeStoragePath (VOLUME_CREATE) and
//
//	CountActiveSnapshotsByVolume (VOLUME_DELETE invariant).
type VolumeStore interface {
	GetVolumeByID(ctx context.Context, id string) (*db.VolumeRow, error)
	LockVolume(ctx context.Context, id, jobID, expectedStatus string, version int) error
	UnlockVolume(ctx context.Context, id, newStatus string) error
	UpdateVolumeStatus(ctx context.Context, id, expectedStatus, newStatus string, version int) error
	SoftDeleteVolume(ctx context.Context, id string, version int) error
	GetActiveAttachmentByVolume(ctx context.Context, volumeID string) (*db.VolumeAttachmentRow, error)
	CloseVolumeAttachment(ctx context.Context, attachmentID string) error

	// SetVolumeStoragePath records the storage_path on a volume row after
	// the storage data-plane has provisioned the block device.
	// Source: P2_VOLUME_MODEL.md §5 (storage_path assigned on create completion).
	// VM-P2B-S3.
	SetVolumeStoragePath(ctx context.Context, id, storagePath string) error

	// CountActiveSnapshotsByVolume returns the number of non-deleted snapshots
	// whose source_volume_id is the given volume.
	// Used by VOLUME_DELETE to enforce SNAP-I-3 at the worker level.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.
	// VM-P2B-S3.
	CountActiveSnapshotsByVolume(ctx context.Context, volumeID string) (int, error)
}

// VolumeDeps holds dependencies for volume job handlers.
// Separate from Deps to keep the instance handler dependency set stable.
type VolumeDeps struct {
	Store VolumeStore
}

// ── SnapshotStore ─────────────────────────────────────────────────────────────
// Subset of *db.Repo used by snapshot job handlers (SNAPSHOT_CREATE,
// SNAPSHOT_DELETE, VOLUME_RESTORE). *db.Repo satisfies this interface.
// Tests inject fakeSnapshotStore.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 (invariants),
//
//	vm-15-02__skill__snapshot-clone-restore-retention-model.md.
//
// VM-P2B-S2.
// VM-P2B-S3: Added CountVolumesBySourceSnapshot to enforce SNAP-I-3
//
//	in SNAPSHOT_DELETE worker.
type SnapshotStore interface {
	// Snapshot operations
	GetSnapshotByID(ctx context.Context, id string) (*db.SnapshotRow, error)
	LockSnapshot(ctx context.Context, id, jobID, expectedStatus string, version int) error
	UnlockSnapshot(ctx context.Context, id, newStatus string) error
	UpdateSnapshotStatus(ctx context.Context, id, expectedStatus, newStatus string, version int) error
	MarkSnapshotAvailable(ctx context.Context, id, storagePath string, version int) error
	SoftDeleteSnapshot(ctx context.Context, id string, version int) error

	// Volume operations needed by VOLUME_RESTORE.
	GetVolumeByID(ctx context.Context, id string) (*db.VolumeRow, error)
	UnlockVolume(ctx context.Context, id, newStatus string) error
	SetVolumeStoragePath(ctx context.Context, id, storagePath string) error

	// CountVolumesBySourceSnapshot returns the number of non-deleted volumes
	// whose source_snapshot_id is the given snapshot.
	// Used by SNAPSHOT_DELETE to enforce SNAP-I-3: cannot delete a snapshot
	// while volumes restored from it still exist.
	// Source: P2_IMAGE_SNAPSHOT_MODEL.md §2.9 SNAP-I-3.
	// VM-P2B-S3.
	CountVolumesBySourceSnapshot(ctx context.Context, snapshotID string) (int, error)
}

// SnapshotDeps holds dependencies for snapshot job handlers.
// Separate from Deps and VolumeDeps to keep dependency sets stable.
type SnapshotDeps struct {
	Store SnapshotStore
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
