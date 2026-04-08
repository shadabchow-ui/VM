// components/Modal.tsx — Confirmation modal.
//
// Used for delete confirmation — names the target instance explicitly.
// ESC to close. Click outside to close.
// Source: M7 spec "Delete confirmation modal names target instance".

import React, { useEffect } from 'react';

interface Props {
  open: boolean;
  title: string;
  children: React.ReactNode;
  confirmLabel?: string;
  confirmDanger?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
  loading?: boolean;
}

export function Modal({
  open,
  title,
  children,
  confirmLabel = 'Confirm',
  confirmDanger = false,
  onConfirm,
  onCancel,
  loading = false,
}: Props) {
  useEffect(() => {
    if (!open) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onCancel();
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [open, onCancel]);

  if (!open) return null;

  return (
    <div
      role="dialog"
      aria-modal="true"
      style={{
        position: 'fixed',
        inset: 0,
        zIndex: 1000,
        background: 'rgba(0,0,0,0.72)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        backdropFilter: 'blur(3px)',
      }}
      onClick={(e) => {
        if (e.target === e.currentTarget) onCancel();
      }}
    >
      <div
        style={{
          background: '#181c27',
          border: '1px solid rgba(255,255,255,0.11)',
          borderRadius: 10,
          padding: '28px 32px',
          maxWidth: 480,
          width: '90vw',
          boxShadow: '0 24px 60px rgba(0,0,0,0.55)',
        }}
      >
        <h3
          style={{
            margin: '0 0 14px',
            color: '#f3f4f6',
            fontSize: 16,
            fontFamily: 'IBM Plex Sans, sans-serif',
            fontWeight: 600,
          }}
        >
          {title}
        </h3>
        <div
          style={{
            color: '#9ca3af',
            fontSize: 13,
            marginBottom: 24,
            lineHeight: 1.55,
            fontFamily: 'IBM Plex Sans, sans-serif',
          }}
        >
          {children}
        </div>
        <div style={{ display: 'flex', gap: 10, justifyContent: 'flex-end' }}>
          <button
            onClick={onCancel}
            style={{
              padding: '7px 16px',
              borderRadius: 6,
              border: '1px solid rgba(255,255,255,0.11)',
              background: 'transparent',
              color: '#9ca3af',
              fontSize: 13,
              cursor: 'pointer',
              fontFamily: 'IBM Plex Sans, sans-serif',
            }}
          >
            Cancel
          </button>
          <button
            onClick={onConfirm}
            disabled={loading}
            style={{
              padding: '7px 16px',
              borderRadius: 6,
              border: 'none',
              background: confirmDanger ? '#dc2626' : '#3b82f6',
              color: '#fff',
              fontSize: 13,
              cursor: loading ? 'not-allowed' : 'pointer',
              opacity: loading ? 0.6 : 1,
              fontFamily: 'IBM Plex Sans, sans-serif',
              fontWeight: 500,
            }}
          >
            {loading ? 'Working…' : confirmLabel}
          </button>
        </div>
      </div>
    </div>
  );
}
