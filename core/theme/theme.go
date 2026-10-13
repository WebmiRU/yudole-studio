// Package theme reads the theme catalog: the directories under themes/.
//
// Тема — это каталог с файлом theme.json, и title в нём обязателен: без названия
// тему не показать в списке, а значит, она не тема. Ключ темы — имя каталога:
// он и есть то, что комната запоминает, и переименование папки сделает старую
// ссылку нерабочей.
//
// Каталог сканируется один раз при запуске и дальше живёт в памяти. Темы не
// появляются и не пропадают во время работы, а перечитывать каталог на каждый
// запрос — значит спрашивать у диска то, что не менялось.
package theme

import (
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// FileName is the manifest inside a theme directory.
const FileName = "theme.json"

// manifest is what theme.json holds. Только обязательное поле: остальное ядро не
// знает и не проверяет — оформление разбирает браузер. Файл читается целиком и
// отдаётся странице как есть, лишние ключи не мешают.
type manifest struct {
	Title string `json:"title"`
}

// Theme is one theme from the catalog. ID is the directory name, Title is what a
// person reads in the list.
type Theme struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Catalog is the list of themes, read once and kept.
type Catalog struct {
	dir  string
	list []Theme
}

// Scan reads dir once and remembers what it found.
//
// Каталога может не быть: темы ещё не заведены, и ядро обязано подняться без
// них. Список тогда пустой.
func Scan(dir string) *Catalog {
	catalog := &Catalog{dir: dir, list: []Theme{}}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			// Каталог есть, но прочитать его не вышло: молчать нельзя — человек
			// увидит пустой список и будет думать, что тем нет.
			slog.Warn("theme: cannot read the directory", "dir", dir, "err", err)
		}
		return catalog
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if item, ok := read(filepath.Join(dir, entry.Name())); ok {
			catalog.list = append(catalog.list, item)
		}
	}

	// Порядок каталогов на диске не гарантирован, а список показывается
	// человеку: без сортировки одна и та же тема прыгала бы между запусками.
	slices.SortFunc(catalog.list, func(a, b Theme) int { return strings.Compare(a.ID, b.ID) })

	return catalog
}

// read разбирает одну тему. Второй отдаёт false, когда это не тема: каталог без
// theme.json, нечитаемый JSON или пустой title.
//
// Каждая из этих причин пишется в лог отдельно: «тем нет» и «тема есть, но
// сломана» — разные новости, и молча пропустить вторую значит оставить
// человека гадать.
func read(dir string) (Theme, bool) {
	name := filepath.Base(dir)

	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("theme: cannot read the manifest", "theme", name, "err", err)
		}
		return Theme{}, false
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		slog.Warn("theme: malformed manifest", "theme", name, "file", FileName, "err", err)
		return Theme{}, false
	}

	// title обязателен: без него в списке нечего показывать, кроме имени папки,
	// а имя папки — это ключ, а не название.
	if strings.TrimSpace(m.Title) == "" {
		slog.Warn("theme: no title in the manifest", "theme", name, "file", FileName)
		return Theme{}, false
	}

	return Theme{ID: name, Title: strings.TrimSpace(m.Title)}, true
}

// List returns the themes found by Scan.
func (c *Catalog) List() []Theme {
	if c == nil {
		return []Theme{}
	}
	return c.list
}

// Dir is where the catalog was read from. Для сообщения в логе и для тестов.
func (c *Catalog) Dir() string {
	if c == nil {
		return ""
	}
	return c.dir
}

// Has reports whether a theme with this id exists. Комната хранит тему по
// имени, и тема могла быть удалена с диска, — тогда настройку надо чинить, а
// не молча показывать пустой выбор.
func (c *Catalog) Has(id string) bool {
	if c == nil {
		return false
	}
	return slices.ContainsFunc(c.list, func(t Theme) bool { return t.ID == id })
}

// DefaultDir finds themes/ next to the kernel: сперва рядом с бинарником, затем
// вверх по дереву до go.work — так же, как каталог плагинов.
func DefaultDir(baseDir, repoRoot string) string {
	for _, candidate := range []string{
		filepath.Join(baseDir, "themes"),
		filepath.Join(repoRoot, "themes"),
	} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return filepath.Join(repoRoot, "themes")
}
