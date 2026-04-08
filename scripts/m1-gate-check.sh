#!/usr/bin/env bash
# m1-gate-check.sh — M1 milestone gate verification.
#
# Run this after all M1 code is committed and DATABASE_URL points to a migrated DB.
# All checks must pass before M2 work begins.
#
# Source: IMPLEMENTATION_PLAN_V1 §M1 Gate Tests, §R-10 (milestones are gates).
#
# Usage:
#   export DATABASE_URL="postgres://user:pass@localhost:5432/compute?sslmode=disable"
#   bash scripts/m1-gate-check.sh

set -euo pipefail

PASS=0
FAIL=0
ERRORS=()

check() {
  local desc="$1"; shift
  if "$@" > /tmp/m1_gate_out 2>&1; then
    echo "  PASS  $desc"
    PASS=$((PASS+1))
  else
    echo "  FAIL  $desc"
    echo "        $(head -3 /tmp/m1_gate_out)"
    FAIL=$((FAIL+1))
    ERRORS+=("$desc")
  fi
}

echo ""
echo "═══════════════════════════════════════════"
echo "  M1 Gate Check"
echo "═══════════════════════════════════════════"
echo ""

# ── Build checks ──────────────────────────────────────────────────────────────
echo "BUILD"

check "go build ./..." \
  go build ./...

check "host-agent binary builds" \
  go build -o /tmp/m1-host-agent ./services/host-agent/

check "resource-manager binary builds" \
  go build -o /tmp/m1-resource-manager ./services/resource-manager/

check "scheduler binary builds" \
  go build -o /tmp/m1-scheduler ./services/scheduler/

echo ""

# ── Unit tests ────────────────────────────────────────────────────────────────
echo "UNIT TESTS"

check "packages unit tests pass" \
  go test -short -count=1 ./packages/... ./internal/...

check "auth CA unit tests pass" \
  go test -short -count=1 ./internal/auth/...

echo ""

# ── Database checks ───────────────────────────────────────────────────────────
echo "DATABASE"

if [ -z "${DATABASE_URL:-}" ]; then
  echo "  SKIP  DATABASE_URL not set — skipping DB checks"
else
  # Apply migrations via psql before checking — idempotent, no 'migrate' binary needed.
  # 002_hosts.up.sql uses IF NOT EXISTS throughout so re-applying is always safe.
  check "001 migration applies (idempotent)" \
    psql "$DATABASE_URL" -q -f db/migrations/001_initial.up.sql

  check "002 migration applies (idempotent)" \
    psql "$DATABASE_URL" -q -f db/migrations/002_hosts.up.sql

  check "002_hosts migration: hosts table exists" \
    psql "$DATABASE_URL" -c "SELECT 1 FROM hosts LIMIT 0" -q

  check "002_hosts migration: bootstrap_tokens table exists" \
    psql "$DATABASE_URL" -c "SELECT 1 FROM bootstrap_tokens LIMIT 0" -q

  check "hosts table: status constraint valid values" \
    psql "$DATABASE_URL" -c \
      "INSERT INTO hosts (id,availability_zone,status,total_cpu,total_memory_mb,total_disk_gb,agent_version) \
       VALUES ('gate-check-tmp','us-east-1a','ready',4,8192,100,'v0') \
       ON CONFLICT (id) DO UPDATE SET updated_at=NOW(); \
       DELETE FROM hosts WHERE id='gate-check-tmp';" -q

  check "hosts table: rejects invalid status" \
    bash -c "! psql '$DATABASE_URL' -c \
      \"INSERT INTO hosts (id,availability_zone,status,total_cpu,total_memory_mb,total_disk_gb,agent_version) \
        VALUES ('bad-status-check','us-east-1a','invalid_status',4,8192,100,'v0')\" -q 2>/dev/null"

  check "M1 integration tests" \
    go test -tags=integration -count=1 -timeout=60s \
      -run "TestHost|TestAuth|TestScheduler|TestMigration|TestHeartbeat" \
      ./test/integration/...
fi

echo ""

# ── Auth enforcement ──────────────────────────────────────────────────────────
echo "AUTH"

check "CA: NewCA generates valid cert" \
  go test -short -count=1 -run TestCA_ ./internal/auth/...

check "CA: SignCSR enforces CN prefix" \
  go test -short -count=1 -run TestCA_SignCSR ./internal/auth/...

check "CA: HostIDFromCert extracts host_id" \
  go test -short -count=1 -run TestCA_HostIDFromCert ./internal/auth/...

echo ""

# ── Result ────────────────────────────────────────────────────────────────────
echo "═══════════════════════════════════════════"
printf "  %d passed  |  %d failed\n" "$PASS" "$FAIL"
echo "═══════════════════════════════════════════"

if [ "$FAIL" -gt 0 ]; then
  echo ""
  echo "FAILING CHECKS:"
  for e in "${ERRORS[@]}"; do
    echo "  • $e"
  done
  echo ""
  echo "M1 GATE: FAIL — do not begin M2 work."
  exit 1
fi

echo ""
echo "M1 GATE: PASS"
echo "M2 work may begin. Next: implement Host Agent VM primitives (start_vm, stop_vm, delete_vm_storage)."
echo ""
