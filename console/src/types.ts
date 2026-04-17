// types/index.ts — All TypeScript types derived from backend API contracts.
//
// VM-P16B additions:
//   - VersionInfo: shape of GET /v1/version response
//   - HealthResponse: shape of GET /healthz response
//
// Source: INSTANCE_MODEL_V1 §2, JOB_MODEL_V1 §1, EVENTS_SCHEMA_V1 §2,
//         AUTH_OWNERSHIP_MODEL_V1 §5, API_ERROR_CONTRACT_V1 §1,
//         vm-16-03__blueprint__ §core_contracts "API as the Single Source of Truth".

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

  // VM-P16B: detect version removal so SDK/CLI can surface an upgrade message.
  // Source: vm-16-03__blueprint__ §interaction_or_ops_contract (410 for removed versions).
  get isVersionRemoved(): boolean {
    return this.status === 410 && this.code === 'api_version_removed';
  }
}

// ── VM-P16B: API compatibility types ─────────────────────────────────────────

// VersionInfo is the response shape for GET /v1/version.
//
// api_version: the server's current stable API version (YYYY-MM-DD).
// min_api_version: the oldest client version the server still accepts.
// service: service identifier for multi-service environments.
//
// Source: compatibility_handlers.go handleVersion,
//         vm-16-03__blueprint__ §implementation_decisions "date-based versioning".
export interface VersionInfo {
  api_version: string;
  min_api_version: string;
  service: string;
}

// HealthResponse is the response shape for GET /healthz.
//
// status: 'ok' when healthy, 'degraded' when DB is unreachable.
// timestamp: RFC3339 wall-clock time of the probe (present when status='ok').
// reason: machine-readable degradation cause (present when status='degraded').
//
// Source: compatibility_handlers.go handleHealthz,
//         P2_M1_GATE_CHECKLIST §PRE-2 (service reachable).
export interface HealthResponse {
  status: 'ok' | 'degraded';
  timestamp?: string;
  reason?: string;
}
