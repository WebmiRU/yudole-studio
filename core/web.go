package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// web serves the built front-end from disk: index.html for "/", chat.html for
// "/chat", every other path straight from the directory. Nothing is cached in
// memory or in the browser, so a rebuilt page is visible on the next reload.
type web struct {
	dir   string
	index string
	files http.Handler
	built time.Time
}

func newWeb(dir string) (*web, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("web dir %q: %w", dir, err)
	}

	index := filepath.Join(abs, "index.html")
	info, err := os.Stat(index)
	if err != nil {
		return nil, fmt.Errorf("web dir %q: %w\nrun `npm run build` to create it", dir, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("web dir %q: %s is a directory", dir, index)
	}

	return &web{
		dir:   abs,
		index: index,
		files: http.FileServer(http.Dir(abs)),
		built: time.Now(),
	}, nil
}

func (w *web) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	rw.Header().Set("Cache-Control", "no-store")

	if page, ok := pagePath(r.URL.Path); ok {
		w.servePage(rw, r, page)
		return
	}
	w.files.ServeHTTP(rw, r)
}

// pagePath maps a short address to the file behind it: "/" to index.html,
// "/chat" to chat.html and the settings addresses to their pages. Serving them
// by name keeps the addresses readable — /chat instead of /chat.html.
//
// /settings — меню разделов, а не раздел: адрес раздела выглядит как вложенный
// путь (/settings/rooms), и ядро отдаёт файл по тому же пути.
func pagePath(urlPath string) (string, bool) {
	switch isIndexPath(urlPath) {
	case true:
		return "index.html", true
	default:
		clean := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))
		switch clean {
		case "/chat":
			return "chat.html", true
		case "/settings":
			return "settings.html", true
		case "/settings/rooms":
			return "settings/rooms.html", true
		case "/settings/clients":
			return "settings/clients.html", true
		}
		return "", false
	}
}

// servePage writes a page directly: http.ServeFile would redirect /index.html
// to /, and we do not want that.
func (w *web) servePage(rw http.ResponseWriter, r *http.Request, name string) {
	file := filepath.Join(w.dir, name)
	if _, err := os.Stat(file); err != nil {
		http.Error(rw, name+" is unavailable", http.StatusNotFound)
		return
	}

	f, err := os.Open(file)
	if err != nil {
		slog.Error("web: open page", "path", file, "err", err)
		http.Error(rw, name+" is unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		slog.Error("web: stat page", "path", file, "err", err)
		http.Error(rw, name+" is unavailable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(rw, r, name, info.ModTime(), f)
}

// isIndexPath reports whether p is the entry point of the front-end.
func isIndexPath(p string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	return clean == "/" || clean == "/index.html"
}

func healthHandler(w *web) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-store")
		health := map[string]any{
			"status":  "ok",
			"web":     w.dir,
			"started": w.built.Format(time.RFC3339),
		}
		if err := json.MarshalEncode(jsontext.NewEncoder(rw), health); err != nil {
			slog.Error("health: write response", "err", err)
		}
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
