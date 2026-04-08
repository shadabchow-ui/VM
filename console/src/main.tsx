// main.tsx — React 18 entry point with global CSS reset.

import React from 'react';
import ReactDOM from 'react-dom/client';
import { App } from './App';

// Inject global reset + base styles.
const style = document.createElement('style');
style.textContent = `
  *, *::before, *::after {
    box-sizing: border-box;
    margin: 0;
    padding: 0;
  }
  html, body, #root {
    height: 100%;
  }
  body {
    background: #0f1117;
    color: #d1d5db;
    font-family: 'IBM Plex Sans', sans-serif;
    -webkit-font-smoothing: antialiased;
    -moz-osx-font-smoothing: grayscale;
  }
  a { color: inherit; }
  button { font-family: inherit; }
  input, textarea, select {
    font-family: inherit;
    color: #f3f4f6;
  }
  select option {
    background: #1a1f2e;
    color: #f3f4f6;
  }
  ::-webkit-scrollbar { width: 5px; height: 5px; }
  ::-webkit-scrollbar-track { background: transparent; }
  ::-webkit-scrollbar-thumb {
    background: rgba(255,255,255,0.08);
    border-radius: 3px;
  }
  ::-webkit-scrollbar-thumb:hover {
    background: rgba(255,255,255,0.14);
  }
`;
document.head.appendChild(style);

const root = document.getElementById('root');
if (!root) throw new Error('Missing #root element');

ReactDOM.createRoot(root).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
