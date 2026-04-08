package auth

// gateway.go — Internal API gateway authentication scaffold.
//
// Full implementation target: M5 (API Contract Complete).
// In M1, all internal service-to-service auth is enforced via mTLS (see ca.go, middleware.go).
// SigV4 external request signing is M5 work.
//
// Source: IMPLEMENTATION_PLAN_V1 §A4 (internal API gateway, BLOCK 1 on external API).
// BLOCKED: Do not implement external auth middleware until M5 gate passes.
