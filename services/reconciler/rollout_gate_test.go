package reconciler

// rollout_gate_test.go — Unit tests for the rollout control gate.
//
// VM-P3C: Rollout Controls.
//
// Tests verify:
//   - Fresh gate is not paused
//   - Pause sets IsPaused() to true and records timestamp + reason
//   - Resume clears IsPaused() and clears timestamp and reason
//   - Pause is idempotent (repeated calls do not panic)
//   - Resume is idempotent (repeated calls do not panic)
//   - Status() returns consistent snapshot
//   - Concurrent Pause/Resume/IsPaused calls do not race (go -race)
//
// Source: VM_PHASE_ROADMAP §9 "bounded rollout controls".

import (
	"sync"
	"testing"
	"time"
)

// TestRolloutGate_FreshGate_IsNotPaused verifies the gate starts in the
// resumed (active) state so it does not block repair dispatch on startup.
func TestRolloutGate_FreshGate_IsNotPaused(t *testing.T) {
	g := NewRolloutGate()
	if g.IsPaused() {
		t.Error("fresh gate must not be paused")
	}
}

// TestRolloutGate_Pause_SetsPausedTrue verifies that Pause suppresses dispatch.
func TestRolloutGate_Pause_SetsPausedTrue(t *testing.T) {
	g := NewRolloutGate()
	g.Pause("upgrading worker v1.4.2")
	if !g.IsPaused() {
		t.Error("gate must be paused after Pause()")
	}
}

// TestRolloutGate_Resume_ClearsPaused verifies that Resume re-enables dispatch.
func TestRolloutGate_Resume_ClearsPaused(t *testing.T) {
	g := NewRolloutGate()
	g.Pause("test pause")
	g.Resume()
	if g.IsPaused() {
		t.Error("gate must not be paused after Resume()")
	}
}

// TestRolloutGate_Status_WhenPaused verifies the Status snapshot when paused.
func TestRolloutGate_Status_WhenPaused(t *testing.T) {
	g := NewRolloutGate()
	before := time.Now()
	g.Pause("schema migration running")

	s := g.Status()
	if !s.Paused {
		t.Error("status.Paused must be true when gate is paused")
	}
	if s.PausedAt == nil {
		t.Fatal("status.PausedAt must not be nil when paused")
	}
	if s.PausedAt.Before(before) {
		t.Errorf("status.PausedAt = %v, must not be before Pause() call at %v", *s.PausedAt, before)
	}
	if s.Reason != "schema migration running" {
		t.Errorf("status.Reason = %q, want %q", s.Reason, "schema migration running")
	}
}

// TestRolloutGate_Status_WhenResumed verifies the Status snapshot after resume.
func TestRolloutGate_Status_WhenResumed(t *testing.T) {
	g := NewRolloutGate()
	g.Pause("test")
	g.Resume()

	s := g.Status()
	if s.Paused {
		t.Error("status.Paused must be false after Resume()")
	}
	if s.PausedAt != nil {
		t.Errorf("status.PausedAt must be nil after Resume(), got %v", *s.PausedAt)
	}
	if s.Reason != "" {
		t.Errorf("status.Reason must be empty after Resume(), got %q", s.Reason)
	}
}

// TestRolloutGate_Pause_IsIdempotent verifies repeated Pause calls do not panic.
func TestRolloutGate_Pause_IsIdempotent(t *testing.T) {
	g := NewRolloutGate()
	g.Pause("first pause")
	g.Pause("second pause") // must not panic
	if !g.IsPaused() {
		t.Error("gate must remain paused after second Pause()")
	}
}

// TestRolloutGate_Resume_IsIdempotent verifies repeated Resume calls do not panic.
func TestRolloutGate_Resume_IsIdempotent(t *testing.T) {
	g := NewRolloutGate()
	g.Resume()
	g.Resume() // must not panic
	if g.IsPaused() {
		t.Error("gate must remain resumed after second Resume()")
	}
}

// TestRolloutGate_PauseResumeCycle verifies a full pause→resume cycle.
func TestRolloutGate_PauseResumeCycle(t *testing.T) {
	g := NewRolloutGate()

	if g.IsPaused() {
		t.Fatal("precondition: gate must start resumed")
	}
	g.Pause("rollout v2")
	if !g.IsPaused() {
		t.Error("after Pause: must be paused")
	}
	g.Resume()
	if g.IsPaused() {
		t.Error("after Resume: must not be paused")
	}
	// Second cycle.
	g.Pause("rollout v3")
	if !g.IsPaused() {
		t.Error("second Pause: must be paused")
	}
	g.Resume()
	if g.IsPaused() {
		t.Error("second Resume: must not be paused")
	}
}

// TestRolloutGate_Concurrent_NoRace verifies the gate is race-free under
// concurrent access. Run with: go test -race ./services/reconciler/...
func TestRolloutGate_Concurrent_NoRace(t *testing.T) {
	g := NewRolloutGate()
	var wg sync.WaitGroup
	const goroutines = 20

	for i := 0; i < goroutines; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			g.Pause("concurrent-test")
		}()
		go func() {
			defer wg.Done()
			g.Resume()
		}()
		go func() {
			defer wg.Done()
			_ = g.IsPaused()
			_ = g.Status()
		}()
	}
	wg.Wait()
	// No assertion — the test passes if there is no race or panic.
}

// TestRolloutGate_Status_FreshGate verifies the Status snapshot of a fresh gate.
func TestRolloutGate_Status_FreshGate(t *testing.T) {
	g := NewRolloutGate()
	s := g.Status()
	if s.Paused {
		t.Error("fresh gate status.Paused must be false")
	}
	if s.PausedAt != nil {
		t.Errorf("fresh gate status.PausedAt must be nil, got %v", *s.PausedAt)
	}
	if s.Reason != "" {
		t.Errorf("fresh gate status.Reason must be empty, got %q", s.Reason)
	}
}
