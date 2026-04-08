// components/StatusBadge.tsx — 9-state color+icon+spinner badge.
//
// All 9 canonical states render with correct color + icon.
// Source: 09-01 §Status Indicators, LIFECYCLE_STATE_MACHINE_V1, M7 spec §Status rendering.

import React from 'react';
import type { InstanceStatus } from '../types';
import { STATE_DISPLAY } from '../utils/states';

interface Props {
  status: InstanceStatus;
  size?: 'sm' | 'md';
}

export function StatusBadge({ status, size = 'md' }: Props) {
  const display = STATE_DISPLAY[status] ?? STATE_DISPLAY['failed'];
  const iconSize = size === 'sm' ? 7 : 8;
  const fontSize = size === 'sm' ? '11px' : '12px';
  const padding = size === 'sm' ? '2px 7px' : '3px 9px';

  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 5,
        padding,
        borderRadius: '4px',
        border: `1px solid ${display.borderColor}`,
        background: display.bgColor,
        color: display.color,
        fontSize,
        fontFamily: 'IBM Plex Mono, monospace',
        fontWeight: 500,
        letterSpacing: '0.02em',
        whiteSpace: 'nowrap',
        lineHeight: 1,
      }}
    >
      {display.spinner ? (
        <SpinnerIcon size={iconSize} color={display.color} />
      ) : display.isError ? (
        <ExclamIcon size={iconSize} color={display.color} />
      ) : (
        <CircleIcon size={iconSize} color={display.color} filled={status === 'running'} />
      )}
      {display.label}
    </span>
  );
}

function SpinnerIcon({ size, color }: { size: number; color: string }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      style={{ animation: 'badge-spin 1s linear infinite', flexShrink: 0 }}
    >
      <style>{`@keyframes badge-spin { from { transform: rotate(0deg); } to { transform: rotate(360deg); } }`}</style>
      <circle
        cx="8"
        cy="8"
        r="6"
        fill="none"
        stroke={color}
        strokeWidth="2.5"
        strokeDasharray="22 8"
        strokeLinecap="round"
      />
    </svg>
  );
}

function CircleIcon({ size, color, filled }: { size: number; color: string; filled: boolean }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" style={{ flexShrink: 0 }}>
      <circle
        cx="8"
        cy="8"
        r="6"
        fill={filled ? color : 'none'}
        stroke={color}
        strokeWidth="2"
      />
    </svg>
  );
}

function ExclamIcon({ size, color }: { size: number; color: string }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" style={{ flexShrink: 0 }}>
      <circle cx="8" cy="8" r="7" fill="none" stroke={color} strokeWidth="1.5" />
      <line x1="8" y1="4.5" x2="8" y2="9" stroke={color} strokeWidth="2" strokeLinecap="round" />
      <circle cx="8" cy="11.5" r="1" fill={color} />
    </svg>
  );
}
