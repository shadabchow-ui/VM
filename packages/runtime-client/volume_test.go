package runtimeclient

// volume_test.go — VM-VOLUME-RUNTIME-F: tests for volume attach/detach contract types.

import (
	"testing"
)

func TestBuildAttachCommand_SetsAllFields(t *testing.T) {
	cmd := BuildAttachCommand(
		"vol-001", "inst-001", "/dev/vdb",
		"/var/lib/storage/volumes/vol-001/disk.img",
		true, false,
	)
	if cmd.VolumeID != "vol-001" {
		t.Errorf("VolumeID = %q, want vol-001", cmd.VolumeID)
	}
	if cmd.InstanceID != "inst-001" {
		t.Errorf("InstanceID = %q, want inst-001", cmd.InstanceID)
	}
	if cmd.DevicePath != "/dev/vdb" {
		t.Errorf("DevicePath = %q, want /dev/vdb", cmd.DevicePath)
	}
	if cmd.StoragePath != "/var/lib/storage/volumes/vol-001/disk.img" {
		t.Errorf("StoragePath = %q, want /var/lib/storage/volumes/vol-001/disk.img", cmd.StoragePath)
	}
	if !cmd.ReadWrite {
		t.Error("ReadWrite should be true")
	}
	if cmd.DeleteOnTermination {
		t.Error("DeleteOnTermination should be false")
	}
}

func TestBuildDetachCommand_SetsAllFields(t *testing.T) {
	cmd := BuildDetachCommand("vol-002", "inst-002", "/dev/vdc", "/var/lib/storage/volumes/vol-002/disk.img")
	if cmd.VolumeID != "vol-002" {
		t.Errorf("VolumeID = %q, want vol-002", cmd.VolumeID)
	}
	if cmd.InstanceID != "inst-002" {
		t.Errorf("InstanceID = %q, want inst-002", cmd.InstanceID)
	}
	if cmd.DevicePath != "/dev/vdc" {
		t.Errorf("DevicePath = %q, want /dev/vdc", cmd.DevicePath)
	}
	if cmd.Force {
		t.Error("Force should default to false")
	}
}

func TestToExtraDisk_ConvertsCorrectly(t *testing.T) {
	cfg := &VolumeAttachConfig{
		VolumeID:    "vol-003",
		InstanceID:  "inst-003",
		DevicePath:  "/dev/vdd",
		StoragePath: "/mnt/storage/volumes/vol-003/disk.img",
		ReadWrite:   true,
	}
	disk := cfg.ToExtraDisk()
	if disk.DiskID != "vol-003" {
		t.Errorf("DiskID = %q, want vol-003", disk.DiskID)
	}
	if disk.HostPath != "/mnt/storage/volumes/vol-003/disk.img" {
		t.Errorf("HostPath = %q", disk.HostPath)
	}
	if disk.DeviceName != "/dev/vdd" {
		t.Errorf("DeviceName = %q, want /dev/vdd", disk.DeviceName)
	}
}
