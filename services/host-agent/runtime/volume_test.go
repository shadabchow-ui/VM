package runtime

// volume_test.go — VM-VOLUME-RUNTIME-F: tests for volume attach/detach validation.

import (
	"testing"
)

func TestValidateVolumeAttachRequest_Valid(t *testing.T) {
	mgr := NewLocalStorageManager("/var/lib/compute-platform/storage")
	req := &VolumeAttachRequest{
		VolumeID:    "vol-001",
		InstanceID:  "inst-001",
		DevicePath:  "/dev/vdb",
		StoragePath: "/var/lib/compute-platform/storage/volumes/vol-001/disk.img",
		ReadWrite:   true,
	}
	if err := ValidateVolumeAttachRequest(req, mgr); err != nil {
		t.Errorf("unexpected error for valid request: %v", err)
	}
}

func TestValidateVolumeAttachRequest_MissingVolumeID(t *testing.T) {
	mgr := NewLocalStorageManager("/var/lib/compute-platform/storage")
	req := &VolumeAttachRequest{
		InstanceID:  "inst-001",
		DevicePath:  "/dev/vdb",
		StoragePath: "/var/lib/compute-platform/storage/volumes/vol-001/disk.img",
	}
	if err := ValidateVolumeAttachRequest(req, mgr); err == nil {
		t.Error("expected error for missing volume_id")
	}
}

func TestValidateVolumeAttachRequest_MissingInstanceID(t *testing.T) {
	mgr := NewLocalStorageManager("/var/lib/compute-platform/storage")
	req := &VolumeAttachRequest{
		VolumeID:    "vol-001",
		DevicePath:  "/dev/vdb",
		StoragePath: "/var/lib/compute-platform/storage/volumes/vol-001/disk.img",
	}
	if err := ValidateVolumeAttachRequest(req, mgr); err == nil {
		t.Error("expected error for missing instance_id")
	}
}

func TestValidateVolumeAttachRequest_MissingDevicePath(t *testing.T) {
	mgr := NewLocalStorageManager("/var/lib/compute-platform/storage")
	req := &VolumeAttachRequest{
		VolumeID:    "vol-001",
		InstanceID:  "inst-001",
		StoragePath: "/var/lib/compute-platform/storage/volumes/vol-001/disk.img",
	}
	if err := ValidateVolumeAttachRequest(req, mgr); err == nil {
		t.Error("expected error for missing device_path")
	}
}

func TestValidateVolumeAttachRequest_MissingStoragePath(t *testing.T) {
	mgr := NewLocalStorageManager("/var/lib/compute-platform/storage")
	req := &VolumeAttachRequest{
		VolumeID:   "vol-001",
		InstanceID: "inst-001",
		DevicePath: "/dev/vdb",
	}
	if err := ValidateVolumeAttachRequest(req, mgr); err == nil {
		t.Error("expected error for missing storage_path")
	}
}

func TestValidateVolumeAttachRequest_PathOutsideRoot(t *testing.T) {
	mgr := NewLocalStorageManager("/var/lib/compute-platform/storage")
	req := &VolumeAttachRequest{
		VolumeID:    "vol-001",
		InstanceID:  "inst-001",
		DevicePath:  "/dev/vdb",
		StoragePath: "/etc/passwd",
	}
	if err := ValidateVolumeAttachRequest(req, mgr); err == nil {
		t.Error("expected path traversal rejection")
	}
}

func TestValidateVolumeAttachRequest_NilManager_Passes(t *testing.T) {
	req := &VolumeAttachRequest{
		VolumeID:    "vol-001",
		InstanceID:  "inst-001",
		DevicePath:  "/dev/vdb",
		StoragePath: "/var/lib/compute-platform/storage/volumes/vol-001/disk.img",
	}
	if err := ValidateVolumeAttachRequest(req, nil); err != nil {
		t.Errorf("unexpected error with nil manager: %v", err)
	}
}

func TestValidateVolumeDetachRequest_Valid(t *testing.T) {
	mgr := NewLocalStorageManager("/var/lib/compute-platform/storage")
	req := &VolumeDetachRequest{
		VolumeID:    "vol-001",
		InstanceID:  "inst-001",
		DevicePath:  "/dev/vdb",
		StoragePath: "/var/lib/compute-platform/storage/volumes/vol-001/disk.img",
	}
	if err := ValidateVolumeDetachRequest(req, mgr); err != nil {
		t.Errorf("unexpected error for valid detach request: %v", err)
	}
}

func TestValidateVolumeDetachRequest_MissingVolumeID(t *testing.T) {
	mgr := NewLocalStorageManager("/var/lib/compute-platform/storage")
	req := &VolumeDetachRequest{
		InstanceID:  "inst-001",
		StoragePath: "/var/lib/compute-platform/storage/volumes/vol-001/disk.img",
	}
	if err := ValidateVolumeDetachRequest(req, mgr); err == nil {
		t.Error("expected error for missing volume_id")
	}
}
