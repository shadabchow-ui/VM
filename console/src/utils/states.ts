import type { InstanceStatus } from '../types';

export type ActionName = 'start' | 'stop' | 'reboot' | 'delete';

type StateDisplay = {
  label: string;
  color: string;
  bgColor: string;
  borderColor: string;
  spinner: boolean;
  isError: boolean;
};

const ALLOWED_ACTIONS: Record<InstanceStatus, ActionName[]> = {
  requested: ['delete'],
  provisioning: ['delete'],
  running: ['stop', 'reboot', 'delete'],
  stopping: [],
  stopped: ['start', 'delete'],
  starting: [],
  rebooting: [],
  deleting: [],
  deleted: [],
  failed: ['delete'],
  error: ['delete'],
  unknown: [],
};

export function isActionAllowed(status: InstanceStatus, action: ActionName): boolean {
  return ALLOWED_ACTIONS[status]?.includes(action) ?? false;
}

export function isTransitional(status: InstanceStatus): boolean {
  return [
    'requested',
    'provisioning',
    'starting',
    'stopping',
    'rebooting',
    'deleting',
  ].includes(status);
}

export const STATE_DISPLAY: Record<InstanceStatus, StateDisplay> = {
  requested: {
    label: 'Requested',
    color: '#2563eb',
    bgColor: '#eff6ff',
    borderColor: '#bfdbfe',
    spinner: true,
    isError: false,
  },
  provisioning: {
    label: 'Provisioning',
    color: '#2563eb',
    bgColor: '#eff6ff',
    borderColor: '#bfdbfe',
    spinner: true,
    isError: false,
  },
  running: {
    label: 'Running',
    color: '#16a34a',
    bgColor: '#f0fdf4',
    borderColor: '#bbf7d0',
    spinner: false,
    isError: false,
  },
  stopping: {
    label: 'Stopping',
    color: '#ea580c',
    bgColor: '#fff7ed',
    borderColor: '#fed7aa',
    spinner: true,
    isError: false,
  },
  stopped: {
    label: 'Stopped',
    color: '#6b7280',
    bgColor: '#f9fafb',
    borderColor: '#e5e7eb',
    spinner: false,
    isError: false,
  },
  starting: {
    label: 'Starting',
    color: '#2563eb',
    bgColor: '#eff6ff',
    borderColor: '#bfdbfe',
    spinner: true,
    isError: false,
  },
  rebooting: {
    label: 'Rebooting',
    color: '#2563eb',
    bgColor: '#eff6ff',
    borderColor: '#bfdbfe',
    spinner: true,
    isError: false,
  },
  deleting: {
    label: 'Deleting',
    color: '#dc2626',
    bgColor: '#fef2f2',
    borderColor: '#fecaca',
    spinner: true,
    isError: false,
  },
  deleted: {
    label: 'Deleted',
    color: '#6b7280',
    bgColor: '#f9fafb',
    borderColor: '#e5e7eb',
    spinner: false,
    isError: false,
  },
  failed: {
    label: 'Failed',
    color: '#dc2626',
    bgColor: '#fef2f2',
    borderColor: '#fecaca',
    spinner: false,
    isError: true,
  },
  error: {
    label: 'Error',
    color: '#dc2626',
    bgColor: '#fef2f2',
    borderColor: '#fecaca',
    spinner: false,
    isError: true,
  },
  unknown: {
    label: 'Unknown',
    color: '#6b7280',
    bgColor: '#f9fafb',
    borderColor: '#e5e7eb',
    spinner: false,
    isError: false,
  },
};
