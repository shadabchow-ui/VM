// types/index.ts — All TypeScript types derived from backend API contracts.
//
// Source: INSTANCE_MODEL_V1 §2, JOB_MODEL_V1 §1, EVENTS_SCHEMA_V1 §2,
//         AUTH_OWNERSHIP_MODEL_V1 §5, API_ERROR_CONTRACT_V1 §1.

// ── Instance ──────────────────────────────────────────────────────────────────

export type InstanceStatus =
  | 'requested'
  | 'provisioning'
  | 'running'
  | 'stopping'
  | 'stopped'
  | 'starting'
  | 'rebooting'
  | 'deleting'
  | 'deleted'
  | 'failed';

export interface Instance {
  id: string;
  name: string;
  status: InstanceStatus;
  instance_type: string;
  image_id: string;
  availability_zone: string;
  region: string;
  labels: Record<string, string>;
  public_ip: string | null;
  private_ip: string | null;
  created_at: string;
  updated_at: string;
}

export interface ListInstancesResponse {
  instances: Instance[];
  total: number;
}

export interface CreateInstanceRequest {
  name: string;
  instance_type: string;
  image_id: string;
  availability_zone: string;
  ssh_key_name: string;
  labels?: Record<string, string>;
}

export interface CreateInstanceResponse {
  instance: Instance;
}

export interface LifecycleResponse {
  instance_id: string;
  job_id: string;
  action: string;
}

// ── Job ───────────────────────────────────────────────────────────────────────

export type JobStatus = 'pending' | 'in_progress' | 'completed' | 'failed' | 'dead';

export interface Job {
  id: string;
  instance_id: string;
  job_type: string;
  status: JobStatus;
  attempt_count: number;
  max_attempts: number;
  error_message?: string | null;
  created_at: string;
  updated_at: string;
  completed_at?: string | null;
}

// ── Event ─────────────────────────────────────────────────────────────────────

export interface InstanceEvent {
  id: string;
  event_type: string;
  message?: string | null;
  actor?: string | null;
  created_at: string;
}

export interface ListEventsResponse {
  events: InstanceEvent[];
  total: number;
}

// ── SSH Key ───────────────────────────────────────────────────────────────────

export interface SSHKey {
  id: string;
  name: string;
  fingerprint: string;
  key_type: string;
  created_at: string;
}

export interface ListSSHKeysResponse {
  ssh_keys: SSHKey[];
  total: number;
}

export interface CreateSSHKeyRequest {
  name: string;
  public_key: string;
}

// ── API Error ─────────────────────────────────────────────────────────────────

export interface ApiErrorBody {
  code: string;
  message: string;
  target?: string;
  request_id: string;
  details: ApiErrorBody[];
}

export interface ApiErrorEnvelope {
  error: ApiErrorBody;
}

export class ApiException extends Error {
  constructor(
    public readonly status: number,
    public readonly body: ApiErrorEnvelope,
  ) {
    super(body.error.message);
  }

  get requestId(): string {
    return this.body.error.request_id ?? '';
  }

  get code(): string {
    return this.body.error.code;
  }

  get isServerError(): boolean {
    return this.status >= 500;
  }

  get isIllegalTransition(): boolean {
    return this.code === 'illegal_state_transition';
  }
}
