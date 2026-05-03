package runtime

// runtime_fake.go — Fake VMRuntime implementation for deterministic tests.
//
// The FakeRuntime implements VMRuntime without executing any real hypervisor.
// It records all operations and allows tests to pre-configure return values
// and errors. Every lifecycle operation is traced in the Calls slice.
//
// Production code MUST NOT use FakeRuntime unless the backend is explicitly
// configured to "fake" (e.g. VM_PLATFORM_RUNTIME=fake for development).

import (
	"context"
	"fmt"
	"sync"
)

// Ensure FakeRuntime implements VMRuntime.
var _ VMRuntime = (*FakeRuntime)(nil)

// FakeRuntimeCall records a single lifecycle operation invocation.
type FakeRuntimeCall struct {
	Op         string // "Create", "Start", "Stop", "Reboot", "Delete", "Inspect", "List"
	InstanceID string
}

// FakeRuntime is a deterministic test implementation of VMRuntime.
// All operations are recorded in Calls. No real processes are launched.
type FakeRuntime struct {
	mu    sync.Mutex
	Calls []FakeRuntimeCall

	DataRootDir string

	// KnownInstances maps instanceID → RuntimeInfo for Inspect/List.
	KnownInstances map[string]RuntimeInfo

	// Errors maps operation → error to return, keyed by instanceID.
	// Key format: "op:instanceID" (e.g. "Create:inst-001").
	// A key of "op:*" matches any instanceID for that operation.
	Errors map[string]error

	// HostID is set by the caller to simulate a real host agent identity.
	HostID string
}

// NewFakeRuntime constructs a FakeRuntime with empty state.
func NewFakeRuntime() *FakeRuntime {
	return &FakeRuntime{
		DataRootDir:    "/tmp/vm-platform-fake",
		KnownInstances: make(map[string]RuntimeInfo),
		Errors:         make(map[string]error),
	}
}

func (f *FakeRuntime) DataRoot() string { return f.DataRootDir }

func (f *FakeRuntime) Create(_ context.Context, spec InstanceSpec) (*RuntimeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeRuntimeCall{Op: "Create", InstanceID: spec.InstanceID})
	if err := f.lookupError("Create", spec.InstanceID); err != nil {
		return nil, err
	}
	info := RuntimeInfo{
		InstanceID: spec.InstanceID,
		State:      "RUNNING",
		PID:        1000 + int32(len(f.KnownInstances)),
		DataDir:    f.DataRootDir + "/" + spec.InstanceID,
		HostID:     f.HostID,
		TapDevice:  spec.TapDevice,
		DiskPaths:  []string{spec.RootfsPath},
		SocketPath: f.DataRootDir + "/" + spec.InstanceID + "/firecracker.sock",
		LogPath:    f.DataRootDir + "/" + spec.InstanceID + "/console.log",
		CPUCores:   spec.CPUCores,
		MemoryMB:   spec.MemoryMB,
	}
	f.KnownInstances[spec.InstanceID] = info
	return &info, nil
}

func (f *FakeRuntime) Start(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeRuntimeCall{Op: "Start", InstanceID: instanceID})
	if err := f.lookupError("Start", instanceID); err != nil {
		return err
	}
	if info, ok := f.KnownInstances[instanceID]; ok {
		info.State = "RUNNING"
		f.KnownInstances[instanceID] = info
	}
	return nil
}

func (f *FakeRuntime) Stop(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeRuntimeCall{Op: "Stop", InstanceID: instanceID})
	if err := f.lookupError("Stop", instanceID); err != nil {
		return err
	}
	if info, ok := f.KnownInstances[instanceID]; ok {
		info.State = "STOPPED"
		info.PID = 0
		f.KnownInstances[instanceID] = info
	}
	return nil
}

func (f *FakeRuntime) Reboot(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeRuntimeCall{Op: "Reboot", InstanceID: instanceID})
	if err := f.lookupError("Reboot", instanceID); err != nil {
		return err
	}
	return nil
}

func (f *FakeRuntime) Delete(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeRuntimeCall{Op: "Delete", InstanceID: instanceID})
	if err := f.lookupError("Delete", instanceID); err != nil {
		return err
	}
	delete(f.KnownInstances, instanceID)
	return nil
}

func (f *FakeRuntime) Inspect(_ context.Context, instanceID string) (*RuntimeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeRuntimeCall{Op: "Inspect", InstanceID: instanceID})
	if err := f.lookupError("Inspect", instanceID); err != nil {
		return nil, err
	}
	if info, ok := f.KnownInstances[instanceID]; ok {
		c := info
		return &c, nil
	}
	return nil, fmt.Errorf("FakeRuntime: instance %q not found", instanceID)
}

func (f *FakeRuntime) List(ctx context.Context) ([]RuntimeInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, FakeRuntimeCall{Op: "List"})
	if err := f.lookupError("List", ""); err != nil {
		return nil, err
	}
	result := make([]RuntimeInfo, 0, len(f.KnownInstances))
	for _, info := range f.KnownInstances {
		result = append(result, info)
	}
	return result, nil
}

func (f *FakeRuntime) lookupError(op, instanceID string) error {
	if err, ok := f.Errors[op+":"+instanceID]; ok {
		return err
	}
	if err, ok := f.Errors[op+":*"]; ok {
		return err
	}
	return nil
}

// CallCount returns the number of recorded calls.
func (f *FakeRuntime) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Calls)
}

// LastCall returns the most recent call or nil.
func (f *FakeRuntime) LastCall() *FakeRuntimeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Calls) == 0 {
		return nil
	}
	return &f.Calls[len(f.Calls)-1]
}

// Reset clears all recorded calls and known instances (keeps error map).
func (f *FakeRuntime) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = nil
	f.KnownInstances = make(map[string]RuntimeInfo)
}
