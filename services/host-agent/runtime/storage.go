package runtime

// storage.go — LocalStorageManager for single-host block-volume artifacts.
//
// Source: P2_VOLUME_MODEL.md §5 (storage path assignment),
//         vm-15-01__blueprint__ §data_model,
//         VM Job 4 — Local block-volume persistence and snapshot foundation.
//
// The LocalStorageManager is the single authority for local storage path derivation
// and artifact safety on a host. It enforces:
//   - All volume/snapshot artifact paths live under a configurable storage root.
//   - No user-controlled path segments — all paths use system-generated IDs.
//   - Path traversal attacks are detected and rejected.
//   - Deterministic, reproducible paths for idempotent operations.
//
// This is a single-host local storage manager. It does NOT implement:
//   - Distributed storage (Ceph, NFS HA, object storage).
//   - Cross-host volume migration or replication.
//   - Network-attached block device provisioning.
//
// Usage:
//   - Worker: derives control-plane storage_path metadata before DB write.
//   - Host-agent: creates/verifies/removes actual file-system artifacts on the host.
//
// Configuration:
//   - LOCAL_STORAGE_ROOT env var → default /var/lib/compute-platform/storage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultStorageRoot = "/var/lib/compute-platform/storage"
)

// LocalStorageManager manages local block-volume and snapshot artifact paths
// under a configurable storage root directory.
type LocalStorageManager struct {
	root string
}

// NewLocalStorageManager constructs a LocalStorageManager.
// root: storage root directory (empty → LOCAL_STORAGE_ROOT env → default).
func NewLocalStorageManager(root string) *LocalStorageManager {
	if root == "" {
		root = os.Getenv("LOCAL_STORAGE_ROOT")
	}
	if root == "" {
		root = defaultStorageRoot
	}
	return &LocalStorageManager{root: filepath.Clean(root)}
}

// Root returns the configured storage root directory.
func (m *LocalStorageManager) Root() string {
	return m.root
}

// VolumeDir returns the directory path for a volume's artifacts.
// Path: <root>/volumes/<volumeID>/
func (m *LocalStorageManager) VolumeDir(volumeID string) string {
	return filepath.Join(m.root, "volumes", volumeID)
}

// VolumeDiskPath returns the path to a volume's disk image file.
// Path: <root>/volumes/<volumeID>/disk.img
func (m *LocalStorageManager) VolumeDiskPath(volumeID string) string {
	return filepath.Join(m.VolumeDir(volumeID), "disk.img")
}

// VolumeLockPath returns the path to a volume's attachment lock file.
// Path: <root>/volumes/<volumeID>/attach.lock
func (m *LocalStorageManager) VolumeLockPath(volumeID string) string {
	return filepath.Join(m.VolumeDir(volumeID), "attach.lock")
}

// SnapshotDir returns the directory path for a snapshot's artifacts.
// Path: <root>/snapshots/<snapshotID>/
func (m *LocalStorageManager) SnapshotDir(snapshotID string) string {
	return filepath.Join(m.root, "snapshots", snapshotID)
}

// SnapshotDataPath returns the path to a snapshot's data artifact.
// Path: <root>/snapshots/<snapshotID>/data
func (m *LocalStorageManager) SnapshotDataPath(snapshotID string) string {
	return filepath.Join(m.SnapshotDir(snapshotID), "data")
}

// RestoreVolumePath returns the path for a volume restored from a snapshot.
// Path: <root>/volumes/<volumeID>/restore-from-<snapshotID>.img
func (m *LocalStorageManager) RestoreVolumePath(snapshotID, volumeID string) string {
	return filepath.Join(m.VolumeDir(volumeID), "restore-from-"+snapshotID+".img")
}

// ValidatePath checks that the given path is under the configured storage root,
// is not traversing outside the root, and contains no dangerous components.
// Returns an error if the path is unsafe.
func (m *LocalStorageManager) ValidatePath(path string) error {
	cleaned := filepath.Clean(path)
	if !strings.HasPrefix(cleaned, m.root+string(filepath.Separator)) && cleaned != m.root {
		return fmt.Errorf("LocalStorageManager: path %q is not under storage root %q", path, m.root)
	}
	if strings.Contains(path, "..") {
		// Double-check: filepath.Clean should have handled this, but be explicit.
		return fmt.Errorf("LocalStorageManager: path %q contains traversal attempt", path)
	}
	for _, segment := range strings.Split(strings.TrimPrefix(cleaned, m.root), string(filepath.Separator)) {
		if segment == "." || segment == ".." {
			return fmt.Errorf("LocalStorageManager: path %q contains illegal segment %q", path, segment)
		}
	}
	return nil
}

// ValidateID checks that a user-supplied or system-generated ID is safe for
// use as a filesystem path component. Rejects empty, ".", "..", "/", and
// any string containing path separators or traversal attempts.
func (m *LocalStorageManager) ValidateID(id string) error {
	if id == "" {
		return fmt.Errorf("LocalStorageManager: empty ID is not allowed as a path component")
	}
	if id == "." || id == ".." {
		return fmt.Errorf("LocalStorageManager: ID %q is a reserved path component", id)
	}
	if strings.ContainsAny(id, "/\\\x00") {
		return fmt.Errorf("LocalStorageManager: ID %q contains illegal characters", id)
	}
	return nil
}

// EnsureDir creates the directory for path and all parents if they do not exist.
// The path must be under the storage root (validated before creation).
func (m *LocalStorageManager) EnsureDir(path string) error {
	if err := m.ValidatePath(path); err != nil {
		return fmt.Errorf("EnsureDir: %w", err)
	}
	dir := path
	if filepath.Ext(path) != "" {
		dir = filepath.Dir(path)
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("EnsureDir: mkdir %s: %w", dir, err)
	}
	return nil
}

// RemoveArtifact removes a file at the given path. The path must be under
// the storage root. Idempotent: returns nil if the file does not exist.
func (m *LocalStorageManager) RemoveArtifact(path string) error {
	if err := m.ValidatePath(path); err != nil {
		return fmt.Errorf("RemoveArtifact: %w", err)
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("RemoveArtifact: remove %s: %w", path, err)
	}
	return nil
}
