package reconciler

// rate_limit.go — Repair job rate limiter.
//
// Prevents thundering herd when systemic drift causes many instances to require
// repair simultaneously (e.g., a bad host-level patch causes mass reboot drift).
//
// Design: a simple sliding-window token counter per instance.
//   - Each instance may have at most maxRepairsPerWindow repair jobs dispatched
//     within windowDuration.
//   - Entries older than the window are expired on each Allow() call.
//   - State is in-memory. Reconciler restart resets all counts — acceptable
//     for Phase 1 since it only delays, never permanently blocks, repairs.
//
// Source: 03-03-reconciliation-loops §Cascading Failures (rate limiting on
//         repair job creation),
//         IMPLEMENTATION_PLAN_V1 §WS-3 (rate limiting output),
//         12-03-risks-and-phase-2-expansion §Thundering herd.

import (
	"sync"
	"time"
)

const (
	// defaultWindowDuration is the sliding window for counting repairs per instance.
	defaultWindowDuration = 5 * time.Minute
	// defaultMaxRepairsPerWindow is the maximum repairs allowed per instance per window.
	defaultMaxRepairsPerWindow = 3
)

// RateLimiter is a per-instance sliding-window repair rate limiter.
// Safe for concurrent use.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time // instanceID → timestamps of recent repairs
	window  time.Duration
	max     int
}

// NewRateLimiter creates a RateLimiter with default parameters.
func NewRateLimiter() *RateLimiter {
	return newRateLimiterWithParams(defaultWindowDuration, defaultMaxRepairsPerWindow)
}

// newRateLimiterWithParams creates a RateLimiter with explicit parameters.
// Used by tests to control window and threshold.
func newRateLimiterWithParams(window time.Duration, max int) *RateLimiter {
	return &RateLimiter{
		buckets: make(map[string][]time.Time),
		window:  window,
		max:     max,
	}
}

// Allow returns true and records a repair timestamp if the instance is within
// its rate limit. Returns false without recording if the limit is exceeded.
// Expired timestamps are pruned on every call.
func (r *RateLimiter) Allow(instanceID string) bool {
	return r.allowAt(instanceID, time.Now())
}

// allowAt is the testable core of Allow with a clock parameter.
func (r *RateLimiter) allowAt(instanceID string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	cutoff := now.Add(-r.window)

	// Expire old entries for this instance.
	current := r.buckets[instanceID]
	valid := current[:0]
	for _, ts := range current {
		if ts.After(cutoff) {
			valid = append(valid, ts)
		}
	}

	if len(valid) >= r.max {
		// Limit exceeded — do not record.
		r.buckets[instanceID] = valid
		return false
	}

	// Within limit — record this repair and allow.
	r.buckets[instanceID] = append(valid, now)
	return true
}
