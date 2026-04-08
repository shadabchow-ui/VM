import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    port: 3000,
    proxy: {
      // Proxy API calls to the resource-manager during development.
      // Set VITE_API_URL env var in production builds.
      '/v1': {
        target: process.env.VITE_API_TARGET ?? 'https://localhost:9090',
        changeOrigin: true,
        secure: false,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: true,
  },
})
