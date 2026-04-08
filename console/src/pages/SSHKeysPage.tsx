// pages/SSHKeysPage.tsx — SSH key management CRUD page.
//
// M7 requirements satisfied:
//   ✓ List keys: name, fingerprint, key type, creation date
//   ✓ Add key: name + public key text, server-side validation, inline errors
//   ✓ Delete key: confirmation modal before delete
//   ✓ Full key never displayed after creation — only fingerprint shown
//   ✓ Empty state with headline + CTA
//   ✓ Loading skeleton
//   ✓ Error states with request_id
//   ✓ 5xx generic message only
//
// Source: M7 spec §D SSH Key Management, 10-02-ssh-key-and-secret-handling.md.

import React, { useState } from 'react';
import { sshKeysApi } from '../api/client';
import { Modal } from '../components/Modal';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import { useSSHKeys } from '../hooks/useSSHKeys';
import type { SSHKey } from '../types';
import { ApiException } from '../types';
import { formatDate } from '../utils/format';

export function SSHKeysPage() {
  const { keys, loading, error, refresh } = useSSHKeys();
  const { showToast } = useToast();

  const [showAdd, setShowAdd] = useState(false);
  const [addName, setAddName] = useState('');
  const [addKey, setAddKey] = useState('');
  const [addLoading, setAddLoading] = useState(false);
  const [addErrors, setAddErrors] = useState<Partial<{ name: string; public_key: string; _general: string }>>({});

  const [deleteTarget, setDeleteTarget] = useState<SSHKey | null>(null);
  const [deleteLoading, setDeleteLoading] = useState(false);

  function resetAdd() {
    setAddName('');
    setAddKey('');
    setAddErrors({});
    setShowAdd(false);
  }

  async function handleAdd() {
    const e: typeof addErrors = {};
    if (!addName.trim()) e.name = 'Name is required.';
    if (!addKey.trim())  e.public_key = 'Public key is required.';
    if (Object.keys(e).length) { setAddErrors(e); return; }

    setAddLoading(true);
    setAddErrors({});
    try {
      await sshKeysApi.create({ name: addName.trim(), public_key: addKey.trim() });
      showToast(`SSH key '${addName}' added`, 'success');
      resetAdd();
      refresh();
    } catch (err) {
      if (err instanceof ApiException) {
        if (err.isServerError) {
          setAddErrors({ _general: `An internal error occurred. (request_id: ${err.requestId})` });
        } else if (err.body.error.details?.length) {
          const fe: typeof addErrors = {};
          for (const d of err.body.error.details) {
            if (d.target) (fe as Record<string, string>)[d.target] = d.message;
          }
          setAddErrors(fe);
        } else {
          setAddErrors({ _general: err.message });
        }
      }
    } finally {
      setAddLoading(false);
    }
  }

  async function confirmDelete() {
    if (!deleteTarget) return;
    setDeleteLoading(true);
    try {
      await sshKeysApi.delete(deleteTarget.id ?? '');
      showToast(`SSH key '${deleteTarget.name}' deleted`, 'info');
      setDeleteTarget(null);
      refresh();
    } catch (err) {
      if (err instanceof ApiException) {
        const msg = err.isServerError
          ? `Internal error. (request_id: ${err.requestId})`
          : err.message;
        showToast(msg, 'error', err.isServerError ? err.requestId : undefined);
      }
    } finally {
      setDeleteLoading(false);
    }
  }

  return (
    <div style={{ padding: '32px 40px', maxWidth: 900 }}>

      {/* ── Header ── */}
      <div style={{ display: 'flex', alignItems: 'flex-start', justifyContent: 'space-between', marginBottom: 26 }}>
        <div>
          <h1 style={{ margin: 0, color: '#f3f4f6', fontSize: 21, fontWeight: 600 }}>SSH Keys</h1>
          <p style={{ margin: '3px 0 0', color: '#4b5563', fontSize: 13 }}>
            Manage public SSH keys for instance authentication.
          </p>
        </div>
        <button
          onClick={() => { setShowAdd(!showAdd); if (showAdd) resetAdd(); }}
          style={{
            padding: '8px 16px',
            borderRadius: 6,
            border: showAdd ? '1px solid rgba(59,130,246,0.35)' : 'none',
            background: showAdd ? 'rgba(59,130,246,0.08)' : '#3b82f6',
            color: showAdd ? '#60a5fa' : '#fff',
            fontSize: 13,
            cursor: 'pointer',
            fontWeight: 500,
            fontFamily: 'IBM Plex Sans, sans-serif',
            flexShrink: 0,
          }}
        >
          {showAdd ? 'Cancel' : '+ Add SSH Key'}
        </button>
      </div>

      {/* ── Add form ── */}
      {showAdd && (
        <div style={{
          padding: '20px 22px',
          border: '1px solid rgba(59,130,246,0.2)',
          borderRadius: 8,
          background: 'rgba(59,130,246,0.03)',
          marginBottom: 22,
        }}>
          <div style={{ fontSize: 13, fontWeight: 600, color: '#93c5fd', marginBottom: 16 }}>
            Add New SSH Key
          </div>

          {addErrors._general && (
            <div style={{ color: '#f87171', fontSize: 12, marginBottom: 12 }}>
              {addErrors._general}
            </div>
          )}

          {/* Name */}
          <div style={{ marginBottom: 14 }}>
            <label style={labelSt}>Key Name</label>
            <input
              type="text"
              value={addName}
              onChange={(e) => { setAddName(e.target.value); setAddErrors((p) => ({ ...p, name: undefined })); }}
              placeholder="e.g. my-laptop"
              style={{ ...inputSt, borderColor: addErrors.name ? 'rgba(248,113,113,0.4)' : undefined }}
            />
            {addErrors.name && <Err msg={addErrors.name} />}
          </div>

          {/* Public key */}
          <div style={{ marginBottom: 16 }}>
            <label style={labelSt}>Public Key</label>
            <textarea
              value={addKey}
              onChange={(e) => { setAddKey(e.target.value); setAddErrors((p) => ({ ...p, public_key: undefined })); }}
              placeholder={'ssh-ed25519 AAAA... user@host'}
              rows={4}
              style={{
                ...inputSt,
                resize: 'vertical',
                minHeight: 78,
                borderColor: addErrors.public_key ? 'rgba(248,113,113,0.4)' : undefined,
              }}
            />
            {addErrors.public_key && <Err msg={addErrors.public_key} />}
            <div style={{ marginTop: 4, fontSize: 11, color: '#374151' }}>
              Accepted types: ssh-ed25519, ecdsa-sha2-nistp256/384/521
            </div>
          </div>

          <button
            onClick={handleAdd}
            disabled={addLoading}
            style={{
              padding: '7px 18px',
              borderRadius: 6,
              border: 'none',
              background: '#3b82f6',
              color: '#fff',
              fontSize: 13,
              cursor: addLoading ? 'not-allowed' : 'pointer',
              opacity: addLoading ? 0.6 : 1,
              fontFamily: 'IBM Plex Sans, sans-serif',
              fontWeight: 500,
            }}
          >
            {addLoading ? 'Adding…' : 'Add Key'}
          </button>
        </div>
      )}

      {/* ── API error ── */}
      {error && !loading && (
        <div style={{ color: '#f87171', fontSize: 13, padding: '12px 16px', border: '1px solid rgba(248,113,113,0.25)', borderRadius: 7, marginBottom: 16 }}>
          Could not load SSH keys.
          {error.requestId && (
            <span style={{ color: '#4b5563', fontFamily: 'IBM Plex Mono, monospace', fontSize: 11, marginLeft: 8 }}>
              request_id: {error.requestId}
            </span>
          )}
        </div>
      )}

      {/* ── Loading ── */}
      {loading && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          {[0, 1].map((i) => <Skeleton key={i} height={50} borderRadius="7px" />)}
        </div>
      )}

      {/* ── Empty state ── */}
      {!loading && !error && keys.length === 0 && !showAdd && (
        <div style={{
          padding: '64px 40px',
          border: '1px dashed rgba(255,255,255,0.09)',
          borderRadius: 10,
          textAlign: 'center',
        }}>
          <div style={{ fontSize: 30, marginBottom: 12, opacity: 0.3 }}>⚿</div>
          <h2 style={{ margin: '0 0 8px', color: '#f3f4f6', fontSize: 17, fontWeight: 600 }}>
            No SSH keys yet
          </h2>
          <p style={{ margin: '0 0 20px', color: '#6b7280', fontSize: 13, maxWidth: 340, marginInline: 'auto', lineHeight: 1.5 }}>
            Add a public SSH key to enable password-free access to your instances.
          </p>
          <button
            onClick={() => setShowAdd(true)}
            style={{ padding: '8px 18px', borderRadius: 7, border: 'none', background: '#3b82f6', color: '#fff', fontSize: 13, cursor: 'pointer', fontWeight: 500 }}
          >
            Add SSH Key
          </button>
        </div>
      )}

      {/* ── Key table ── */}
      {!loading && !error && keys.length > 0 && (
        <table style={{ width: '100%', borderCollapse: 'collapse', fontSize: 13 }}>
          <thead>
            <tr>
              {['NAME', 'TYPE', 'FINGERPRINT', 'ADDED', ''].map((h, i) => (
                <th
                  key={i}
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
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {keys.map((key) => (
              <tr key={key.id} style={{ borderBottom: '1px solid rgba(255,255,255,0.05)' }}>
                <td style={{ ...tdSt, color: '#d1d5db', fontWeight: 500 }}>{key.name}</td>
                <td style={{ ...tdSt, color: '#9ca3af', fontFamily: 'IBM Plex Mono, monospace', fontSize: 12 }}>{key.key_type}</td>
                <td style={{ ...tdSt, color: '#6b7280', fontFamily: 'IBM Plex Mono, monospace', fontSize: 11, wordBreak: 'break-all' }}>
                  {key.fingerprint}
                </td>
                <td style={{ ...tdSt, color: '#4b5563', fontSize: 12, whiteSpace: 'nowrap' }}>
                  {formatDate(key.created_at ?? '')}
                </td>
                <td style={{ ...tdSt, textAlign: 'right' }}>
                  <button
                    onClick={() => setDeleteTarget(key)}
                    style={{
                      padding: '4px 10px',
                      borderRadius: 5,
                      border: '1px solid rgba(220,38,38,0.28)',
                      background: 'rgba(220,38,38,0.07)',
                      color: '#f87171',
                      fontSize: 12,
                      cursor: 'pointer',
                      fontFamily: 'IBM Plex Sans, sans-serif',
                    }}
                  >
                    Delete
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {/* ── Delete modal ── */}
      <Modal
        open={deleteTarget !== null}
        title="Delete SSH Key"
        confirmLabel="Delete Key"
        confirmDanger
        onConfirm={confirmDelete}
        onCancel={() => setDeleteTarget(null)}
        loading={deleteLoading}
      >
        <p style={{ margin: 0 }}>
          Delete SSH key{' '}
          <strong style={{ color: '#f3f4f6', fontFamily: 'IBM Plex Mono, monospace' }}>
            {deleteTarget?.name}
          </strong>
          ? Existing instances using this key will continue to work, but the key
          cannot be used for newly launched instances.
        </p>
      </Modal>
    </div>
  );
}

// ── Style helpers ─────────────────────────────────────────────────────────────

const labelSt: React.CSSProperties = {
  display: 'block',
  fontSize: 11,
  fontWeight: 600,
  color: '#4b5563',
  marginBottom: 6,
  letterSpacing: '0.07em',
  fontFamily: 'IBM Plex Sans, sans-serif',
};

const inputSt: React.CSSProperties = {
  width: '100%',
  padding: '9px 12px',
  borderRadius: 6,
  border: '1px solid rgba(255,255,255,0.11)',
  background: 'rgba(255,255,255,0.04)',
  color: '#f3f4f6',
  fontSize: 13,
  fontFamily: 'IBM Plex Mono, monospace',
  outline: 'none',
  boxSizing: 'border-box',
};

const tdSt: React.CSSProperties = { padding: '12px 14px', verticalAlign: 'middle' };

function Err({ msg }: { msg: string }) {
  return <div style={{ marginTop: 4, color: '#f87171', fontSize: 12 }}>{msg}</div>;
}
