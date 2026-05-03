# VM-VOLUME-RUNTIME-PHASE-F evidence note

## Summary
Volume create/attach/detach/delete runtime foundation implemented through worker handlers, host-agent runtime contract, and API tests.

## Files changed/created

| File | Change |
|------|--------|
| `services/resource-manager/volume_handlers_test.go` | **New** — 27 API-level volume tests (create, get, list, delete, attach, detach, list-instance-volumes) |
| `packages/runtime-client/volume.go` | **New** — VolumeAttachConfig/DetachConfig types, command-builder (BuildAttachCommand, BuildDetachCommand), ToExtraDisk conversion |
| `packages/runtime-client/volume_test.go` | **New** — 3 contract type tests |
| `services/host-agent/runtime/volume.go` | **New** — Host-agent volume attach/detach request/response types + validation helpers |
| `services/host-agent/runtime/volume_test.go` | **New** — 9 validation tests |
| `internal/db/job_repo.go` | **Fixed** — ImageID column added to 5 SELECT/RETURNING queries to match JobRow Scan targets (forward-compatible with VM-P2C image_id migration) |

## Volume runtime behavior implemented

1. **VOLUME_CREATE** — Worker handler (`volume_create.go`) with lock→provision→unlock→available state machine. Idempotent.
2. **VOLUME_ATTACH** — Worker handler (`volume_handlers.go`) with available→attaching→in_use state machine. Rolls back on missing attachment.
3. **VOLUME_DETACH** — Worker handler with in_use→detaching→available state machine. Closes attachment record. Idempotent.
4. **VOLUME_DELETE** — Worker handler with available→deleting→deleted state machine. Rejects in_use volumes. Idempotent.
5. **Runtime contract** — VolumeAttachConfig/DetachConfig types define volume_id, instance_id, device_path, storage_path, read/write mode, delete_on_termination. ToExtraDisk() integrates with stopped-instance attach via CreateInstance ExtraDisks.
6. **Host-agent runtime** — Validation helpers for volume attach/detach requests with LocalStorageManager path safety checks.

## Hot attach support status

**Explicitly unsupported.** Running-instance attach/detach returns 409 `illegal_state_transition` with message "Instance must be stopped". Tested in `TestAttachVolume_HotAttach_Rejected` and `TestDetachVolume_HotDetach_Rejected`.

## Validation commands and pass/fail evidence

```
go test ./services/resource-manager/ -run 'TestCreateVolume|TestListVolume|TestGetVolume|TestDeleteVolume|TestAttachVolume|TestDetachVolume|TestListInstanceVolume' -count=1
→ PASS (27 tests)

go test ./services/worker/handlers/... -run 'Volume' -count=1
→ PASS (all volume handler tests: create, attach, detach, delete)

go test ./packages/runtime-client/... -count=1
→ PASS (3 volume contract type tests)

go test ./services/host-agent/runtime/... -run 'Volume|Storage' -count=1
→ PASS (9 volume validation tests, all storage path tests)

go test ./internal/db/... -count=1
→ PASS

go build ./...
→ PASS
```

## Repo/doc drift found

1. `P2_VOLUME_MODEL.md` / `P2_IMAGE_SNAPSHOT_MODEL.md` — referenced in code comments but not present in `git ls-files '*.md'`. Contracts inferred from code/tests and `docs/contracts/` files.
2. `phase-2-decision-records-and-contract-bundle.md` — not present in git.
3. `vm blueprint.md` — not present in git.
4. `JOB_MODEL_V1.md`, `API_ERROR_CONTRACT_V1.md`, `AUTH_OWNERSHIP_MODEL_V1.md` — present at `docs/contracts/` path, not root.
5. `internal/db/job_repo.go` ImageID column mismatch — ImageID was added to JobRow struct but DB migration for `jobs.image_id` column was never created. SELECT queries now include `COALESCE(image_id, NULL)::VARCHAR(64)` as forward-compatible placeholder. VM-P2C migration still needed.

## Remaining storage blockers

1. **Live hot-attach** — Requires kernel-level block device hotplug support in the VMM (Firecracker/QEMU) and corresponding host-agent RPCs. Current behavior correctly rejects with 409.
2. **Storage backend** — Volume data-plane operations (actual disk creation on NFS/block storage) are simulated via storage_path assignment. Real storage provisioning requires integrating with the storage backend.
3. **Multi-attach** — Explicitly out of scope. VOL-I-1 (single active attachment per volume) enforced at DB layer by unique partial index.
4. **Cross-host volume migration** — Not implemented. Volumes are host-local; migration requires distributed storage.
