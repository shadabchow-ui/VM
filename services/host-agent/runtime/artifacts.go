package runtime

// artifacts.go — Instance artifact path management under a configurable data root.
//
// All runtime instance artifacts (PID files, sockets, console logs, metadata)
// live under a single configurable data root directory. This manager enforces:
//   - All artifact paths are under the data root — no path traversal.
//   - Deterministic path derivation from instance IDs.
//   - Idempotent directory creation and cleanup.
//
// Layout:
//   <dataRoot>/
//     <instanceID>/
//       instance.pid        — PID file for the VM process
//       instance.sock       — hypervisor API socket (Firecracker only)
//       console.log         — VM serial console output
//       metadata.json       — instance metadata snapshot
//
// Source: VM Job 2 — artifact layout for instance runtime metadata.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ArtifactManager manages instance runtime artifacts under a configurable root.
type ArtifactManager struct {
	dataRoot string
}

// NewArtifactManager constructs an ArtifactManager with the given data root.
// dataRoot: top-level directory (empty → VM_PLATFORM_DATA_ROOT env → default).
func NewArtifactManager(dataRoot string) *ArtifactManager {
	if dataRoot == "" {
		dataRoot = os.Getenv("VM_PLATFORM_DATA_ROOT")
	}
	if dataRoot == "" {
		dataRoot = DefaultDataRoot
	}
	return &ArtifactManager{dataRoot: filepath.Clean(dataRoot)}
}

// DataRoot returns the configured data root directory.
func (m *ArtifactManager) DataRoot() string { return m.dataRoot }

// InstanceDir returns the per-instance subdirectory path.
// Path: <dataRoot>/<instanceID>/
func (m *ArtifactManager) InstanceDir(instanceID string) string {
	return filepath.Join(m.dataRoot, instanceID)
}

// PIDPath returns the PID file path for an instance.
func (m *ArtifactManager) PIDPath(instanceID string) string {
	return filepath.Join(m.InstanceDir(instanceID), "instance.pid")
}

// SocketPath returns the hypervisor API socket path (Firecracker only).
func (m *ArtifactManager) SocketPath(instanceID string) string {
	return filepath.Join(m.InstanceDir(instanceID), "instance.sock")
}

// ConsolePath returns the console log file path for an instance.
func (m *ArtifactManager) ConsolePath(instanceID string) string {
	return filepath.Join(m.InstanceDir(instanceID), "console.log")
}

// SeedPath returns the cloud-init config-drive seed ISO path for an instance.
func (m *ArtifactManager) SeedPath(instanceID string) string {
	return filepath.Join(m.InstanceDir(instanceID), "seed.iso")
}

// MetadataPath returns the instance metadata file path.
func (m *ArtifactManager) MetadataPath(instanceID string) string {
	return filepath.Join(m.InstanceDir(instanceID), "metadata.json")
}

// ValidatePath checks that the given path is strictly under the data root
// and contains no path traversal components. Returns an error if unsafe.
func (m *ArtifactManager) ValidatePath(path string) error {
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, m.dataRoot+string(filepath.Separator)) && cleaned != m.dataRoot {
		return fmt.Errorf("ArtifactManager: path %q is not under data root %q", path, m.dataRoot)
	}
	for _, segment := range strings.Split(strings.TrimPrefix(cleaned, m.dataRoot), string(filepath.Separator)) {
		if segment == "." || segment == ".." {
			return fmt.Errorf("ArtifactManager: path %q contains illegal segment %q", path, segment)
		}
		if segment == "" {
			continue
		}
	}
	return nil
}

// EnsureInstanceDir creates the per-instance directory if it does not exist.
// Idempotent: returns nil if the directory already exists.
func (m *ArtifactManager) EnsureInstanceDir(instanceID string) error {
	dir := m.InstanceDir(instanceID)
	if err := m.ValidatePath(dir); err != nil {
		return fmt.Errorf("EnsureInstanceDir: %w", err)
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("EnsureInstanceDir: %w", err)
	}
	return nil
}

// RemoveInstanceDir removes the per-instance directory and all contents.
// Idempotent: returns nil if the directory does not exist.
func (m *ArtifactManager) RemoveInstanceDir(instanceID string) error {
	dir := m.InstanceDir(instanceID)
	if err := m.ValidatePath(dir); err != nil {
		return fmt.Errorf("RemoveInstanceDir: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("RemoveInstanceDir: %w", err)
	}
	return nil
}

// InstanceIDs returns all instance IDs that have directories under the data root.
func (m *ArtifactManager) InstanceIDs() ([]string, error) {
	entries, err := os.ReadDir(m.dataRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}
