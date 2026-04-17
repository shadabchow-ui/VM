package reconciler

// rollout_gate.go — Rollout control gate for repair dispatch.
//
// VM-P3C: Rollout Controls.
//
// RolloutGate provides a bounded, operator-controlled pause/resume mechanism
// for the reconciler's repair dispatch path. During a risky rollout (e.g. a
// worker binary upgrade, a DB schema migration, or a host-agent patch), an
// operator can pause repair dispatch so the reconciler does not create new
// repair jobs that the half-upgraded fleet cannot safely process.
//
// Design principles:
//   - The gate is a simple atomic boolean — no new orchestration system.
//   - Pausing does NOT stop the classifier or the periodic resync.
//     Instances continue to be scanned and drift detected; only dispatch is
//     suppressed. The next resync after resume will catch any accumulated drift.
//   - Pausing does NOT terminate running VMs or cancel in-flight jobs.
//     It only prevents NEW repair jobs from being inserted.
//   - The gate is in-memory. A process restart clears it (paused → resumed).
//     This is intentional: a restart is itself a rollout event that naturally
//     re-enables the gate.
//   - The gate is safe for concurrent use.
//
// Wire-up:
//   1. Construct: gate := NewRolloutGate()
//   2. Pass to Dispatcher: dispatcher.SetGate(gate)
//   3. Pass to HTTP handler: rolloutHandler{gate: gate}
//   4. Register routes via registerRolloutRoutes(mux, gate)
//
// Source: vm-16-03__blueprint__ §core_contracts "Asynchronous Operation Lifecycle",
//         VM_PHASE_ROADMAP §9 "deeper automation and rollout controls",
//         P2_M1_INFRASTRUCTURE_HARDENING_PLAN §3 (operator procedures during upgrades).

import (
	"sync/atomic"
	"time"
)

// RolloutGate controls whether the repair dispatcher may issue new repair jobs.
// Safe for concurrent use; all operations are lock-free.
type RolloutGate struct {
	// paused is 1 when dispatch is suppressed, 0 when active.
	paused atomic.Int32

	// pausedAt records when the gate was last paused (zero if not paused).
	// Stored as UnixNano via atomic; read non-atomically for observability only.
	pausedAtNano atomic.Int64

	// reason holds the operator-supplied pause reason (best-effort; not
	// guaranteed to be the exact current reason on concurrent updates).
	reason atomic.Value // stores string
}

// NewRolloutGate returns a RolloutGate in the resumed (active) state.
func NewRolloutGate() *RolloutGate {
	g := &RolloutGate{}
	g.reason.Store("")
	return g
}

// Pause suppresses repair dispatch. Idempotent.
// reason is a human-readable explanation stored for observability (e.g. "upgrading worker v1.4.2").
// Source: VM_PHASE_ROADMAP §9 "bounded rollout controls".
func (g *RolloutGate) Pause(reason string) {
	g.paused.Store(1)
	g.pausedAtNano.Store(time.Now().UnixNano())
	g.reason.Store(reason)
}

// Resume re-enables repair dispatch. Idempotent.
func (g *RolloutGate) Resume() {
	g.paused.Store(0)
	g.pausedAtNano.Store(0)
	g.reason.Store("")
}

// IsPaused reports whether dispatch is currently suppressed.
// Called by Dispatcher.Dispatch on every repair attempt.
func (g *RolloutGate) IsPaused() bool {
	return g.paused.Load() == 1
}

// Status returns a snapshot of the gate state for the operator status endpoint.
func (g *RolloutGate) Status() RolloutGateStatus {
	paused := g.paused.Load() == 1
	var pausedAt *time.Time
	if ns := g.pausedAtNano.Load(); ns != 0 {
		t := time.Unix(0, ns).UTC()
		pausedAt = &t
	}
	reason, _ := g.reason.Load().(string)
	return RolloutGateStatus{
		Paused:   paused,
		PausedAt: pausedAt,
		Reason:   reason,
	}
}

// RolloutGateStatus is the observable snapshot of the gate.
// Returned by the admin status endpoint.
type RolloutGateStatus struct {
	// Paused is true when repair dispatch is suppressed.
	Paused bool `json:"paused"`
	// PausedAt is the wall-clock time the gate was last paused. Nil when active.
	PausedAt *time.Time `json:"paused_at,omitempty"`
	// Reason is the operator-supplied pause reason. Empty when active.
	Reason string `json:"reason,omitempty"`
}
