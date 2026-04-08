// components/Layout.tsx — Left sidebar navigation + main content area.
//
// Persistent left-hand sidebar per 09-01 §Navigation Structure.
// Breadcrumb on nested pages (detail, create).

import React from 'react';
import { Link, useLocation } from 'react-router-dom';

const NAV = [
  { label: 'Instances', path: '/instances', icon: '⬡' },
  { label: 'SSH Keys',  path: '/ssh-keys',  icon: '⚿' },
];

export function Layout({ children }: { children: React.ReactNode }) {
  const { pathname } = useLocation();

  return (
    <div
      style={{
        display: 'flex',
        height: '100vh',
        overflow: 'hidden',
        background: '#0f1117',
        fontFamily: 'IBM Plex Sans, sans-serif',
      }}
    >
      {/* ── Sidebar ── */}
      <aside
        style={{
          width: 216,
          flexShrink: 0,
          background: '#12151e',
          borderRight: '1px solid rgba(255,255,255,0.07)',
          display: 'flex',
          flexDirection: 'column',
        }}
      >
        {/* Logo */}
        <div
          style={{
            padding: '18px 20px 16px',
            borderBottom: '1px solid rgba(255,255,255,0.07)',
            display: 'flex',
            alignItems: 'center',
            gap: 6,
          }}
        >
          <span
            style={{
              fontFamily: 'IBM Plex Mono, monospace',
              fontSize: 13,
              fontWeight: 500,
              color: '#f3f4f6',
              letterSpacing: '0.06em',
            }}
          >
            COMPUTE
          </span>
          <span style={{ color: '#3b82f6', fontSize: 16, lineHeight: 1 }}>›</span>
        </div>

        {/* Section label */}
        <div
          style={{
            padding: '14px 20px 6px',
            color: '#374151',
            fontSize: 10,
            letterSpacing: '0.1em',
            fontWeight: 600,
          }}
        >
          INFRASTRUCTURE
        </div>

        {/* Nav links */}
        <nav style={{ flex: 1 }}>
          {NAV.map((item) => {
            const active = pathname.startsWith(item.path);
            return (
              <Link
                key={item.path}
                to={item.path}
                style={{
                  display: 'flex',
                  alignItems: 'center',
                  gap: 9,
                  padding: '9px 20px',
                  textDecoration: 'none',
                  color: active ? '#e5e7eb' : '#6b7280',
                  background: active ? 'rgba(59,130,246,0.09)' : 'transparent',
                  borderLeft: active ? '2px solid #3b82f6' : '2px solid transparent',
                  fontSize: 13,
                  transition: 'all 0.12s',
                }}
              >
                <span style={{ fontSize: 15, opacity: active ? 1 : 0.7 }}>{item.icon}</span>
                {item.label}
              </Link>
            );
          })}
        </nav>

        {/* Footer */}
        <div
          style={{
            padding: '14px 20px',
            borderTop: '1px solid rgba(255,255,255,0.06)',
            color: '#374151',
            fontSize: 10,
            fontFamily: 'IBM Plex Mono, monospace',
            letterSpacing: '0.04em',
          }}
        >
          Phase 1 · M7
        </div>
      </aside>

      {/* ── Main ── */}
      <main style={{ flex: 1, overflow: 'auto', display: 'flex', flexDirection: 'column' }}>
        {children}
      </main>
    </div>
  );
}

// Breadcrumb — used on detail and create pages.
// Source: 09-01 §"breadcrumb navigation element".
export function Breadcrumb({ items }: { items: Array<{ label: string; path?: string }> }) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 6,
        fontSize: 12,
        color: '#4b5563',
        fontFamily: 'IBM Plex Mono, monospace',
        marginBottom: 22,
      }}
    >
      {items.map((item, i) => (
        <React.Fragment key={i}>
          {i > 0 && <span style={{ color: '#2d3340' }}>›</span>}
          {item.path ? (
            <Link
              to={item.path}
              style={{ color: '#4b5563', textDecoration: 'none' }}
            >
              {item.label}
            </Link>
          ) : (
            <span style={{ color: '#9ca3af' }}>{item.label}</span>
          )}
        </React.Fragment>
      ))}
    </div>
  );
}
