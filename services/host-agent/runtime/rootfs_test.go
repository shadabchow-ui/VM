package runtime

// rootfs_test.go — Unit tests for RootfsManager.
//
// Source: 11-02-phase-1-test-strategy-and-lifecycle-test-matrix.md,
//         IMPLEMENTATION_PLAN_V1 (idempotency verified for all host agent primitives).
//
// These tests run without a real NFS mount or qemu-img binary.
// They verify the idempotency logic and path derivation.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func newTestRootfsManager(t *testing.T) (*RootfsManager, string) {
	t.Helper()
	dir := t.TempDir()
	m := NewRootfsManager(dir, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	return m, dir
}

func TestOverlayPath(t *testing.T) {
	m, dir := newTestRootfsManager(t)
	got := m.OverlayPath("inst_abc123")
	want := filepath.Join(dir, "inst_abc123.qcow2")
	if got != want {
		t.Errorf("OverlayPath = %q, want %q", got, want)
	}
}

func TestDelete_AlreadyAbsent_IsNoOp(t *testing.T) {
	m, _ := newTestRootfsManager(t)
	// Deleting a non-existent file must return nil (idempotent).
	if err := m.Delete("inst_nonexistent"); err != nil {
		t.Errorf("Delete of absent file = %v, want nil", err)
	}
}

func TestDelete_ExistingFile_RemovesIt(t *testing.T) {
	m, dir := newTestRootfsManager(t)
	path := filepath.Join(dir, "inst_existing.qcow2")
	if err := os.WriteFile(path, []byte("fake qcow2"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("inst_existing"); err != nil {
		t.Errorf("Delete = %v, want nil", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still exists after Delete")
	}
}

func TestDelete_CalledTwice_IsIdempotent(t *testing.T) {
	m, dir := newTestRootfsManager(t)
	path := filepath.Join(dir, "inst_twice.qcow2")
	_ = os.WriteFile(path, []byte("x"), 0644)
	if err := m.Delete("inst_twice"); err != nil {
		t.Fatal(err)
	}
	// Second call must also return nil.
	if err := m.Delete("inst_twice"); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
}

func TestMaterialize_BaseImageNotFound_ReturnsError(t *testing.T) {
	m, _ := newTestRootfsManager(t)
	ctx := context.Background()
	_, err := m.Materialize(ctx, "inst_x", "/nonexistent/base.qcow2")
	if err == nil {
		t.Error("expected error when base image is absent, got nil")
	}
}
