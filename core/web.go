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

// web serves the built front-end from disk: index.html for "/", every other
// path straight from the directory. Nothing is cached in memory or in the
// browser, so a rebuilt page is visible on the next reload.
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

	if isIndexPath(r.URL.Path) {
		w.serveIndex(rw, r)
		return
	}
	w.files.ServeHTTP(rw, r)
}

// serveIndex writes index.html directly: http.ServeFile would redirect
// /index.html to /, and we do not want that.
func (w *web) serveIndex(rw http.ResponseWriter, r *http.Request) {
	f, err := os.Open(w.index)
	if err != nil {
		slog.Error("web: open index", "path", w.index, "err", err)
		http.Error(rw, "index.html is unavailable", http.StatusInternalServerError)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		slog.Error("web: stat index", "path", w.index, "err", err)
		http.Error(rw, "index.html is unavailable", http.StatusInternalServerError)
		return
	}
	http.ServeContent(rw, r, "index.html", info.ModTime(), f)
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
