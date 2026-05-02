package runtime

// console.go — Console log capture for VM instances.
//
// Captures the VM's serial console output to a log file under the instance's
// data directory. The console log is written by piping the hypervisor's serial
// output (e.g. QEMU's -serial file:path or Firecracker's logger) to a file.
//
// For Firecracker, the serial output is captured by the firecracker process
// itself via the --log-path flag (not yet implemented; Phase 2).
//
// For QEMU, the -serial flag can target a file directly, so the console
// capture is built into the QEMU command generation.
//
// Source: VM Job 2 — console log capture requirement.

import (
	"fmt"
	"os"
	"path/filepath"
)

// ConsoleLogger manages console log capture for VM instances.
type ConsoleLogger struct {
	artifacts *ArtifactManager
}

// NewConsoleLogger constructs a ConsoleLogger using the given ArtifactManager.
func NewConsoleLogger(artifacts *ArtifactManager) *ConsoleLogger {
	return &ConsoleLogger{artifacts: artifacts}
}

// ConsolePath returns the console log file path for an instance.
func (c *ConsoleLogger) ConsolePath(instanceID string) string {
	return c.artifacts.ConsolePath(instanceID)
}

// EnsureConsoleFile creates an empty console log file for the instance.
// Idempotent: returns nil if the file already exists.
func (c *ConsoleLogger) EnsureConsoleFile(instanceID string) error {
	path := c.ConsolePath(instanceID)
	if err := c.artifacts.ValidatePath(path); err != nil {
		return fmt.Errorf("EnsureConsoleFile: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("EnsureConsoleFile: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return fmt.Errorf("EnsureConsoleFile: %w", err)
	}
	return f.Close()
}

// ReadConsole reads the current contents of the console log for the instance.
// Returns an empty string if the file does not exist.
func (c *ConsoleLogger) ReadConsole(instanceID string) (string, error) {
	path := c.ConsolePath(instanceID)
	if err := c.artifacts.ValidatePath(path); err != nil {
		return "", fmt.Errorf("ReadConsole: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("ReadConsole: %w", err)
	}
	return string(data), nil
}
