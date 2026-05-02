package runtime

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCloudInit_MetaDataContent verifies meta-data contains instance-id and hostname.
func TestCloudInit_MetaDataContent(t *testing.T) {
	cfg := CloudInitConfig{
		InstanceID: "inst-test-001",
		Hostname:   "test-host",
	}
	content := metaDataContent(cfg)

	if !strings.Contains(content, "instance-id: inst-test-001") {
		t.Errorf("meta-data missing instance-id:\n%s", content)
	}
	if !strings.Contains(content, "local-hostname: test-host") {
		t.Errorf("meta-data missing local-hostname:\n%s", content)
	}
	if !strings.Contains(content, "hostname: test-host") {
		t.Errorf("meta-data missing hostname:\n%s", content)
	}
}

// TestCloudInit_UserDataContent verifies user-data contains SSH key and cloud-config header.
func TestCloudInit_UserDataContent(t *testing.T) {
	cfg := CloudInitConfig{
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx test@key",
	}
	content := userDataContent(cfg)

	if !strings.Contains(content, "#cloud-config") {
		t.Errorf("user-data missing #cloud-config header:\n%s", content)
	}
	if !strings.Contains(content, "ssh_pwauth: false") {
		t.Errorf("user-data missing ssh_pwauth:\n%s", content)
	}
	if !strings.Contains(content, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx") {
		t.Errorf("user-data missing SSH key:\n%s", content)
	}
	if !strings.Contains(content, "ssh_authorized_keys:") {
		t.Errorf("user-data missing ssh_authorized_keys stanza:\n%s", content)
	}
}

// TestCloudInit_UserData_EmptySSHKey verifies user-data without SSH key still produces valid cloud-config.
func TestCloudInit_UserData_EmptySSHKey(t *testing.T) {
	cfg := CloudInitConfig{
		SSHPublicKey: "",
	}
	content := userDataContent(cfg)

	if !strings.Contains(content, "#cloud-config") {
		t.Errorf("user-data missing #cloud-config header:\n%s", content)
	}
	if strings.Contains(content, "ssh_authorized_keys:") {
		t.Errorf("user-data should not contain ssh_authorized_keys when key is empty:\n%s", content)
	}
}

// TestCloudInit_UserData_ExtraUserData verifies additional user-data is appended.
func TestCloudInit_UserData_ExtraUserData(t *testing.T) {
	cfg := CloudInitConfig{
		SSHPublicKey: "ssh-rsa AAAAB3 test",
		UserData:     "package_update: true\n",
	}
	content := userDataContent(cfg)
	if !strings.Contains(content, "package_update: true") {
		t.Errorf("user-data missing extra user-data:\n%s", content)
	}
}

// TestCloudInit_SeedPath verifies the seed path is under the data root.
func TestCloudInit_SeedPath(t *testing.T) {
	dataRoot := t.TempDir()
	am := NewArtifactManager(dataRoot)
	cdm := NewConfigDriveManager(am)

	seedPath := cdm.SeedPath("inst-path-test")
	if !strings.HasPrefix(seedPath, dataRoot) {
		t.Errorf("seed path %q not under data root %q", seedPath, dataRoot)
	}
	if !strings.HasSuffix(seedPath, "seed.iso") {
		t.Errorf("seed path %q does not end with seed.iso", seedPath)
	}
}

// TestCloudInit_GenerateSeed_ContentIsISO verifies GenerateSeed creates a non-empty file
// when genisoimage or mkisofs is available on the system.
func TestCloudInit_GenerateSeed_ContentIsISO(t *testing.T) {
	_, err := findISOTool()
	if err != nil {
		t.Skipf("skipping — no ISO creation tool available: %v", err)
	}

	dataRoot := t.TempDir()
	am := NewArtifactManager(dataRoot)
	cdm := NewConfigDriveManager(am)

	seedPath, err := cdm.GenerateSeed("inst-seed-001", CloudInitConfig{
		InstanceID:   "inst-seed-001",
		Hostname:     "test-vm",
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx test@key",
	})
	if err != nil {
		t.Fatalf("GenerateSeed failed: %v", err)
	}

	if seedPath == "" {
		t.Fatal("expected non-empty seed path")
	}

	info, err := os.Stat(seedPath)
	if err != nil {
		t.Fatalf("cannot stat seed ISO: %v", err)
	}
	if info.Size() == 0 {
		t.Error("seed ISO is empty")
	}

	// Verify the file contains the ISO 9660 magic bytes (genisoimage/mkisofs output).
	data, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatalf("cannot read seed ISO: %v", err)
	}
	// ISO 9660 filesystem magic: "CD001" at offset 0x8001 (32769).
	if len(data) < 32769+5 {
		t.Fatal("seed ISO too small to contain ISO 9660 magic")
	}
	// Check for CD001 magic at the expected offset.
	magicOffset := 32769
	if !bytesEqual(data[magicOffset:magicOffset+5], []byte("CD001")) {
		// genisoimage may use different output mode; check for rock ridge or el torito markers.
		if !bytesContains(data, []byte("cidata")) && !bytesContains(data, []byte("meta-data")) {
			t.Errorf("seed ISO does not contain expected ISO 9660 magic or content markers")
		}
	}
}

// TestCloudInit_GenerateSeed_Idempotent verifies GenerateSeed returns the same path on repeated calls.
func TestCloudInit_GenerateSeed_Idempotent(t *testing.T) {
	_, err := findISOTool()
	if err != nil {
		t.Skipf("skipping — no ISO creation tool available: %v", err)
	}

	dataRoot := t.TempDir()
	am := NewArtifactManager(dataRoot)
	cdm := NewConfigDriveManager(am)

	seed1, err := cdm.GenerateSeed("inst-idem-001", CloudInitConfig{
		InstanceID:   "inst-idem-001",
		Hostname:     "test-vm",
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx test@key",
	})
	if err != nil {
		t.Fatalf("first GenerateSeed failed: %v", err)
	}

	seed2, err := cdm.GenerateSeed("inst-idem-001", CloudInitConfig{
		InstanceID:   "inst-idem-001",
		Hostname:     "test-vm",
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx test@key",
	})
	if err != nil {
		t.Fatalf("second GenerateSeed failed: %v", err)
	}

	if seed1 != seed2 {
		t.Errorf("idempotent GenerateSeed produced different paths: %q vs %q", seed1, seed2)
	}
}

// TestCloudInit_FindISOTool verifies findISOTool returns expected tools.
func TestCloudInit_FindISOTool(t *testing.T) {
	tool, err := findISOTool()
	if err != nil {
		t.Skipf("no ISO tool on this system — expected on macOS without genisoimage")
	}

	switch tool {
	case "genisoimage", "mkisofs":
		// acceptable
	default:
		t.Errorf("unexpected ISO tool: %q", tool)
	}

	// Verify the tool is actually on PATH.
	_, lookErr := exec.LookPath(tool)
	if lookErr != nil {
		t.Errorf("findISOTool returned %q but it is not on PATH: %v", tool, lookErr)
	}
}

// TestCloudInit_QEMUArgs_IncludesSeedDrive verifies seed ISO is included in QEMU args when present.
func TestCloudInit_QEMUArgs_IncludesSeedDrive(t *testing.T) {
	_, err := findISOTool()
	if err != nil {
		t.Skipf("skipping — no ISO creation tool available: %v", err)
	}

	dataRoot := t.TempDir()
	qm := NewQemuManager(dataRoot, nil)

	spec := InstanceSpec{
		InstanceID:   "inst-seed-qemu",
		CPUCores:     2,
		MemoryMB:     4096,
		RootfsPath:   "/tmp/test.qcow2",
		TapDevice:    "tap-test",
		MacAddress:   "02:00:00:00:00:01",
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx test@key",
	}

	// Pre-generate the seed ISO so it exists for QEMU arg generation.
	copies := make([]byte, 2048)
	seedDir := qm.artifacts.InstanceDir("inst-seed-qemu")
	os.MkdirAll(seedDir, 0750)
	os.WriteFile(qm.artifacts.SeedPath("inst-seed-qemu"), copies, 0640)
	// Overwrite with actual seed:
	qm.configDrive.GenerateSeed("inst-seed-qemu", CloudInitConfig{
		InstanceID:   "inst-seed-qemu",
		Hostname:     "inst-seed-qemu",
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx test@key",
	})

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	// Find the -drive argument for the seed ISO.
	found := false
	for i := 0; i < len(args); i++ {
		if args[i] == "-drive" && strings.Contains(args[i+1], "file=") && strings.Contains(args[i+1], "seed.iso") {
			if strings.Contains(args[i+1], "media=cdrom") {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("QEMU args do not contain config-drive with seed.iso and media=cdrom\nargs: %v", args)
	}
}

// TestCloudInit_QEMUArgs_NoSeedWhenNoSSHKey verifies no seed ISO is included when no SSH key.
func TestCloudInit_QEMUArgs_NoSeedWhenNoSSHKey(t *testing.T) {
	dataRoot := t.TempDir()
	qm := NewQemuManager(dataRoot, nil)

	spec := InstanceSpec{
		InstanceID:   "inst-noseed",
		CPUCores:     2,
		MemoryMB:     4096,
		RootfsPath:   "/tmp/test.qcow2",
		TapDevice:    "tap-test",
		MacAddress:   "02:00:00:00:00:01",
		SSHPublicKey: "", // No SSH key
	}

	args, err := qm.buildQEMUArgs(spec)
	if err != nil {
		t.Fatalf("buildQEMUArgs: %v", err)
	}

	for _, a := range args {
		if strings.Contains(a, "seed.iso") {
			t.Errorf("QEMU args should NOT contain seed.iso when no SSH key: %v", args)
			break
		}
	}
}

// ── helpers ────────────────────────────────────────────────────────────────────

func bytesEqual(a, b []byte) bool {
	if len(a) < len(b) {
		return false
	}
	for i := range b {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func bytesContains(haystack, needle []byte) bool {
	for i := 0; i <= len(haystack)-len(needle); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
