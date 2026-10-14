// Package theme reads theme manifests and safely serves their assets.
package theme

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	FileName = "theme.json"
	TemplateFile = "template.html"
	StyleFile = "style.css"
)

type manifest struct {
	Title string `json:"title"`
	Styles []string `json:"styles,omitempty"`
	Scripts []string `json:"scripts,omitempty"`
}

// Theme is the descriptor sent to the browser theme engine. Asset arrays retain
// the exact order declared by theme.json.
type Theme struct {
	ID string `json:"id"`
	Title string `json:"title"`
	Template string `json:"template"`
	Styles []string `json:"styles,omitempty"`
	Scripts []string `json:"scripts,omitempty"`
}

type Catalog struct {
	dir string
	list []Theme
}

func Scan(dir string) *Catalog {
	catalog := &Catalog{dir: dir, list: []Theme{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("theme: cannot read the directory", "dir", dir, "err", err)
		}
		return catalog
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") { continue }
		if item, ok := read(dir, entry.Name()); ok { catalog.list = append(catalog.list, item) }
	}
	slices.SortFunc(catalog.list, func(a, b Theme) int { return strings.Compare(a.ID, b.ID) })
	return catalog
}

func read(root, name string) (Theme, bool) {
	dir := filepath.Join(root, name)
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		if !os.IsNotExist(err) { slog.Warn("theme: cannot read manifest", "theme", name, "err", err) }
		return Theme{}, false
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Warn("theme: malformed manifest", "theme", name, "err", err)
		return Theme{}, false
	}
	if strings.TrimSpace(m.Title) == "" {
		slog.Warn("theme: no title in manifest", "theme", name)
		return Theme{}, false
	}
	template, ok := assetURL(name, dir, TemplateFile, ".html")
	if !ok {
		slog.Warn("theme: required template is missing", "theme", name, "file", TemplateFile)
		return Theme{}, false
	}
	styles, ok := assets(name, dir, m.Styles, ".css")
	if !ok { return Theme{}, false }
	scripts, ok := assets(name, dir, m.Scripts, ".js")
	if !ok { return Theme{}, false }
	return Theme{ID:name, Title:strings.TrimSpace(m.Title), Template:template, Styles:styles, Scripts:scripts}, true
}

// assetURL accepts only flat files to keep the theme handler inside its directory.
func assetURL(themeID, dir, file, extension string) (string, bool) {
	if file == "" || filepath.Base(file) != file || file == "." || file == ".." || filepath.Ext(file) != extension {
		return "", false
	}
	info, err := os.Stat(filepath.Join(dir, file))
	if err != nil || info.IsDir() { return "", false }
	return "/themes/" + url.PathEscape(themeID) + "/" + url.PathEscape(file), true
}

func assets(themeID, dir string, files []string, extension string) ([]string, bool) {
	result := make([]string, 0, len(files))
	for _, file := range files {
		asset, ok := assetURL(themeID, dir, file, extension)
		if !ok {
			slog.Warn("theme: invalid or missing asset", "theme", themeID, "file", file, "extension", extension)
			return nil, false
		}
		result = append(result, asset)
	}
	return result, true
}

func (c *Catalog) List() []Theme {
	if c == nil { return []Theme{} }
	return slices.Clone(c.list)
}

func (c *Catalog) Get(id string) (Theme, bool) {
	if c == nil { return Theme{}, false }
	for _, item := range c.list {
		if item.ID == id { return item, true }
	}
	return Theme{}, false
}

func (c *Catalog) Has(id string) bool {
	_, ok := c.Get(id)
	return ok
}

func (c *Catalog) Dir() string {
	if c == nil { return "" }
	return c.dir
}

// Handler serves only flat files from a catalogued theme. The bare theme URL
// remains a shortcut for its required message template.
func Handler(c *Catalog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/themes/"), "/")
		name, file, _ := strings.Cut(rest, "/")
		if name == "" || !c.Has(name) { http.NotFound(w, r); return }
		if file == "" { file = TemplateFile }
		if strings.Contains(file, "/") || file == ".." || filepath.Base(file) != file {
			http.NotFound(w, r); return
		}
		full := filepath.Join(c.dir, name, file)
		info, err := os.Stat(full)
		if err != nil || info.IsDir() { http.NotFound(w, r); return }
		f, err := os.Open(full)
		if err != nil {
			slog.Error("theme: open file", "theme", name, "file", file, "err", err)
			http.Error(w, "theme file is unavailable", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, file, info.ModTime(), f)
	})
}

func DefaultDir(baseDir, repoRoot string) string {
	for _, candidate := range []string{filepath.Join(baseDir, "themes"), filepath.Join(repoRoot, "themes")} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() { return candidate }
	}
	return filepath.Join(repoRoot, "themes")
}
