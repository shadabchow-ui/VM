package statemachine_test

// validator_test.go — State machine transition matrix tests.
//
// Source: IMPLEMENTATION_PLAN_V1 §19 (full unit test suite: 9 states × 5 actions).
// Every legal transition must return the correct next state.
// Every illegal transition must return ErrInvalidTransition.

import (
	"errors"
	"testing"

	sm "github.com/compute-platform/compute-platform/packages/state-machine"
)

// legalTransitions is the expected (state, action) → next_state mapping.
// Source: LIFECYCLE_STATE_MACHINE_V1.
var legalTransitions = []struct {
	from   sm.State
	action sm.Action
	want   sm.State
}{
	{sm.StateRequested, sm.ActionCreate, sm.StateProvisioning},
	{sm.StateRequested, sm.ActionDelete, sm.StateDeleting},
	{sm.StateProvisioning, sm.ActionDelete, sm.StateDeleting},
	{sm.StateRunning, sm.ActionStop, sm.StateStopping},
	{sm.StateRunning, sm.ActionReboot, sm.StateRebooting},
	{sm.StateRunning, sm.ActionDelete, sm.StateDeleting},
	{sm.StateStopped, sm.ActionStart, sm.StateStarting},
	{sm.StateStopped, sm.ActionDelete, sm.StateDeleting},
	{sm.StateFailed, sm.ActionDelete, sm.StateDeleting},
}

// illegalTransitions are (state, action) pairs that must return ErrInvalidTransition.
var illegalTransitions = []struct {
	from   sm.State
	action sm.Action
}{
	// Can't start/stop/reboot from requested
	{sm.StateRequested, sm.ActionStart},
	{sm.StateRequested, sm.ActionStop},
	{sm.StateRequested, sm.ActionReboot},
	// Provisioning: no start/stop/reboot
	{sm.StateProvisioning, sm.ActionCreate},
	{sm.StateProvisioning, sm.ActionStart},
	{sm.StateProvisioning, sm.ActionStop},
	{sm.StateProvisioning, sm.ActionReboot},
	// Running: no create/start
	{sm.StateRunning, sm.ActionCreate},
	{sm.StateRunning, sm.ActionStart},
	// Stopping: nothing permitted
	{sm.StateStopping, sm.ActionCreate},
	{sm.StateStopping, sm.ActionStart},
	{sm.StateStopping, sm.ActionStop},
	{sm.StateStopping, sm.ActionReboot},
	{sm.StateStopping, sm.ActionDelete},
	// Stopped: no create/stop/reboot
	{sm.StateStopped, sm.ActionCreate},
	{sm.StateStopped, sm.ActionStop},
	{sm.StateStopped, sm.ActionReboot},
	// Starting: nothing permitted
	{sm.StateStarting, sm.ActionCreate},
	{sm.StateStarting, sm.ActionStart},
	{sm.StateStarting, sm.ActionStop},
	{sm.StateStarting, sm.ActionReboot},
	{sm.StateStarting, sm.ActionDelete},
	// Rebooting: nothing permitted
	{sm.StateRebooting, sm.ActionCreate},
	{sm.StateRebooting, sm.ActionStart},
	{sm.StateRebooting, sm.ActionStop},
	{sm.StateRebooting, sm.ActionReboot},
	{sm.StateRebooting, sm.ActionDelete},
	// Deleting: nothing permitted
	{sm.StateDeleting, sm.ActionCreate},
	{sm.StateDeleting, sm.ActionStart},
	{sm.StateDeleting, sm.ActionStop},
	{sm.StateDeleting, sm.ActionReboot},
	{sm.StateDeleting, sm.ActionDelete},
	// Deleted: terminal — nothing permitted
	{sm.StateDeleted, sm.ActionCreate},
	{sm.StateDeleted, sm.ActionStart},
	{sm.StateDeleted, sm.ActionStop},
	{sm.StateDeleted, sm.ActionReboot},
	{sm.StateDeleted, sm.ActionDelete},
	// Failed: only delete permitted
	{sm.StateFailed, sm.ActionCreate},
	{sm.StateFailed, sm.ActionStart},
	{sm.StateFailed, sm.ActionStop},
	{sm.StateFailed, sm.ActionReboot},
}

func TestTransition_LegalTransitions(t *testing.T) {
	for _, tc := range legalTransitions {
		t.Run(string(tc.from)+"_"+string(tc.action), func(t *testing.T) {
			got, err := sm.Transition(tc.from, tc.action)
			if err != nil {
				t.Fatalf("Transition(%q, %q): unexpected error: %v", tc.from, tc.action, err)
			}
			if got != tc.want {
				t.Errorf("Transition(%q, %q) = %q, want %q", tc.from, tc.action, got, tc.want)
			}
		})
	}
}

func TestTransition_IllegalTransitions(t *testing.T) {
	for _, tc := range illegalTransitions {
		t.Run(string(tc.from)+"_"+string(tc.action), func(t *testing.T) {
			_, err := sm.Transition(tc.from, tc.action)
			if err == nil {
				t.Fatalf("Transition(%q, %q): expected ErrInvalidTransition, got nil", tc.from, tc.action)
			}
			if !errors.Is(err, sm.ErrInvalidTransition) {
				t.Errorf("Transition(%q, %q): expected ErrInvalidTransition, got %v", tc.from, tc.action, err)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	if !sm.IsTerminal(sm.StateDeleted) {
		t.Error("StateDeleted should be terminal")
	}
	for _, s := range sm.AllStates() {
		if s == sm.StateDeleted {
			continue
		}
		if sm.IsTerminal(s) {
			t.Errorf("State %q should not be terminal", s)
		}
	}
}

func TestIsTransitional(t *testing.T) {
	transitional := map[sm.State]bool{
		sm.StateProvisioning: true,
		sm.StateStopping:     true,
		sm.StateStarting:     true,
		sm.StateRebooting:    true,
		sm.StateDeleting:     true,
	}
	for _, s := range sm.AllStates() {
		got := sm.IsTransitional(s)
		want := transitional[s]
		if got != want {
			t.Errorf("IsTransitional(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestAllStates_Count(t *testing.T) {
	// Source: LIFECYCLE_STATE_MACHINE_V1 — exactly 10 states (9 + failed).
	states := sm.AllStates()
	if len(states) != 10 {
		t.Errorf("AllStates() returned %d states, want 10", len(states))
	}
}

func TestAllActions_Count(t *testing.T) {
	// Source: LIFECYCLE_STATE_MACHINE_V1 — exactly 5 actions.
	actions := sm.AllActions()
	if len(actions) != 5 {
		t.Errorf("AllActions() returned %d actions, want 5", len(actions))
	}
}
