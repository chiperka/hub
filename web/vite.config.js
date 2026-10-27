import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import tailwindcss from '@tailwindcss/vite'

// `npm run dev` proxies the API to a locally running chiperka-hub (default :8080).
const api = process.env.CHIPERKA_API || 'http://127.0.0.1:8080'

export default defineConfig({
  plugins: [vue(), tailwindcss()],
  server: {
    proxy: {
      '/api': { target: api, changeOrigin: true },
      '/health': { target: api, changeOrigin: true },
    },
  },
  build: { outDir: 'dist', emptyOutDir: true, chunkSizeWarningLimit: 1000 },
})
