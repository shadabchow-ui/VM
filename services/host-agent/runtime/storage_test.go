package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalStorageManager_DefaultRoot(t *testing.T) {
	// Unset env so default is used.
	os.Unsetenv("LOCAL_STORAGE_ROOT")
	m := NewLocalStorageManager("")
	if m.Root() != defaultStorageRoot {
		t.Errorf("default root = %q, want %q", m.Root(), defaultStorageRoot)
	}
}

func TestLocalStorageManager_EnvRoot(t *testing.T) {
	os.Setenv("LOCAL_STORAGE_ROOT", "/custom/storage/root")
	defer os.Unsetenv("LOCAL_STORAGE_ROOT")
	m := NewLocalStorageManager("")
	if m.Root() != "/custom/storage/root" {
		t.Errorf("env root = %q, want /custom/storage/root", m.Root())
	}
}

func TestLocalStorageManager_ExplicitRoot(t *testing.T) {
	m := NewLocalStorageManager("/explicit/path")
	if m.Root() != "/explicit/path" {
		t.Errorf("explicit root = %q, want /explicit/path", m.Root())
	}
}

func TestLocalStorageManager_VolumeDir(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	got := m.VolumeDir("vol-abc123")
	want := "/test/storage/volumes/vol-abc123"
	if got != want {
		t.Errorf("VolumeDir = %q, want %q", got, want)
	}
}

func TestLocalStorageManager_VolumeDiskPath(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	got := m.VolumeDiskPath("vol-abc123")
	want := "/test/storage/volumes/vol-abc123/disk.img"
	if got != want {
		t.Errorf("VolumeDiskPath = %q, want %q", got, want)
	}
}

func TestLocalStorageManager_VolumeLockPath(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	got := m.VolumeLockPath("vol-abc123")
	want := "/test/storage/volumes/vol-abc123/attach.lock"
	if got != want {
		t.Errorf("VolumeLockPath = %q, want %q", got, want)
	}
}

func TestLocalStorageManager_SnapshotDir(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	got := m.SnapshotDir("snap-xyz789")
	want := "/test/storage/snapshots/snap-xyz789"
	if got != want {
		t.Errorf("SnapshotDir = %q, want %q", got, want)
	}
}

func TestLocalStorageManager_SnapshotDataPath(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	got := m.SnapshotDataPath("snap-xyz789")
	want := "/test/storage/snapshots/snap-xyz789/data"
	if got != want {
		t.Errorf("SnapshotDataPath = %q, want %q", got, want)
	}
}

func TestLocalStorageManager_RestoreVolumePath(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	got := m.RestoreVolumePath("snap-xyz", "vol-abc")
	want := "/test/storage/volumes/vol-abc/restore-from-snap-xyz.img"
	if got != want {
		t.Errorf("RestoreVolumePath = %q, want %q", got, want)
	}
}

func TestLocalStorageManager_RootCleaned(t *testing.T) {
	m := NewLocalStorageManager("/test/storage//extra/..")
	if !strings.HasSuffix(m.Root(), "/test/storage") {
		t.Errorf("cleaned root = %q, want trailing /test/storage", m.Root())
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// Path safety: ValidatePath
// ═════════════════════════════════════════════════════════════════════════════

func TestValidatePath_ValidPathUnderRoot(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	path := m.VolumeDiskPath("vol-abc")
	if err := m.ValidatePath(path); err != nil {
		t.Errorf("expected valid path, got error: %v", err)
	}
}

func TestValidatePath_PathOutsideRoot(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	if err := m.ValidatePath("/etc/passwd"); err == nil {
		t.Error("expected error for path outside root, got nil")
	}
}

func TestValidatePath_TraversalAttempt(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	path := m.root + "/volumes/../../../etc/passwd"
	if err := m.ValidatePath(path); err == nil {
		t.Error("expected error for traversal attempt, got nil")
	}
}

func TestValidatePath_SubtleTraversal(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	path := m.root + "/volumes/vol-abc/../../../test/storage/sub/escape"
	if err := m.ValidatePath(path); err == nil {
		t.Error("expected error for subtle traversal, got nil")
	}
}

func TestValidatePath_RootExactly(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	if err := m.ValidatePath(m.root); err != nil {
		t.Errorf("expected root path to be valid, got error: %v", err)
	}
}

func TestValidatePath_EmptyString(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	if err := m.ValidatePath(""); err == nil {
		t.Error("expected error for empty path, got nil")
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// ID validation
// ═════════════════════════════════════════════════════════════════════════════

func TestValidateID_Valid(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	for _, id := range []string{"vol-abc123", "snap-xyz789", "v-001", "disk_inst-xyz"} {
		if err := m.ValidateID(id); err != nil {
			t.Errorf("ValidateID(%q): unexpected error: %v", id, err)
		}
	}
}

func TestValidateID_Empty(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	if err := m.ValidateID(""); err == nil {
		t.Error("expected error for empty ID, got nil")
	}
}

func TestValidateID_Dot(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	for _, id := range []string{".", ".."} {
		if err := m.ValidateID(id); err == nil {
			t.Errorf("expected error for ID %q, got nil", id)
		}
	}
}

func TestValidateID_WithSlash(t *testing.T) {
	m := NewLocalStorageManager("/test/storage")
	for _, id := range []string{"vol/abc", "vol\\abc", "vol\x00abc"} {
		if err := m.ValidateID(id); err == nil {
			t.Errorf("expected error for ID %q, got nil", id)
		}
	}
}

// ═════════════════════════════════════════════════════════════════════════════
// EnsureDir / RemoveArtifact
// ═════════════════════════════════════════════════════════════════════════════

func TestEnsureDir_CreatesDirectory(t *testing.T) {
	dir := t.TempDir()
	m := NewLocalStorageManager(dir)
	volDir := m.VolumeDir("vol-test")
	if err := m.EnsureDir(volDir); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, err := os.Stat(volDir)
	if err != nil {
		t.Fatalf("Stat after EnsureDir: %v", err)
	}
	if !info.IsDir() {
		t.Error("EnsureDir path is not a directory")
	}
}

func TestEnsureDir_RejectsOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	m := NewLocalStorageManager(dir)
	if err := m.EnsureDir("/etc/hacked"); err == nil {
		t.Error("expected error for path outside root, got nil")
	}
}

func TestEnsureDir_Idempotent(t *testing.T) {
	dir := t.TempDir()
	m := NewLocalStorageManager(dir)
	volDir := m.VolumeDir("vol-test")
	if err := m.EnsureDir(volDir); err != nil {
		t.Fatalf("first EnsureDir: %v", err)
	}
	if err := m.EnsureDir(volDir); err != nil {
		t.Fatalf("second EnsureDir: %v", err)
	}
}

func TestRemoveArtifact_RemovesFile(t *testing.T) {
	dir := t.TempDir()
	m := NewLocalStorageManager(dir)
	volDir := m.VolumeDir("vol-test")
	if err := m.EnsureDir(volDir); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	lockPath := filepath.Join(volDir, "test-file")
	if err := os.WriteFile(lockPath, []byte("test"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := m.RemoveArtifact(lockPath); err != nil {
		t.Fatalf("RemoveArtifact: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Error("file still exists after RemoveArtifact")
	}
}

func TestRemoveArtifact_Idempotent(t *testing.T) {
	dir := t.TempDir()
	m := NewLocalStorageManager(dir)
	path := filepath.Join(dir, "volumes", "vol-gone", "disk.img")
	if err := m.EnsureDir(path); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	// First removal should succeed.
	if err := m.RemoveArtifact(path); err != nil {
		t.Fatalf("first RemoveArtifact: %v", err)
	}
	// Second removal should be idempotent (no error).
	if err := m.RemoveArtifact(path); err != nil {
		t.Fatalf("second RemoveArtifact (idempotent): %v", err)
	}
}

func TestRemoveArtifact_RejectsOutsideRoot(t *testing.T) {
	dir := t.TempDir()
	m := NewLocalStorageManager(dir)
	if err := m.RemoveArtifact("/etc/deleted"); err == nil {
		t.Error("expected error for path outside root, got nil")
	}
}

func TestEnsureDir_FilePathParentCreatesDir(t *testing.T) {
	dir := t.TempDir()
	m := NewLocalStorageManager(dir)
	diskPath := m.VolumeDiskPath("vol-disk-parent")
	if err := m.EnsureDir(diskPath); err != nil {
		t.Fatalf("EnsureDir (file path): %v", err)
	}
	parent := filepath.Dir(diskPath)
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("Stat parent dir: %v", err)
	}
	if !info.IsDir() {
		t.Error("parent of disk path is not a directory")
	}
}
