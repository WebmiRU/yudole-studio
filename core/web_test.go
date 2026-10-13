package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func newTestWeb(t *testing.T) *web {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "index.html"), "<h1>home</h1>")
	writeFile(t, filepath.Join(dir, "settings.html"), "<h1>settings</h1>")
	writeFile(t, filepath.Join(dir, "chat.html"), "<h1>chat</h1>")
	writeFile(t, filepath.Join(dir, "rooms.html"), "<h1>rooms</h1>")
	writeFile(t, filepath.Join(dir, "assets", "app.css"), "body{}")

	page, err := newWeb(dir)
	if err != nil {
		t.Fatalf("newWeb: %v", err)
	}
	return page
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestWebServesIndex(t *testing.T) {
	page := newTestWeb(t)

	for _, path := range []string{"/", "/index.html"} {
		rec := get(t, page, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", path, rec.Code)
		}
		body, _ := io.ReadAll(rec.Body)
		if string(body) != "<h1>home</h1>" {
			t.Errorf("GET %s: body %q, want %q", path, body, "<h1>home</h1>")
		}
	}
}

// Разделы живут по своим адресам: настройки — /settings, чат — /chat. Главная
// занимается только меню, поэтому и путать их больше не с чем.
func TestWebServesSectionsByShortAddress(t *testing.T) {
	page := newTestWeb(t)

	cases := map[string]string{
		// Комнаты — раздел по умолчанию, поэтому и /settings, и /rooms ведут
		// в них, а чат-клиенты ушли на свой адрес.
		"/settings":         "<h1>rooms</h1>",
		"/rooms":            "<h1>rooms</h1>",
		"/settings/clients": "<h1>settings</h1>",
		"/chat":             "<h1>chat</h1>",
	}
	for path, want := range cases {
		rec := get(t, page, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", path, rec.Code)
			continue
		}
		body, _ := io.ReadAll(rec.Body)
		if string(body) != want {
			t.Errorf("GET %s: body %q, want %q", path, body, want)
		}
	}
}

func TestWebServesStaticFiles(t *testing.T) {
	page := newTestWeb(t)

	rec := get(t, page, "/assets/app.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-store")
	}
}

func TestWebMissingFileIs404(t *testing.T) {
	page := newTestWeb(t)

	if rec := get(t, page, "/nope.txt"); rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
}

// A rebuilt page must be visible without a restart.
func TestWebReflectsRebuild(t *testing.T) {
	page := newTestWeb(t)

	writeFile(t, page.index, "<h1>rebuilt</h1>")

	rec := get(t, page, "/")
	body, _ := io.ReadAll(rec.Body)
	if string(body) != "<h1>rebuilt</h1>" {
		t.Errorf("body %q, want %q", body, "<h1>rebuilt</h1>")
	}
}

func TestNewWebRejectsDirWithoutIndex(t *testing.T) {
	if _, err := newWeb(t.TempDir()); err == nil {
		t.Fatal("expected an error for a directory without index.html")
	}
}

func TestDefaultWebDirPrefersDist(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "html", "index.html"), "<h1>sources</h1>")
	writeFile(t, filepath.Join(root, "dist", "index.html"), "<h1>build</h1>")
	t.Chdir(root)

	if got, want := defaultWebDir(), filepath.Join(root, "dist"); got != want {
		t.Errorf("defaultWebDir() = %q, want %q", got, want)
	}
}

func TestDefaultWebDirFallsBackToSources(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "html", "index.html"), "<h1>sources</h1>")
	t.Chdir(root)

	if got, want := defaultWebDir(), filepath.Join(root, "html"); got != want {
		t.Errorf("defaultWebDir() = %q, want %q", got, want)
	}
}

// The shipped project must keep a servable front-end: html/index.html, or a
// dist/ produced by `npm run build`.
func TestProjectHasServableWebDir(t *testing.T) {
	dir := defaultWebDir()
	if !isDir(dir) {
		t.Fatalf("web dir %q does not exist", dir)
	}
	if _, err := newWeb(dir); err != nil {
		t.Errorf("web dir %q: %v", dir, err)
	}
}

func TestDisplayAddr(t *testing.T) {
	cases := map[string]string{
		":1080":        "localhost:1080",
		"127.0.0.1:80": "127.0.0.1:80",
		"[::1]:1080":   "[::1]:1080",
	}
	for in, want := range cases {
		if got := displayAddr(in); got != want {
			t.Errorf("displayAddr(%q) = %q, want %q", in, got, want)
		}
	}
}
