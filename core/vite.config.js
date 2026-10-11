import { defineConfig } from 'vite';

// Разработка: npm run dev поднимает сервер с HMR на :1081 и отдаёт страницу
// из html/. Продакшен: npm run build собирает всё в dist/, который раздаёт
// Go-сервер (go run .).
export default defineConfig({
  root: 'html',
  publicDir: false,

  server: {
    port: 1081,
    strictPort: true,
    open: false,
    // Go-ядро на :1080 — на случай, если странице понадобится что-то с API.
    proxy: {
      '/api': { target: 'http://127.0.0.1:1080', changeOrigin: true },
    },
  },

  build: {
    outDir: '../dist',
    emptyOutDir: true,
    sourcemap: true,
    target: 'es2022',
    assetsInlineLimit: 0,
  },

  css: {
    devSourcemap: true,
  },
});