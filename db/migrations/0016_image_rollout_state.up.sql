-- 0016_image_rollout_state.up.sql
--
-- VM-P3B Job 3: publication / rollout orchestrator seam.
--
-- Creates:
--
--   image_rollouts — tracks the staged rollout of a validated PLATFORM image
--                   through canary and full-production phases.
--
-- Rollout lifecycle (per vm-13-01__blueprint__ §Publication and Rollout Orchestrator):
--
--   pending     → canary     : rollout worker starts canary phase (small % of traffic)
--   canary      → promoting  : canary metrics pass; worker begins promotion
--   promoting   → completed  : family alias atomically updated; image is ACTIVE
--   canary      → rolling_back: canary metrics fail; worker begins rollback
--   promoting   → rolling_back: rare; promotion-time failure triggers rollback
--   rolling_back→ rolled_back : rollback complete; image marked FAILED
--
-- A rollout row is created by the IMAGE_PUBLISH job when it begins work on a
-- validated (PENDING_VALIDATION→ACTIVE candidate) image. The rollout record
-- drives the orchestrator's state machine independently of the image lifecycle
-- state so that the image state remains the authoritative API-visible state
-- while the orchestrator has its own progression tracking.
--
-- canary_percent: the traffic percentage currently in canary. 0 when pending.
-- completed_at:   set when status reaches 'completed' or 'rolled_back'.
-- failure_reason: set when the rollout fails (status = 'rolling_back'|'rolled_back').
--
-- Source: vm-13-01__blueprint__ §Publication and Rollout Orchestrator,
--         vm-13-01__skill__ §instructions "Implement a Publication & Rollout Orchestrator".

CREATE TABLE image_rollouts (
    id              VARCHAR(64)  PRIMARY KEY,       -- idgen prefix "rol"
    image_id        UUID         NOT NULL UNIQUE    -- one active rollout per image
                    REFERENCES images(id) ON DELETE CASCADE,
    job_id          VARCHAR(64)  NOT NULL,          -- the IMAGE_PUBLISH job driving this rollout
    family_name     VARCHAR(255) NOT NULL,          -- target family for alias promotion
    status          VARCHAR(20)  NOT NULL DEFAULT 'pending',
    canary_percent  SMALLINT     NOT NULL DEFAULT 0,
    failure_reason  TEXT         NULL,
    started_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    completed_at    TIMESTAMPTZ  NULL,

    CONSTRAINT chk_rollout_status CHECK (
        status IN ('pending','canary','promoting','completed','rolling_back','rolled_back')
    ),
    CONSTRAINT chk_rollout_canary_pct CHECK (canary_percent BETWEEN 0 AND 100)
);

-- Lookup: find the active rollout for a given image.
CREATE INDEX idx_rollouts_image_id ON image_rollouts (image_id);
-- Lookup: find all rollouts for a family (used by promotion gate).
CREATE INDEX idx_rollouts_family   ON image_rollouts (family_name, status);
