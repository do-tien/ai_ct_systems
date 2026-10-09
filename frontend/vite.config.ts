import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The browser talks to the backend through Vite's dev proxy on a relative
// path, so the frontend never calls Docker service names or Orthanc directly.
const backendUrl = process.env.BACKEND_URL ?? 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/healthz': backendUrl,
    },
  },
})
