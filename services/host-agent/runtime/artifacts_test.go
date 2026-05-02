package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// TestArtifactManager_PathsDerivedFromDataRoot verifies all path methods produce
// paths under the data root and with expected suffixes.
func TestArtifactManager_PathsDerivedFromDataRoot(t *testing.T) {
	m := NewArtifactManager("/var/lib/compute-platform/instances")

	instDir := m.InstanceDir("inst-abc123")
	if instDir != "/var/lib/compute-platform/instances/inst-abc123" {
		t.Errorf("InstanceDir = %q", instDir)
	}

	pid := m.PIDPath("inst-abc123")
	if pid != "/var/lib/compute-platform/instances/inst-abc123/instance.pid" {
		t.Errorf("PIDPath = %q", pid)
	}

	sock := m.SocketPath("inst-abc123")
	if sock != "/var/lib/compute-platform/instances/inst-abc123/instance.sock" {
		t.Errorf("SocketPath = %q", sock)
	}

	console := m.ConsolePath("inst-abc123")
	if console != "/var/lib/compute-platform/instances/inst-abc123/console.log" {
		t.Errorf("ConsolePath = %q", console)
	}

	meta := m.MetadataPath("inst-abc123")
	if meta != "/var/lib/compute-platform/instances/inst-abc123/metadata.json" {
		t.Errorf("MetadataPath = %q", meta)
	}

	if m.DataRoot() != "/var/lib/compute-platform/instances" {
		t.Errorf("DataRoot = %q", m.DataRoot())
	}
}

// TestArtifactManager_DefaultDataRoot verifies the default data root is used
// when none is provided.
func TestArtifactManager_DefaultDataRoot(t *testing.T) {
	m := NewArtifactManager("")
	if m.DataRoot() != DefaultDataRoot {
		t.Errorf("expected default data root %q, got %q", DefaultDataRoot, m.DataRoot())
	}
}

// TestArtifactManager_ValidatePath tests path traversal detection.
func TestArtifactManager_ValidatePath(t *testing.T) {
	m := NewArtifactManager("/var/lib/compute-platform/instances")

	tests := []struct {
		path    string
		wantErr bool
	}{
		{"/var/lib/compute-platform/instances", false},
		{"/var/lib/compute-platform/instances/inst-001", false},
		{"/var/lib/compute-platform/instances/inst-001/instance.pid", false},
		{"/var/lib/compute-platform/instances/inst-001/console.log", false},
		{"/etc/passwd", true},
		{"/var/lib/compute-platform/instances/../../../etc/passwd", true},
		{"/tmp/something-else", true},
		{"/", true},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			err := m.ValidatePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePath(%q) error = %v, wantErr = %v", tt.path, err, tt.wantErr)
			}
		})
	}
}

// TestArtifactManager_EnsureAndRemoveInstanceDir tests directory lifecycle.
func TestArtifactManager_EnsureAndRemoveInstanceDir(t *testing.T) {
	root := t.TempDir()
	m := NewArtifactManager(root)

	// Ensure creates the directory.
	if err := m.EnsureInstanceDir("inst-001"); err != nil {
		t.Fatalf("EnsureInstanceDir: %v", err)
	}

	// Verify directory exists.
	if _, err := os.Stat(m.InstanceDir("inst-001")); os.IsNotExist(err) {
		t.Fatal("instance dir was not created")
	}

	// Idempotent: second call succeeds.
	if err := m.EnsureInstanceDir("inst-001"); err != nil {
		t.Fatalf("EnsureInstanceDir (idempotent): %v", err)
	}

	// Remove deletes the directory.
	if err := m.RemoveInstanceDir("inst-001"); err != nil {
		t.Fatalf("RemoveInstanceDir: %v", err)
	}

	// Verify directory is gone.
	if _, err := os.Stat(m.InstanceDir("inst-001")); !os.IsNotExist(err) {
		t.Fatal("instance dir was not removed")
	}

	// Idempotent: remove non-existent directory succeeds.
	if err := m.RemoveInstanceDir("inst-001"); err != nil {
		t.Fatalf("RemoveInstanceDir (idempotent): %v", err)
	}
}

// TestArtifactManager_InstanceIDs lists instance IDs from data root.
func TestArtifactManager_InstanceIDs(t *testing.T) {
	root := t.TempDir()
	m := NewArtifactManager(root)

	if err := m.EnsureInstanceDir("inst-a"); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureInstanceDir("inst-b"); err != nil {
		t.Fatal(err)
	}

	// Create a file (not a dir) at root to verify directories-only filtering.
	f, _ := os.Create(filepath.Join(root, "not-an-instance"))
	if f != nil {
		f.Close()
	}

	ids, err := m.InstanceIDs()
	if err != nil {
		t.Fatalf("InstanceIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 instance IDs, got %d: %v", len(ids), ids)
	}
}

// TestArtifactManager_EnvVarDataRoot verifies env var overrides.
func TestArtifactManager_EnvVarDataRoot(t *testing.T) {
	t.Setenv("VM_PLATFORM_DATA_ROOT", "/opt/custom/instances")
	m := NewArtifactManager("")
	if m.DataRoot() != "/opt/custom/instances" {
		t.Errorf("expected env-overridden data root, got %q", m.DataRoot())
	}
}

// TestConsoleLogger_EnsureAndReadConsole tests console log lifecycle.
func TestConsoleLogger_EnsureAndReadConsole(t *testing.T) {
	root := t.TempDir()
	am := NewArtifactManager(root)
	cl := NewConsoleLogger(am)

	if err := cl.EnsureConsoleFile("inst-001"); err != nil {
		t.Fatalf("EnsureConsoleFile: %v", err)
	}

	// ReadConsole on empty file returns empty string.
	content, err := cl.ReadConsole("inst-001")
	if err != nil {
		t.Fatalf("ReadConsole: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty console, got %q", content)
	}

	// Verify file path.
	expectedPath := filepath.Join(root, "inst-001", "console.log")
	if cl.ConsolePath("inst-001") != expectedPath {
		t.Errorf("ConsolePath = %q, want %q", cl.ConsolePath("inst-001"), expectedPath)
	}
}

// TestConsoleLogger_ReadNonExistent returns empty string.
func TestConsoleLogger_ReadNonExistent(t *testing.T) {
	root := t.TempDir()
	am := NewArtifactManager(root)
	cl := NewConsoleLogger(am)

	content, err := cl.ReadConsole("no-such-instance")
	if err != nil {
		t.Fatalf("ReadConsole: %v", err)
	}
	if content != "" {
		t.Errorf("expected empty console for non-existent instance, got %q", content)
	}
}
