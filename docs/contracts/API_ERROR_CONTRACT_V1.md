# API_ERROR_CONTRACT_V1

## API Error Envelope and HTTP Status Code Mapping — Phase 1 Contract

**Status:** FROZEN — changes require formal review with downstream dependency analysis.

---

## 1. Error Response Shape

All API error responses use this exact JSON envelope. No alternative shapes are permitted.

```json
{
  "error": {
    "code": "image_not_found",
    "message": "The specified image_id does not exist or is not accessible.",
    "target": "image_id",
    "request_id": "req_a1b2c3d4e5f6",
    "details": []
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `error.code` | `string` | Yes | Machine-readable error code. Snake_case. Stable across API versions — clients parse this. |
| `error.message` | `string` | Yes | Human-readable description. Localization-ready. Must never contain stack traces, internal hostnames, DB errors, file paths, or internal service names. |
| `error.target` | `string` | No | The request field that caused the error. Null for errors not attributable to a specific field. |
| `error.request_id` | `string` | Yes | UUID for the request. Propagated from API edge. Always present, even on 5xx. Format: `req_` prefix + opaque ID. |
| `error.details` | `array` | No | Array of sub-errors for batch validation failures. Each element has `code`, `message`, `target`. Empty array if not applicable. |

**`details` element shape (for multi-field validation):**

```json
{
  "code": "invalid_value",
  "message": "Instance type 'gp1.xxxxl' is not a valid instance type.",
  "target": "instance_type"
}
```

---

## 2. HTTP Status Code Mapping

| HTTP Status | Meaning | Client Retry? | Error Codes (examples) |
|-------------|---------|---------------|----------------------|
| `400` | Validation failure. Request must be modified. | No | `invalid_request`, `missing_field`, `invalid_value`, `invalid_block_device_mapping`, `missing_idempotency_key` |
| `401` | Missing or invalid authentication. | No (fix auth) | `authentication_required`, `invalid_signature`, `expired_signature`, `invalid_access_key` |
| `403` | Authenticated but not authorized (non-ownership cases only). | No | `forbidden` |
| `404` | Resource not found **or owned by another account**. | No | `instance_not_found`, `image_not_found`, `ssh_key_not_found`, `job_not_found` |
| `409` | Conflict: idempotency key mismatch or illegal state transition. | No | `illegal_state_transition`, `idempotency_key_mismatch`, `instance_locked` |
| `422` | Request syntactically valid but semantically unprocessable. | No | `quota_exceeded`, `availability_zone_unavailable` |
| `429` | Rate limit exceeded. | Yes, after `Retry-After` header | `rate_limit_exceeded` |
| `500` | Internal server error. Generic message only. | No (report with `request_id`) | `internal_error` |
| `503` | Dependency unavailable. Temporary. | Yes, with exponential backoff | `service_unavailable` |

---

## 3. Security Rule: 404 vs 403

**On unauthorized access attempts, return `404 Not Found`, not `403 Forbidden`.**

This prevents confirming the existence of resources the requester does not own. The only exception where `403` is used is for account-level actions where resource existence is not leaked (e.g., attempting an action the account type does not support).

**Authorization check order:** Auth checks always precede existence checks. If the requester is not the owner, return 404 regardless of whether the resource exists.

---

## 4. Phase 1 Error Code Catalog

### Validation Errors (400)

| Code | Target | Message Template |
|------|--------|-----------------|
| `invalid_request` | (varies) | The request body is malformed or missing required fields. |
| `missing_field` | `{field}` | The field '{field}' is required. |
| `invalid_value` | `{field}` | The value for '{field}' is not valid. {reason}. |
| `invalid_instance_type` | `instance_type` | Instance type '{value}' is not a valid or active instance type. |
| `invalid_image_id` | `image_id` | Image '{value}' does not exist or is not accessible. |
| `invalid_ssh_key` | `ssh_key_name` | SSH key '{value}' does not exist or is not owned by this account. |
| `invalid_availability_zone` | `availability_zone` | Availability zone '{value}' does not exist or is not accepting new instances. |
| `invalid_block_device_mapping` | `block_devices` | Block device mapping is invalid. {reason}. |
| `delete_on_termination_required` | `block_devices[0].delete_on_termination` | Phase 1 requires delete_on_termination to be true. |
| `missing_idempotency_key` | `Idempotency-Key` | The Idempotency-Key header is required for mutating requests. |
| `invalid_name` | `name` | Instance name must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$ and be unique within the account. |
| `name_already_exists` | `name` | An instance with name '{value}' already exists in this account. |
| `invalid_labels` | `labels` | Labels must not exceed 64 keys. Keys max 63 chars, values max 255 chars. |

### Authentication Errors (401)

| Code | Message Template |
|------|-----------------|
| `authentication_required` | Authentication is required. Provide a valid signed request. |
| `invalid_signature` | The request signature does not match. Verify your SecretAccessKey and signing method. |
| `expired_signature` | The request signature has expired. Ensure client clock is synchronized. |
| `invalid_access_key` | The AccessKeyId provided does not exist or has been revoked. |

### Conflict Errors (409)

| Code | Target | Message Template |
|------|--------|-----------------|
| `illegal_state_transition` | `status` | Cannot perform '{action}' on an instance in '{current_state}' state. |
| `idempotency_key_mismatch` | `Idempotency-Key` | The Idempotency-Key has been used with a different request payload. |
| `instance_locked` | `id` | Instance is currently being modified by another operation. Try again shortly. |

### Quota Errors (422)

| Code | Target | Message Template |
|------|--------|-----------------|
| `quota_exceeded` | (varies) | Account quota for {resource} has been reached. Current: {current}, limit: {limit}. |
| `availability_zone_unavailable` | `availability_zone` | The requested availability zone is not currently accepting new instances. |

### Rate Limit (429)

| Code | Message Template | Headers |
|------|-----------------|---------|
| `rate_limit_exceeded` | Too many requests. Retry after {seconds} seconds. | `Retry-After: {seconds}` |

### Server Errors (500, 503)

| Code | Message Template |
|------|-----------------|
| `internal_error` | An internal error occurred. Reference request_id '{request_id}' when contacting support. |
| `service_unavailable` | The service is temporarily unavailable. Please retry. |

**5xx responses must never include:** stack traces, internal hostnames, database error messages, file paths, internal service names, or any infrastructure topology detail.

---

## 5. Async Operation Error Reporting

For operations that return `202 Accepted`, errors during async execution are reported via the Job resource and the instance's `status_details` field:

**Job result on failure:**

```json
{
  "status": "FAILED",
  "result": {
    "error_code": "PROVISION_TIMEOUT",
    "error_message": "Instance provisioning timed out after 15 minutes.",
    "failed_step": "wait_for_ready"
  }
}
```

**Instance `status_details` on failure:**

```json
{
  "code": "PROVISION_TIMEOUT",
  "message": "Instance provisioning timed out after 15 minutes."
}
```

### Async Error Codes

| Code | Description |
|------|-------------|
| `PROVISION_TIMEOUT` | Provisioning exceeded timeout. |
| `HOST_UNAVAILABLE` | No host with sufficient capacity. |
| `ROOTFS_CREATION_FAILED` | Failed to create root disk CoW overlay. |
| `NETWORK_SETUP_FAILED` | Failed to allocate IP or create TAP device. |
| `VM_LAUNCH_FAILED` | Firecracker/hypervisor failed to start VM. |
| `STOP_FORCE_FAILED` | VM did not respond to force stop. Operator alert. |
| `REBOOT_TIMEOUT` | Reboot did not complete within timeout. |
| `DELETE_FAILED` | Resource cleanup failed after max retries. |
| `UNEXPECTEDLY_TERMINATED` | VM process disappeared without control plane action. |
| `IP_ALLOCATION_FAILED` | IP pool exhausted or allocation transaction failed. |

**Sensitive information policy:** Async error codes and messages must not include raw infrastructure errors, hostnames, or database messages. They must be safe for user-visible display.

---

## 6. Validation Execution Order

All synchronous pre-acceptance checks execute in this order. First failing check determines the error response.

```
1. Authentication: request signature valid and key active → 401 if fails
2. Authorization: principal owns the target resource → 404 if fails
3. Schema: all required fields present, correct types → 400 if fails
4. Resource existence: image exists, SSH key exists → 400 (or 404 for target resource)
5. Instance type valid and active → 400 if fails
6. Availability zone exists and accepting → 400/422 if fails
7. Quota: principal has not exceeded limits → 422 if fails
8. State guard: current instance state allows requested action → 409 if fails
9. Idempotency: check for existing key → 409 if mismatch, cached response if match
```

---

## 7. Invariants

| Rule | Description |
|------|-------------|
| Every error response includes `request_id` | Even on 5xx. Clients use this for support escalation. |
| 5xx responses are generic | Never expose internal details. |
| 404 for cross-account access | Never 403. Prevents resource existence enumeration. |
| `error.code` values are stable | Clients depend on machine-readable codes. Adding new codes is non-breaking. Removing or renaming is breaking. |
| Auth before existence | Authorization check always runs before existence check. |
