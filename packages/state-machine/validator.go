package statemachine

// validator.go — Instance lifecycle state machine transition validator.
//
// Source: LIFECYCLE_STATE_MACHINE_V1, INSTANCE_MODEL_V1 §3,
//         IMPLEMENTATION_PLAN_V1 §19 (state machine transition validator with 9×5 matrix).
//
// The validator is the single source of truth for legal state transitions.
// It is called by:
//   - The API handler (before enqueuing a job)
//   - The worker (before updating DB state)
//   - The reconciler (before corrective actions)
//
// No state mutation in the DB may bypass this validator. Source: §R-10, §invariant I-1.

import (
	"errors"
	"fmt"
)

// State is the canonical instance state enum.
// Source: LIFECYCLE_STATE_MACHINE_V1, INSTANCE_MODEL_V1 §3.
type State string

const (
	StateRequested    State = "requested"
	StateProvisioning State = "provisioning"
	StateRunning      State = "running"
	StateStopping     State = "stopping"
	StateStopped      State = "stopped"
	StateStarting     State = "starting"
	StateRebooting    State = "rebooting"
	StateDeleting     State = "deleting"
	StateDeleted      State = "deleted"
	StateFailed       State = "failed"
)

// Action is a lifecycle operation that triggers a state transition.
type Action string

const (
	ActionCreate  Action = "create"
	ActionStart   Action = "start"
	ActionStop    Action = "stop"
	ActionReboot  Action = "reboot"
	ActionDelete  Action = "delete"
)

// ErrInvalidTransition is returned when a (state, action) pair has no legal next state.
var ErrInvalidTransition = errors.New("invalid state transition")

// transitions is the authoritative 9-state × 5-action transition table.
// Source: LIFECYCLE_STATE_MACHINE_V1.
// A missing entry means the action is illegal in that state.
var transitions = map[State]map[Action]State{
	StateRequested: {
		ActionCreate: StateProvisioning,
		ActionDelete: StateDeleting,
	},
	StateProvisioning: {
		// Provisioning completes asynchronously — worker drives to running or failed.
		ActionDelete: StateDeleting,
	},
	StateRunning: {
		ActionStop:   StateStopping,
		ActionReboot: StateRebooting,
		ActionDelete: StateDeleting,
	},
	StateStopping: {
		// Stopping completes asynchronously — worker drives to stopped or failed.
	},
	StateStopped: {
		ActionStart:  StateStarting,
		ActionDelete: StateDeleting,
	},
	StateStarting: {
		// Starting completes asynchronously — worker drives to running or failed.
	},
	StateRebooting: {
		// Rebooting completes asynchronously — worker drives to running or failed.
	},
	StateDeleting: {
		// Deleting completes asynchronously — worker drives to deleted or failed.
	},
	StateDeleted: {
		// Terminal state — no transitions permitted.
	},
	StateFailed: {
		ActionDelete: StateDeleting,
	},
}

// Transition validates the (currentState, action) pair and returns the next state.
// Returns ErrInvalidTransition if the action is illegal in the current state.
// Source: LIFECYCLE_STATE_MACHINE_V1, IMPLEMENTATION_PLAN_V1 §invariant I-1.
func Transition(current State, action Action) (State, error) {
	actionMap, stateOK := transitions[current]
	if !stateOK {
		return "", fmt.Errorf("%w: unknown state %q", ErrInvalidTransition, current)
	}
	next, actionOK := actionMap[action]
	if !actionOK {
		return "", fmt.Errorf("%w: action %q is not permitted in state %q", ErrInvalidTransition, action, current)
	}
	return next, nil
}

// IsTerminal returns true if the state is a terminal state (no further transitions possible).
// Source: LIFECYCLE_STATE_MACHINE_V1.
func IsTerminal(s State) bool {
	return s == StateDeleted
}

// IsTransitional returns true if the state is a transient state driven by async worker.
func IsTransitional(s State) bool {
	switch s {
	case StateProvisioning, StateStopping, StateStarting, StateRebooting, StateDeleting:
		return true
	}
	return false
}

// AllStates returns all 9 canonical states.
// Used by tests to verify complete coverage. Source: IMPLEMENTATION_PLAN_V1 §19 (9×5 matrix).
func AllStates() []State {
	return []State{
		StateRequested, StateProvisioning, StateRunning,
		StateStopping, StateStopped, StateStarting,
		StateRebooting, StateDeleting, StateDeleted, StateFailed,
	}
}

// AllActions returns all 5 canonical actions.
func AllActions() []Action {
	return []Action{ActionCreate, ActionStart, ActionStop, ActionReboot, ActionDelete}
}
