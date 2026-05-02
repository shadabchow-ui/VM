package runtime

import (
	"testing"
)

// TestQEMUArgs_BaseSpec verifies QEMU command generation for a base instance spec.
func TestQEMUArgs_BaseSpec(t *testing.T) {
	qm := NewQemuManager("/tmp/vm-test-instances", nil)

	spec := InstanceSpec{
		InstanceID: "inst-test001",
		CPUCores:   2,
		MemoryMB:   4096,
		RootfsPath: "/mnt/nfs/vols/inst-test001.qcow2",
		TapDevice:  "tap-test001",
		MacAddress: "02:00:00:00:00:01",
		PrivateIP:  "10.0.0.5",
	}

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	assertFlagHas(t, args, "-name", "inst-test001")
	assertFlagHas(t, args, "-smp", "cpus=2")
	assertFlagHas(t, args, "-m", "4096M")
	assertFlagHas(t, args, "-drive", "file=/mnt/nfs/vols/inst-test001.qcow2,if=virtio,format=qcow2")
	assertFlagHas(t, args, "-netdev", "tap,id=net0,ifname=tap-test001,script=no,downscript=no")
	assertFlagHas(t, args, "-device", "virtio-net-pci,netdev=net0,mac=02:00:00:00:00:01")
	assertFlagHas(t, args, "-display", "none")
	assertFlagHas(t, args, "-daemonize", "")
}

// TestQEMUArgs_WithKernelPath verifies kernel boot args are added when kernel path is set.
func TestQEMUArgs_WithKernelPath(t *testing.T) {
	qm := NewQemuManager("/tmp/vm-test-instances", nil)

	spec := InstanceSpec{
		InstanceID: "inst-kernel001",
		CPUCores:   4,
		MemoryMB:   8192,
		RootfsPath: "/mnt/nfs/vols/inst-kernel001.qcow2",
		KernelPath: "/opt/kernels/vmlinux-5.15",
		TapDevice:  "tap-kernel001",
		MacAddress: "02:00:00:00:00:02",
		PrivateIP:  "10.0.0.10",
	}

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	assertFlagHas(t, args, "-kernel", "/opt/kernels/vmlinux-5.15")
	assertFlagHasPrefix(t, args, "-append", "console=ttyS0 ip=10.0.0.10")
}

// TestQEMUArgs_ConsolePath verifies console log path is in -serial argument.
func TestQEMUArgs_ConsolePath(t *testing.T) {
	qm := NewQemuManager("/tmp/vm-test-instances", nil)

	spec := InstanceSpec{
		InstanceID: "inst-console001",
		CPUCores:   1,
		MemoryMB:   2048,
		RootfsPath: "/mnt/nfs/vols/inst-console001.qcow2",
		TapDevice:  "tap-con001",
		MacAddress: "02:00:00:00:00:03",
	}

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	assertFlagHasPrefix(t, args, "-serial", "file:")
	assertFlagHasPrefix(t, args, "-qmp", "unix:")
}

// TestQEMUArgs_DifferentShapes verifies args vary by shape.
func TestQEMUArgs_DifferentShapes(t *testing.T) {
	qm := NewQemuManager("/tmp/vm-test-instances", nil)

	tests := []struct {
		name     string
		cpuCores int32
		memoryMB int32
		wantSMP  string
		wantMem  string
	}{
		{"small", 2, 4096, "cpus=2", "4096M"},
		{"medium", 4, 8192, "cpus=4", "8192M"},
		{"large", 8, 16384, "cpus=8", "16384M"},
		{"xlarge", 16, 32768, "cpus=16", "32768M"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := InstanceSpec{
				InstanceID: "inst-" + tt.name,
				CPUCores:   tt.cpuCores,
				MemoryMB:   tt.memoryMB,
				RootfsPath: "/mnt/nfs/vols/qcow2",
				TapDevice:  "tap-test",
				MacAddress: "02:00:00:00:00:99",
			}
			args, err := qm.buildQEMUArgs(spec)
			if err != nil {
				t.Fatalf("buildQEMUArgs: %v", err)
			}
			assertFlagHas(t, args, "-smp", tt.wantSMP)
			assertFlagHas(t, args, "-m", tt.wantMem)
		})
	}
}

// TestQEMUArgs_DryRunDoesNotCallBinary verifies dry-run mode skips binary launch.
func TestQEMUArgs_DryRunDoesNotCallBinary(t *testing.T) {
	t.Setenv("QEMU_DRY_RUN", "true")
	qm := NewQemuManager("/tmp/vm-test-instances", nil)

	if !qm.dryRun {
		t.Fatal("expected dryRun to be true when QEMU_DRY_RUN=true")
	}

	info, err := qm.Create(t.Context(), InstanceSpec{
		InstanceID: "dry-run-test",
		CPUCores:   2,
		MemoryMB:   4096,
		RootfsPath: "/tmp/test.qcow2",
		TapDevice:  "tap-test",
		MacAddress: "02:00:00:00:00:01",
	})
	if err != nil {
		t.Fatalf("Create (dry-run): %v", err)
	}
	if info.State != "RUNNING" {
		t.Errorf("expected RUNNING state in dry-run, got %q", info.State)
	}
}

// TestQEMUArgs_DataRoot verifies that data root is used correctly.
func TestQEMUArgs_DataRoot(t *testing.T) {
	dataRoot := t.TempDir()
	qm := NewQemuManager(dataRoot, nil)

	if qm.DataRoot() != dataRoot {
		t.Errorf("DataRoot = %q, want %q", qm.DataRoot(), dataRoot)
	}

	spec := InstanceSpec{
		InstanceID: "inst-dataroot",
		CPUCores:   1,
		MemoryMB:   1024,
		RootfsPath: "/mnt/nfs/vols/test.qcow2",
		TapDevice:  "tap-test",
		MacAddress: "02:00:00:00:00:aa",
	}

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	assertFlagHasPrefix(t, args, "-qmp", "unix:"+dataRoot+"/inst-dataroot/instance.sock")
	assertFlagHasPrefix(t, args, "-serial", "file:"+dataRoot+"/inst-dataroot/console.log")
	assertFlagHasPrefix(t, args, "-pidfile", dataRoot+"/inst-dataroot/instance.pid")
}

// assertFlagHas checks that args contains a flag with the given value.
// If value is empty, checks only flag presence.
func assertFlagHas(t *testing.T, args []string, flag, value string) {
	t.Helper()
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			if value == "" {
				return
			}
			if i+1 < len(args) && args[i+1] == value {
				return
			}
		}
	}
	t.Errorf("args does not contain flag %q with value %q\nargs: %v", flag, value, args)
}

// assertFlagHasPrefix checks that args contains a flag whose value starts with prefix.
func assertFlagHasPrefix(t *testing.T, args []string, flag, prefix string) {
	t.Helper()
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			if i+1 < len(args) && hasPrefix(args[i+1], prefix) {
				return
			}
		}
	}
	t.Errorf("args does not contain flag %q with value starting with %q\nargs: %v", flag, prefix, args)
}

// TestQEMUArgs_ExtraDisks verifies extra block devices generate virtio-blk drive args.
func TestQEMUArgs_ExtraDisks(t *testing.T) {
	qm := NewQemuManager("/tmp/vm-test-instances", nil)

	spec := InstanceSpec{
		InstanceID: "inst-extradisk001",
		CPUCores:   2,
		MemoryMB:   4096,
		RootfsPath: "/mnt/nfs/vols/inst-extradisk001.qcow2",
		TapDevice:  "tap-extradisk001",
		MacAddress: "02:00:00:00:00:05",
		ExtraDisks: []ExtraDisk{
			{DiskID: "vol-data1", HostPath: "/mnt/nfs/vols/vol-data1/disk.img", DeviceName: "/dev/vdb"},
			{DiskID: "vol-data2", HostPath: "/mnt/nfs/vols/vol-data2/disk.img", DeviceName: "/dev/vdc"},
		},
	}

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	assertFlagHas(t, args, "-drive", "file=/mnt/nfs/vols/vol-data1/disk.img,if=virtio,format=qcow2,serial=vol-vol-data1")
	assertFlagHas(t, args, "-drive", "file=/mnt/nfs/vols/vol-data2/disk.img,if=virtio,format=qcow2,serial=vol-vol-data2")
}

// TestQEMUArgs_ExtraDisks_EmptyHostPathSkips verifies an ExtraDisk with empty HostPath is skipped.
func TestQEMUArgs_ExtraDisks_EmptyHostPathSkips(t *testing.T) {
	qm := NewQemuManager("/tmp/vm-test-instances", nil)

	spec := InstanceSpec{
		InstanceID: "inst-skip-empty",
		CPUCores:   1,
		MemoryMB:   2048,
		RootfsPath: "/mnt/nfs/vols/inst-skip-empty.qcow2",
		TapDevice:  "tap-skip-empty",
		MacAddress: "02:00:00:00:00:06",
		ExtraDisks: []ExtraDisk{
			{DiskID: "vol-bad", HostPath: "", DeviceName: "/dev/vdb"},
			{DiskID: "vol-good", HostPath: "/path/to/good.img", DeviceName: "/dev/vdc"},
		},
	}

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	// Vol-bad with empty path must not appear.
	assertFlagNotPresent(t, args, "vol-bad")

	// Vol-good must appear.
	assertFlagHas(t, args, "-drive", "file=/path/to/good.img,if=virtio,format=qcow2,serial=vol-vol-good")
}

// TestQEMUArgs_ExtraDisks_None verifies no extra drive args when ExtraDisks is empty.
func TestQEMUArgs_ExtraDisks_None(t *testing.T) {
	qm := NewQemuManager("/tmp/vm-test-instances", nil)

	spec := InstanceSpec{
		InstanceID: "inst-noextras",
		CPUCores:   1,
		MemoryMB:   2048,
		RootfsPath: "/tmp/test.qcow2",
		TapDevice:  "tap-noextras",
		MacAddress: "02:00:00:00:00:07",
	}

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	// The only -drive arg should be the root disk.
	driveCount := 0
	for _, a := range args {
		if a == "-drive" {
			driveCount++
		}
	}
	if driveCount != 1 {
		t.Errorf("expected exactly 1 -drive arg (root disk only), got %d\nargs: %v", driveCount, args)
	}
}

// assertFlagNotPresent checks that args does NOT contain value anywhere.
func assertFlagNotPresent(t *testing.T, args []string, value string) {
	t.Helper()
	for _, a := range args {
		if a == value {
			t.Errorf("args unexpectedly contains %q\nargs: %v", value, args)
			return
		}
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
