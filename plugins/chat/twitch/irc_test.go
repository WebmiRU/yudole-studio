package main

import (
	"strings"
	"testing"
)

func TestParseIRCv3TagsAndTrailing(t *testing.T) {
	m, err := parseIRC("@display-name=Cool\\sUser;emotes=25:0-4 :cooluser!cooluser@cooluser.tmi.twitch.tv PRIVMSG #some_channel :Kappa hi")
	if err != nil {
		t.Fatal(err)
	}
	if m.Command != "PRIVMSG" {
		t.Fatalf("command = %q", m.Command)
	}
	if m.Prefix != "cooluser!cooluser@cooluser.tmi.twitch.tv" {
		t.Fatalf("prefix = %q", m.Prefix)
	}
	if len(m.Params) != 1 || m.Params[0] != "#some_channel" {
		t.Fatalf("params = %#v", m.Params)
	}
	if m.Trailing != "Kappa hi" {
		t.Fatalf("trailing = %q", m.Trailing)
	}
	if m.Tags["display-name"] != "Cool User" || m.Tags["emotes"] != "25:0-4" {
		t.Fatalf("unexpected tags: %#v", m.Tags)
	}
}

func TestIRCv3TagEscapes(t *testing.T) {
	got := unescapeTag("one\\stwo\\:three\\\\four\\rfive\\nsix")
	want := "one two;three\\four\rfive\nsix"
	if got != want {
		t.Fatalf("unescapeTag() = %q, want %q", got, want)
	}
}

func TestUTF16Offsets(t *testing.T) {
	s := "😀Kappa!"
	start, ok := utf16Offset(s, 2)
	if !ok || start != 4 {
		t.Fatalf("start = %d, ok = %v; want 4, true", start, ok)
	}
	end, ok := utf16Offset(s, 7)
	if !ok || end != 9 {
		t.Fatalf("end = %d, ok = %v; want 9, true", end, ok)
	}
	if _, ok := utf16Offset(s, 1); ok {
		t.Fatal("offset inside a surrogate pair must be rejected")
	}
}

func TestRenderEmotesEscapesText(t *testing.T) {
	got := renderEmotes("<hello> Kappa!", "25:8-12")
	if !strings.Contains(got, "&lt;hello&gt;") {
		t.Fatalf("plain text wasn't HTML-escaped: %s", got)
	}
	if !strings.Contains(got, "alt=\"Kappa\"") {
		t.Fatalf("emote alt text missing: %s", got)
	}
	if !strings.Contains(got, "src=\"https://static-cdn.jtvnw.net/emoticons/v2/25/default/dark/3.0\"") {
		t.Fatalf("emote URL missing: %s", got)
	}
}

func TestRenderEmotesSupportsModernIDs(t *testing.T) {
	id := "emotesv2_4c3b4ed516de493bbcd2df2f5d450f49"
	got := renderEmotes("Kappa", id+":0-4")
	if !strings.Contains(got, "/"+id+"/") {
		t.Fatalf("modern Twitch emote ID was not rendered: %s", got)
	}
}

func TestRenderEmotesWithoutRanges(t *testing.T) {
	if got := renderEmotes("plain <text>", ""); got != "" {
		t.Fatalf("expected empty HTML, got %q", got)
	}
}

// Идентификатор смайла попадает прямо в адрес картинки, поэтому главное —
// чтобы оттуда нельзя было выйти за пределы каталога.
//
// Тег режется по «/» на группы смайлов, так что «../evil» не отбрасывается
// целиком: первая часть отбрасывается как группа без позиций, а «evil»
// остаётся и проверку проходит. Само по себе это безобидно — в адрес попадает
// только то, что проверку прошло, а она оставляет буквы, цифры, дефис и
// подчёркивание. Проверяем именно это свойство, а не выдуманное «буквы нельзя».
func TestEmoteURLNeverEscapesItsDirectory(t *testing.T) {
	const prefix = "https://static-cdn.jtvnw.net/emoticons/v2/"

	for _, tag := range []string{
		"../evil:0-4",
		"a/../../etc:0-4",
		"25:0-4/../../../root:0-4",
		"evil?x=1:0-4",
		"evil#fragment:0-4",
		"evil name:0-4",
		"%2e%2e%2fevil:0-4",
		"javascript:0-4",
	} {
		got := renderEmotes("Kappa", tag)
		if got == "" {
			// Отброшено целиком — тоже правильно.
			continue
		}

		// Всё, что стоит между префиксом и «/default», — идентификатор смайла.
		// Он обязан быть годным: иначе адрес собрали бы из мусора.
		rest := got[strings.Index(got, prefix)+len(prefix):]
		id := rest[:strings.Index(rest, "/default")]

		if !validEmoteID(id) {
			t.Errorf("в адрес попал негодный идентификатор %q (тег %q): %s", id, tag, got)
		}
		if strings.Contains(got, "..") {
			t.Errorf("адрес вышел за каталог при теге %q: %s", tag, got)
		}
	}
}

// Обычный идентификатор — буквы, цифры, дефис и подчёркивание — проходит:
// новые смайлы Twitch приходят именно такими.
func TestRenderEmotesAcceptsNormalIDs(t *testing.T) {
	for _, id := range []string{"25", "emotesv2_4c3b4ed516de493bbcd2df2f5d450f49", "Some-NaMe_9"} {
		if got := renderEmotes("Kappa", id+":0-4"); !strings.Contains(got, "/"+id+"/") {
			t.Errorf("идентификатор %q должен был пройти, получили %q", id, got)
		}
	}
}

func TestValidLogin(t *testing.T) {
	for _, s := range []string{"abc", "cool_name", "123"} {
		if !validLogin(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "a.b", "https://twitch.tv/a", "name with space", "-bad"} {
		if validLogin(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestParseSettingsNormalizesChannels(t *testing.T) {
	got, fields := parseSettings(map[string]any{
		"nick":     "@MyLogin",
		"token":    "oauth:abc123",
		"channels": []any{"#OneChannel", map[string]any{"value": "SecondChannel", "label": "Second"}},
	})
	if len(fields) != 0 {
		t.Fatalf("unexpected validation errors: %#v", fields)
	}
	if got.Nick != "mylogin" || got.Token != "abc123" {
		t.Fatalf("credentials were not normalized: %#v", got)
	}
	if len(got.Channels) != 2 || got.Channels[0] != "onechannel" || got.Channels[1] != "secondchannel" {
		t.Fatalf("channels were not normalized: %#v", got.Channels)
	}
}

func TestParseSettingsRejectsChannelURLs(t *testing.T) {
	_, fields := parseSettings(map[string]any{
		"nick":     "valid_login",
		"token":    "token",
		"channels": []any{"https://www.twitch.tv/someone"},
	})
	if fields["channels"] == "" {
		t.Fatalf("channel URL should be rejected: %#v", fields)
	}
}
