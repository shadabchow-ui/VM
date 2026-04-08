// components/ActionButton.tsx — State-aware button that is disabled (not hidden) for illegal transitions.
//
// Tooltip explains why the action is unavailable.
// Source: M7 spec "Action buttons disabled (not hidden) for illegal transitions, with tooltip".

import React, { useState } from 'react';
import type { InstanceStatus } from '../types';
import type { ActionName } from '../utils/states';
import { isActionAllowed } from '../utils/states';

interface Props {
  action: ActionName;
  status: InstanceStatus;
  label: string;
  onClick: () => void;
  loading?: boolean;
  variant?: 'default' | 'danger';
  size?: 'sm' | 'md';
}

const REASON: Record<ActionName, string> = {
  start:  'Instance must be stopped to start',
  stop:   'Instance must be running to stop',
  reboot: 'Instance must be running to reboot',
  delete: 'Cannot delete in current state',
};

export function ActionButton({
  action,
  status,
  label,
  onClick,
  loading = false,
  variant = 'default',
  size = 'sm',
}: Props) {
  const allowed = isActionAllowed(status, action);
  const disabled = !allowed || loading;
  const [tip, setTip] = useState(false);

  const pad = size === 'sm' ? '5px 11px' : '8px 16px';
  const fs = size === 'sm' ? 12 : 13;

  const borderColor =
    variant === 'danger' ? 'rgba(220,38,38,0.35)' : 'rgba(255,255,255,0.1)';
  const bg =
    variant === 'danger'
      ? disabled ? 'rgba(220,38,38,0.08)' : 'rgba(220,38,38,0.15)'
      : disabled ? 'rgba(255,255,255,0.03)' : 'rgba(255,255,255,0.07)';
  const color =
    variant === 'danger'
      ? disabled ? 'rgba(248,113,113,0.35)' : '#f87171'
      : disabled ? 'rgba(255,255,255,0.22)' : '#d1d5db';

  return (
    <div style={{ position: 'relative', display: 'inline-block' }}>
      <button
        aria-disabled={disabled}
        aria-label={disabled ? `${label} — ${REASON[action]}` : label}
        onClick={disabled ? undefined : onClick}
        onMouseEnter={() => !allowed && setTip(true)}
        onMouseLeave={() => setTip(false)}
        style={{
          padding: pad,
          borderRadius: 5,
          border: `1px solid ${borderColor}`,
          background: bg,
          color,
          fontSize: fs,
          cursor: disabled ? 'not-allowed' : 'pointer',
          fontFamily: 'IBM Plex Sans, sans-serif',
          fontWeight: 500,
          transition: 'background 0.12s, color 0.12s',
          whiteSpace: 'nowrap',
        }}
      >
        {loading ? '…' : label}
      </button>

      {tip && !allowed && (
        <div
          style={{
            position: 'absolute',
            bottom: 'calc(100% + 6px)',
            left: '50%',
            transform: 'translateX(-50%)',
            background: '#0d1018',
            border: '1px solid rgba(255,255,255,0.1)',
            borderRadius: 5,
            padding: '5px 9px',
            fontSize: 11,
            color: '#9ca3af',
            whiteSpace: 'nowrap',
            zIndex: 200,
            boxShadow: '0 4px 14px rgba(0,0,0,0.5)',
            fontFamily: 'IBM Plex Sans, sans-serif',
            pointerEvents: 'none',
          }}
        >
          {REASON[action]}
        </div>
      )}
    </div>
  );
}
