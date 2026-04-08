#!/usr/bin/env bash
# scripts/m8-gate-check.sh — M8 Release Readiness Gate Check.
#
# Source: IMPLEMENTATION_PLAN_V1 §M8 exit criteria.
# Runs all M8 validation suites that execute without a real DB or KVM hardware.
# Prints a ✅/❌ summary and exits non-zero on any failure.
#
# Usage:
#   bash scripts/m8-gate-check.sh
#   make m8-gate
#
# With integration tests (requires DATABASE_URL):
#   DATABASE_URL=postgres://user:pass@host/db bash scripts/m8-gate-check.sh --with-integration
#   make m8-gate-full

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

WITH_INTEGRATION=false
for arg in "$@"; do
  [[ "$arg" == "--with-integration" ]] && WITH_INTEGRATION=true
done

PASS=0
FAIL=0
RESULTS=()

# run_check NAME CMD [ARGS...]
# Runs the command, records PASS or FAIL.
run_check() {
  local name="$1"
  shift
  printf "  ▶  %-70s" "$name"
  local out
  if out=$("$@" 2>&1); then
    printf "✅\n"
    RESULTS+=("✅  $name")
    ((PASS++)) || true
  else
    printf "❌\n"
    # Print the first 10 lines of failure output indented
    echo "$out" | head -10 | sed 's/^/       /'
    RESULTS+=("❌  $name")
    ((FAIL++)) || true
  fi
}

# check_file NAME PATH
# Verifies a required artifact file exists.
check_file() {
  local name="$1"
  local path="$2"
  printf "  ▶  %-70s" "$name"
  if [[ -f "$path" ]]; then
    printf "✅\n"
    RESULTS+=("✅  $name")
    ((PASS++)) || true
  else
    printf "❌\n"
    echo "       Missing: $path" >&2
    RESULTS+=("❌  $name — file not found: $path")
    ((FAIL++)) || true
  fi
}

echo ""
echo "════════════════════════════════════════════════════════════════════════"
echo "  M8 Release Gate Check"
echo "════════════════════════════════════════════════════════════════════════"
echo ""
echo "── Lifecycle Matrix ─────────────────────────────────────────────────────"

run_check "State machine: full 9×5 transition matrix" \
  go test -count=1 -short \
  -run "TestTransition|TestIsTerminal|TestIsTransitional|TestAllStates|TestAllActions" \
  ./packages/state-machine/...

run_check "Lifecycle sequences: create→stop→start→reboot→delete" \
  go test -count=1 -short \
  -run "TestLifecycle" \
  ./services/worker/handlers/...

run_check "Delete from running and stopped" \
  go test -count=1 -short \
  -run "TestLifecycle_Delete|TestDeleteHandler" \
  ./services/worker/handlers/...

echo ""
echo "── Failure Injection ────────────────────────────────────────────────────"

run_check "Create: no hosts → failed" \
  go test -count=1 -short \
  -run "TestCreateHandler_NoHosts" \
  ./services/worker/handlers/...

run_check "Create: IP failure → failed" \
  go test -count=1 -short \
  -run "TestCreateHandler_IPAllocFailure" \
  ./services/worker/handlers/...

run_check "Create: runtime failure → IP released, failed" \
  go test -count=1 -short \
  -run "TestCreateHandler_CreateInstanceFailure" \
  ./services/worker/handlers/...

run_check "Stop: runtime failure → stays in stopping" \
  go test -count=1 -short \
  -run "TestStopHandler_StopInstanceFailure|TestLifecycle_StopFailure" \
  ./services/worker/handlers/...

run_check "Start: runtime failure → failed" \
  go test -count=1 -short \
  -run "TestLifecycle_StartFailure|TestStartHandler_CreateInstanceFailure" \
  ./services/worker/handlers/...

run_check "Reboot: runtime failure → failed" \
  go test -count=1 -short \
  -run "TestLifecycle_RebootFailure|TestRebootHandler_CreateInstanceFailure" \
  ./services/worker/handlers/...

echo ""
echo "── Idempotency / Duplicate Delivery ─────────────────────────────────────"

run_check "Idempotency-Key: create + lifecycle (same/different/conflict)" \
  go test -count=1 -short \
  -run "TestIdempotency" \
  ./services/resource-manager/...

run_check "Duplicate queue delivery: re-entrant stop, create, delete no-op" \
  go test -count=1 -short \
  -run "TestStopHandler_DuplicateDelivery|TestCreateHandler_AlreadyProvisioning|TestDeleteHandler_AlreadyDeleted" \
  ./services/worker/handlers/...

echo ""
echo "── Reconciler / Timeout / Optimistic Lock ───────────────────────────────"

run_check "Reconciler: drift detection, repair jobs, no direct mutation" \
  go test -count=1 -short \
  ./reconciler/...

run_check "Janitor: stuck job scan and requeue" \
  go test -count=1 -short \
  -run "TestJanitor" \
  ./services/worker/...

run_check "Worker loop: routing, completed/failed/dead, error message" \
  go test -count=1 -short \
  -run "TestWorkerLoop" \
  ./services/worker/...

run_check "Optimistic lock: concurrent updates, stale version, wrong state, version increment" \
  go test -count=1 -short \
  -run "TestOptimisticLock" \
  ./services/worker/handlers/...

echo ""
echo "── Logging and Events ───────────────────────────────────────────────────"

run_check "Usage events: start/end written at correct lifecycle steps" \
  go test -count=1 -short \
  -run "TestCreateHandler_EventsWritten|TestStopHandler_UsageEnd|TestDeleteHandler_UsageEnd|TestLifecycle_UsageEvents" \
  ./services/worker/handlers/...

echo ""
echo "── Security and Secret Handling ─────────────────────────────────────────"

run_check "No private key material in logs or events (all handler paths)" \
  go test -count=1 -short \
  -run "TestSecretLeak" \
  ./services/worker/handlers/...

run_check "Auth: missing header → 401, ownership boundary → 404" \
  go test -count=1 -short \
  -run "TestAuth|TestOwnership" \
  ./services/resource-manager/...

echo ""
echo "── DB Repo Unit Tests ───────────────────────────────────────────────────"

run_check "DB repo: all SQL methods compile and pass unit tests" \
  go test -count=1 -short \
  ./internal/db/...

echo ""
echo "── Required Artifact Files ──────────────────────────────────────────────"

check_file "Failure injection outcomes doc" \
  "docs/m8-failure-injection-outcomes.md"

check_file "Release gate checklist" \
  "docs/M8_RELEASE_GATE_CHECKLIST.md"

echo ""
echo "── Build Verification ───────────────────────────────────────────────────"

run_check "All packages compile (go build ./...)" \
  go build ./...

# ── Integration tests (optional) ─────────────────────────────────────────────

if [[ "$WITH_INTEGRATION" == "true" ]]; then
  echo ""
  echo "── Integration Tests (require DATABASE_URL) ─────────────────────────────"
  if [[ -z "${DATABASE_URL:-}" ]]; then
    echo "  ⚠️  DATABASE_URL not set — skipping integration tests"
    RESULTS+=("⚠️  Integration tests skipped (DATABASE_URL not set)")
  else
    run_check "Integration: M1 host registration + heartbeat" \
      go test -tags=integration -count=1 -timeout=60s \
      -run "TestHost|TestAuth|TestHeartbeat|TestMigration" \
      ./test/integration/...

    run_check "Integration: M2 vertical slice (create→running→delete)" \
      go test -tags=integration -count=1 -timeout=120s \
      -run "TestM2" \
      ./test/integration/...
  fi
fi

# ── Summary ───────────────────────────────────────────────────────────────────

echo ""
echo "════════════════════════════════════════════════════════════════════════"
echo "  M8 Gate Results"
echo "════════════════════════════════════════════════════════════════════════"
for r in "${RESULTS[@]}"; do
  echo "  $r"
done
echo ""
TOTAL=$(( PASS + FAIL ))
echo "  Total: $TOTAL   Pass: $PASS   Fail: $FAIL"
echo "════════════════════════════════════════════════════════════════════════"

if [[ $FAIL -gt 0 ]]; then
  echo ""
  echo "  ❌  M8 GATE: FAIL"
  echo "      $FAIL check(s) did not pass. Release is BLOCKED."
  echo "      Fix all failures above before signing the release gate."
  exit 1
else
  echo ""
  echo "  ✅  M8 GATE: PASS"
  echo "      All $PASS checks passed."
  echo "      Proceed to release sign-off in docs/M8_RELEASE_GATE_CHECKLIST.md"
  exit 0
fi
