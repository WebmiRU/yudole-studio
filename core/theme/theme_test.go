package theme

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeTheme создаёт каталог темы с манифестом.
func writeTheme(t *testing.T, dir, name, title string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", name, err)
	}
	if title == "" {
		return full
	}
	manifest := `{"title":` + quote(title) + `}`
	if err := os.WriteFile(filepath.Join(full, FileName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write %s: %v", FileName, err)
	}
	if err := os.WriteFile(filepath.Join(full, TemplateFile), []byte(`<template><article>{{payload.message.text}}</article></template>`), 0o644); err != nil {
		t.Fatalf("write %s: %v", TemplateFile, err)
	}
	return full
}

// writeFile кладёт файл в каталог темы: картинки и стиль.
func writeFile(t *testing.T, dir, name, file, body string) {
	t.Helper()
	full := filepath.Join(dir, name, file)
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

func quote(s string) string {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			out = append(out, '\\', byte(r))
		default:
			out = append(out, string(r)...)
		}
	}
	return string(append(out, '"'))
}

func ids(catalog *Catalog) []string {
	list := catalog.List()
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.ID)
	}
	return out
}

func titles(catalog *Catalog) []string {
	list := catalog.List()
	out := make([]string, 0, len(list))
	for _, t := range list {
		out = append(out, t.Title)
	}
	return out
}

func TestScanReadsTitles(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "starcraft", "StarCraft")
	writeTheme(t, dir, "redalert2", "Red Alert 2")
	writeTheme(t, dir, "system", "Системная")

	if got := ids(Scan(dir)); !slices.Equal(got, []string{"redalert2", "starcraft", "system"}) {
		t.Errorf("Scan ids: %v", got)
	}

	// Название из манифеста, а не имя папки.
	want := []string{"Red Alert 2", "StarCraft", "Системная"}
	if got := titles(Scan(dir)); !slices.Equal(got, want) {
		t.Errorf("Scan titles: %v, want %v", got, want)
	}
}

// Порядок каталогов на диске не задан, а человек видит список: без сортировки
// одна и та же тема прыгала бы между запусками.
func TestScanSortsByDirectoryName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zulu", "alpha", "mike"} {
		writeTheme(t, dir, name, name)
	}

	got := ids(Scan(dir))
	want := []string{"alpha", "mike", "zulu"}
	if !slices.Equal(got, want) {
		t.Errorf("Scan: %v, want %v", got, want)
	}
}

// Тема обязана объявить себя: каталог без theme.json — это просто каталог.
func TestScanSkipsDirectoryWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "starcraft", "StarCraft")
	writeTheme(t, dir, "пустая-папка", "")

	got := ids(Scan(dir))
	want := []string{"starcraft"}
	if !slices.Equal(got, want) {
		t.Errorf("Scan: %v, want %v", got, want)
	}
}

// title обязателен: без него в списке нечего показать.
func TestScanSkipsThemeWithoutTitle(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "system", "Системная")
	writeTheme(t, dir, "без-имени", "")

	if got := ids(Scan(dir)); !slices.Equal(got, []string{"system"}) {
		t.Errorf("Scan: %v, want [system]", got)
	}
}

// Пустой title и одни пробелы — одно и то же: названия нет.
func TestScanSkipsBlankTitle(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "theme")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(full, FileName), []byte(`{"title":"   "}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := ids(Scan(dir)); len(got) != 0 {
		t.Errorf("Scan: %v, want empty", got)
	}
}

// Сломанный JSON не должен ронять ядро: тема пропускается, остальные видны.
func TestScanSkipsMalformedManifest(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "system", "Системная")
	broken := filepath.Join(dir, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, FileName), []byte(`{"title":`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := ids(Scan(dir)); !slices.Equal(got, []string{"system"}) {
		t.Errorf("Scan: %v, want [system]", got)
	}
}

// Лишние ключи в манифесте — не беда: тема может описывать цвета и прочее, ядро
// их не читает, но и не должно ими мешать.
func TestScanToleratesExtraKeys(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "starcraft")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data := `{"title":"StarCraft","colors":{"bg":"#000"},"version":3}`
	if err := os.WriteFile(filepath.Join(full, FileName), []byte(data), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(full, TemplateFile), []byte("<template><article/></template>"), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	if got := titles(Scan(dir)); !slices.Equal(got, []string{"StarCraft"}) {
		t.Errorf("Scan titles: %v, want [StarCraft]", got)
	}
}

// Файл рядом с каталогом темы мусор, а не тема.
func TestScanSkipsLooseFiles(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "system", "Системная")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("не тема"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if got := ids(Scan(dir)); !slices.Equal(got, []string{"system"}) {
		t.Errorf("Scan: %v, want [system]", got)
	}
}

// Служебные каталоги не должны попадать в список тем.
func TestScanSkipsDotDirectories(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "system", "Системная")
	writeTheme(t, dir, ".git", "не тема")

	if got := ids(Scan(dir)); !slices.Equal(got, []string{"system"}) {
		t.Errorf("Scan: %v, want [system]", got)
	}
}

// Тем может не быть вообще: ядро обязано подняться и с пустым каталогом.
func TestScanMissingDirectoryIsEmpty(t *testing.T) {
	if got := ids(Scan(filepath.Join(t.TempDir(), "нет-такого"))); len(got) != 0 {
		t.Errorf("Scan: %v, want empty", got)
	}
}

func TestScanEmptyDirectoryIsEmpty(t *testing.T) {
	if got := ids(Scan(t.TempDir())); len(got) != 0 {
		t.Errorf("Scan: %v, want empty", got)
	}
}

// Список не должен меняться после сканирования: он снят один раз при старте.
func TestListIsSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "system", "Системная")
	catalog := Scan(dir)

	writeTheme(t, dir, "поздняя", "Поздняя")

	if got := ids(catalog); !slices.Equal(got, []string{"system"}) {
		t.Errorf("List after adding a theme: %v, want [system]", got)
	}
}

// Комната хранит тему по имени каталога, и тема могла быть удалена: тогда
// настройку надо чинить, а не молча оставлять.
func TestHas(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "system", "Системная")
	catalog := Scan(dir)

	if !catalog.Has("system") {
		t.Error("Has(system) = false, want true")
	}
	if catalog.Has("выдумка") {
		t.Error("Has(выдумка) = true, want false")
	}
}

// Пустой каталог в поле не должен ронять страницу: значение приходит из
// настроек, а там может быть чем угодно.
func TestNilCatalogIsUsable(t *testing.T) {
	var catalog *Catalog

	if got := catalog.List(); len(got) != 0 {
		t.Errorf("nil List: %v, want empty", got)
	}
	if catalog.Has("system") {
		t.Error("nil Has = true, want false")
	}
	if catalog.Dir() != "" {
		t.Error("nil Dir is not empty")
	}
}

// Темы ищутся рядом с ядром, а если его там нет — в корне репозитория.

func TestScanRequiresTemplateAndKeepsAssetOrder(t *testing.T) {
	dir := t.TempDir()
	full := filepath.Join(dir, "ordered")
	if err := os.MkdirAll(full, 0o755); err != nil { t.Fatal(err) }
	manifest := `{"title":"Ordered","styles":["base.css","detail.css"],"scripts":["first.js","second.js"]}`
	if err := os.WriteFile(filepath.Join(full, FileName), []byte(manifest), 0o644); err != nil { t.Fatal(err) }
	for _, file := range []string{"base.css", "detail.css", "first.js", "second.js"} {
		if err := os.WriteFile(filepath.Join(full, file), []byte(""), 0o644); err != nil { t.Fatal(err) }
	}
	// No template: not a valid theme.
	if len(Scan(dir).List()) != 0 { t.Fatal("theme without template was accepted") }
	if err := os.WriteFile(filepath.Join(full, TemplateFile), []byte("<template/>"), 0o644); err != nil { t.Fatal(err) }
	item, ok := Scan(dir).Get("ordered")
	if !ok { t.Fatal("valid theme not found") }
	if len(item.Styles) != 2 || !strings.HasSuffix(item.Styles[0], "/base.css") || !strings.HasSuffix(item.Styles[1], "/detail.css") {
		t.Errorf("styles lost order: %#v", item.Styles)
	}
	if len(item.Scripts) != 2 || !strings.HasSuffix(item.Scripts[0], "/first.js") || !strings.HasSuffix(item.Scripts[1], "/second.js") {
		t.Errorf("scripts lost order: %#v", item.Scripts)
	}
}
func TestDefaultDirPrefersBase(t *testing.T) {
	base := t.TempDir()
	repo := t.TempDir()
	for _, dir := range []string{base, repo} {
		if err := os.MkdirAll(filepath.Join(dir, "themes"), 0o755); err != nil {
			t.Fatalf("mkdir themes: %v", err)
		}
	}

	if got := DefaultDir(base, repo); got != filepath.Join(base, "themes") {
		t.Errorf("DefaultDir: %s, want %s", got, filepath.Join(base, "themes"))
	}
}

func TestDefaultDirFallsBackToRepo(t *testing.T) {
	base := t.TempDir()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "themes"), 0o755); err != nil {
		t.Fatalf("mkdir themes: %v", err)
	}

	if got := DefaultDir(base, repo); got != filepath.Join(repo, "themes") {
		t.Errorf("DefaultDir: %s, want %s", got, filepath.Join(repo, "themes"))
	}
}

// Каталога нет нигде: путь всё равно должен быть внятным, чтобы человек понял,
// куда класть темы.
func TestDefaultDirReportsWhereToPutThemes(t *testing.T) {
	repo := t.TempDir()

	got := DefaultDir(t.TempDir(), repo)
	want := filepath.Join(repo, "themes")
	if got != want {
		t.Errorf("DefaultDir: %s, want %s", got, want)
	}
}

// Handler отдаёт стиль темы и её картинки, а всё, что не тема, — 404.
func TestHandlerServesThemeFiles(t *testing.T) {
	dir := t.TempDir()
	writeTheme(t, dir, "alliance", "Альянс")
	writeFile(t, dir, "alliance", StyleFile, ".topbar{}")
	writeFile(t, dir, "alliance", "emblem.svg", "<svg/>")

	handler := Handler(Scan(dir))

	template := httptest.NewRecorder()
	handler.ServeHTTP(template, httptest.NewRequest("GET", "/themes/alliance", nil))
	if template.Code != http.StatusOK {
		t.Fatalf("template: код %d, а нужен 200", template.Code)
	}
	if !strings.Contains(template.Body.String(), "payload.message.text") {
		t.Errorf("template: в ответе нет шаблона: %q", template.Body.String())
	}
	style := httptest.NewRecorder()
	handler.ServeHTTP(style, httptest.NewRequest("GET", "/themes/alliance/style.css", nil))
	if style.Code != http.StatusOK || !strings.Contains(style.Body.String(), ".topbar") {
		t.Errorf("style: status=%d body=%q", style.Code, style.Body.String())
	}

	img := httptest.NewRecorder()
	handler.ServeHTTP(img, httptest.NewRequest("GET", "/themes/alliance/emblem.svg", nil))
	if img.Code != http.StatusOK {
		t.Errorf("картинка: код %d, а нужен 200", img.Code)
	}

	// Каталог без theme.json — не тема, даже если файлы внутри есть.
	writeTheme(t, dir, "no-manifest", "")
	writeFile(t, dir, "no-manifest", TemplateFile, "<template/>")
	for _, addr := range []string{
		"/themes/no-manifest",
		"/themes/absent",
		// Выход за пределы каталога тем: assets must stay within a theme.
		"/themes/alliance/../theme.json",
		"/themes/alliance/../../etc/passwd",
	} {
		bad := httptest.NewRecorder()
		handler.ServeHTTP(bad, httptest.NewRequest("GET", addr, nil))
		if bad.Code != http.StatusNotFound {
			t.Errorf("%s: код %d, а нужен 404", addr, bad.Code)
		}
	}
}
