// api/client.ts — Typed API client for the resource-manager.
//
// All calls include X-Principal-ID from env or localStorage (dev fallback).
// Errors thrown as ApiException — never swallowed.
// 5xx → caller shows generic message + request_id.
//
// Demo mode:
// Set VITE_DEMO_MODE=true to use in-browser sample data instead of calling the backend.

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

const BASE_URL = (import.meta.env.VITE_API_URL as string | undefined) ?? '';
const DEMO_MODE = (import.meta.env.VITE_DEMO_MODE as string | undefined) === 'true';

function now(): string {
  return new Date().toISOString();
}

const demoInstances: Instance[] = [
  {
    id: 'inst-demo-running',
    name: 'demo-web-01',
    status: 'running',
    instance_type: 'c1.small',
    image_id: 'ubuntu-22-04-demo',
    availability_zone: 'az-a',
    region: 'local',
    labels: { env: 'demo', app: 'web' },
    public_ip: '203.0.113.10',
    private_ip: '10.0.0.10',
    created_at: now(),
    updated_at: now(),
  },
  {
    id: 'inst-demo-stopped',
    name: 'demo-worker-01',
    status: 'stopped',
    instance_type: 'c1.medium',
    image_id: 'debian-12-demo',
    availability_zone: 'az-a',
    region: 'local',
    labels: { env: 'demo', app: 'worker' },
    public_ip: null,
    private_ip: '10.0.0.11',
    created_at: now(),
    updated_at: now(),
  },
];

const demoSSHKeys: SSHKey[] = [
  {
    id: 'key-demo-main',
    name: 'demo-main-key',
    fingerprint: 'SHA256:demo-key-fingerprint',
    key_type: 'ed25519',
    created_at: now(),
  },
];

function demoJob(instanceId: string, action: string): LifecycleResponse {
  return {
    instance_id: instanceId,
    job_id: `job-demo-${action}-${Date.now()}`,
    action,
  };
}

// Principal ID — in production comes from the auth gateway / session.
// For development: set VITE_PRINCIPAL_ID env var, or set principal_id in localStorage.
function getPrincipalID(): string {
  return (
    (import.meta.env.VITE_PRINCIPAL_ID as string | undefined) ||
    localStorage.getItem('principal_id') ||
    '00000000-0000-0000-0000-000000000001'
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
    if (DEMO_MODE) {
      return Promise.resolve({ instances: demoInstances, total: demoInstances.length });
    }
    return request<ListInstancesResponse>('GET', '/v1/instances');
  },

  get(id: string): Promise<Instance> {
    if (DEMO_MODE) {
      return Promise.resolve(demoInstances.find((item) => item.id === id) ?? demoInstances[0]);
    }
    return request<Instance>('GET', `/v1/instances/${id}`);
  },

  create(req: CreateInstanceRequest, idempotencyKey: string): Promise<CreateInstanceResponse> {
    if (DEMO_MODE) {
      const instance: Instance = {
        id: `inst-demo-${Date.now()}`,
        name: req.name,
        status: 'provisioning',
        instance_type: req.instance_type,
        image_id: req.image_id,
        availability_zone: req.availability_zone,
        region: 'local',
        labels: req.labels ?? { env: 'demo' },
        public_ip: null,
        private_ip: '10.0.0.99',
        created_at: now(),
        updated_at: now(),
      };
      demoInstances.unshift(instance);
      return Promise.resolve({ instance });
    }

    return request<CreateInstanceResponse>('POST', '/v1/instances', req, {
      'Idempotency-Key': idempotencyKey,
    });
  },

  stop(id: string): Promise<LifecycleResponse> {
    if (DEMO_MODE) return Promise.resolve(demoJob(id, 'stop'));
    return request<LifecycleResponse>('POST', `/v1/instances/${id}/stop`);
  },

  start(id: string): Promise<LifecycleResponse> {
    if (DEMO_MODE) return Promise.resolve(demoJob(id, 'start'));
    return request<LifecycleResponse>('POST', `/v1/instances/${id}/start`);
  },

  reboot(id: string): Promise<LifecycleResponse> {
    if (DEMO_MODE) return Promise.resolve(demoJob(id, 'reboot'));
    return request<LifecycleResponse>('POST', `/v1/instances/${id}/reboot`);
  },

  delete(id: string): Promise<LifecycleResponse> {
    if (DEMO_MODE) {
      const index = demoInstances.findIndex((item) => item.id === id);
      if (index >= 0) {
        demoInstances[index] = {
          ...demoInstances[index],
          status: 'deleted',
          updated_at: now(),
        };
      }
      return Promise.resolve(demoJob(id, 'delete'));
    }
    return request<LifecycleResponse>('DELETE', `/v1/instances/${id}`);
  },

  getJob(instanceId: string, jobId: string): Promise<Job> {
    if (DEMO_MODE) {
      return Promise.resolve({
        id: jobId,
        instance_id: instanceId,
        job_type: 'demo',
        status: 'completed',
        attempt_count: 1,
        max_attempts: 3,
        error_message: null,
        created_at: now(),
        updated_at: now(),
        completed_at: now(),
      });
    }
    return request<Job>('GET', `/v1/instances/${instanceId}/jobs/${jobId}`);
  },

  listEvents(instanceId: string): Promise<ListEventsResponse> {
    if (DEMO_MODE) {
      return Promise.resolve({
        total: 3,
        events: [
          {
            id: 'evt-demo-1',
            event_type: 'instance.provisioning.start',
            message: 'Demo provisioning started.',
            actor: 'system',
            created_at: now(),
          },
          {
            id: 'evt-demo-2',
            event_type: 'instance.network.ready',
            message: 'Demo private IP assigned.',
            actor: 'system',
            created_at: now(),
          },
          {
            id: 'evt-demo-3',
            event_type: 'instance.running',
            message: `Demo instance ${instanceId} is running.`,
            actor: 'system',
            created_at: now(),
          },
        ],
      });
    }
    return request<ListEventsResponse>('GET', `/v1/instances/${instanceId}/events`);
  },
};

// ── SSH Keys ──────────────────────────────────────────────────────────────────

export const sshKeysApi = {
  list(): Promise<ListSSHKeysResponse> {
    if (DEMO_MODE) {
      return Promise.resolve({ ssh_keys: demoSSHKeys, total: demoSSHKeys.length });
    }
    return request<ListSSHKeysResponse>('GET', '/v1/ssh-keys');
  },

  create(req: CreateSSHKeyRequest): Promise<SSHKey> {
    if (DEMO_MODE) {
      const key: SSHKey = {
        id: `key-demo-${Date.now()}`,
        name: req.name,
        fingerprint: 'SHA256:demo-created-key',
        key_type: 'ed25519',
        created_at: now(),
      };
      demoSSHKeys.unshift(key);
      return Promise.resolve(key);
    }
    return request<SSHKey>('POST', '/v1/ssh-keys', req);
  },

  delete(id: string): Promise<void> {
    if (DEMO_MODE) {
      const index = demoSSHKeys.findIndex((item) => item.id === id);
      if (index >= 0) {
        demoSSHKeys.splice(index, 1);
      }
      return Promise.resolve();
    }
    return request<void>('DELETE', `/v1/ssh-keys/${id}`);
  },
};
