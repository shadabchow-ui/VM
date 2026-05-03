package runtimeclient

// volume.go — Volume attach/detach runtime contract types and command builder.
//
// VM-VOLUME-RUNTIME-F: Defines the canonical runtime contract for block-volume
// attach and detach operations. These types serve as the interface between the
// worker and the host-agent for volume lifecycle operations.
//
// Architecture principle:
//   - Volume attachment for stopped instances is handled implicitly through
//     ExtraDisks in CreateInstance — the worker assembles attached volume info
//     and passes it as extra block devices when the instance starts.
//   - For future hot-attach support, these types provide the contract shape
//     for explicit AttachVolume/DetachVolume RPCs.
//   - Host-agent runtime volume commands are idempotent by contract.
//
// Contract fields:
//   - volume_id: the canonical volume identifier
//   - instance_id: the target instance identifier
//   - device_path: the block device path inside the VM (e.g. /dev/vdb)
//   - source/storage_path: the host-level path to the volume disk artifact
//   - read_write: whether the attachment is read-write (true) or read-only (false)
//   - delete_on_termination: whether the volume should be deleted with the instance
//
// Source: P2_VOLUME_MODEL.md §4 (attach/detach flows), §2.4 (attachment model),
//         RUNTIMESERVICE_GRPC_V1 §4 (idempotency contract).

// VolumeAttachConfig describes the parameters for attaching a block volume
// to an instance at the runtime level.
//
// This is the command-builder contract: the worker constructs a
// VolumeAttachConfig, and the host-agent executes it idempotently.
// For stopped-instance attach, this config is converted into an
// ExtraDiskConfig entry in the CreateInstance request.
type VolumeAttachConfig struct {
	// VolumeID is the canonical volume identifier (vol_ prefix + KSUID).
	VolumeID string `json:"volume_id"`

	// InstanceID is the target instance identifier.
	InstanceID string `json:"instance_id"`

	// DevicePath is the block device path inside the VM (e.g. "/dev/vdb").
	DevicePath string `json:"device_path"`

	// StoragePath is the host-level path to the volume disk image artifact.
	// Derived from LocalStorageManager.VolumeDiskPath(volumeID).
	StoragePath string `json:"storage_path"`

	// ReadWrite determines the access mode. True for read-write (default),
	// false for read-only (future: snapshot mounts).
	ReadWrite bool `json:"read_write"`

	// DeleteOnTermination controls whether the volume is destroyed when
	// the instance is terminated. Default false for data volumes.
	DeleteOnTermination bool `json:"delete_on_termination"`
}

// VolumeDetachConfig describes the parameters for detaching a block volume
// from an instance at the runtime level.
//
// Detach is idempotent: if the volume is already detached, the operation
// succeeds without modification.
type VolumeDetachConfig struct {
	// VolumeID is the canonical volume identifier.
	VolumeID string `json:"volume_id"`

	// InstanceID is the instance from which to detach.
	InstanceID string `json:"instance_id"`

	// DevicePath is the block device path being released.
	DevicePath string `json:"device_path"`

	// StoragePath is the host-level path to the volume artifact being released.
	StoragePath string `json:"storage_path"`

	// Force if true bypasses safety checks (for cleanup/recovery).
	// Default false — the host-agent verifies the device is not in use.
	Force bool `json:"force"`
}

// VolumeAttachResult reports the result of an AttachVolume operation.
type VolumeAttachResult struct {
	// VolumeID is the canonical volume identifier.
	VolumeID string `json:"volume_id"`

	// InstanceID is the instance the volume is attached to.
	InstanceID string `json:"instance_id"`

	// DevicePath is the actual device path assigned (may differ from request).
	DevicePath string `json:"device_path"`

	// State is the result state: "attached" on success.
	State string `json:"state"`
}

// VolumeDetachResult reports the result of a DetachVolume operation.
type VolumeDetachResult struct {
	// VolumeID is the canonical volume identifier.
	VolumeID string `json:"volume_id"`

	// InstanceID is the instance the volume was detached from.
	InstanceID string `json:"instance_id"`

	// State is the result state: "detached" on success.
	State string `json:"state"`
}

// VolumeAttachRequest is the wire request for attaching a volume.
type VolumeAttachRequest struct {
	VolumeID            string `json:"volume_id"`
	InstanceID          string `json:"instance_id"`
	DevicePath          string `json:"device_path"`
	StoragePath         string `json:"storage_path"`
	ReadWrite           bool   `json:"read_write"`
	DeleteOnTermination bool   `json:"delete_on_termination"`
}

// VolumeAttachResponse is the wire response for attaching a volume.
type VolumeAttachResponse struct {
	VolumeID   string `json:"volume_id"`
	InstanceID string `json:"instance_id"`
	DevicePath string `json:"device_path"`
	State      string `json:"state"`
}

// VolumeDetachRequest is the wire request for detaching a volume.
type VolumeDetachRequest struct {
	VolumeID   string `json:"volume_id"`
	InstanceID string `json:"instance_id"`
	DevicePath string `json:"device_path"`
	StoragePath string `json:"storage_path"`
	Force      bool   `json:"force"`
}

// VolumeDetachResponse is the wire response for detaching a volume.
type VolumeDetachResponse struct {
	VolumeID   string `json:"volume_id"`
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
}

// ToExtraDisk converts a VolumeAttachConfig into an ExtraDiskConfig entry
// suitable for inclusion in a CreateInstanceRequest. This is the primary
// integration path for stopped-instance volume attach.
//
// When an instance is started (or restarted), the worker collects all
// active volume attachments, converts each to an ExtraDiskConfig via this
// method, and passes them to CreateInstance. The host-agent mounts the
// block devices as part of VM provisioning.
func (c *VolumeAttachConfig) ToExtraDisk() ExtraDiskConfig {
	return ExtraDiskConfig{
		DiskID:     c.VolumeID,
		HostPath:   c.StoragePath,
		DeviceName: c.DevicePath,
	}
}

// BuildAttachCommand constructs a VolumeAttachConfig suitable for the
// host-agent runtime. This is the command-builder foundation that the
// worker uses to construct idempotent volume attach commands.
//
// When hot-attach is implemented, the worker passes the built config
// directly to the host-agent via AttachVolume RPC.
//
// For stopped-instance attach (current Phase 2 behavior), the config
// is converted to ExtraDiskConfig via ToExtraDisk() and included in
// the next CreateInstance call.
//
// Source: VM Job 4 — Local block-volume persistence.
func BuildAttachCommand(volumeID, instanceID, devicePath, storagePath string, readWrite, deleteOnTermination bool) *VolumeAttachConfig {
	return &VolumeAttachConfig{
		VolumeID:            volumeID,
		InstanceID:          instanceID,
		DevicePath:          devicePath,
		StoragePath:         storagePath,
		ReadWrite:           readWrite,
		DeleteOnTermination: deleteOnTermination,
	}
}

// BuildDetachCommand constructs a VolumeDetachConfig suitable for the
// host-agent runtime. This is the command-builder foundation for
// idempotent volume detach commands.
//
// Source: VM Job 4 — Local block-volume persistence.
func BuildDetachCommand(volumeID, instanceID, devicePath, storagePath string) *VolumeDetachConfig {
	return &VolumeDetachConfig{
		VolumeID:   volumeID,
		InstanceID: instanceID,
		DevicePath: devicePath,
		StoragePath: storagePath,
		Force:      false,
	}
}
