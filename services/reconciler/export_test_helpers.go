package reconciler

// export_test_helpers.go — Exported accessors for integration test assertions.
//
// VM-P3C: These functions expose package-internal constants to integration tests
// that live outside the reconciler package (test/integration/...).
//
// They are NOT part of the public API — only integration tests should call them.
//
// Source: IMPLEMENTATION_PLAN_V1 §R-07 "periodic resync every 5 minutes" —
//         the resync interval is non-negotiable and must be verifiable by
//         tests that run in the integration package.

import "time"

// ResyncIntervalForTest returns the reconciler's periodic resync interval.
// Used by integration tests to assert the R-07 non-negotiable requirement
// without coupling the test to the internal constant name.
// Source: IMPLEMENTATION_PLAN_V1 §R-07.
func ResyncIntervalForTest() time.Duration {
	return resyncInterval
}
