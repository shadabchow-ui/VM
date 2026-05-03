package runtime

// volume.go — Host-agent runtime volume attach/detach command-builder.
//
// VM-VOLUME-RUNTIME-F: Provides the host-agent-side foundation for volume
// attach and detach operations. All methods are idempotent by contract.
//
// Architecture:
//   - For stopped-instance attach: volumes are passed as ExtraDisks
//     in the CreateInstance / StartInstance request. The host-agent mounts
//     the additional block devices during VM provisioning.
//   - For future hot-attach: the host-agent exposes AttachVolume /
//     DetachVolume RPCs that accept VolumeAttachRequest / VolumeDetachRequest
//     from the runtime-client package.
//
// This file defines the structural contract and validation helpers.
// Actual storage backend operations (e.g., qemu-img, bind-mount, NFS attach)
// are invoked from the VMRuntime implementation when needed.
//
// Idempotency rules:
//   - AttachVolume on an already-attached device → no-op, return current state.
//   - DetachVolume on an already-detached device → no-op, return current state.
//   - All methods validate paths against the LocalStorageManager root.
//
// Source: P2_VOLUME_MODEL.md §4 (attach/detach), §7 (VOL-I-1 single attachment),
//         RUNTIMESERVICE_GRPC_V1 §4 (idempotency),
//         vm-15-01__blueprint__ §data_model.

import (
	"fmt"
)

// VolumeAttachRequest is the host-agent-side attach command.
// Mirrors the runtime-client VolumeAttachRequest for protocol alignment.
type VolumeAttachRequest struct {
	VolumeID            string `json:"volume_id"`
	InstanceID          string `json:"instance_id"`
	DevicePath          string `json:"device_path"`
	StoragePath         string `json:"storage_path"`
	ReadWrite           bool   `json:"read_write"`
	DeleteOnTermination bool   `json:"delete_on_termination"`
}

// VolumeAttachResponse is the host-agent-side attach result.
type VolumeAttachResponse struct {
	VolumeID   string `json:"volume_id"`
	InstanceID string `json:"instance_id"`
	DevicePath string `json:"device_path"`
	State      string `json:"state"` // "attached"
}

// VolumeDetachRequest is the host-agent-side detach command.
type VolumeDetachRequest struct {
	VolumeID    string `json:"volume_id"`
	InstanceID  string `json:"instance_id"`
	DevicePath  string `json:"device_path"`
	StoragePath string `json:"storage_path"`
	Force       bool   `json:"force"`
}

// VolumeDetachResponse is the host-agent-side detach result.
type VolumeDetachResponse struct {
	VolumeID   string `json:"volume_id"`
	InstanceID string `json:"instance_id"`
	State      string `json:"state"` // "detached"
}

// ValidateVolumeAttachRequest checks that all required fields in a volume
// attach request are present and that the storage_path is under the
// configured storage root.
//
// Returns nil when valid, or an error describing the first validation failure.
func ValidateVolumeAttachRequest(req *VolumeAttachRequest, mgr *LocalStorageManager) error {
	if req.VolumeID == "" {
		return fmt.Errorf("VolumeAttachRequest: volume_id is required")
	}
	if req.InstanceID == "" {
		return fmt.Errorf("VolumeAttachRequest: instance_id is required")
	}
	if req.DevicePath == "" {
		return fmt.Errorf("VolumeAttachRequest: device_path is required")
	}
	if req.StoragePath == "" {
		return fmt.Errorf("VolumeAttachRequest: storage_path is required")
	}
	if mgr != nil {
		if err := mgr.ValidatePath(req.StoragePath); err != nil {
			return fmt.Errorf("VolumeAttachRequest: %w", err)
		}
	}
	return nil
}

// ValidateVolumeDetachRequest checks that all required fields in a volume
// detach request are present.
func ValidateVolumeDetachRequest(req *VolumeDetachRequest, mgr *LocalStorageManager) error {
	if req.VolumeID == "" {
		return fmt.Errorf("VolumeDetachRequest: volume_id is required")
	}
	if req.InstanceID == "" {
		return fmt.Errorf("VolumeDetachRequest: instance_id is required")
	}
	if req.StoragePath == "" {
		return fmt.Errorf("VolumeDetachRequest: storage_path is required")
	}
	if mgr != nil {
		if err := mgr.ValidatePath(req.StoragePath); err != nil {
			return fmt.Errorf("VolumeDetachRequest: %w", err)
		}
	}
	return nil
}
