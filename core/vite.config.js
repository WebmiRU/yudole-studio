import { defineConfig } from 'vite';

// Разработка: npm run dev поднимает сервер с HMR на :1081 и отдаёт страницу
// из html/. Продакшен: npm run build собирает всё в dist/, который раздаёт
// Go-сервер (go run .).
export default defineConfig({
  root: 'html',
  publicDir: false,

  // Две страницы: настройки и живой чат. У каждой свой html и свой скрипт.
  build: {
    outDir: '../dist',
    emptyOutDir: true,
    sourcemap: true,
    target: 'es2022',
    assetsInlineLimit: 0,
    rollupOptions: {
      input: {
        // Главная — меню разделов. Настройки ушли на отдельную страницу: из
        // меню на них ведёт ссылка /settings, а не сама главная.
        main: 'html/index.html',
        settings: 'html/settings.html',
        chat: 'html/chat.html',
      },
    },
  },

  server: {
    port: 1081,
    strictPort: true,
    open: false,
    // Go-ядро на :1080 — на случай, если странице понадобится что-то с API.
    // ws: обязателен: без него живой чат на /api/chat застревает на
    // «подключение…», потому что апгрейд соединения никто не передаёт ядру.
    proxy: {
      '/api': { target: 'http://127.0.0.1:1080', changeOrigin: true, ws: true },
    },
  },

  css: {
    devSourcemap: true,
  },
});