// pages/InstancesListPage.tsx — Primary instance list dashboard.
//
// M7 requirements satisfied:
//   ✓ Table: name, status, public IP, private IP, image, plan, zone, actions
//   ✓ Default sort: newest first (from API: ORDER BY created_at DESC)
//   ✓ Empty state: active headline + Create Instance CTA + docs link
//   ✓ Loading state: skeleton rows (not blank page)
//   ✓ Per-instance actions: stop, start, reboot, delete
//   ✓ Action buttons disabled (not hidden) for illegal transitions, with tooltips
//   ✓ Delete confirmation modal names target instance
//   ✓ Polling for transitional states (via useInstances hook)
//   ✓ Error state with request_id displayed
//   ✓ 5xx shows generic message only
//
// Source: 09-01 §Instance List Page, 09-02 §all states.

import React, { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { instancesApi } from '../api/client';
import { ActionButton } from '../components/ActionButton';
import { CopyButton } from '../components/CopyButton';
import { Modal } from '../components/Modal';
import { Skeleton, SkeletonRow } from '../components/Skeleton';
import { StatusBadge } from '../components/StatusBadge';
import { useToast } from '../components/Toast';
import { useInstances } from '../hooks/useInstances';
import type { Instance } from '../types';
import { ApiException } from '../types';
import { imageDisplayName, instanceTypeDisplay } from '../utils/format';
import type { ActionName } from '../utils/states';

export function InstancesListPage() {
  const { instances, loading, error, refresh } = useInstances();
  const { showToast } = useToast();
  const navigate = useNavigate();
  const [actionLoading, setActionLoading] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<Instance | null>(null);
  const [deleteLoading, setDeleteLoading] = useState(false);

  async function runAction(instance: Instance, action: ActionName) {
    const key = `${instance.id}:${action}`;
    setActionLoading(key);
    try {
      if (action === 'stop')   await instancesApi.stop(instance.id);
      if (action === 'start')  await instancesApi.start(instance.id);
      if (action === 'reboot') await instancesApi.reboot(instance.id);
      showToast(`${capitalize(action)} request for '${instance.name}' initiated`, 'info');
      refresh();
    } catch (err) {
      dispatchError(err, showToast);
    } finally {
      setActionLoading(null);
    }
  }

  async function confirmDelete() {
    if (!deleteTarget) return;
    setDeleteLoading(true);
    try {
      await instancesApi.delete(deleteTarget.id);
      showToast(`Deleting '${deleteTarget.name}'…`, 'info');
      setDeleteTarget(null);
      refresh();
    } catch (err) {
      dispatchError(err, showToast);
    } finally {
      setDeleteLoading(false);
    }
  }

  return (
    <div style={{ padding: '32px 40px', flex: 1, minWidth: 0 }}>

      {/* ── Header ── */}
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 26 }}>
        <div>
          <h1 style={{ margin: 0, color: '#f3f4f6', fontSize: 21, fontWeight: 600 }}>
            Instances
          </h1>
          {!loading && !error && (
            <p style={{ margin: '3px 0 0', color: '#4b5563', fontSize: 12 }}>
              {instances.length} instance{instances.length !== 1 ? 's' : ''}
            </p>
          )}
        </div>
        <Link
          to="/instances/create"
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 5,
            padding: '8px 16px',
            borderRadius: 6,
            background: '#3b82f6',
            color: '#fff',
            textDecoration: 'none',
            fontSize: 13,
            fontWeight: 500,
            flexShrink: 0,
          }}
        >
          + Create Instance
        </Link>
      </div>

      {/* ── Error state ── */}
      {error && !loading && (
        <div style={{
          padding: '16px 20px',
          border: '1px solid rgba(248,113,113,0.28)',
          borderRadius: 7,
          background: 'rgba(248,113,113,0.06)',
          marginBottom: 20,
        }}>
          <p style={{ margin: 0, color: '#f87171', fontSize: 13 }}>
            {error.isServerError
              ? 'Could not load instances. Please try again.'
              : error.message}
          </p>
          {error.requestId && (
            <p style={{ margin: '5px 0 0', color: '#4b5563', fontSize: 11, fontFamily: 'IBM Plex Mono, monospace' }}>
              request_id: {error.requestId}
            </p>
          )}
          <button
            onClick={refresh}
            style={{ marginTop: 10, padding: '5px 12px', borderRadius: 5, border: '1px solid rgba(248,113,113,0.3)', background: 'transparent', color: '#f87171', cursor: 'pointer', fontSize: 12 }}
          >
            Retry
          </button>
        </div>
      )}

      {/* ── Loading skeleton ── */}
      {loading && (
        <table style={{ width: '100%', borderCollapse: 'collapse' }}>
          <thead><THead /></thead>
          <tbody>
            {[0, 1, 2].map((i) => <SkeletonRow key={i} />)}
          </tbody>
        </table>
      )}

      {/* ── Empty state ── */}
      {!loading && !error && instances.length === 0 && (
        <div style={{
          display: 'flex',
          flexDirection: 'column',
          alignItems: 'center',
          justifyContent: 'center',
          padding: '80px 40px',
          border: '1px dashed rgba(255,255,255,0.09)',
          borderRadius: 12,
          textAlign: 'center',
        }}>
          <div style={{ fontSize: 38, marginBottom: 16, opacity: 0.3, lineHeight: 1 }}>⬡</div>
          <h2 style={{ margin: '0 0 8px', color: '#f3f4f6', fontSize: 18, fontWeight: 600 }}>
            Create a virtual machine to get started
          </h2>
          <p style={{ margin: '0 0 24px', color: '#6b7280', fontSize: 13, maxWidth: 380, lineHeight: 1.55 }}>
            Launch your first compute instance. Connect via SSH within minutes of provisioning.
          </p>
          <Link
            to="/instances/create"
            style={{
              padding: '9px 20px',
              borderRadius: 7,
              background: '#3b82f6',
              color: '#fff',
              textDecoration: 'none',
              fontSize: 13,
              fontWeight: 500,
              marginBottom: 14,
            }}
          >
            Create Instance
          </Link>
          <a
            href="https://docs.compute-platform.internal/quickstart"
            target="_blank"
            rel="noopener noreferrer"
            style={{ color: '#4b5563', fontSize: 12, textDecoration: 'none' }}
          >
            Quick Start Guide →
          </a>
        </div>
      )}

      {/* ── Instance table ── */}
      {!loading && !error && instances.length > 0 && (
        <div style={{ overflowX: 'auto' }}>
          <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13, minWidth: 860 }}>
            <thead><THead /></thead>
            <tbody>
              {instances.map((inst) => (
                <InstanceRow
                  key={inst.id}
                  instance={inst}
                  actionLoading={actionLoading}
                  onNavigate={() => navigate(`/instances/${inst.id}`)}
                  onAction={(action) => runAction(inst, action)}
                  onDelete={() => setDeleteTarget(inst)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* ── Delete modal ── */}
      <Modal
        open={deleteTarget !== null}
        title="Delete Instance"
        confirmLabel="Delete"
        confirmDanger
        onConfirm={confirmDelete}
        onCancel={() => setDeleteTarget(null)}
        loading={deleteLoading}
      >
        <p style={{ margin: 0 }}>
          Permanently delete{' '}
          <strong style={{ color: '#f3f4f6', fontFamily: 'IBM Plex Mono, monospace' }}>
            {deleteTarget?.name}
          </strong>
          ? All data on this instance will be lost. This cannot be undone.
        </p>
      </Modal>
    </div>
  );
}

// ── Sub-components ────────────────────────────────────────────────────────────

function THead() {
  const cols = ['NAME', 'STATUS', 'PUBLIC IP', 'PRIVATE IP', 'IMAGE', 'PLAN', 'ZONE', 'ACTIONS'];
  return (
    <tr>
      {cols.map((c) => (
        <th
          key={c}
          style={{
            padding: '9px 14px',
            textAlign: 'left',
            fontSize: 10,
            fontWeight: 600,
            color: '#374151',
            letterSpacing: '0.08em',
            borderBottom: '1px solid rgba(255,255,255,0.08)',
            whiteSpace: 'nowrap',
          }}
        >
          {c}
        </th>
      ))}
    </tr>
  );
}

interface RowProps {
  instance: Instance;
  actionLoading: string | null;
  onNavigate: () => void;
  onAction: (action: ActionName) => void;
  onDelete: () => void;
}

function InstanceRow({ instance: inst, actionLoading, onNavigate, onAction, onDelete }: RowProps) {
  const isDeleting = inst.status === 'deleting' || inst.status === 'deleted';

  return (
    <tr
      style={{
        borderBottom: '1px solid rgba(255,255,255,0.05)',
        opacity: isDeleting ? 0.4 : 1,
        transition: 'opacity 0.2s',
      }}
    >
      {/* Name */}
      <td style={td}>
        <button
          onClick={onNavigate}
          style={{
            background: 'none',
            border: 'none',
            color: '#93c5fd',
            cursor: 'pointer',
            fontSize: 13,
            padding: 0,
            fontFamily: 'IBM Plex Mono, monospace',
            textAlign: 'left',
          }}
        >
          {inst.name}
        </button>
      </td>

      {/* Status */}
      <td style={td}>
        <StatusBadge status={inst.status} size="sm" />
      </td>

      {/* Public IP */}
      <td style={{ ...td, fontFamily: 'IBM Plex Mono, monospace' }}>
        {inst.public_ip ? (
          <span style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
            <span style={{ color: '#d1d5db' }}>{inst.public_ip}</span>
            <CopyButton value={inst.public_ip} label="public IP" />
          </span>
        ) : (
          <span style={{ color: '#2d3340' }}>—</span>
        )}
      </td>

      {/* Private IP */}
      <td style={{ ...td, color: '#6b7280', fontFamily: 'IBM Plex Mono, monospace' }}>
        {inst.private_ip ?? '—'}
      </td>

      {/* Image */}
      <td style={{ ...td, color: '#9ca3af' }}>
        {imageDisplayName(inst.image_id ?? '')}
      </td>

      {/* Plan */}
      <td style={{ ...td, color: '#6b7280', fontFamily: 'IBM Plex Mono, monospace', fontSize: 12 }}>
        {inst.instance_type}
      </td>

      {/* Zone */}
      <td style={{ ...td, color: '#6b7280', fontSize: 12 }}>
        {inst.availability_zone}
      </td>

      {/* Actions */}
      <td style={{ ...td, paddingRight: 16 }}>
        <div style={{ display: 'flex', gap: 5, flexWrap: 'wrap', alignItems: 'center' }}>
          <ActionButton action="stop"   status={inst.status} label="Stop"   onClick={() => onAction('stop')}   loading={actionLoading === `${inst.id}:stop`} />
          <ActionButton action="start"  status={inst.status} label="Start"  onClick={() => onAction('start')}  loading={actionLoading === `${inst.id}:start`} />
          <ActionButton action="reboot" status={inst.status} label="Reboot" onClick={() => onAction('reboot')} loading={actionLoading === `${inst.id}:reboot`} />
          <ActionButton action="delete" status={inst.status} label="Delete" variant="danger" onClick={onDelete} loading={actionLoading === `${inst.id}:delete`} />
        </div>
      </td>
    </tr>
  );
}

// ── Helpers ───────────────────────────────────────────────────────────────────

const td: React.CSSProperties = { padding: '12px 14px', verticalAlign: 'middle', color: '#d1d5db' };

function capitalize(s: string) {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function dispatchError(err: unknown, showToast: (m: string, t: 'error', rid?: string) => void) {
  if (err instanceof ApiException) {
    const msg = err.isServerError
      ? `An internal error occurred. (request_id: ${err.requestId})`
      : err.message;
    showToast(msg, 'error', err.isServerError ? err.requestId : undefined);
  }
}
