import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // ws: true — shell session WebSockets (/api/session/...) must be
      // upgraded through the dev proxy too, not just http requests.
      "/api": { target: "http://localhost:8080", ws: true },
      // Public installer endpoints live on the backend too — without these
      // the /download page gets empty cards (vite itself serves no
      // /installer.json).
      "/installer.json": { target: "http://localhost:8080" },
      "/installer": { target: "http://localhost:8080" },
    },
  },
})
