// api/client.ts — Typed API client for the resource-manager.
//
// All calls include X-Principal-ID from env or localStorage (dev fallback).
// Errors thrown as ApiException — never swallowed.
// 5xx → caller shows generic message + request_id.
//
// Source: API_ERROR_CONTRACT_V1 §1, §7; AUTH_OWNERSHIP_MODEL_V1 §1.

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
} from '../types';
import { ApiException } from '../types';

// API base URL — configured via Vite env or empty string (dev proxy handles it).
const BASE_URL = (import.meta.env.VITE_API_URL as string | undefined) ?? '';

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
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'X-Principal-ID': getPrincipalID(),
    ...extraHeaders,
  };

  const res = await fetch(`${BASE_URL}${path}`, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });

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
        request_id: '',
        details: [],
      },
    });
  }

  if (!res.ok) {
    throw new ApiException(res.status, json as ApiErrorEnvelope);
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
