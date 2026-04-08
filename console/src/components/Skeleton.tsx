// components/Skeleton.tsx — Shimmer skeleton primitives for loading states.
// Source: 09-02 §Loading States — skeleton screens with shimmer animation.

import React from 'react';

interface SkeletonProps {
  width?: string | number;
  height?: string | number;
  borderRadius?: string;
  style?: React.CSSProperties;
}

const shimmerStyle = `
  @keyframes skeleton-shimmer {
    0%   { background-position: -600px 0; }
    100% { background-position:  600px 0; }
  }
`;

export function Skeleton({ width = '100%', height = 16, borderRadius = '4px', style }: SkeletonProps) {
  return (
    <>
      <style>{shimmerStyle}</style>
      <span
        style={{
          display: 'block',
          width,
          height,
          borderRadius,
          background:
            'linear-gradient(90deg, rgba(255,255,255,0.04) 25%, rgba(255,255,255,0.09) 50%, rgba(255,255,255,0.04) 75%)',
          backgroundSize: '1200px 100%',
          animation: 'skeleton-shimmer 1.6s infinite',
          ...style,
        }}
      />
    </>
  );
}

export function SkeletonRow() {
  const widths = [120, 80, 90, 80, 100, 70, 80, 100];
  return (
    <tr style={{ borderBottom: '1px solid rgba(255,255,255,0.06)' }}>
      {widths.map((w, i) => (
        <td key={i} style={{ padding: '14px 16px' }}>
          <Skeleton width={w} height={13} />
        </td>
      ))}
    </tr>
  );
}

export function SkeletonCard() {
  return (
    <div
      style={{
        padding: '20px 24px',
        border: '1px solid rgba(255,255,255,0.07)',
        borderRadius: '8px',
        display: 'flex',
        flexDirection: 'column',
        gap: '12px',
      }}
    >
      <Skeleton width="38%" height={14} />
      <Skeleton width="62%" height={13} />
      <Skeleton width="80%" height={13} />
      <Skeleton width="55%" height={13} />
    </div>
  );
}
