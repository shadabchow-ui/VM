export type InstanceStatus =
  | 'requested'
  | 'provisioning'
  | 'running'
  | 'stopping'
  | 'stopped'
  | 'rebooting'
  | 'deleting'
  | 'deleted'
  | 'failed'
  | 'error'
  | 'unknown'
  | 'starting';

export interface ApiErrorDetail {
  target?: string;
  field?: string;
  message: string;
  code?: string;
}

export interface ApiErrorEnvelope {
  error: {
    code: string;
    message: string;
    target?: string;
    request_id?: string;
    details?: ApiErrorDetail[];
  };
}

export class ApiException extends Error {
  status: number;
  requestId?: string;
  body: ApiErrorEnvelope;
  isServerError: boolean;

  constructor(
    status: number,
    body: ApiErrorEnvelope = {
      error: {
        code: 'unknown_error',
        message: 'Unknown error',
      },
    }
  ) {
    super(body?.error?.message || 'Unknown error');
    this.name = 'ApiException';
    this.status = status;
    this.body = body;
    this.requestId = body?.error?.request_id;
    this.isServerError = status >= 500;
  }
}

export interface InstanceEvent {
  id?: string;
  instance_id?: string;
  type?: string;
  event_type?: string;
  message: string;
  created_at?: string;
  timestamp?: string;
  actor?: string;
}

export interface SSHKey {
  id?: string;
  name: string;
  fingerprint?: string;
  public_key?: string;
  created_at?: string;
  key_type?: string;
}

export interface Instance {
  id: string;
  name?: string;
  display_name?: string;
  status: InstanceStatus;
  image_id?: string;
  image?: string;
  instance_type?: string;
  plan?: string;
  region?: string;
  availability_zone?: string;
  public_ip?: string | null;
  private_ip?: string | null;
  ssh_key_name?: string;
  created_at?: string;
  updated_at?: string;
  labels?: Record<string, string>;
  status_details?: {
    code?: string;
    message?: string;
  } | null;
}

export interface Job {
  id: string;
  status: string;
  type?: string;
  instance_id?: string;
  result?: unknown;
  created_at?: string;
  updated_at?: string;
}

export interface ListInstancesResponse {
  instances: Instance[];
  next_page_token?: string;
}

export interface ListEventsResponse {
  events: InstanceEvent[];
  next_page_token?: string;
}

export interface ListSSHKeysResponse {
  ssh_keys: SSHKey[];
  next_page_token?: string;
}

export interface CreateInstanceRequest {
  name?: string;
  image_id: string;
  instance_type: string;
  availability_zone: string;
  ssh_key_name: string;
  labels?: Record<string, string>;
  root_disk_gb?: number;
  user_data?: string;
}

export interface CreateInstanceResponse {
  instance_id?: string;
  job_id?: string;
  instance?: Instance;
  job?: Job;
}

export interface LifecycleResponse {
  job_id?: string;
  job?: Job;
  instance?: Instance;
}

export interface InstanceActionResponse {
  job_id?: string;
  job?: Job;
}

export interface CreateSSHKeyRequest {
  name: string;
  public_key: string;
}

export interface DeleteResponse {
  job_id?: string;
  success?: boolean;
}

export interface PaginationParams {
  page_size?: number;
  page_token?: string;
}
