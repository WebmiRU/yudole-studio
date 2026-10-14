import { defineConfig } from 'vite';

// Короткие адреса страниц. Те же, что в pagePath ядра: /settings — комнаты,
// /settings/clients — чат-клиенты, /chat — лента.
//
// Зачем: dev-сервер ищет файл по имени адреса, поэтому без этой таблицы
// /settings отдал бы settings.html — то есть чат-клиентов вместо комнат, и
// ссылка «Комнаты» молча возвращала бы туда же, откуда кликнули. На :1080 всё
// работало, на :1081 — нет; расхождение адресов между сборкой и разработкой
// стоило бы слишком дорого, чтобы его искать потом по симптому.
//
// Middleware добавляется напрямую, а не через возвращаемую функцию: прямое
// добавление в configureServer встаёт ДО внутренних middleware Vite, иначе
// подменять адрес было бы поздно.
function shortAddresses(pages) {
  return {
    name: 'studio-short-addresses',
    configureServer(server) {
      server.middlewares.use((req, _res, next) => {
        const path = req.url.split('?')[0];
        const page = pages[path];
        if (page) {
          req.url = `/${page}`;
        }
        next();
      });
    },
  };
}

export default defineConfig({
  root: 'html',
  publicDir: false,

  plugins: [
    shortAddresses({
      '/': 'index.html',
      '/chat': 'chat.html',
      '/settings': 'settings.html',
      '/settings/rooms': 'settings/rooms.html',
      '/settings/clients': 'settings/clients.html',
    }),
  ],

  // Каждая страница со своим html и своим скриптом.
  build: {
    outDir: '../dist',
    emptyOutDir: true,
    sourcemap: true,
    target: 'es2022',
    assetsInlineLimit: 0,
    rollupOptions: {
      input: {
        // Главная — меню разделов. Настройки — тоже меню, а не раздел:
        // разделы живут по вложенным адресам /settings/rooms и
        // /settings/clients, и файлы лежат в той же папке.
        main: 'html/index.html',
        settings: 'html/settings.html',
        rooms: 'html/settings/rooms.html',
        clients: 'html/settings/clients.html',
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
      // Файлы тем: страница берёт стиль темы и её картинки с ядра, а не из
      // node_modules. Без этого правила на :1081 тема не грузилась бы вовсе,
      // и тему можно было бы проверить только на собранной странице.
      '/themes': { target: 'http://127.0.0.1:1080', changeOrigin: true },
    },
  },

  css: {
    devSourcemap: true,
  },
});
