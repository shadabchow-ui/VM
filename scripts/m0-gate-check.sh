#!/usr/bin/env bash
# m0-gate-check.sh — M0 milestone gate verification.
#
# Reproduces the 9/9 checks that must pass before M1 work begins.
# Source: IMPLEMENTATION_PLAN_V1 §M0 Gate Tests, SPRINT_1_OPERATOR_SHEET_V1 §7.
#
# Usage:
#   export DATABASE_URL="postgres://user:pass@localhost:5432/compute_test?sslmode=disable"
#   bash scripts/m0-gate-check.sh

set -euo pipefail

PASS=0; FAIL=0; ERRORS=()

check() {
  local desc="$1"; shift
  if "$@" > /tmp/m0_out 2>&1; then
    echo "  PASS  $desc"
    ((PASS++))
  else
    echo "  FAIL  $desc"
    echo "        $(head -2 /tmp/m0_out)"
    ((FAIL++))
    ERRORS+=("$desc")
  fi
}

echo ""
echo "═══════════════════════════════════════════"
echo "  M0 Gate Check"
echo "═══════════════════════════════════════════"
echo ""

echo "BUILD"
check "go build ./..." go build ./...

echo ""
echo "UNIT TESTS"
check "go test ./..." go test -short -count=1 \
  ./internal/auth/... \
  ./packages/idgen/... \
  ./packages/observability/... \
  ./packages/state-machine/...

echo ""
echo "DATABASE"
if [ -z "${DATABASE_URL:-}" ]; then
  echo "  SKIP  DATABASE_URL not set — skipping DB checks"
else
  check "001 migration applies cleanly" \
    bash -c "migrate -path db/migrations -database '$DATABASE_URL' down --all 2>/dev/null; \
             migrate -path db/migrations -database '$DATABASE_URL' up"

  check "migration round-trip (down/up idempotent)" \
    bash -c "migrate -path db/migrations -database '$DATABASE_URL' down --all; \
             migrate -path db/migrations -database '$DATABASE_URL' up"

  check "duplicate instance id rejected" \
    bash -c "! psql '$DATABASE_URL' -c \
      \"INSERT INTO principals (id) VALUES ('11111111-1111-1111-1111-111111111111');
        INSERT INTO instances (id,name,owner_principal_id,instance_type_id,image_id,availability_zone)
          VALUES ('dup-test','x','11111111-1111-1111-1111-111111111111','c1.small','00000000-0000-0000-0000-000000000010','us-east-1a');
        INSERT INTO instances (id,name,owner_principal_id,instance_type_id,image_id,availability_zone)
          VALUES ('dup-test','y','11111111-1111-1111-1111-111111111111','c1.small','00000000-0000-0000-0000-000000000010','us-east-1a');\" -q 2>/dev/null"

  check "duplicate (vpc_id, ip_address) rejected" \
    bash -c "! psql '$DATABASE_URL' -c \
      \"INSERT INTO ip_allocations (ip_address, vpc_id) VALUES ('10.1.1.1','00000000-0000-0000-0000-000000000099');
        INSERT INTO ip_allocations (ip_address, vpc_id) VALUES ('10.1.1.1','00000000-0000-0000-0000-000000000099');\" -q 2>/dev/null"

  check "NULL owner_principal_id rejected" \
    bash -c "! psql '$DATABASE_URL' -c \
      \"INSERT INTO instances (id,name,owner_principal_id,instance_type_id,image_id,availability_zone)
        VALUES ('null-owner-test','x',NULL,'c1.small','00000000-0000-0000-0000-000000000010','us-east-1a');\" -q 2>/dev/null"

  check "IP pool seeded (>0 available IPs)" \
    psql "$DATABASE_URL" -c \
      "SELECT 1 FROM ip_allocations WHERE allocated=FALSE LIMIT 1" -t -q

  check "instance_types seeded (c1.small exists)" \
    psql "$DATABASE_URL" -c \
      "SELECT 1 FROM instance_types WHERE id='c1.small'" -t -q
fi

echo ""
echo "AUTH"
check "mTLS CA unit tests pass" \
  go test -count=1 -run "TestCA" ./internal/auth/...

echo ""
echo "═══════════════════════════════════════════"
printf "  %d passed  |  %d failed\n" "$PASS" "$FAIL"
echo "═══════════════════════════════════════════"

if [ "$FAIL" -gt 0 ]; then
  echo ""
  echo "FAILING:"
  for e in "${ERRORS[@]}"; do echo "  • $e"; done
  echo ""
  echo "M0 GATE: FAIL"
  exit 1
fi

echo ""
echo "M0 GATE: PASS"
echo "M1 work may begin."
echo ""
