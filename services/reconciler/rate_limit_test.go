package reconciler

// rate_limit_test.go — Unit tests for the repair job rate limiter.
//
// All tests use the internal allowAt(instanceID, now) method to control the
// clock deterministically. No goroutines, no real time.Sleep.
//
// Coverage required by M4 gate:
//   - Normal low-frequency repairs pass through
//   - Repeated drift over threshold gets suppressed
//   - Window expiry resets the count (tokens refresh)
//   - Per-instance isolation: one instance's limit does not affect another

import (
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newTestLimiter returns a limiter with a 5-minute window and max=3,
// matching defaults but using the internal constructor so tests remain stable
// if defaults change.
func newTestLimiter() *RateLimiter {
	return newRateLimiterWithParams(5*time.Minute, 3)
}

var (
	t0 = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(1 * time.Minute)
	t2 = t0.Add(2 * time.Minute)
	t3 = t0.Add(3 * time.Minute)
	t4 = t0.Add(4 * time.Minute)
	// t5 is outside the 5-minute window from t0.
	t5 = t0.Add(5*time.Minute + 1*time.Second)
	t6 = t0.Add(6 * time.Minute)
)

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestRateLimiter_AllowsNormalLowFrequencyRepairs verifies that repairs below
// the threshold all succeed.
// Source: 03-03 §Cascading Failures (normal low-frequency repair still dispatches).
func TestRateLimiter_AllowsNormalLowFrequencyRepairs(t *testing.T) {
	rl := newTestLimiter()
	inst := "inst-rl-001"

	// 3 repairs spaced out — all under the max=3 limit.
	if !rl.allowAt(inst, t0) {
		t.Error("first repair: expected Allow=true")
	}
	if !rl.allowAt(inst, t1) {
		t.Error("second repair: expected Allow=true")
	}
	if !rl.allowAt(inst, t2) {
		t.Error("third repair: expected Allow=true")
	}
}

// TestRateLimiter_BlocksWhenThresholdExceeded verifies that the (max+1)th repair
// within the window is suppressed.
// Source: 03-03 §Cascading Failures "rate limiting on repair job creation".
func TestRateLimiter_BlocksWhenThresholdExceeded(t *testing.T) {
	rl := newTestLimiter() // max=3
	inst := "inst-rl-002"

	rl.allowAt(inst, t0)
	rl.allowAt(inst, t1)
	rl.allowAt(inst, t2)

	// 4th repair within the window must be blocked.
	if rl.allowAt(inst, t3) {
		t.Error("4th repair within window: expected Allow=false (rate limit exceeded)")
	}
}

// TestRateLimiter_WindowExpiryResetsCount verifies that once timestamps fall
// outside the window they are pruned and new repairs are allowed.
func TestRateLimiter_WindowExpiryResetsCount(t *testing.T) {
	rl := newTestLimiter() // window=5min, max=3
	inst := "inst-rl-003"

	// Fill the bucket at t0.
	rl.allowAt(inst, t0)
	rl.allowAt(inst, t0)
	rl.allowAt(inst, t0)

	// At t5 (just past the 5-min window from t0), all t0 entries are expired.
	if !rl.allowAt(inst, t5) {
		t.Error("after window expiry: expected Allow=true (old entries pruned)")
	}
}

// TestRateLimiter_InstanceIsolation verifies that one instance's limit does not
// affect another instance.
func TestRateLimiter_InstanceIsolation(t *testing.T) {
	rl := newTestLimiter() // max=3
	instA := "inst-rl-004a"
	instB := "inst-rl-004b"

	// Exhaust instA's limit.
	rl.allowAt(instA, t0)
	rl.allowAt(instA, t1)
	rl.allowAt(instA, t2)
	if rl.allowAt(instA, t3) {
		t.Error("instA 4th repair: expected blocked")
	}

	// instB should still be fully open.
	if !rl.allowAt(instB, t0) {
		t.Error("instB first repair: expected Allow=true (independent bucket)")
	}
}

// TestRateLimiter_ExactlyAtMax_LastAllowed verifies the boundary: the max-th
// call is allowed, the (max+1)-th is not.
func TestRateLimiter_ExactlyAtMax_LastAllowed(t *testing.T) {
	rl := newRateLimiterWithParams(5*time.Minute, 1) // max=1
	inst := "inst-rl-005"

	if !rl.allowAt(inst, t0) {
		t.Error("1st repair with max=1: expected Allow=true")
	}
	if rl.allowAt(inst, t1) {
		t.Error("2nd repair with max=1: expected Allow=false")
	}
}

// TestRateLimiter_PartialWindowExpiry verifies that only expired entries are
// pruned — entries within the window still count toward the limit.
func TestRateLimiter_PartialWindowExpiry(t *testing.T) {
	rl := newTestLimiter() // window=5min, max=3
	inst := "inst-rl-006"

	// Place two repairs at t0 (will expire at t5+).
	rl.allowAt(inst, t0)
	rl.allowAt(inst, t0)
	// Place one repair at t4 (still within window at t6: t6-t4 = 2min < 5min).
	rl.allowAt(inst, t4)

	// At t6: t0 entries are expired (t6-t0=6min>5min), but t4 entry is not.
	// Effective count = 1. max=3. Two more should be allowed.
	if !rl.allowAt(inst, t6) {
		t.Error("at t6 after partial expiry: 2nd slot should be open")
	}
	if !rl.allowAt(inst, t6) {
		t.Error("at t6 after partial expiry: 3rd slot should be open")
	}
	// Now at count=3 (t4 + two t6 entries) — next should be blocked.
	if rl.allowAt(inst, t6) {
		t.Error("at t6: limit reached, expected blocked")
	}
}

// TestRateLimiter_NewInstance_AlwaysAllowsFirstRepair verifies that an instance
// that has never been seen is immediately allowed.
func TestRateLimiter_NewInstance_AlwaysAllowsFirstRepair(t *testing.T) {
	rl := newTestLimiter()
	if !rl.allowAt("brand-new-instance", t0) {
		t.Error("first-ever repair for new instance: expected Allow=true")
	}
}
