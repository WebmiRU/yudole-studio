package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"studio/plugins/chat/goodgame/chat"
)

// openTestStore opens a base in a temporary file, so tests do not touch the
// one next to the plugin.
func openTestStore(t *testing.T) *store {
	t.Helper()

	path := filepath.Join(t.TempDir(), "data.db")
	t.Setenv("GOODGAME_SMILES_DB", path)

	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// realSmiles is a piece of the actual global.json: булевым animated, числами
// id и bind, нулём у channel_id — ровно то, что присылает GoodGame.
const realSmiles = `{
 "Smiles": [
   {"id": 3841, "bind": 0, "name": "sing", "donat": 0, "premium": 0,
    "internal_id": 204, "paid": 0, "animated": true, "tag": "?tuvney",
    "img": "https://goodgame.ru/images/smiles/sing.png?0",
    "img_big": "https://goodgame.ru/images/smiles/sing-big.png?0",
    "img_gif": "https://goodgame.ru/images/anismiles/sing-gif.gif?0",
    "channel": null, "channel_id": 0,
    "tags": "соб, пес, доге"}
 ],
 "Channel_Smiles": {
   "190074": [
     {"id": 10664, "bind": 0, "name": "jokie5", "donat": 0, "premium": 1,
      "internal_id": 5, "paid": 0, "animated": false, "tag": "?w5ps6p",
      "img": "https://goodgame.ru/images/smiles/jokie/jokie5.png?0",
      "img_big": "https://goodgame.ru/images/smiles/jokie/jokie5-big.png?0",
      "img_gif": "", "channel": "SHIMORO", "channel_id": "190074", "tags": ""}
   ]
 },
 "SmilesPacked": "sing,jokie5",
 "timezone_offset": "10800",
 "_compiled_at_": "2026-10-12 09:35:09"
}`

func parsedSmiles(t *testing.T) (chat.Smiles, []chat.Smiley) {
	t.Helper()

	var file chat.Smiles
	if err := json.Unmarshal([]byte(realSmiles), &file); err != nil {
		t.Fatalf("разобрать: %v", err)
	}
	return file, flatten(file)
}

func TestStoreKeepsEverythingAndRemembersWhen(t *testing.T) {
	st := openTestStore(t)
	file, list := parsedSmiles(t)

	at := time.Date(2026, 10, 12, 16, 30, 0, 0, time.UTC)
	if err := st.Replace(file, list, at); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	count, err := st.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Errorf("в базе %d смайлов, want 2", count)
	}

	got, ok := st.UpdatedAt()
	if !ok {
		t.Fatal("дату обновления не записали")
	}
	if !got.Equal(at) {
		t.Errorf("дата = %v, want %v", got, at)
	}
	if compiled := st.CompiledAt(); compiled != "2026-10-12 09:35:09" {
		t.Errorf("когда собран файл = %q", compiled)
	}
}

// В базе лежит смайл целиком, в память — только написание и картинки.
func TestStoreKeepsTheWholeSmileyButLoadsOnlyWhatWeNeed(t *testing.T) {
	st := openTestStore(t)
	file, list := parsedSmiles(t)

	if err := st.Replace(file, list, time.Now()); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	var (
		name, channel, tags, small, big, gif string
		animated                             string
	)
	err := st.db.QueryRow(`
SELECT name, channel, tags, img, img_big, img_gif, animated
FROM smiles WHERE name = 'sing'`).Scan(&name, &channel, &tags, &small, &big, &gif, &animated)
	if err != nil {
		t.Fatalf("прочитать из базы: %v", err)
	}

	if tags != "соб, пес, доге" {
		t.Errorf("теги не сохранились: %q", tags)
	}
	if channel != "" {
		t.Errorf("у общего смайла канал = %q, want пусто", channel)
	}
	if animated != "1" {
		t.Errorf("animated = %q, want 1 — в файле это булево", animated)
	}

	set, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	sing, ok := set.Lookup(":sing:")
	if !ok {
		t.Fatal(":sing: не нашёлся по написанию с двоеточиями")
	}
	if sing.Image != "https://goodgame.ru/images/smiles/sing-big.png?0" {
		t.Errorf("картинка = %q, want самая большая", sing.Image)
	}
	if sing.Gif == "" {
		t.Error("анимированной картинки нет, хотя она есть в файле")
	}

	jokie, ok := set.Lookup("jokie5")
	if !ok {
		t.Fatal("jokie5 не нашёлся без двоеточий")
	}
	if jokie.Gif != "" {
		t.Errorf("гифка = %q, want пусто — в файле её нет", jokie.Gif)
	}
}

// Обновление переписывает базу целиком: пропавшие смайлы не должны в ней
// оставаться, а дата — соответствовать новому содержимому.
func TestReplaceDropsWhatIsGone(t *testing.T) {
	st := openTestStore(t)
	file, list := parsedSmiles(t)

	if err := st.Replace(file, list, time.Now()); err != nil {
		t.Fatal(err)
	}

	file.Common = file.Common[:0]
	second := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	// В базу пишется RFC3339, то есть до секунды: сравнивать надо с таким же.
	if err := st.Replace(file, flatten(file), second); err != nil {
		t.Fatal(err)
	}

	count, _ := st.Count()
	if count != 1 {
		t.Errorf("в базе %d смайлов, want 1 — пропавший должен исчезнуть", count)
	}
	got, _ := st.UpdatedAt()
	if !got.Equal(second) {
		t.Errorf("дата = %v, want %v", got, second)
	}
}

// Скачивание берёт файл по-честному: разбираем настоящий global.json и раскладываем
// в один список.
func TestFetchSmilesFlattensChannels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(realSmiles))
	}))
	defer server.Close()

	old := smilesURL
	smilesURL = server.URL
	defer func() { smilesURL = old }()

	file, list, err := fetchSmiles(context.Background())
	if err != nil {
		t.Fatalf("fetchSmiles: %v", err)
	}

	if len(list) != 2 {
		t.Fatalf("в списке %d смайлов, want 2", len(list))
	}
	if file.Packed != "sing,jokie5" {
		t.Errorf("packed = %q", file.Packed)
	}

	var common, scoped int
	for _, one := range list {
		if one.ChannelID == "" {
			common++
		} else {
			scoped++
		}
	}
	if common != 1 || scoped != 1 {
		t.Errorf("общих %d, канальных %d, want 1 и 1", common, scoped)
	}
}

// Скачали не то — надо сказать, а не записать в базу пустоту.
func TestFetchSmilesRefusesNonsense(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>не смайлы</html>`))
	}))
	defer server.Close()

	old := smilesURL
	smilesURL = server.URL
	defer func() { smilesURL = old }()

	if _, _, err := fetchSmiles(context.Background()); err == nil {
		t.Fatal("разобрали HTML как список смайлов — так не пойдёт")
	}
}

func TestOpenStoreRefusesAnUnwritablePath(t *testing.T) {
	t.Setenv("GOODGAME_SMILES_DB", filepath.Join(t.TempDir(), "нет", "такой", "data.db"))
	if _, err := openStore(); err == nil {
		t.Fatal("открыли базу по несуществующему пути")
	}
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
