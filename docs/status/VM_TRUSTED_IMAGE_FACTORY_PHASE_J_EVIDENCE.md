# VM-TRUSTED-IMAGE-FACTORY-PHASE-J Evidence

## Execution date
2026-05-03

## Validation commands and results
```
go build ./...                                          → PASS
go test ./services/resource-manager/... -count=1        → PASS (all ok)
go test ./services/worker/... -count=1                  → PASS
go test ./services/worker/handlers/... -count=1         → PASS
go test ./internal/db/... -count=1                      → PASS
```

## Summary of changes

### 1. Image lifecycle
Preserved ACTIVE, DEPRECATED, OBSOLETE, FAILED, PENDING_VALIDATION semantics.
Added VALIDATING validation_status transition (SetValidationInProgress).
Schema already supported 'validating' in CHECK constraint — naturally supported.

### 2. Artifact metadata (migration 0017)
Added to images table: format (VARCHAR 20), size_bytes (BIGINT), image_digest (VARCHAR 255), validation_error (TEXT).
New repo methods: WriteImageArtifactMetadata, SetImageValidationError, SetValidationInProgress.

### 3. Import quarantine
Imported images (source_type=IMPORT) start in PENDING_VALIDATION status.
ImageIsLaunchable blocks PENDING_VALIDATION → no launch until validation passes.
Remote import source allowlist/config: left as explicit follow-up (no existing config infrastructure).

### 4. Validation job skeleton (services/worker/handlers/image_validate.go)
IMAGE_VALIDATE handler: format check, digest check, min disk metadata check, boot test (fails closed).
Uses idgen "ivr" prefix for validation result IDs.
All pass → PromoteValidatedImage → ACTIVE. Any fail → FailValidatedImage → FAILED.

### 5. Publish/promote
POST /v1/images/{id}/promote endpoint.
Only PENDING_VALIDATION images can be promoted.
Requires AllStagesPassed (all required stages recorded as pass).
Transition is DB-atomic: status='PENDING_VALIDATION' check in WHERE clause.

### 6. Family alias
ResolveFamilyLatest/ResolveFamilyByVersion unchanged — already excludes OBSOLETE/FAILED/PENDING_VALIDATION via `AND status IN ('ACTIVE', 'DEPRECATED')`.

### 7. Tests added
- imported image starts non-launchable (import quarantine enforced)
- blocked states (OBSOLETE, FAILED, PENDING_VALIDATION) fail launch
- DEPRECATED remains launchable
- validation failure records reason via SetImageValidationError
- promote requires AllStagesPassed
- promote rejects non-PENDING_VALIDATION images (422)
- promote rejects non-owned images (404)
- family latest excludes PENDING_VALIDATION and FAILED
- artifact metadata written and read correctly
- VALIDATING transition sets validation_status to "validating" and clears error
- VALIDATING transition rejected for non-PENDING_VALIDATION images

### 8. Files changed
- db/migrations/0017_image_artifact_metadata.up.sql (new)
- db/migrations/0017_image_artifact_metadata.down.sql (new)
- internal/db/image_repo.go (ImageRow fields, selectImageCols, scanImage, CreateImage, new methods)
- services/resource-manager/image_errors.go (added promote error code)
- services/resource-manager/image_handlers.go (added promote endpoint, handlePromoteImage)
- services/resource-manager/image_types.go (added PromoteImageResponse)
- services/resource-manager/instance_handlers_test.go (memPool: instRow 13-col fix, Exec UPDATE images cases, Query image_validation_results, INSERT images/imval, types)
- services/resource-manager/mempool_image_patch_test.go (imageRow/imageRows 26-col)
- services/resource-manager/image_catalog_test.go (new validation/promote/quarantine tests)
- services/worker/handlers/image_validate.go (new IMAGE_VALIDATE handler)
- docs/status/VM_TRUSTED_IMAGE_FACTORY_PHASE_J_EVIDENCE.md (new)
- services/resource-manager/image_handlers_test.go (TestListImages_MethodNotAllowed fix)
