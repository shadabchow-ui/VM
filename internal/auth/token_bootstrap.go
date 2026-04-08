package auth

// token_bootstrap.go — Bootstrap token lifecycle for Host Agent mTLS issuance.
//
// The token generation (IssueBootstrapToken) and consumption (ConsumeBootstrapToken)
// are implemented in:
//   - internal/db/db.go         (DB persistence: InsertBootstrapToken, ConsumeBootstrapToken)
//   - services/resource-manager/inventory.go (service layer: IssueBootstrapToken)
//   - tools/internal-cli/commands.go         (operator tooling: issue-bootstrap-token command)
//
// Source: AUTH_OWNERSHIP_MODEL_V1 §6, 05-02-host-runtime-worker-design.md §Bootstrap.
//
// Token properties (enforced at DB layer):
//   - Stored as SHA-256(raw_token) — raw token never persisted.
//   - 1-hour validity window.
//   - Single-use: consumed atomically via UPDATE SET used=TRUE WHERE used=FALSE.
//   - One pending token per host (ON CONFLICT (host_id) replaces previous token).
