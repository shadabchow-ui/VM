package runtime

// cloudinit.go — Cloud-init NoCloud config-drive seed ISO generation.
//
// Source: VM Job 6 — Linux/KVM VM lifecycle + cloud-init/SSH acceptance gate.
//
// Generates a minimal NoCloud config-drive ISO containing:
//   - meta-data:   instance-id, hostname, local-hostname
//   - user-data:   cloud-init directive to install SSH public key(s)
//   - vendor-data: empty in Phase 1
//   - network-config: DHCP v1 in Phase 1
//
// The seed ISO is passed to QEMU as a secondary virtio drive with media=cdrom
// so cloud-init inside the guest detects and applies the configuration on first boot.
//
// Required on host: genisoimage / mkisofs (from cdrtools or genisoimage package).
// If the tool is not found, seed generation is skipped with a warning.

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// CloudInitConfig holds the parameters for generating a NoCloud config-drive.
type CloudInitConfig struct {
	InstanceID   string
	Hostname     string
	SSHPublicKey string
	UserData     string // additional cloud-init user-data (empty in Phase 1)
}

// ConfigDriveManager generates NoCloud config-drive ISO images for QEMU VMs.
type ConfigDriveManager struct {
	artifacts *ArtifactManager
}

// NewConfigDriveManager constructs a ConfigDriveManager.
func NewConfigDriveManager(artifacts *ArtifactManager) *ConfigDriveManager {
	return &ConfigDriveManager{artifacts: artifacts}
}

// SeedPath returns the config-drive seed ISO path for an instance.
func (c *ConfigDriveManager) SeedPath(instanceID string) string {
	return c.artifacts.SeedPath(instanceID)
}

// metaDataTemplate returns the cloud-init meta-data content.
func metaDataContent(cfg CloudInitConfig) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\nhostname: %s\n",
		cfg.InstanceID, cfg.Hostname, cfg.Hostname)
}

// userDataTemplate returns the cloud-init user-data content.
// Phase 1: minimal cloud-init that installs the SSH public key and performs
// basic boot-time configuration. No package installs, no runcmd, no write_files
// beyond the SSH authorized_keys stanza.
func userDataContent(cfg CloudInitConfig) string {
	content := "#cloud-config\n"
	content += "ssh_pwauth: false\n"
	content += "disable_root: true\n"
	if cfg.SSHPublicKey != "" {
		content += "ssh_authorized_keys:\n"
		content += fmt.Sprintf("  - %s\n", cfg.SSHPublicKey)
	}
	if cfg.UserData != "" {
		content += cfg.UserData + "\n"
	}
	return content
}

// GenerateSeed creates a NoCloud config-drive ISO at the seed path for the instance.
// Returns the seed path on success.
// Idempotent: if the seed ISO already exists, returns the path without regenerating.
func (c *ConfigDriveManager) GenerateSeed(instanceID string, cfg CloudInitConfig) (string, error) {
	seedPath := c.SeedPath(instanceID)

	// Idempotency: if seed already exists, skip generation.
	if _, err := os.Stat(seedPath); err == nil {
		return seedPath, nil
	}

	// Create a temporary directory for the NoCloud source files.
	dir := filepath.Join(c.artifacts.InstanceDir(instanceID), ".seed-src")
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", fmt.Errorf("GenerateSeed: mkdir src: %w", err)
	}
	defer os.RemoveAll(dir)

	// Write meta-data.
	if err := os.WriteFile(filepath.Join(dir, "meta-data"), []byte(metaDataContent(cfg)), 0640); err != nil {
		return "", fmt.Errorf("GenerateSeed: write meta-data: %w", err)
	}

	// Write user-data.
	if err := os.WriteFile(filepath.Join(dir, "user-data"), []byte(userDataContent(cfg)), 0640); err != nil {
		return "", fmt.Errorf("GenerateSeed: write user-data: %w", err)
	}

	// Write vendor-data (empty in Phase 1).
	if err := os.WriteFile(filepath.Join(dir, "vendor-data"), []byte(""), 0640); err != nil {
		return "", fmt.Errorf("GenerateSeed: write vendor-data: %w", err)
	}

	// Write network-config (DHCP in Phase 1).
	networkConfig := "version: 1\nconfig:\n  - type: physical\n    name: eth0\n    subnets:\n      - type: dhcp\n"
	if err := os.WriteFile(filepath.Join(dir, "network-config"), []byte(networkConfig), 0640); err != nil {
		return "", fmt.Errorf("GenerateSeed: write network-config: %w", err)
	}

	// Build the ISO with genisoimage or mkisofs.
	isoTool, err := findISOTool()
	if err != nil {
		return "", fmt.Errorf("GenerateSeed: %w", err)
	}

	// Ensure the seed output directory exists.
	if err := c.artifacts.EnsureInstanceDir(instanceID); err != nil {
		return "", fmt.Errorf("GenerateSeed: ensure instance dir: %w", err)
	}

	args := []string{
		"-output", seedPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		dir,
	}

	var stderr bytes.Buffer
	cmd := exec.Command(isoTool, args...)
	cmd.Stderr = &stderr
	if out, err := cmd.Output(); err != nil {
		return "", fmt.Errorf("GenerateSeed: %s: %w\nstderr: %s\nstdout: %s", isoTool, err, stderr.String(), string(out))
	}

	return seedPath, nil
}

// findISOTool returns the available ISO creation tool on the host.
func findISOTool() (string, error) {
	for _, tool := range []string{"genisoimage", "mkisofs"} {
		if _, err := exec.LookPath(tool); err == nil {
			return tool, nil
		}
	}
	return "", fmt.Errorf("no ISO creation tool found (install genisoimage or mkisofs)")
}
