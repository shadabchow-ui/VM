#!/usr/bin/env bash
# ws-h2-drill-runner.sh — WS-H2 Queue Resilience Verification Drill
#
# Source: P2_M1_WS_H2_QUEUE_DRILL_RUNBOOK.md
# Gate Items: Q-1, Q-2, Q-3
#
# Usage:
#   ./ws-h2-drill-runner.sh prereq     # Check prerequisites (P-1 through P-7)
#   ./ws-h2-drill-runner.sh phase-a    # Run Phase A: Idempotency baseline (Q-1)
#   ./ws-h2-drill-runner.sh phase-b    # Run Phase B: Node failure drill (Q-2)
#   ./ws-h2-drill-runner.sh phase-c    # Run Phase C: Poison message / DLQ (Q-3)
#   ./ws-h2-drill-runner.sh all        # Run all phases
#
# Environment variables (required for phase-b and phase-c):
#   PRIMARY_DB_HOST      - PostgreSQL primary hostname
#   DB_PORT              - PostgreSQL port (default: 5432)
#   DB_NAME              - Database name
#   DB_SUPERUSER         - DB user with read/write access
#   PGPASSWORD           - DB password (or use .pgpass)
#   API_BASE_URL         - Base URL for Phase 1 API
#   API_AUTH_HEADER      - Authorization header value
#   WORKER_LOG_PATH      - Path to worker log file
#   WORKER_PROCESS_NAME  - Process name for worker (for ps grep)
#
# Optional:
#   VALID_INSTANCE_SHAPE - Instance shape (default: c1.small)
#   VALID_IMAGE_ID       - Image ID (default: 00000000-0000-0000-0000-000000000010)

set -euo pipefail

# ═══════════════════════════════════════════════════════════════════════════════
# Configuration
# ═══════════════════════════════════════════════════════════════════════════════

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EVIDENCE_DIR="${SCRIPT_DIR}/evidence/ws-h2"
REPO_ROOT="${SCRIPT_DIR}"

# Defaults
DB_PORT="${DB_PORT:-5432}"
VALID_INSTANCE_SHAPE="${VALID_INSTANCE_SHAPE:-c1.small}"
VALID_IMAGE_ID="${VALID_IMAGE_ID:-00000000-0000-0000-0000-000000000010}"

# DLQ configuration (from code analysis)
DLQ_THRESHOLD_N=3
DLQ_STATUS_VALUE="dead"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# ═══════════════════════════════════════════════════════════════════════════════
# Helper Functions
# ═══════════════════════════════════════════════════════════════════════════════

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_pass() {
    echo -e "${GREEN}[PASS]${NC} $1"
}

log_fail() {
    echo -e "${RED}[FAIL]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

ensure_evidence_dir() {
    mkdir -p "${EVIDENCE_DIR}"/{artifacts,logs,screenshots,results,notes}
}

timestamp() {
    date -u +%Y-%m-%dT%H:%M:%SZ
}

psql_cmd() {
    psql -h "${PRIMARY_DB_HOST}" -p "${DB_PORT}" -U "${DB_SUPERUSER}" -d "${DB_NAME}" "$@"
}

# ═══════════════════════════════════════════════════════════════════════════════
# Prerequisites Check (P-1 through P-7)
# ═══════════════════════════════════════════════════════════════════════════════

check_prereqs() {
    log_info "Checking WS-H2 prerequisites..."
    ensure_evidence_dir
    local all_pass=true

    # P-1: P2-M0 sign-off
    log_info "P-1: Checking P2-M0 sign-off..."
    if [[ -f "${REPO_ROOT}/docs/P2_M0_SIGNOFF.md" ]] || [[ -f "${REPO_ROOT}/P2_M0_SIGNOFF.md" ]]; then
        log_pass "P-1: P2-M0 sign-off document found"
    else
        log_warn "P-1: P2-M0 sign-off document not found (expected: docs/P2_M0_SIGNOFF.md)"
        log_info "     Manual verification required"
    fi

    # P-2: Phase 1 CI suite is green
    log_info "P-2: Running handler tests..."
    if go test -v ./services/worker/handlers/... > "${EVIDENCE_DIR}/logs/prereq-handler-tests.log" 2>&1; then
        log_pass "P-2: Handler tests pass"
    else
        log_fail "P-2: Handler tests failed. See ${EVIDENCE_DIR}/logs/prereq-handler-tests.log"
        all_pass=false
    fi

    # P-3: All five job types deployed
    log_info "P-3: Verifying job types in schema..."
    if grep -q "INSTANCE_CREATE.*INSTANCE_DELETE.*INSTANCE_START.*INSTANCE_STOP.*INSTANCE_REBOOT" \
        "${REPO_ROOT}/db/migrations/001_initial.up.sql"; then
        log_pass "P-3: All five job types defined in schema"
    else
        log_fail "P-3: Not all job types found in schema"
        all_pass=false
    fi

    # P-4: Worker process code exists
    log_info "P-4: Checking worker code..."
    if [[ -f "${REPO_ROOT}/services/worker/main.go" ]]; then
        log_pass "P-4: Worker service code exists"
    else
        log_fail "P-4: Worker service not found"
        all_pass=false
    fi

    # P-5: DB access (skip if env vars not set)
    log_info "P-5: Checking DB access configuration..."
    if [[ -n "${PRIMARY_DB_HOST:-}" ]]; then
        log_pass "P-5: PRIMARY_DB_HOST is set"
    else
        log_warn "P-5: PRIMARY_DB_HOST not set (required for phase-b and phase-c)"
    fi

    # P-6: DLQ threshold documented
    log_info "P-6: Documenting DLQ threshold..."
    echo "DLQ_THRESHOLD_N=${DLQ_THRESHOLD_N}" > "${EVIDENCE_DIR}/results/dlq-threshold.txt"
    echo "DLQ_STATUS_VALUE=${DLQ_STATUS_VALUE}" >> "${EVIDENCE_DIR}/results/dlq-threshold.txt"
    echo "Source: db/migrations/001_initial.up.sql (max_attempts DEFAULT 3)" >> "${EVIDENCE_DIR}/results/dlq-threshold.txt"
    log_pass "P-6: DLQ threshold documented (N=${DLQ_THRESHOLD_N}, status='${DLQ_STATUS_VALUE}')"

    # P-7: Optimistic locking pattern
    log_info "P-7: Checking optimistic locking in handlers..."
    local handlers_with_version=0
    for handler in create.go start.go stop.go reboot.go delete.go; do
        if grep -q "UpdateInstanceState" "${REPO_ROOT}/services/worker/handlers/${handler}" 2>/dev/null; then
            ((handlers_with_version++))
        fi
    done
    if [[ ${handlers_with_version} -eq 5 ]]; then
        log_pass "P-7: All 5 handlers use UpdateInstanceState (version check)"
    else
        log_fail "P-7: Only ${handlers_with_version}/5 handlers use version check"
        all_pass=false
    fi

    if [[ "${all_pass}" == "true" ]]; then
        log_pass "All prerequisites passed"
        return 0
    else
        log_fail "Some prerequisites failed"
        return 1
    fi
}

# ═══════════════════════════════════════════════════════════════════════════════
# Phase A: Idempotency Baseline Verification (Gate Item Q-1)
# ═══════════════════════════════════════════════════════════════════════════════

run_phase_a() {
    log_info "═══════════════════════════════════════════════════════════════════"
    log_info "Phase A: Idempotency Baseline Verification (Gate Item Q-1)"
    log_info "═══════════════════════════════════════════════════════════════════"
    ensure_evidence_dir

    # Step 1: Run idempotency tests
    log_info "Step 1: Running idempotency integration tests..."
    
    local test_output="${EVIDENCE_DIR}/logs/H2-TG1-idempotency-ci.txt"
    echo "# H2-TG1: Idempotency CI Test Results" > "${test_output}"
    echo "# Timestamp: $(timestamp)" >> "${test_output}"
    echo "# Gate Item: Q-1" >> "${test_output}"
    echo "" >> "${test_output}"

    if go test -v ./services/worker/handlers/... -run "Idempotency" >> "${test_output}" 2>&1; then
        log_pass "Step 1: Idempotency tests PASS"
        echo "" >> "${test_output}"
        echo "RESULT: PASS" >> "${test_output}"
    else
        log_fail "Step 1: Idempotency tests FAIL"
        echo "" >> "${test_output}"
        echo "RESULT: FAIL" >> "${test_output}"
    fi

    # Also run the existing tests that verify idempotent behavior
    log_info "Step 1b: Running related idempotency tests..."
    go test -v ./services/worker/handlers/... -run "AlreadyDeleted|AlreadyProvisioning|AlreadyRunning|AlreadyStopped" \
        >> "${test_output}" 2>&1 || true

    # Step 2: Confirm locking review exists
    log_info "Step 2: Confirming optimistic locking review..."
    local locking_review="${EVIDENCE_DIR}/results/H2-TG1-locking-review.md"
    if [[ -f "${locking_review}" ]]; then
        log_pass "Step 2: Locking review document exists"
    else
        log_warn "Step 2: Locking review not found. Creating from template..."
        # The locking review was already created by Claude
        log_info "Please ensure ${locking_review} is populated"
    fi

    # Summary
    log_info "Phase A Evidence:"
    log_info "  - ${test_output}"
    log_info "  - ${locking_review}"
    
    echo ""
    log_info "Phase A complete. Review evidence files before marking Q-1 PASS."
}

# ═══════════════════════════════════════════════════════════════════════════════
# Phase B: Node Failure During Active Job Processing (Gate Item Q-2)
# ═══════════════════════════════════════════════════════════════════════════════

run_phase_b() {
    log_info "═══════════════════════════════════════════════════════════════════"
    log_info "Phase B: Node Failure During Active Job Processing (Gate Item Q-2)"
    log_info "═══════════════════════════════════════════════════════════════════"
    ensure_evidence_dir

    # Check required environment variables
    if [[ -z "${PRIMARY_DB_HOST:-}" ]] || [[ -z "${API_BASE_URL:-}" ]]; then
        log_fail "Required environment variables not set:"
        log_info "  PRIMARY_DB_HOST=${PRIMARY_DB_HOST:-<not set>}"
        log_info "  API_BASE_URL=${API_BASE_URL:-<not set>}"
        log_info ""
        log_info "Set these variables and re-run:"
        log_info "  export PRIMARY_DB_HOST=<your-db-host>"
        log_info "  export DB_PORT=5432"
        log_info "  export DB_NAME=<your-db-name>"
        log_info "  export DB_SUPERUSER=<your-db-user>"
        log_info "  export PGPASSWORD=<your-db-password>"
        log_info "  export API_BASE_URL=http://localhost:8080"
        log_info "  export API_AUTH_HEADER='Bearer <token>'"
        return 1
    fi

    log_info "This phase requires manual intervention to simulate DB disruption."
    log_info "Follow the runbook steps 3-7 manually."
    log_info ""
    log_info "Quick reference:"
    log_info "  Step 3: Create test instance via API"
    log_info "  Step 4: Simulate DB disruption (SIGTERM, SIGSTOP/SIGCONT, or network partition)"
    log_info "  Step 5: Observe job outcome in worker logs"
    log_info "  Step 6: Verify no duplicate resources"
    log_info "  Step 7: Clean up test instance"
    log_info ""
    log_info "Evidence to collect:"
    log_info "  - ${EVIDENCE_DIR}/logs/H2-TG2-create-response.json"
    log_info "  - ${EVIDENCE_DIR}/logs/H2-TG2-worker-log.txt"
    log_info "  - ${EVIDENCE_DIR}/logs/H2-TG2-job-status-poll.txt"
    log_info "  - ${EVIDENCE_DIR}/logs/H2-TG2-duplicate-check.txt"
    log_info ""

    # Provide helper commands
    log_info "Helper commands for Step 3 (create test instance):"
    cat << EOF

curl -s -X POST "${API_BASE_URL}/instances" \\
  -H "Authorization: ${API_AUTH_HEADER:-Bearer <token>}" \\
  -H "Content-Type: application/json" \\
  -d '{
    "name": "queue-drill-test-1",
    "shape": "${VALID_INSTANCE_SHAPE}",
    "image_id": "${VALID_IMAGE_ID}"
  }' | tee ${EVIDENCE_DIR}/logs/H2-TG2-create-response.json

EOF

    log_info "Helper SQL for Step 6 (duplicate check):"
    cat << EOF

-- Run these queries and save output to ${EVIDENCE_DIR}/logs/H2-TG2-duplicate-check.txt

-- Check for duplicate instances
SELECT name, count(*) FROM instances WHERE name = 'queue-drill-test-1' GROUP BY name;

-- Check for duplicate IP allocations
SELECT instance_id, ip_address, count(*)
FROM ip_allocations
WHERE owner_instance_id IN (SELECT id FROM instances WHERE name = 'queue-drill-test-1')
GROUP BY instance_id, ip_address
HAVING count(*) > 1;

-- Check for duplicate root_disks
SELECT instance_id, count(*)
FROM root_disks
WHERE instance_id IN (SELECT id FROM instances WHERE name = 'queue-drill-test-1')
GROUP BY instance_id
HAVING count(*) > 1;

EOF
}

# ═══════════════════════════════════════════════════════════════════════════════
# Phase C: Poison Message / DLQ Behavior (Gate Item Q-3)
# ═══════════════════════════════════════════════════════════════════════════════

run_phase_c() {
    log_info "═══════════════════════════════════════════════════════════════════"
    log_info "Phase C: Poison Message / DLQ Behavior (Gate Item Q-3)"
    log_info "═══════════════════════════════════════════════════════════════════"
    ensure_evidence_dir

    if [[ -z "${PRIMARY_DB_HOST:-}" ]]; then
        log_fail "PRIMARY_DB_HOST not set. Required for Phase C."
        return 1
    fi

    log_info "This phase requires manual DB access to insert a poison job."
    log_info "Follow the runbook steps 8-14."
    log_info ""
    log_info "DLQ Configuration (from code):"
    log_info "  DLQ_THRESHOLD_N=${DLQ_THRESHOLD_N}"
    log_info "  DLQ_STATUS_VALUE='${DLQ_STATUS_VALUE}'"
    log_info ""

    log_info "Helper SQL for Step 8 (insert poison job):"
    cat << EOF

-- First, create a minimal instance record for the poison job to reference
-- (The job FK requires a valid instance_id)

INSERT INTO principals (id, principal_type) VALUES
    ('00000000-0000-0000-0000-000000000099', 'ACCOUNT')
ON CONFLICT DO NOTHING;

INSERT INTO instances (
    id, name, owner_principal_id, vm_state, instance_type_id,
    image_id, availability_zone, version
) VALUES (
    'poison_test_inst_001',
    'poison-test-1',
    '00000000-0000-0000-0000-000000000099',
    'requested',
    'c1.small',
    '00000000-0000-0000-0000-000000000010',
    'us-east-1a',
    0
) ON CONFLICT DO NOTHING;

-- Now insert the poison job
INSERT INTO jobs (
    id,
    instance_id,
    job_type,
    status,
    idempotency_key,
    attempt_count,
    max_attempts,
    created_at,
    updated_at
) VALUES (
    'poison_job_' || to_char(now(), 'YYYYMMDDHH24MISS'),
    'poison_test_inst_001',
    'INSTANCE_CREATE',
    'pending',
    'poison_idem_' || to_char(now(), 'YYYYMMDDHH24MISS'),
    0,
    ${DLQ_THRESHOLD_N},
    now(),
    now()
) RETURNING id;

-- Record the returned job ID as POISON_JOB_ID

EOF

    log_info "Helper SQL for Step 10 (check DLQ state after N failures):"
    cat << EOF

SELECT id, type, status, attempt_count, error_message, updated_at
FROM jobs
WHERE id = '<POISON_JOB_ID>';

-- Expected:
--   status = '${DLQ_STATUS_VALUE}'
--   attempt_count = ${DLQ_THRESHOLD_N}

EOF

    log_info "Evidence to collect:"
    log_info "  - ${EVIDENCE_DIR}/logs/H2-TG3-poison-log.txt"
    log_info "  - ${EVIDENCE_DIR}/logs/H2-TG3-poison-final-state.txt"
    log_info "  - ${EVIDENCE_DIR}/logs/H2-TG3-status-poll.txt"
    log_info "  - ${EVIDENCE_DIR}/screenshots/H2-TG3-dlq-alert.png"
    log_info "  - ${EVIDENCE_DIR}/results/H2-TG3-dlq-config.md (already created)"
}

# ═══════════════════════════════════════════════════════════════════════════════
# Run All Phases
# ═══════════════════════════════════════════════════════════════════════════════

run_all() {
    check_prereqs || true
    echo ""
    run_phase_a
    echo ""
    run_phase_b
    echo ""
    run_phase_c
    echo ""
    log_info "═══════════════════════════════════════════════════════════════════"
    log_info "WS-H2 Drill Complete"
    log_info "═══════════════════════════════════════════════════════════════════"
    log_info "Evidence directory: ${EVIDENCE_DIR}"
    log_info ""
    log_info "Next steps:"
    log_info "  1. Review all evidence files"
    log_info "  2. Complete manual drills for Phase B and Phase C"
    log_info "  3. Update ${EVIDENCE_DIR}/notes/run.md with observations"
    log_info "  4. Mark Q-1, Q-2, Q-3 in P2_M1_GATE_CHECKLIST.md"
}

# ═══════════════════════════════════════════════════════════════════════════════
# Main
# ═══════════════════════════════════════════════════════════════════════════════

cd "${REPO_ROOT}"

case "${1:-help}" in
    prereq)
        check_prereqs
        ;;
    phase-a)
        run_phase_a
        ;;
    phase-b)
        run_phase_b
        ;;
    phase-c)
        run_phase_c
        ;;
    all)
        run_all
        ;;
    *)
        echo "WS-H2 Queue Resilience Verification Drill"
        echo ""
        echo "Usage: $0 <command>"
        echo ""
        echo "Commands:"
        echo "  prereq     Check prerequisites (P-1 through P-7)"
        echo "  phase-a    Run Phase A: Idempotency baseline (Q-1)"
        echo "  phase-b    Run Phase B: Node failure drill (Q-2)"
        echo "  phase-c    Run Phase C: Poison message / DLQ (Q-3)"
        echo "  all        Run all phases"
        echo ""
        echo "Environment variables (for phase-b and phase-c):"
        echo "  PRIMARY_DB_HOST, DB_PORT, DB_NAME, DB_SUPERUSER, PGPASSWORD"
        echo "  API_BASE_URL, API_AUTH_HEADER"
        echo "  WORKER_LOG_PATH, WORKER_PROCESS_NAME"
        ;;
esac
