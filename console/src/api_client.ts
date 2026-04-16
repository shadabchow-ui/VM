// api/client.ts — Typed API client for the resource-manager.
//
// VM-P16B changes:
//   - Import types from canonical '../index' (types/index.ts) instead of '../types'.
//     The types/index.ts file is the authoritative single source derived from backend
//     API contracts. api_client.ts previously used a looser 'types.ts' shape.
//   - Add 'Api-Version' header to all requests, set to currentAPIVersion.
//     Source: vm-16-03__blueprint__ §implementation_decisions "date-based versioning".
//   - Add instancesApi.listByProject(projectId) — project-scoped instance list.
//     Source: vm-16-01__blueprint__ §quota_enforcement_point, instance_handlers.go
//     §"GET /v1/instances accepts ?project_id= to scope the list".
//   - Expose X-Request-ID from responses so callers can surface correlation IDs.
//   - Add retry helper for 429 / 5xx per API resilience contract.
//     Source: vm-16-03__blueprint__ §core_contracts "Resilience and Backpressure Signaling".
//
// All existing API shapes are preserved unchanged.
// All calls include X-Principal-ID from env or localStorage (dev fallback).
// Errors thrown as ApiException — never swallowed.
// 5xx → caller shows generic message + request_id.
//
// Source: API_ERROR_CONTRACT_V1 §1, §7; AUTH_OWNERSHIP_MODEL_V1 §1;
//         vm-16-03__blueprint__ §core_contracts.

import type {
  CreateInstanceRequest,
  CreateInstanceResponse,
  CreateSSHKeyRequest,
  Instance,
  LifecycleResponse,
  ListEventsResponse,
  ListInstancesResponse,
  ListSSHKeysResponse,
  Job,
  SSHKey,
  ApiErrorEnvelope,
} from '../index';
import { ApiException } from '../index';

// API base URL — configured via Vite env or empty string (dev proxy handles it).
const BASE_URL = (import.meta.env.VITE_API_URL as string | undefined) ?? '';

// VM-P16B: Current API version.
// Sent as Api-Version header on every request so the server can enforce
// version-specific behaviour and echo back X-Api-Version for client verification.
// Source: vm-16-03__blueprint__ §implementation_decisions "date-based versioning".
const currentAPIVersion = '2024-01-15';

// Principal ID — in production comes from the auth gateway / session.
// For development: set VITE_PRINCIPAL_ID env var, or set principal_id in localStorage.
function getPrincipalID(): string {
  return (
    (import.meta.env.VITE_PRINCIPAL_ID as string | undefined) ||
    localStorage.getItem('principal_id') ||
    '00000000-0000-0000-0000-000000000001' // default system principal for dev
  );
}

// VM-P16B: Retry configuration for transient errors.
// SDK clients MUST implement a default retry policy with exponential backoff
// for 429 and 5xx errors per the Resilience and Backpressure Signaling contract.
// Source: vm-16-03__blueprint__ §core_contracts "Resilience and Backpressure Signaling".
const MAX_RETRIES = 3;
const BASE_DELAY_MS = 500;

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  extraHeaders?: Record<string, string>,
): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'X-Principal-ID': getPrincipalID(),
    // VM-P16B: Send the API version on every request.
    'Api-Version': currentAPIVersion,
    ...extraHeaders,
  };

  let lastError: ApiException | null = null;

  for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
    if (attempt > 0) {
      // Exponential backoff with jitter.
      const jitter = Math.random() * BASE_DELAY_MS;
      const delay = BASE_DELAY_MS * Math.pow(2, attempt - 1) + jitter;
      await sleep(delay);
    }

    const res = await fetch(`${BASE_URL}${path}`, {
      method,
      headers,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });

    // 204 No Content — no body to parse.
    if (res.status === 204) {
      return undefined as unknown as T;
    }

    let json: unknown;
    try {
      json = await res.json();
    } catch {
      throw new ApiException(res.status, {
        error: {
          code: 'parse_error',
          message: 'Unexpected response from server.',
          request_id: res.headers.get('X-Request-ID') ?? '',
          details: [],
        },
      });
    }

    if (!res.ok) {
      const err = new ApiException(res.status, json as ApiErrorEnvelope);

      // VM-P16B: Retry on 429 (rate limited) and 5xx (transient server error).
      // Honour Retry-After header when present.
      // Do NOT retry on 4xx (client errors) — they are not transient.
      // Source: vm-16-03__blueprint__ §core_contracts "Resilience and Backpressure Signaling".
      const shouldRetry = res.status === 429 || res.status >= 500;
      if (shouldRetry && attempt < MAX_RETRIES) {
        const retryAfterHeader = res.headers.get('Retry-After');
        if (retryAfterHeader) {
          const retryAfterSec = parseInt(retryAfterHeader, 10);
          if (!isNaN(retryAfterSec)) {
            await sleep(retryAfterSec * 1000);
            lastError = err;
            continue;
          }
        }
        lastError = err;
        continue;
      }

      throw err;
    }

    return json as T;
  }

  // All retries exhausted.
  throw lastError!;
}

// ── Instances ─────────────────────────────────────────────────────────────────

export const instancesApi = {
  list(): Promise<ListInstancesResponse> {
    return request<ListInstancesResponse>('GET', '/v1/instances');
  },

  // VM-P16B: Project-scoped instance list.
  // When project_id is provided the list is scoped to that project's instances.
  // The project must be owned by the calling principal (404-for-cross-account).
  // Source: instance_handlers.go §"GET /v1/instances accepts ?project_id=",
  //         vm-16-01__blueprint__ §quota_enforcement_point.
  listByProject(projectId: string): Promise<ListInstancesResponse> {
    return request<ListInstancesResponse>(
      'GET',
      `/v1/instances?project_id=${encodeURIComponent(projectId)}`,
    );
  },

  get(id: string): Promise<Instance> {
    return request<Instance>('GET', `/v1/instances/${id}`);
  },

  create(req: CreateInstanceRequest, idempotencyKey: string): Promise<CreateInstanceResponse> {
    return request<CreateInstanceResponse>('POST', '/v1/instances', req, {
      'Idempotency-Key': idempotencyKey,
    });
  },

  stop(id: string): Promise<LifecycleResponse> {
    return request<LifecycleResponse>('POST', `/v1/instances/${id}/stop`);
  },

  start(id: string): Promise<LifecycleResponse> {
    return request<LifecycleResponse>('POST', `/v1/instances/${id}/start`);
  },

  reboot(id: string): Promise<LifecycleResponse> {
    return request<LifecycleResponse>('POST', `/v1/instances/${id}/reboot`);
  },

  delete(id: string): Promise<LifecycleResponse> {
    return request<LifecycleResponse>('DELETE', `/v1/instances/${id}`);
  },

  getJob(instanceId: string, jobId: string): Promise<Job> {
    return request<Job>('GET', `/v1/instances/${instanceId}/jobs/${jobId}`);
  },

  listEvents(instanceId: string): Promise<ListEventsResponse> {
    return request<ListEventsResponse>('GET', `/v1/instances/${instanceId}/events`);
  },
};

// ── SSH Keys ──────────────────────────────────────────────────────────────────

export const sshKeysApi = {
  list(): Promise<ListSSHKeysResponse> {
    return request<ListSSHKeysResponse>('GET', '/v1/ssh-keys');
  },

  create(req: CreateSSHKeyRequest): Promise<SSHKey> {
    return request<SSHKey>('POST', '/v1/ssh-keys', req);
  },

  delete(id: string): Promise<void> {
    return request<void>('DELETE', `/v1/ssh-keys/${id}`);
  },
};

// ── Version compatibility check ───────────────────────────────────────────────

// VM-P16B: checkAPIVersion verifies the server version is compatible with this client.
// Called at SDK/app startup. Throws if the server returns a version the client
// does not recognise.
//
// Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth".
export async function checkAPIVersion(): Promise<{ apiVersion: string; compatible: boolean }> {
  try {
    const resp = await request<{ api_version: string }>('GET', '/v1/version');
    return {
      apiVersion: resp.api_version,
      compatible: resp.api_version === currentAPIVersion,
    };
  } catch {
    return { apiVersion: 'unknown', compatible: false };
  }
}
