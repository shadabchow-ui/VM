package runtime

// executor.go — Command execution abstraction for testability.
//
// The Executor interface hides os/exec so that unit tests can assert on
// generated commands without requiring ip(8), iptables(8), or root on the
// test machine. The real executor delegates to exec.CommandContext; the fake
// executor records calls and returns configurable errors for deterministic tests.
//
// Source: VM Job 3 — executor abstraction for host-agent networking.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Executor abstracts shell command execution so that host-agent networking
// code can be tested without real ip(8) or iptables(8) binaries.
type Executor interface {
	// Run executes name with the given args and returns an error on failure.
	Run(ctx context.Context, name string, args ...string) error

	// RunOutput executes the command and returns combined stdout+stderr.
	// Used by callers that need to parse command output (e.g. ip link show).
	RunOutput(ctx context.Context, name string, args ...string) (string, error)
}

// RealExecutor runs commands via os/exec. This is the production implementation.
type RealExecutor struct{}

// NewRealExecutor constructs the production executor.
func NewRealExecutor() *RealExecutor {
	return &RealExecutor{}
}

// Run executes the command; returns error with combined output on failure.
func (e *RealExecutor) Run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cmd %s %s: %w\noutput: %s",
			name, strings.Join(args, " "), err, string(out))
	}
	return nil
}

// RunOutput returns the combined stdout+stderr of the command.
func (e *RealExecutor) RunOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("cmd %s %s: %w\noutput: %s",
			name, strings.Join(args, " "), err, string(out))
	}
	return string(out), nil
}

// FakeExecutor records command invocations for test assertions.
// Each call is appended to Calls. Pre-configured errors allow simulating
// command failures without root or Linux kernel support.
type FakeExecutor struct {
	mu    sync.Mutex
	Calls []FakeCall

	// RunErrors maps a command signature → error to return.
	// Key format: "name arg0 arg1 ..." (space-separated).
	RunErrors map[string]error

	// RunOutputs maps a command signature → (output, error) pair.
	RunOutputs map[string]fakeOutputResult
}

type fakeOutputResult struct {
	Output string
	Err    error
}

// FakeCall is a recorded command invocation.
type FakeCall struct {
	Name string
	Args []string
}

// NewFakeExecutor constructs a FakeExecutor with empty maps.
func NewFakeExecutor() *FakeExecutor {
	return &FakeExecutor{
		RunErrors:  make(map[string]error),
		RunOutputs: make(map[string]fakeOutputResult),
	}
}

func (f *FakeExecutor) Run(_ context.Context, name string, args ...string) error {
	key := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{Name: name, Args: append([]string{}, args...)})
	if err, ok := f.RunErrors[key]; ok {
		f.mu.Unlock()
		return err
	}
	f.mu.Unlock()
	return nil
}

func (f *FakeExecutor) RunOutput(_ context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{Name: name, Args: append([]string{}, args...)})
	if res, ok := f.RunOutputs[key]; ok {
		f.mu.Unlock()
		return res.Output, res.Err
	}
	f.mu.Unlock()
	return "", nil
}

// LastCall returns the most recently recorded call or nil.
func (f *FakeExecutor) LastCall() *FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Calls) == 0 {
		return nil
	}
	return &f.Calls[len(f.Calls)-1]
}

// CallCount returns the number of recorded calls.
func (f *FakeExecutor) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// Reset clears call history (keeps error maps).
func (f *FakeExecutor) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = nil
}
