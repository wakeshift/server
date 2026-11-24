import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// In development, Vite serves the app and proxies the backend's routes to
// the Go server, on port 8370 unless BACKEND_PORT says otherwise.
const backendURL = `http://localhost:${process.env.BACKEND_PORT || '8370'}`

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    host: 'localhost',
    port: 5173,
    proxy: {
      '/api': backendURL,
      '/auth': backendURL,
      '/health': backendURL,
      '/ready': backendURL,
      '/v1': { target: backendURL, ws: true },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
})
