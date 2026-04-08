// App.tsx — React Router v6 shell.
// Layout wraps all routes. ToastProvider context at root.

import React from 'react';
import { BrowserRouter, Navigate, Route, Routes } from 'react-router-dom';
import { Layout } from './components/Layout';
import { ToastProvider } from './components/Toast';
import { CreateInstancePage } from './pages/CreateInstancePage';
import { InstanceDetailPage } from './pages/InstanceDetailPage';
import { InstancesListPage } from './pages/InstancesListPage';
import { SSHKeysPage } from './pages/SSHKeysPage';

export function App() {
  return (
    <BrowserRouter>
      <ToastProvider>
        <Layout>
          <Routes>
            <Route path="/" element={<Navigate to="/instances" replace />} />
            <Route path="/instances" element={<InstancesListPage />} />
            <Route path="/instances/create" element={<CreateInstancePage />} />
            <Route path="/instances/:id" element={<InstanceDetailPage />} />
            <Route path="/ssh-keys" element={<SSHKeysPage />} />
            <Route path="*" element={<NotFound />} />
          </Routes>
        </Layout>
      </ToastProvider>
    </BrowserRouter>
  );
}

function NotFound() {
  return (
    <div style={{
      padding: '80px 40px',
      textAlign: 'center',
      color: '#4b5563',
      fontFamily: 'IBM Plex Mono, monospace',
    }}>
      <div style={{ fontSize: 13, marginBottom: 8 }}>404</div>
      <p style={{ margin: 0, color: '#d1d5db', fontSize: 16 }}>Page not found</p>
    </div>
  );
}
