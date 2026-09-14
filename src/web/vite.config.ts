import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// XNC_API_TARGET：dev/preview 的 API 反代目标（默认本地 server）。UI-only
// 迭代时可用 XNC_API_TARGET=https://xnc.app npx vite preview 对生产 API 做
// 视觉验证，无需本机起 server 栈。
const apiTarget = process.env.XNC_API_TARGET ?? 'http://localhost:8080'

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      // ws: true — shell session WebSockets (/api/session/...) must be
      // upgraded through the dev proxy too, not just http requests.
      // changeOrigin — 远端目标（尤其 caddy SNI 路由）按 Host 匹配证书，
      // 不改写会被 TLS 层直接拒绝（实测 xnc.app EPROTO/SSL alert 80）。
      '/api': { target: apiTarget, ws: true, changeOrigin: true },
      // Public installer endpoints live on the backend too — without these
      // the /download page gets empty cards (vite itself serves no
      // /installer.json).
      '/installer.json': { target: apiTarget, changeOrigin: true },
      '/installer': { target: apiTarget, changeOrigin: true },
    },
  },
  preview: {
    proxy: {
      '/api': { target: apiTarget, ws: true, changeOrigin: true },
      '/installer.json': { target: apiTarget, changeOrigin: true },
      '/installer': { target: apiTarget, changeOrigin: true },
    },
  },
})
