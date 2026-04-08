// components/CopyButton.tsx — Inline copy-to-clipboard button.
// Source: 09-01 §"copy to clipboard action on hover".

import React, { useState } from 'react';

interface Props {
  value: string;
  label?: string;
}

export function CopyButton({ value, label }: Props) {
  const [copied, setCopied] = useState(false);

  async function handleCopy() {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // Fallback for non-HTTPS (dev proxy, etc.).
      try {
        const el = document.createElement('textarea');
        el.value = value;
        el.style.position = 'fixed';
        el.style.opacity = '0';
        document.body.appendChild(el);
        el.focus();
        el.select();
        document.execCommand('copy');
        document.body.removeChild(el);
      } catch {
        return;
      }
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  }

  return (
    <button
      onClick={handleCopy}
      title={`Copy ${label ?? value}`}
      style={{
        background: 'transparent',
        border: '1px solid rgba(255,255,255,0.1)',
        borderRadius: 4,
        padding: '2px 7px',
        cursor: 'pointer',
        color: copied ? '#34d399' : '#6b7280',
        fontSize: 11,
        fontFamily: 'IBM Plex Mono, monospace',
        transition: 'color 0.18s',
        whiteSpace: 'nowrap',
        flexShrink: 0,
      }}
    >
      {copied ? '✓' : 'copy'}
    </button>
  );
}
