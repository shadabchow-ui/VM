// utils/format.ts — Date, IP, SSH command, and catalog formatting helpers.

export function formatDate(iso: string): string {
  try {
    return new Intl.DateTimeFormat('en-US', {
      year: 'numeric',
      month: 'short',
      day: 'numeric',
      hour: '2-digit',
      minute: '2-digit',
      timeZoneName: 'short',
    }).format(new Date(iso));
  } catch {
    return iso;
  }
}

export function formatRelativeTime(iso: string): string {
  try {
    const diff = Date.now() - new Date(iso).getTime();
    const seconds = Math.floor(diff / 1000);
    if (seconds < 60) return `${seconds}s ago`;
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return `${minutes}m ago`;
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.floor(hours / 24);
    return `${days}d ago`;
  } catch {
    return iso;
  }
}

export function buildSSHCommand(publicIP: string | null | undefined): string {
  if (!publicIP) return '';
  return `ssh root@${publicIP}`;
}

// Image ID → human-readable display name.
// Source: INSTANCE_MODEL_V1 §7 (Phase 1 image catalog).
export function imageDisplayName(imageID: string): string {
  const map: Record<string, string> = {
    img_ubuntu2204: 'Ubuntu 22.04 LTS',
    img_debian12: 'Debian 12',
    // UUID-based IDs from DB seed (001_initial.up.sql).
    '00000000-0000-0000-0000-000000000010': 'Ubuntu 22.04 LTS',
    '00000000-0000-0000-0000-000000000011': 'Debian 12',
  };
  return map[imageID] ?? imageID;
}

// Instance type → human-readable display with specs.
// Source: INSTANCE_MODEL_V1 §6 (Phase 1 shape catalog).
export function instanceTypeDisplay(typeID: string): string {
  const map: Record<string, string> = {
    'c1.small':   'c1.small — 2 vCPU / 4 GB',
    'c1.medium':  'c1.medium — 4 vCPU / 8 GB',
    'c1.large':   'c1.large — 8 vCPU / 16 GB',
    'c1.xlarge':  'c1.xlarge — 16 vCPU / 32 GB',
    'gp1.small':  'gp1.small — 2 vCPU / 4 GB',
    'gp1.medium': 'gp1.medium — 4 vCPU / 8 GB',
    'gp1.large':  'gp1.large — 8 vCPU / 16 GB',
    'gp1.xlarge': 'gp1.xlarge — 16 vCPU / 32 GB',
  };
  return map[typeID] ?? typeID;
}

// Generate a client-side idempotency key for create requests.
// Source: JOB_MODEL_V1 §6.
export function generateIdempotencyKey(): string {
  return `console-${Date.now()}-${Math.random().toString(36).slice(2, 9)}`;
}
