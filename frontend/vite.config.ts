import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// The browser talks to the backend through Vite's dev proxy on a relative
// path, so the frontend never calls Docker service names or Orthanc directly.
const backendUrl = process.env.BACKEND_URL ?? 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // Same-origin routing to the Go backend. API writes preserve the public
      // authority for the backend's Origin check. The read-only DICOMweb gate
      // accepts only a loopback authority, so that proxy uses the backend
      // target authority while the browser still stays on this origin.
      '/healthz': { target: backendUrl, changeOrigin: false },
      '/api': { target: backendUrl, changeOrigin: false },
      '/dicom-web': { target: backendUrl, changeOrigin: true },
    },
  },
  worker: {
    // The DICOM image loader spawns its decoder through
    // `new Worker(new URL('./decodeImageFrameWorker.js', import.meta.url))`,
    // so the emitted worker must stay an ES module.
    format: 'es',
  },
  optimizeDeps: {
    // Prebundling the loader would rewrite its worker URL and WASM asset
    // resolution; keep it transformable and optimize the CommonJS deps it
    // pulls in, which the browser cannot consume as raw ESM.
    exclude: ['@cornerstonejs/dicom-image-loader'],
    include: [
      'dicom-parser',
      '@cornerstonejs/codec-libjpeg-turbo-8bit/decodewasmjs',
      '@cornerstonejs/codec-charls/decodewasmjs',
      '@cornerstonejs/codec-openjpeg/decodewasmjs',
      '@cornerstonejs/codec-openjph/wasmjs',
    ],
  },
})
