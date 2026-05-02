package main

// registration_test.go — Unit tests for bootstrap token loading.
//
// These tests cover the readBootstrapToken helper in isolation: env var path,
// file path, and every whitespace edge case that has caused 401s in practice.
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §6, 05-02-host-runtime-worker-design.md §Bootstrap.
//
// No network, no DB, no Resource Manager required. Run:
//   go test ./services/host-agent/... -run TestReadBootstrapToken -v

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReadBootstrapToken_EnvVar verifies the env var path returns the token
// with leading/trailing whitespace stripped (defensive: some shells add spaces).
func TestReadBootstrapToken_EnvVar(t *testing.T) {
	t.Setenv(tokenEnvVar, "  mytoken123  ")

	got, err := readBootstrapToken()
	if err != nil {
		t.Fatalf("readBootstrapToken: %v", err)
	}
	if got != "mytoken123" {
		t.Errorf("got %q, want %q", got, "mytoken123")
	}
}

// TestReadBootstrapToken_EnvVar_TakesPriorityOverFile verifies that when both
// env var and file are present, the env var wins.
func TestReadBootstrapToken_EnvVar_TakesPriorityOverFile(t *testing.T) {
	dir := t.TempDir()
	fakeTokenFile := filepath.Join(dir, "bootstrap-token")
	if err := os.WriteFile(fakeTokenFile, []byte("file-token"), 0600); err != nil {
		t.Fatal(err)
	}

	// Override the package-level tokenFile for this test.
	origTokenFile := tokenFile
	tokenFile = fakeTokenFile
	t.Cleanup(func() { tokenFile = origTokenFile })

	t.Setenv(tokenEnvVar, "env-token")

	got, err := readBootstrapToken()
	if err != nil {
		t.Fatalf("readBootstrapToken: %v", err)
	}
	if got != "env-token" {
		t.Errorf("got %q, want %q", got, "env-token")
	}
}

// TestReadBootstrapToken_File_PlainNewline is the most common real-world case:
//   echo "$TOKEN" | sudo tee /etc/host-agent/bootstrap-token
// `echo` appends a trailing \n which must be stripped.
func TestReadBootstrapToken_File_PlainNewline(t *testing.T) {
	tok := "abc123rawtoken"
	writeTokenFile(t, tok+"\n")

	got, err := readBootstrapToken()
	if err != nil {
		t.Fatalf("readBootstrapToken: %v", err)
	}
	if got != tok {
		t.Errorf("got %q, want %q", got, tok)
	}
}

// TestReadBootstrapToken_File_CRLF covers tokens written from a Windows/Mac
// terminal or copied from a web UI where the clipboard appends \r\n.
func TestReadBootstrapToken_File_CRLF(t *testing.T) {
	tok := "abc123rawtoken"
	writeTokenFile(t, tok+"\r\n")

	got, err := readBootstrapToken()
	if err != nil {
		t.Fatalf("readBootstrapToken: %v", err)
	}
	if got != tok {
		t.Errorf("got %q, want %q", got, tok)
	}
}

// TestReadBootstrapToken_File_LeadingAndTrailingWhitespace covers tokens written
// with surrounding spaces or blank lines from a text editor.
func TestReadBootstrapToken_File_LeadingAndTrailingWhitespace(t *testing.T) {
	tok := "abc123rawtoken"
	writeTokenFile(t, "  \n"+tok+"\n  ")

	got, err := readBootstrapToken()
	if err != nil {
		t.Fatalf("readBootstrapToken: %v", err)
	}
	if got != tok {
		t.Errorf("got %q, want %q", got, tok)
	}
}

// TestReadBootstrapToken_File_NoNewline covers `printf '%s' "$TOKEN"` which
// writes the token with no trailing newline — also must work.
func TestReadBootstrapToken_File_NoNewline(t *testing.T) {
	tok := "abc123rawtoken"
	writeTokenFile(t, tok) // no newline

	got, err := readBootstrapToken()
	if err != nil {
		t.Fatalf("readBootstrapToken: %v", err)
	}
	if got != tok {
		t.Errorf("got %q, want %q", got, tok)
	}
}

// TestReadBootstrapToken_File_Empty verifies that an empty file returns an error
// (not an empty string sent to the CSR endpoint, which would produce a 401).
func TestReadBootstrapToken_File_Empty(t *testing.T) {
	writeTokenFile(t, "")

	_, err := readBootstrapToken()
	if err == nil {
		t.Fatal("expected error for empty token file, got nil")
	}
}

// TestReadBootstrapToken_File_WhitespaceOnly verifies whitespace-only content
// is treated as empty (not sent as a token, which would produce a 401).
func TestReadBootstrapToken_File_WhitespaceOnly(t *testing.T) {
	writeTokenFile(t, "   \n\r\n  ")

	_, err := readBootstrapToken()
	if err == nil {
		t.Fatal("expected error for whitespace-only token file, got nil")
	}
}

// TestReadBootstrapToken_File_Missing verifies that a missing file returns an
// error (not a panic), so the operator sees a clear message on startup.
func TestReadBootstrapToken_File_Missing(t *testing.T) {
	// Point tokenFile at a path that does not exist.
	origTokenFile := tokenFile
	tokenFile = "/tmp/does-not-exist-bootstrap-token-test"
	t.Cleanup(func() { tokenFile = origTokenFile })

	// Clear env so the file path is actually exercised.
	t.Setenv(tokenEnvVar, "")

	_, err := readBootstrapToken()
	if err == nil {
		t.Fatal("expected error for missing token file, got nil")
	}
}

// writeTokenFile writes content to a temp file and redirects the package-level
// tokenFile path to it for the duration of the test.
func writeTokenFile(t *testing.T, content string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap-token")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}

	origTokenFile := tokenFile
	tokenFile = path
	t.Cleanup(func() { tokenFile = origTokenFile })

	// Also clear env var so the file path is exercised, not the env path.
	t.Setenv(tokenEnvVar, "")
}
