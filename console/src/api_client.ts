// api/client.ts — Typed API client for the resource-manager.
//
// VM-P16B additions:
//   - Api-Version header sent on all /v1/* requests.
//     Value: API_VERSION constant matching server currentAPIVersion.
//     Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth",
//             §implementation_decisions "date-based versioning".
//   - X-Request-ID is read from responses and attached to ApiException for
//     correlation. Source: API_ERROR_CONTRACT_V1 §7.
//   - compatApi: version() and health() methods for SDK/CLI compatibility checks.
//
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
  HealthResponse,
  Instance,
  LifecycleResponse,
  ListEventsResponse,
  ListInstancesResponse,
  ListSSHKeysResponse,
  Job,
  SSHKey,
  VersionInfo,
  ApiErrorEnvelope,
} from './types';
import { ApiException } from './types';

// API base URL — configured via Vite env or empty string (dev proxy handles it).
const BASE_URL = (import.meta.env.VITE_API_URL as string | undefined) ?? '';

// API_VERSION is the stable date-based version sent on every /v1/* request.
// Must match server's currentAPIVersion in compatibility_handlers.go.
// Source: vm-16-03__blueprint__ §implementation_decisions "date-based versioning".
const API_VERSION = '2024-01-15';

// Principal ID — in production comes from the auth gateway / session.
// For development: set VITE_PRINCIPAL_ID env var, or set principal_id in localStorage.
function getPrincipalID(): string {
  return (
    (import.meta.env.VITE_PRINCIPAL_ID as string | undefined) ||
    localStorage.getItem('principal_id') ||
    '00000000-0000-0000-0000-000000000001' // default system principal for dev
  );
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  extraHeaders?: Record<string, string>,
): Promise<T> {
  // VM-P16B: Api-Version is sent on all /v1/* paths so the server can enforce
  // version negotiation and the client can detect deprecation via X-Api-Version.
  const isVersionedPath = path.startsWith('/v1/');

  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'X-Principal-ID': getPrincipalID(),
    ...(isVersionedPath ? { 'Api-Version': API_VERSION } : {}),
    ...extraHeaders,
  };

  const res = await fetch(`${BASE_URL}${path}`, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

  // VM-P16B: read X-Request-ID for error correlation.
  const requestId = res.headers.get('X-Request-ID') ?? '';

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
        request_id: requestId,
        details: [],
      },
    });
  }

  if (!res.ok) {
    // Propagate the server's structured error. If request_id is missing from
    // the body (shouldn't happen per API_ERROR_CONTRACT_V1 §7), fall back to
    // the response header value.
    const envelope = json as ApiErrorEnvelope;
    if (!envelope.error.request_id) {
      envelope.error.request_id = requestId;
    }
    throw new ApiException(res.status, envelope);
  }

  return json as T;
}

// ── Instances ─────────────────────────────────────────────────────────────────

export const instancesApi = {
  list(): Promise<ListInstancesResponse> {
    return request<ListInstancesResponse>('GET', '/v1/instances');
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

// ── Compatibility / operational endpoints ─────────────────────────────────────
//
// VM-P16B: version() and health() allow SDK/CLI tooling to verify API
// compatibility before making resource requests.
//
// Source: vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth",
//         vm-16-03__blueprint__ §interaction_or_ops_contract (410 for removed versions).

export const compatApi = {
  // version() fetches /v1/version which returns the server's current API version.
  // SDK/CLI tools should call this on startup to detect version drift and
  // surface upgrade warnings before the user's request fails with 410.
  version(): Promise<VersionInfo> {
    return request<VersionInfo>('GET', '/v1/version');
  },

  // health() fetches /healthz — unauthenticated liveness probe.
  // No Api-Version header is sent because /healthz predates versioning.
  health(): Promise<HealthResponse> {
    return request<HealthResponse>('GET', '/healthz');
  },
};
