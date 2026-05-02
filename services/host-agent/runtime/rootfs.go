package runtime

// rootfs.go — Root disk materialisation: qcow2 CoW overlay on NFS.
//
// Source: RUNTIMESERVICE_GRPC_V1 §7 step 1,
//         06-01-root-disk-model-and-persistence-semantics.md,
//         IMPLEMENTATION_PLAN_V1 §30.
//
// Phase 1 contract:
//   - delete_on_termination=true always (R-15).
//   - Storage path: <NFS_ROOT>/<instance_id>.qcow2
//   - Materialize is idempotent: if the overlay already exists and passes
//     integrity check, returns the existing path without re-creating it.
//   - Delete is idempotent: if the file is absent, returns nil (no-op).
//   - All exec calls log stdout+stderr on failure for operator diagnosis.
//
// Required environment:
//   NFS_ROOT   path to the NFS mount point (e.g. /mnt/nfs/vols)
//   qemu-img must be on PATH (installed with qemu-utils on the host).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const defaultNFSRoot = "/mnt/nfs/vols"

// resolveImagePath maps an object:// image URL to a local filesystem path using
// IMAGE_CATALOG, a comma-separated list of key=value pairs.
// Example:
//
//	IMAGE_CATALOG="object://images/ubuntu-22.04-base.qcow2=/Users/sha/images/ubuntu-22.04-base.qcow2"
func resolveImagePath(in string) string {
	catalog := os.Getenv("IMAGE_CATALOG")
	if catalog == "" {
		return in
	}
	for _, pair := range strings.Split(catalog, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == in {
			return strings.TrimSpace(v)
		}
	}
	return in
}

// RootfsManager handles qcow2 CoW overlay lifecycle.
type RootfsManager struct {
	nfsRoot string
	log     *slog.Logger
}

// NewRootfsManager constructs a RootfsManager.
// nfsRoot: path to NFS mount (empty → use NFS_ROOT env, then /mnt/nfs/vols).
func NewRootfsManager(nfsRoot string, log *slog.Logger) *RootfsManager {
	if nfsRoot == "" {
		nfsRoot = os.Getenv("NFS_ROOT")
	}
	if nfsRoot == "" {
		nfsRoot = defaultNFSRoot
	}
	return &RootfsManager{nfsRoot: nfsRoot, log: log}
}

// OverlayPath returns the canonical NFS path for an instance's root disk.
// Format: <nfsRoot>/<instanceID>.qcow2
// Source: 06-01-root-disk-model-and-persistence-semantics.md §storage_path.
func (m *RootfsManager) OverlayPath(instanceID string) string {
	return filepath.Join(m.nfsRoot, instanceID+".qcow2")
}

// Materialize creates a qcow2 CoW overlay backed by the base image at baseImagePath.
// Idempotent: if the overlay already exists and passes integrity check, returns its path.
// baseImagePath: local NFS path to the base image (already present on the NFS share).
//
// Source: IMPLEMENTATION_PLAN_V1 §30 (rootfs materialisation idempotent).
func (m *RootfsManager) Materialize(ctx context.Context, instanceID, baseImagePath string) (string, error) {
	overlayPath := m.OverlayPath(instanceID)
	baseImagePath = resolveImagePath(baseImagePath)

	// Idempotency: if overlay exists and is healthy, return it.
	if _, err := os.Stat(overlayPath); err == nil {
		if checkErr := m.checkIntegrity(ctx, overlayPath); checkErr == nil {
			m.log.Info("rootfs overlay already exists — reusing",
				"instance_id", instanceID,
				"path", overlayPath,
			)
			return overlayPath, nil
		}
		// Corrupt overlay: remove and recreate.
		m.log.Warn("rootfs overlay corrupt — removing and recreating",
			"instance_id", instanceID,
			"path", overlayPath,
		)
		if err := os.Remove(overlayPath); err != nil {
			return "", fmt.Errorf("Materialize: remove corrupt overlay: %w", err)
		}
	}

	// Verify base image is accessible.
	if _, err := os.Stat(baseImagePath); err != nil {
		return "", fmt.Errorf("Materialize: base image not accessible at %s: %w", baseImagePath, err)
	}

	// qemu-img create -f qcow2 -b <backing> -F qcow2 <overlay>
	// The -F qcow2 flag is required for newer qemu-img versions.
	args := []string{
		"create",
		"-f", "qcow2",
		"-b", baseImagePath,
		"-F", "qcow2",
		overlayPath,
	}
	if err := m.runCmd(ctx, "qemu-img", args...); err != nil {
		return "", fmt.Errorf("Materialize: qemu-img create: %w", err)
	}

	m.log.Info("rootfs overlay created",
		"instance_id", instanceID,
		"path", overlayPath,
		"backing", baseImagePath,
	)
	return overlayPath, nil
}

// Delete removes the instance's qcow2 overlay file.
// Idempotent: if the file does not exist, returns nil.
// Source: RUNTIMESERVICE_GRPC_V1 DeleteInstanceRequest.delete_root_disk=true (Phase 1 always true).
func (m *RootfsManager) Delete(instanceID string) error {
	overlayPath := m.OverlayPath(instanceID)
	if err := os.Remove(overlayPath); err != nil {
		if os.IsNotExist(err) {
			m.log.Info("rootfs overlay already absent — idempotent no-op",
				"instance_id", instanceID,
				"path", overlayPath,
			)
			return nil
		}
		return fmt.Errorf("Delete rootfs %s: %w", overlayPath, err)
	}
	m.log.Info("rootfs overlay deleted",
		"instance_id", instanceID,
		"path", overlayPath,
	)
	return nil
}

// checkIntegrity runs qemu-img check on the overlay to detect corruption.
func (m *RootfsManager) checkIntegrity(ctx context.Context, path string) error {
	return m.runCmd(ctx, "qemu-img", "check", path)
}

// runCmd executes a system command, capturing combined output for logging on failure.
func (m *RootfsManager) runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cmd %s %s: %w\noutput: %s",
			name, strings.Join(args, " "), err, string(out))
	}
	return nil
}
