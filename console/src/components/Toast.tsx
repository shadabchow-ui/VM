// components/Toast.tsx — Non-blocking toast notification system.
//
// Used for lifecycle action feedback (optimistic updates), errors, and success.
// request_id displayed in error toasts for debugging.
// Source: 09-02 §Lifecycle Action Feedback, API_ERROR_CONTRACT_V1 §7.

import React, { createContext, useCallback, useContext, useState } from 'react';

export type ToastType = 'success' | 'error' | 'info';

interface ToastItem {
  id: string;
  message: string;
  type: ToastType;
  requestId?: string;
}

interface ToastContextValue {
  showToast: (message: string, type?: ToastType, requestId?: string) => void;
}

const ToastContext = createContext<ToastContextValue>({ showToast: () => {} });

export function useToast() {
  return useContext(ToastContext);
}

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<ToastItem[]>([]);

  const showToast = useCallback((message: string, type: ToastType = 'info', requestId?: string) => {
    const id = `${Date.now()}-${Math.random().toString(36).slice(2, 7)}`;
    setToasts((prev) => [...prev.slice(-4), { id, message, type, requestId }]);
    setTimeout(() => {
      setToasts((prev) => prev.filter((t) => t.id !== id));
    }, 5000);
  }, []);

  return (
    <ToastContext.Provider value={{ showToast }}>
      {children}
      <ToastContainer toasts={toasts} />
    </ToastContext.Provider>
  );
}

function ToastContainer({ toasts }: { toasts: ToastItem[] }) {
  if (toasts.length === 0) return null;
  return (
    <div
      style={{
        position: 'fixed',
        bottom: 24,
        right: 24,
        display: 'flex',
        flexDirection: 'column',
        gap: 8,
        zIndex: 9999,
        maxWidth: 420,
        pointerEvents: 'none',
      }}
    >
      {toasts.map((t) => (
        <ToastBubble key={t.id} toast={t} />
      ))}
    </div>
  );
}

function ToastBubble({ toast }: { toast: ToastItem }) {
  const palette = {
    success: { bg: 'rgba(52,211,153,0.10)', border: 'rgba(52,211,153,0.28)', bar: '#34d399' },
    error:   { bg: 'rgba(248,113,113,0.10)', border: 'rgba(248,113,113,0.28)', bar: '#f87171' },
    info:    { bg: 'rgba(96,165,250,0.10)',  border: 'rgba(96,165,250,0.28)',  bar: '#60a5fa' },
  }[toast.type];

  return (
    <div
      style={{
        background: '#14171f',
        border: `1px solid ${palette.border}`,
        borderLeft: `3px solid ${palette.bar}`,
        borderRadius: 7,
        padding: '11px 15px',
        display: 'flex',
        flexDirection: 'column',
        gap: 4,
        boxShadow: '0 8px 28px rgba(0,0,0,0.45)',
        pointerEvents: 'all',
        animation: 'toast-in 0.18s ease',
      }}
    >
      <style>{`
        @keyframes toast-in {
          from { transform: translateX(16px); opacity: 0; }
          to   { transform: none; opacity: 1; }
        }
      `}</style>
      <span style={{ color: '#e5e7eb', fontSize: 13, fontFamily: 'IBM Plex Sans, sans-serif', lineHeight: 1.4 }}>
        {toast.message}
      </span>
      {toast.requestId && (
        <span style={{ color: '#4b5563', fontSize: 10, fontFamily: 'IBM Plex Mono, monospace' }}>
          request_id: {toast.requestId}
        </span>
      )}
    </div>
  );
}
