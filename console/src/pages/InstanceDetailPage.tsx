// pages/InstanceDetailPage.tsx — Instance detail page.
//
// M7 requirements satisfied:
//   ✓ Header: instance name + status badge + primary action buttons
//   ✓ Connection info card: public IP, private IP, SSH command, copy buttons
//   ✓ Connection info disabled/placeholder when not running
//   ✓ Configuration details card
//   ✓ Event history card
//   ✓ Action buttons disabled (not hidden) for illegal transitions, with tooltips
//   ✓ Delete confirmation modal names target instance
//   ✓ Polling during transitional states (via useInstance hook)
//   ✓ Skeleton loading state
//   ✓ Error state with request_id
//   ✓ 5xx generic message only
//
// Source: 09-01 §Instance Detail Page, M7 spec §C.

import React, { useState } from 'react';
import { useNavigate, useParams } from 'react-router-dom';
import { instancesApi } from '../api/client';
import { ActionButton } from '../components/ActionButton';
import { CopyButton } from '../components/CopyButton';
import { Breadcrumb } from '../components/Layout';
import { Modal } from '../components/Modal';
import { SkeletonCard } from '../components/Skeleton';
import { StatusBadge } from '../components/StatusBadge';
import { useToast } from '../components/Toast';
import { useEvents } from '../hooks/useEvents';
import { useInstance } from '../hooks/useInstance';
import { ApiException } from '../types';
import {
  buildSSHCommand,
  formatDate,
  formatRelativeTime,
  imageDisplayName,
  instanceTypeDisplay,
} from '../utils/format';
import type { ActionName } from '../utils/states';

export function InstanceDetailPage() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { showToast } = useToast();

  const { instance, loading, error, refresh } = useInstance(id ?? '');
  const { events, loading: eventsLoading } = useEvents(id ?? '');

  const [actionLoading, setActionLoading] = useState<ActionName | null>(null);
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [deleteLoading, setDeleteLoading] = useState(false);

  async function runAction(action: ActionName) {
    if (!instance) return;
    setActionLoading(action);
    try {
      if (action === 'stop')   await instancesApi.stop(instance.id);
      if (action === 'start')  await instancesApi.start(instance.id);
      if (action === 'reboot') await instancesApi.reboot(instance.id);
      showToast(`${cap(action)} request for '${instance.name}' initiated`, 'info');
      refresh();
    } catch (err) {
      apiErr(err, showToast);
    } finally {
      setActionLoading(null);
    }
  }

  async function confirmDelete() {
    if (!instance) return;
    setDeleteLoading(true);
    try {
      await instancesApi.delete(instance.id);
      showToast(`Deleting '${instance.name}'…`, 'info');
      navigate('/instances');
    } catch (err) {
      apiErr(err, showToast);
      setDeleteLoading(false);
    }
  }

  // ── Loading ──
  if (loading) {
    return (
      <div style={{ padding: '32px 40px' }}>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8, marginBottom: 20 }}>
          <div style={{ width: 120, height: 12, borderRadius: 4, background: 'rgba(255,255,255,0.06)' }} />
        </div>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14, maxWidth: 960 }}>
          {[0, 1, 2, 3].map((i) => <SkeletonCard key={i} />)}
        </div>
      </div>
    );
  }

  // ── Error ──
  if (error) {
    return (
      <div style={{ padding: '32px 40px' }}>
        <Breadcrumb items={[{ label: 'Instances', path: '/instances' }, { label: 'Error' }]} />
        <div style={{
          padding: '18px 22px',
          borderRadius: 8,
          border: '1px solid rgba(248,113,113,0.28)',
          background: 'rgba(248,113,113,0.06)',
          color: '#f87171',
          fontSize: 13,
          maxWidth: 520,
        }}>
          <p style={{ margin: 0 }}>
            {error.isServerError
              ? 'Could not load instance details. Please try again.'
              : error.message}
          </p>
          {error.requestId && (
            <p style={{ margin: '5px 0 0', color: '#4b5563', fontSize: 11, fontFamily: 'IBM Plex Mono, monospace' }}>
              request_id: {error.requestId}
            </p>
          )}
          <button
            onClick={refresh}
            style={{ marginTop: 12, padding: '5px 12px', borderRadius: 5, border: '1px solid rgba(248,113,113,0.3)', background: 'transparent', color: '#f87171', cursor: 'pointer', fontSize: 12 }}
          >
            Retry
          </button>
        </div>
      </div>
    );
  }

  if (!instance) return null;

  const isRunning = instance.status === 'running';
  const sshCmd = buildSSHCommand(instance.public_ip);

  return (
    <div style={{ padding: '32px 40px' }}>
      <Breadcrumb items={[
        { label: 'Instances', path: '/instances' },
        { label: instance.name ?? instance.id },
      ]} />

      {/* ── Header ── */}
      <div style={{
        display: 'flex',
        alignItems: 'flex-start',
        justifyContent: 'space-between',
        marginBottom: 28,
        gap: 20,
      }}>
        <div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12, marginBottom: 5 }}>
            <h1 style={{
              margin: 0,
              color: '#f3f4f6',
              fontSize: 20,
              fontWeight: 600,
              fontFamily: 'IBM Plex Mono, monospace',
            }}>
              {instance.name}
            </h1>
            <StatusBadge status={instance.status} />
          </div>
          <div style={{ color: '#374151', fontSize: 11, fontFamily: 'IBM Plex Mono, monospace' }}>
            {instance.id}
          </div>
        </div>

        {/* Action buttons */}
        <div style={{ display: 'flex', gap: 7, flexShrink: 0 }}>
          <ActionButton action="stop"   status={instance.status} label="Stop"   onClick={() => runAction('stop')}   loading={actionLoading === 'stop'}   size="md" />
          <ActionButton action="start"  status={instance.status} label="Start"  onClick={() => runAction('start')}  loading={actionLoading === 'start'}  size="md" />
          <ActionButton action="reboot" status={instance.status} label="Reboot" onClick={() => runAction('reboot')} loading={actionLoading === 'reboot'} size="md" />
          <ActionButton action="delete" status={instance.status} label="Delete" variant="danger" onClick={() => setDeleteOpen(true)} loading={actionLoading === 'delete'} size="md" />
        </div>
      </div>

      {/* ── Cards grid ── */}
      <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 14, maxWidth: 960 }}>

        {/* Connection Info — spans full width, near top per spec */}
        <Card title="Connection" span={2}>
          {!isRunning && (
            <div style={{
              padding: '9px 13px',
              borderRadius: 6,
              background: 'rgba(251,146,60,0.07)',
              border: '1px solid rgba(251,146,60,0.18)',
              color: '#fb923c',
              fontSize: 12,
              marginBottom: 14,
            }}>
              Connection details are available once the instance is running.
            </div>
          )}
          <DetailRow label="Public IP">
            {instance.public_ip && isRunning ? (
              <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <code style={mono}>{instance.public_ip}</code>
                <CopyButton value={instance.public_ip} label="public IP" />
              </span>
            ) : (
              <span style={dimMono}>{instance.public_ip ?? 'Assigning…'}</span>
            )}
          </DetailRow>
          <DetailRow label="Private IP">
            <span style={isRunning ? mono : dimMono}>
              {instance.private_ip ?? '—'}
            </span>
          </DetailRow>
          <DetailRow label="SSH Command">
            {isRunning && sshCmd ? (
              <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                <code style={{ ...mono, background: 'rgba(147,197,253,0.07)', padding: '3px 8px', borderRadius: 4, color: '#93c5fd' }}>
                  {sshCmd}
                </code>
                <CopyButton value={sshCmd} label="SSH command" />
              </span>
            ) : (
              <span style={dimMono}>Available when running</span>
            )}
          </DetailRow>
        </Card>

        {/* Configuration */}
        <Card title="Configuration">
          <DetailRow label="Image">{imageDisplayName(instance.image_id ?? '')}</DetailRow>
          <DetailRow label="Plan">
            <span style={{ ...mono, color: '#9ca3af' }}>{instanceTypeDisplay(instance.instance_type ?? '')}</span>
          </DetailRow>
          <DetailRow label="Region">{instance.region}</DetailRow>
          <DetailRow label="Zone">{instance.availability_zone}</DetailRow>
          <DetailRow label="Created">{formatDate(instance.created_at ?? '')}</DetailRow>
          <DetailRow label="Updated">{formatDate(instance.updated_at ?? '')}</DetailRow>
        </Card>

        {/* Status / metadata */}
        <Card title="Status">
          <DetailRow label="State"><StatusBadge status={instance.status} size="sm" /></DetailRow>
          <DetailRow label="Instance ID">
            <span style={{ ...mono, color: '#6b7280', fontSize: 11 }}>{instance.id}</span>
          </DetailRow>
        </Card>

        {/* Event History — full width */}
        <Card title="Event History" span={2}>
          {eventsLoading ? (
            <p style={{ margin: 0, color: '#4b5563', fontSize: 13 }}>Loading events…</p>
          ) : events.length === 0 ? (
            <p style={{ margin: 0, color: '#374151', fontSize: 13 }}>
              No events recorded yet.
            </p>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column' }}>
              {events.map((ev) => (
                <div
                  key={ev.id}
                  style={{
                    display: 'grid',
                    gridTemplateColumns: '140px 220px 1fr',
                    gap: 14,
                    padding: '8px 0',
                    borderBottom: '1px solid rgba(255,255,255,0.04)',
                    fontSize: 12,
                    alignItems: 'start',
                  }}
                >
                  <span style={{ color: '#4b5563', fontFamily: 'IBM Plex Mono, monospace' }}>
                    {formatRelativeTime(ev.created_at ?? '')}
                  </span>
                  <span style={{ color: '#60a5fa', fontFamily: 'IBM Plex Mono, monospace' }}>
                    {ev.event_type}
                  </span>
                  <span style={{ color: '#9ca3af', lineHeight: 1.4 }}>
                    {ev.message ?? ''}
                    {ev.actor && ev.actor !== 'system' && (
                      <span style={{ color: '#374151', marginLeft: 6, fontSize: 11 }}>
                        — {ev.actor}
                      </span>
                    )}
                  </span>
                </div>
              ))}
            </div>
          )}
        </Card>
      </div>

      {/* ── Delete modal ── */}
      <Modal
        open={deleteOpen}
        title="Delete Instance"
        confirmLabel="Delete"
        confirmDanger
        onConfirm={confirmDelete}
        onCancel={() => setDeleteOpen(false)}
        loading={deleteLoading}
      >
        <p style={{ margin: 0 }}>
          Permanently delete{' '}
          <strong style={{ color: '#f3f4f6', fontFamily: 'IBM Plex Mono, monospace' }}>
            {instance.name}
          </strong>
          ? All data will be lost and this action cannot be undone.
        </p>
      </Modal>
    </div>
  );
}

// ── Sub-components ────────────────────────────────────────────────────────────

function Card({
  title,
  children,
  span,
}: {
  title: string;
  children: React.ReactNode;
  span?: 1 | 2;
}) {
  return (
    <div
      style={{
        padding: '20px 22px',
        border: '1px solid rgba(255,255,255,0.08)',
        borderRadius: 8,
        background: 'rgba(255,255,255,0.015)',
        gridColumn: span === 2 ? '1 / -1' : undefined,
      }}
    >
      <div style={{
        fontSize: 10,
        fontWeight: 600,
        color: '#374151',
        letterSpacing: '0.1em',
        marginBottom: 14,
        fontFamily: 'IBM Plex Sans, sans-serif',
      }}>
        {title.toUpperCase()}
      </div>
      {children}
    </div>
  );
}

function DetailRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div style={{
      display: 'flex',
      alignItems: 'center',
      gap: 12,
      padding: '7px 0',
      borderBottom: '1px solid rgba(255,255,255,0.04)',
    }}>
      <span style={{
        width: 110,
        flexShrink: 0,
        color: '#4b5563',
        fontSize: 12,
        fontFamily: 'IBM Plex Sans, sans-serif',
      }}>
        {label}
      </span>
      <span style={{
        color: '#d1d5db',
        fontSize: 13,
        display: 'flex',
        alignItems: 'center',
        gap: 8,
        minWidth: 0,
        wordBreak: 'break-all',
      }}>
        {children}
      </span>
    </div>
  );
}

// ── Style helpers ─────────────────────────────────────────────────────────────

const mono: React.CSSProperties = {
  fontFamily: 'IBM Plex Mono, monospace',
  fontSize: 12,
  color: '#d1d5db',
};

const dimMono: React.CSSProperties = {
  fontFamily: 'IBM Plex Mono, monospace',
  fontSize: 12,
  color: '#374151',
};

// ── Error / action helpers ────────────────────────────────────────────────────

function cap(s: string) {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function apiErr(
  err: unknown,
  showToast: (m: string, t: 'error', rid?: string) => void,
) {
  if (err instanceof ApiException) {
    const msg = err.isServerError
      ? `An internal error occurred. (request_id: ${err.requestId})`
      : err.message;
    showToast(msg, 'error', err.isServerError ? err.requestId : undefined);
  }
}
