# WS-H2 Queue Resilience Verification — Run Notes

**Workstream:** WS-H2  
**Gate Items:** Q-1, Q-2, Q-3  
**Operator:** ____________________  
**Start Date:** ____________________  
**End Date:** ____________________

---

## Prerequisites Check

| # | Prerequisite | Status | Notes |
|---|--------------|--------|-------|
| P-1 | P2-M0 sign-off complete | ☐ PASS / ☐ FAIL | |
| P-2 | Phase 1 CI suite green | ☐ PASS / ☐ FAIL | CI run ID: |
| P-3 | All 5 job types deployed | ☐ PASS / ☐ FAIL | |
| P-4 | Worker process running | ☐ PASS / ☐ FAIL | |
| P-5 | DB access confirmed | ☐ PASS / ☐ FAIL | |
| P-6 | DLQ threshold documented | ☐ PASS / ☐ FAIL | N = 3 |
| P-7 | Optimistic locking confirmed | ☐ PASS / ☐ FAIL | |

---

## Phase A: Idempotency Baseline (Q-1)

### Step 1: Idempotency Tests

**Test Run Timestamp:** ____________________

**Tests Executed:**
- [ ] TestIdempotency_INSTANCE_CREATE_SecondDeliveryIsNoOp
- [ ] TestIdempotency_INSTANCE_CREATE_AlreadyProvisioningResumes
- [ ] TestIdempotency_INSTANCE_START_SecondDeliveryIsNoOp
- [ ] TestIdempotency_INSTANCE_START_AlreadyRunningReturnsNil
- [ ] TestIdempotency_INSTANCE_STOP_SecondDeliveryIsNoOp
- [ ] TestIdempotency_INSTANCE_STOP_AlreadyStoppedReturnsNil
- [ ] TestIdempotency_INSTANCE_STOP_AlreadyDeletedReturnsNil
- [ ] TestIdempotency_INSTANCE_REBOOT_SecondDeliveryCompletesFromRebooting
- [ ] TestIdempotency_INSTANCE_REBOOT_ReentrantFromRebootingState
- [ ] TestIdempotency_INSTANCE_DELETE_SecondDeliveryIsNoOp
- [ ] TestIdempotency_INSTANCE_DELETE_AlreadyDeletedReturnsNil
- [ ] TestIdempotency_INSTANCE_DELETE_ResourcesFreedOnce

**Result:** ☐ PASS / ☐ FAIL

**Evidence:** `evidence/ws-h2/logs/H2-TG1-idempotency-ci.txt`

### Step 2: Locking Review

**Reviewer:** ____________________

**Handlers Reviewed:**
- [ ] CreateHandler — uses version check
- [ ] StartHandler — uses version check
- [ ] StopHandler — uses version check
- [ ] RebootHandler — uses version check
- [ ] DeleteHandler — uses version check

**Result:** ☐ PASS / ☐ FAIL

**Evidence:** `evidence/ws-h2/results/H2-TG1-locking-review.md`

### Q-1 Gate Status: ☐ PASS / ☐ FAIL

---

## Phase B: Node Failure Drill (Q-2)

### Step 3: Create Test Instance

**Timestamp:** ____________________

**Instance Name:** queue-drill-test-1  
**Job ID:** ____________________

**Evidence:** `evidence/ws-h2/logs/H2-TG2-create-response.json`

### Step 4: Simulate DB Disruption

**Disruption Method:** ☐ SIGTERM / ☐ SIGSTOP+SIGCONT / ☐ Network partition

**Disruption Start:** ____________________  
**Disruption End:** ____________________  
**Duration:** ____________________

### Step 5: Job Outcome

**Job Final Status:** ☐ COMPLETED / ☐ FAILED / ☐ STUCK

**Worker Behavior:**
- [ ] Clean reconnect observed
- [ ] No crash loop
- [ ] No panic

**Evidence:** 
- `evidence/ws-h2/logs/H2-TG2-worker-log.txt`
- `evidence/ws-h2/logs/H2-TG2-job-status-poll.txt`

### Step 6: Duplicate Resource Check

| Resource | Duplicate Count | Expected |
|----------|-----------------|----------|
| Instances | | 0 |
| IP allocations | | 0 |
| Root disks | | 0 |

**Evidence:** `evidence/ws-h2/logs/H2-TG2-duplicate-check.txt`

### Step 7: Cleanup

- [ ] Test instance deleted

### Q-2 Gate Status: ☐ PASS / ☐ FAIL

---

## Phase C: Poison Message / DLQ (Q-3)

### Step 8: Insert Poison Job

**Timestamp:** ____________________

**Poison Job ID:** ____________________

### Step 9: Monitor Retries

**Attempt 1:** ____________________  
**Attempt 2:** ____________________  
**Attempt 3:** ____________________

### Step 10: DLQ State Confirmed

| Field | Actual Value | Expected |
|-------|--------------|----------|
| status | | 'dead' |
| attempt_count | | 3 |
| Worker crashed? | ☐ Yes / ☐ No | No |

**Evidence:** `evidence/ws-h2/logs/H2-TG3-poison-final-state.txt`

### Step 11: Concurrent Job Completed

**Drain Job ID:** ____________________  
**Drain Job Status:** ☐ COMPLETED / ☐ OTHER

**Evidence:** `evidence/ws-h2/logs/H2-TG3-status-poll.txt`

### Step 12: DLQ Alert Fired

**Alert visible in channel:** ☐ Yes / ☐ No  
**Alert timestamp:** ____________________

**Evidence:** `evidence/ws-h2/screenshots/H2-TG3-dlq-alert.png`

### Step 13: DLQ Configuration Documented

- [x] DLQ threshold N = 3
- [x] DLQ status value = 'dead'
- [x] Code reference documented

**Evidence:** `evidence/ws-h2/results/H2-TG3-dlq-config.md`

### Step 14: Cleanup

- [ ] Poison job left in DLQ (expected)
- [ ] Drain test instance deleted

### Q-3 Gate Status: ☐ PASS / ☐ FAIL

---

## Summary

| Gate Item | Status | Evidence |
|-----------|--------|----------|
| Q-1 | ☐ PASS / ☐ FAIL | H2-TG1-*.txt, H2-TG1-*.md |
| Q-2 | ☐ PASS / ☐ FAIL | H2-TG2-*.txt, H2-TG2-*.json |
| Q-3 | ☐ PASS / ☐ FAIL | H2-TG3-*.txt, H2-TG3-*.md, H2-TG3-*.png |

---

## Issues Encountered

| Issue | Resolution | Owner |
|-------|------------|-------|
| | | |

---

## Sign-Off

**WS-H2 Complete:** ☐ Yes / ☐ No

**Operator Signature:** ____________________  
**Date:** ____________________

**Ready for P2_M1_GATE_CHECKLIST.md update:** ☐ Yes / ☐ No
