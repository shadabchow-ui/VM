#!/bin/bash
# Phase L — Developer Validation Gate
# ===================================
# Runs all safety-checked validation gates for the developer/operator
# confidence layer. Safe by default — no DB, no Linux/KVM, no root required.
#
# Usage:
#   bash scripts/phase-l-gate-check.sh                 # unit gate only
#   DATABASE_URL=postgres://... bash scripts/phase-l-gate-check.sh --with-integration
#   bash scripts/phase-l-gate-check.sh --dry-run        # show what would run
#
# Gates:
#   1. Unit gate: go test all non-integration packages
#   2. Build gate: go build ./...
#   3. Resource-manager gate: go test ./services/resource-manager/...
#   4. Worker gate: go test ./services/worker/...
#   5. Reconciler gate: go test ./services/reconciler/...
#   6. DB gate: go test ./internal/db/...
#   7. Integration DB gate (opt-in, requires DATABASE_URL)
#   8. Runtime/KVM gate (opt-in, requires Linux+KVM+CAP_NET_ADMIN)
#   9. Docs check: verify required docs exist
#   10. API schema check: verify /v1/openapi.json is valid JSON

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BOLD='\033[1m'
NC='\033[0m'

PASS=0
FAIL=0
SKIP=0

DRY_RUN=false
WITH_INTEGRATION=false
WITH_RUNTIME=false

for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=true ;;
    --with-integration) WITH_INTEGRATION=true ;;
    --with-runtime) WITH_RUNTIME=true ;;
  esac
done

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$repo_root"

gate() {
  local name="$1"
  local cmd="$2"
  local opt_in="${3:-false}"

  if [ "$opt_in" = "true" ] && [ "$DRY_RUN" = false ]; then
    # Check if the opt-in flag was provided
    case "$name" in
      *Integration*) [ "$WITH_INTEGRATION" != true ] && return ;;
      *Runtime*|*KVM*) [ "$WITH_RUNTIME" != true ] && return ;;
    esac
  fi

  if [ "$DRY_RUN" = true ]; then
    echo -e "${YELLOW}[DRY-RUN]${NC} ${name}: ${cmd}"
    return
  fi

  echo -e "${BOLD}── ${name}${NC}"
  if eval "$cmd"; then
    echo -e "  ${GREEN}PASS${NC}"
    PASS=$((PASS + 1))
  else
    echo -e "  ${RED}FAIL${NC}"
    FAIL=$((FAIL + 1))
  fi
  echo
}

echo "============================================"
echo " Phase L — Developer Validation Gate"
echo " $(date -u +"%Y-%m-%dT%H:%M:%SZ")"
if [ "$DRY_RUN" = true ]; then
  echo " MODE: dry-run (no commands execute)"
else
  echo " MODE: live"
fi
echo "============================================"
echo

# ── Gate 1: Build ────────────────────────────────────────────────────────
gate "1. Build gate (go build ./...)" \
  "go build ./..."

# ── Gate 2: Unit tests (resource-manager) ─────────────────────────────────
gate "2. Resource manager unit gate" \
  "go test -count=1 -timeout=120s ./services/resource-manager/..."

# ── Gate 3: Worker handler tests ──────────────────────────────────────────
gate "3. Worker handlers gate" \
  "go test -count=1 -timeout=120s ./services/worker/handlers/..."

# ── Gate 4: Worker loop tests ─────────────────────────────────────────────
gate "4. Worker loop gate" \
  "go test -count=1 -timeout=30s ./services/worker/..."

# ── Gate 5: Reconciler tests ──────────────────────────────────────────────
gate "5. Reconciler gate" \
  "go test -count=1 -timeout=120s ./services/reconciler/..."

# ── Gate 5b: Reconciler (old path) ────────────────────────────────────────
if [ -d "reconciler" ]; then
  gate "5b. Reconciler (legacy path) gate" \
    "go test -count=1 -timeout=30s ./reconciler/..."
fi

# ── Gate 6: DB repo tests ─────────────────────────────────────────────────
gate "6. DB repo unit gate" \
  "go test -count=1 -short -timeout=60s ./internal/db/..."

# ── Gate 7: Auth tests ────────────────────────────────────────────────────
gate "7. Auth gate" \
  "go test -count=1 -timeout=30s ./internal/auth/..."

# ── Gate 8: Packages tests ────────────────────────────────────────────────
gate "8. Domain model + idgen + state machine gate" \
  "go test -count=1 -timeout=30s ./packages/..."


# ── Gate 9: Host agent runtime tests (no KVM required) ────────────────────
gate "9. Host agent runtime gate (dry-run, no KVM)" \
  "go test -count=1 -timeout=60s ./services/host-agent/runtime/... -run 'TestDry|TestUnit|TestFake|TestConfig' 2>&1 || go test -count=1 -short -timeout=60s ./services/host-agent/runtime/..."

# ── Gate 10: Integration DB gate (opt-in) ─────────────────────────────────
if [ "$WITH_INTEGRATION" = true ]; then
  if [ -z "${DATABASE_URL:-}" ]; then
    echo -e "${YELLOW}[SKIP]${NC} 10. Integration DB gate: DATABASE_URL not set"
    SKIP=$((SKIP + 1))
  else
    gate "10. Integration DB gate" \
      "go test -tags=integration -count=1 -timeout=300s ./test/integration/..." \
      "true"
  fi
else
  if [ "$DRY_RUN" = true ]; then
    echo -e "${YELLOW}[DRY-RUN]${NC} 10. Integration DB gate: bash scripts/phase-l-gate-check.sh --with-integration"
  else
    echo -e "${YELLOW}[SKIP]${NC} 10. Integration DB gate: use --with-integration and set DATABASE_URL"
    SKIP=$((SKIP + 1))
  fi
fi

# ── Gate 11: Runtime/KVM gate (opt-in, explicitly tagged) ─────────────────
if [ "$WITH_RUNTIME" = true ]; then
  echo -e "${YELLOW}[SKIP]${NC} 11. Runtime/KVM gate: requires Linux + KVM + CAP_NET_ADMIN"
  echo "  Run manually:"
  echo "    sudo NETWORK_DRY_RUN=false go test -v -count=1 ./services/host-agent/runtime/... -run TestPrivileged"
  echo "    sudo REALVM_E2E=1 NETWORK_DRY_RUN=false go test -v -count=1 -tags=e2e ./test/e2e/..."
  SKIP=$((SKIP + 1))
else
  if [ "$DRY_RUN" = true ]; then
    echo -e "${YELLOW}[DRY-RUN]${NC} 11. Runtime/KVM gate: opt-in only (--with-runtime)"
  else
    echo -e "${YELLOW}[SKIP]${NC} 11. Runtime/KVM gate: opt-in only (--with-runtime, requires Linux+KVM)"
    SKIP=$((SKIP + 1))
  fi
fi

# ── Gate 12: Docs presence check ──────────────────────────────────────────
echo -e "${BOLD}── 12. Docs presence check${NC}"
docs_ok=true
for f in \
  "README.md" \
  "docs/api/openapi.yaml" \
  "docs/guides/curl-golden-path.md" \
  "docs/guides/troubleshooting.md" \
  "docs/guides/operations-timeline.md" \
  "docs/contracts/API_ERROR_CONTRACT_V1.md" \
  "docs/contracts/AUTH_OWNERSHIP_MODEL_V1.md" \
  "docs/contracts/JOB_MODEL_V1.md" \
  "docs/contracts/EVENTS_SCHEMA_V1.md" \
  "docs/contracts/INSTANCE_MODEL_V1.md" \
  "docs/contracts/LIFECYCLE_STATE_MACHINE_V1.md"; do
  if [ -f "$repo_root/$f" ]; then
    echo -e "  ${GREEN}✓${NC} $f"
  else
    echo -e "  ${RED}✗${NC} $f MISSING"
    docs_ok=false
  fi
done
if [ "$docs_ok" = true ]; then
  echo -e "  ${GREEN}PASS${NC}"
  PASS=$((PASS + 1))
else
  echo -e "  ${RED}FAIL${NC}"
  FAIL=$((FAIL + 1))
fi
echo

# ── Gate 13: OpenAPI spec valid JSON ──────────────────────────────────────
echo -e "${BOLD}── 13. OpenAPI spec check${NC}"
# Verify the handler returns valid JSON structure by checking the Go test
# for the OpenAPI handler
if go test -count=1 -timeout=30s ./services/resource-manager/... -run TestHandleOpenAPI 2>/dev/null; then
  echo -e "  ${GREEN}PASS${NC} OpenAPI handler test passes"
  PASS=$((PASS + 1))
else
  echo -e "  ${RED}FAIL${NC} OpenAPI handler test failed"
  FAIL=$((FAIL + 1))
fi
echo

# ══════════════════════════════════════════════════════════════════════════
echo "============================================"
echo " Phase L Gate Summary"
echo "============================================"
echo -e "  ${GREEN}PASS:${NC} $PASS"
echo -e "  ${RED}FAIL:${NC} $FAIL"
echo -e "  ${YELLOW}SKIP:${NC} $SKIP"
echo

if [ "$DRY_RUN" = true ]; then
  echo "DRY-RUN complete — no commands executed."
  exit 0
fi

if [ "$FAIL" -gt 0 ]; then
  echo "Phase L gate: FAILED ($FAIL failure(s))"
  exit 1
else
  echo "Phase L gate: PASSED"
  exit 0
fi
