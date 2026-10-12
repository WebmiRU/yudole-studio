package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"studio/core/protocol"
)

func TestNormalizeChannel(t *testing.T) {
	tests := []struct {
		input string
		want  string
		bad   bool
	}{
		{input: "streamer", want: "streamer"},
		{input: "@streamer", want: "streamer"},
		{input: "https://live.vkvideo.ru/streamer/", want: "streamer"},
		{input: "live.vkvideo.ru/streamer", want: "streamer"},
		{input: "https://live.vkvideo.ru/@streamer/some-page", want: "streamer"},
		{input: "https://vkplay.live/streamer", want: "streamer"},
		{input: "https://example.org/streamer", bad: true},
		{input: "ftp://live.vkvideo.ru/streamer", bad: true},
		{input: "https://live.vkvideo.ru/", bad: true},
		{input: "channel name", bad: true},
		{input: "live.vkvideo.ru.evil.test/streamer", bad: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			got, err := normalizeChannel(test.input)
			if test.bad {
				if err == nil {
					t.Fatalf("normalizeChannel(%q) = %q, want error", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeChannel(%q): %v", test.input, err)
			}
			if got != test.want {
				t.Fatalf("normalizeChannel(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

func TestParseSettingsNormalizesAndDeduplicates(t *testing.T) {
	input := map[string]any{
		"channels": []any{
			"streamer",
			"https://live.vkvideo.ru/streamer/",
			map[string]any{"value": "@second", "label": "Second"},
		},
	}
	got, fields, items := parseSettings(input)
	if len(fields) != 0 || len(items) != 0 {
		t.Fatalf("unexpected validation errors: fields=%v items=%v", fields, items)
	}
	want := []string{"streamer", "second"}
	if !reflect.DeepEqual(got.Channels, want) {
		t.Fatalf("channels = %#v, want %#v", got.Channels, want)
	}
}

func TestParseSettingsRejectsBadURL(t *testing.T) {
	got, fields, items := parseSettings(map[string]any{
		"channels": []any{"https://example.org/not-vk"},
	})
	if len(got.Channels) != 0 {
		t.Fatalf("unexpected accepted channels: %#v", got.Channels)
	}
	if fields["channels"] == "" || items["channels"]["https://example.org/not-vk"] == "" {
		t.Fatalf("missing field/item errors: fields=%v items=%v", fields, items)
	}
}

func TestResolveChannelUsesPublicWebSocketChannel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/blog/streamer" {
			t.Errorf("request path = %q, want /v1/blog/streamer", r.URL.Path)
		}
		if r.Header.Get("Origin") != "https://live.vkvideo.ru" {
			t.Errorf("Origin = %q", r.Header.Get("Origin"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"publicWebSocketChannel\":\"public-chat:9988\",\"blogUrl\":\"streamer\",\"owner\":{\"id\":12,\"displayName\":\"Streamer Name\",\"name\":\"Streamer\",\"nick\":\"streamer\"}}"))
	}))
	defer server.Close()

	channel, err := resolveChannel(context.Background(), server.Client(), server.URL+"/v1", "https://live.vkvideo.ru/streamer")
	if err != nil {
		t.Fatal(err)
	}
	if channel.ID != "9988" {
		t.Fatalf("channel ID = %q, want 9988", channel.ID)
	}
	if channel.Slug != "streamer" || channel.DisplayName != "Streamer Name" || channel.OwnerNick != "streamer" {
		t.Fatalf("unexpected channel result: %#v", channel)
	}
}

func TestGetConnectTokenSupportsDirectAndWrappedResponses(t *testing.T) {
	for _, response := range []string{"{\"token\":\"abc\"}", "{\"data\":{\"token\":\"abc\"}}"} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/ws/connect" {
					t.Errorf("request path = %q", r.URL.Path)
				}
				_, _ = w.Write([]byte(response))
			}))
			defer server.Close()
			token, err := getConnectToken(context.Background(), server.Client(), server.URL+"/v1")
			if err != nil {
				t.Fatal(err)
			}
			if token != "abc" {
				t.Fatalf("token = %q, want abc", token)
			}
		})
	}
}

func TestMessageTextCombinesBlocks(t *testing.T) {
	blocks := []vkBlock{
		{Type: "text", Content: "Привет, "},
		{Type: "mention", DisplayName: "Alice"},
		{Type: "text", Content: "! "},
		{Type: "link", Content: "ссылка"},
		// Смайл с картинкой: в читаемый текст его код не идёт, он появляется
		// только в разметке — отдельный тест проверяет именно это.
		{Type: "smile", ID: "w", Name: "wave", LargeURL: "https://x/w"},
	}
	if got, want := lineText(blocks), "Привет, @Alice! ссылка"; got != want {
		t.Fatalf("messageText = %q, want %q", got, want)
	}
}

func TestSeenIDsSuppressesDuplicatesAndEvictsOldEntries(t *testing.T) {
	seen := &seenIDs{set: make(map[string]struct{}), order: make([]string, 0, maxMessageIDs)}
	if !seen.Add("a") || seen.Add("a") {
		t.Fatal("duplicate message ID was not suppressed")
	}
	for i := 0; i < maxMessageIDs; i++ {
		seen.Add("id-" + strconv.Itoa(i))
	}
	if !seen.Add("a") {
		t.Fatal("old message ID should be evicted after the bounded cache fills")
	}
}

func TestProtocolAckRemainsValidJSON(t *testing.T) {
	line, err := protocol.Encode(protocol.MustNew(protocol.KindAck, protocol.AckPayload{
		Values: map[string]any{"channels": []string{"streamer"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(line, &envelope); err != nil {
		t.Fatal(err)
	}
	if string(envelope["kind"]) != "\"ack\"" {
		t.Fatalf("unexpected protocol envelope: %s", line)
	}
}

// Текст реплики приходит закодированным дважды. Печатать его как есть нельзя:
// в ленте появляется мусор вида ["привет","unstyled",[]], и читать нечего.
func TestMessageTextDecodesDoubleEncodedContent(t *testing.T) {
	cases := []struct {
		name   string
		blocks []vkBlock
		want   string
	}{
		{
			name: "простой текст",
			blocks: []vkBlock{{
				Type:    "text",
				Content: `["дискорд не работает((","unstyled",[]]`,
			}},
			want: "дискорд не работает((",
		},
		{
			name: "упоминание перед текстом",
			blocks: []vkBlock{
				{Type: "mention", DisplayName: "Dank1n", Nick: "Dank1n", Name: "Dank1n"},
				{Type: "text", Content: `[" привет","unstyled",[]]`},
			},
			want: "@Dank1n привет",
		},
		{
			name: "оформление не попадает в текст",
			blocks: []vkBlock{{
				Type:    "text",
				Content: `["жирный","bold",[]]`,
			}},
			want: "жирный",
		},
		{
			name: "обычная строка без разметки остаётся как есть",
			blocks: []vkBlock{{
				Type:    "text",
				Content: `просто текст`,
			}},
			want: "просто текст",
		},
		{
			name: "смайлы и ссылка",
			blocks: []vkBlock{
				{Type: "smile", ID: "f", Name: "fire", LargeURL: "https://x/f"},
				{Type: "text", Content: `[" привет","unstyled",[]]`},
			},
			want: "привет",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineText(tc.blocks); got != tc.want {
				t.Fatalf("messageText() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Сообщения приходят и по двум топикам сразу, поэтому одно и то же реплика
// должна отсеиваться по её id.
func TestChannelNameFromPush(t *testing.T) {
	cases := map[string]string{
		"public-chat:17758307":         "17758307",
		"channel-chat:17758307":        "17758307",
		"channel-chat:17758307@356786": "17758307",
		"channel-info:17758307":        "17758307",
		"broken":                       "",
	}
	for topic, want := range cases {
		if got := channelNameFromPush(topic); got != want {
			t.Errorf("channelNameFromPush(%q) = %q, want %q", topic, got, want)
		}
	}
}

// Смайлы приходят отдельными узлами внутри текста. Без них реплика выглядит
// обрезанной, и человек по ней не вспомнит, что вообще писал.
func TestMessageTextKeepsSmilesInsideText(t *testing.T) {
	cases := []struct {
		name   string
		blocks []vkBlock
		want   string
	}{
		{
			name: "смайл внутри текста",
			blocks: []vkBlock{{
				Type:    "text",
				Content: `["привет ", "unstyled",[{"type":"smile","id":"f","name":"fire","largeUrl":"https://x/f"}]]`,
			}},
			want: "привет",
		},
		{
			name: "текст со смайлом и упоминанием",
			blocks: []vkBlock{{
				Type:    "text",
				Content: `["привет ","unstyled",[{"type":"smile","id":"f","name":"fire","largeUrl":"https://x/f"},{"type":"mention","displayName":"Dank1n"}]]`,
			}},
			want: "привет",
		},
		{
			// Узел устроен как [текст, оформление, содержимое], поэтому дети
			// идут после текста, а не внутрь слова.
			name: "оформление не печатается, содержимое — да",
			blocks: []vkBlock{{
				Type:    "text",
				Content: `["слово","bold",[{"type":"smile","id":"q","name":"party","largeUrl":"https://x/q"}]]`,
			}},
			want: "слово",
		},
		{
			name: "смайл отдельным блоком",
			blocks: []vkBlock{
				{Type: "smile", ID: "f", Name: "fire", LargeURL: "https://x/f"},
				{Type: "text", Content: `[" привет","unstyled",[]]`},
			},
			want: "привет",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineText(tc.blocks); got != tc.want {
				t.Fatalf("messageText() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Анимированный смайл приходит десятками одинаковых узлов — по одному на кадр.
// Без отсечки одна реплика превращалась в двадцать одинаковых подписей.
func TestMessageTextCollapsesRepeatedSmiles(t *testing.T) {
	blocks := []vkBlock{{
		Type: "text",
		Content: `["", "unstyled", [` +
			`{"type":"smile","id":"a","name":"CptCheer7","largeUrl":"https://x/a"},` +
			`{"type":"smile","id":"a","name":"CptCheer7","largeUrl":"https://x/a"},` +
			`{"type":"smile","id":"a","name":"CptCheer7","largeUrl":"https://x/a"}]]`,
	}}
	text, html := messageLine(blocks)
	if text != "" {
		t.Errorf("код смайла не должен попадать в читаемый текст: %q", text)
	}
	if got := strings.Count(html, "<img"); got != 1 {
		t.Fatalf("картинок смайла %d, ожидалась одна: %q", got, html)
	}

	// Разные смайлы в одной реплике остаются все: человек мог послать и то и то.
	mixed := []vkBlock{{
		Type: "text",
		Content: `["", "unstyled", [` +
			`{"type":"smile","id":"a","name":"fire","largeUrl":"https://x/a"},` +
			`{"type":"smile","id":"b","name":"party","largeUrl":"https://x/b"}]]`,
	}}
	_, mixedHTML := messageLine(mixed)
	if got := strings.Count(mixedHTML, "<img"); got != 2 {
		t.Fatalf("разных смайлов %d, ожидалось два: %q", got, mixedHTML)
	}
}

// Адреса чата приходят со страницы вперемешку со слотами и чужими каналами.
// Сверяться надо по номеру канала, иначе подписка уедет не туда.
func TestTopicBelongsTo(t *testing.T) {
	cases := []struct {
		topic string
		id    string
		want  bool
	}{
		{"channel-chat:17758307", "17758307", true},
		{"channel-chat:17758307@0", "17758307", true},
		{"channel-chat:17758307@356786", "17758307", true},
		{"channel-chat:18091066", "17758307", false},
		{"channel-chat:177583070", "17758307", false},
		{"channel-chat:17758307", "", false},
	}
	for _, tc := range cases {
		if got := topicBelongsTo(tc.topic, tc.id); got != tc.want {
			t.Errorf("topicBelongsTo(%q, %q) = %v, want %v", tc.topic, tc.id, got, tc.want)
		}
	}
}

// Адреса собираются со страницы один раз и без повторов.
func TestTopicRegexFindsSlotAddresses(t *testing.T) {
	page := `{"wsChatChannel":"channel-chat:17758307","wsChatSlotChannel":"channel-chat:17758307@0",` +
		`"wsChatChannel":"channel-chat:30026162","wsChatSlotChannel":"channel-chat:30026162@358161"}`
	found := map[string]bool{}
	for _, match := range topicRe.FindAllStringSubmatch(page, -1) {
		found[match[1]] = true
	}
	want := map[string]bool{
		"channel-chat:17758307":        true,
		"channel-chat:17758307@0":      true,
		"channel-chat:30026162":        true,
		"channel-chat:30026162@358161": true,
	}
	if len(found) != len(want) {
		t.Fatalf("найдено %d адресов: %v", len(found), found)
	}
	for topic := range want {
		if !found[topic] {
			t.Errorf("не найден адрес %q", topic)
		}
	}
}

// Повторы между блоками тоже убираются: анимированный смайл приходит и
// отдельными блоками, и узлами внутри текста.
func TestMessageTextCollapsesSmilesAcrossBlocks(t *testing.T) {
	blocks := []vkBlock{
		{Type: "smile", ID: "x", Name: "hardplayDance", LargeURL: "https://x/icon"},
		{Type: "text", Content: `["", "unstyled", [{"type":"smile","id":"x",` +
			`"name":"hardplayDance","largeUrl":"https://x/icon"}]]`},
	}
	_, html := messageLine(blocks)
	if got := strings.Count(html, "<img"); got != 1 {
		t.Fatalf("один и тот же смайл напечатан %d раз(а): %q", got, html)
	}
}

// Сведения о канале нужны в ленте один раз — сразу после подключения.
// VK шлёт их на каждое изменение счётчика, и без ограничения лента через
// минуту превращалась в перечень «HardPlay — название трансляции, 1941 в канале».
func TestChannelStateReportsOnlyOnce(t *testing.T) {
	state := &channelState{}
	if !state.shouldReport() {
		t.Fatal("сразу после подключения показать канал надо")
	}
	if state.shouldReport() {
		t.Error("повторно показывать канал в ленте нельзя")
	}
	if state.shouldReport() {
		t.Error("и третий раз тоже нельзя")
	}
}

// lineText is only the readable half of a line: the html half is checked in its
// own tests, and повторять разбор в каждом было бы только шумом.
func lineText(blocks []vkBlock) string {
	text, _ := messageLine(blocks)
	return text
}

func TestMessageLinePutsSmilesIntoHTML(t *testing.T) {
	blocks := []vkBlock{{
		Type: "text",
		Content: `["привет ", "unstyled", [{"type":"smile","id":"abc-123",` +
			`"name":"legionaveGgigiggg","isAnimated":true,` +
			`"largeUrl":"https://images.live.vkvideo.ru/smile/abc-123/icon/size/large"}]]`,
	}}

	text, html := messageLine(blocks)
	if text != "привет" {
		t.Fatalf("читаемый текст = %q, ожидалось без кода смайла", text)
	}
	if !strings.Contains(html, `<img class="smile"`) ||
		!strings.Contains(html, `src="https://images.live.vkvideo.ru/smile/abc-123/icon/size/large"`) {
		t.Fatalf("в разметке нет исходного адреса картинки смайла: %q", html)
	}
	// Код смайла в разметку попадать не должен: человек его не писал.
	if strings.Contains(html, "legionaveGgigiggg") &&
		!strings.Contains(html, `alt="legionaveGgigiggg"`) {
		t.Errorf("код смайла попал в разметку как текст: %q", html)
	}
}

// Повторяющиеся смайлы сохраняются каждый раз и в том порядке, в котором пришли.
func TestMessageLinePreservesRepeatedSmilesInOrder(t *testing.T) {
	cat := `{"type":"smile","id":"a","name":"Cat","largeUrl":"https://images.live.vkvideo.ru/smile/a/icon/size/large"}`
	party := `{"type":"smile","id":"b","name":"Party","largeUrl":"https://images.live.vkvideo.ru/smile/b/icon/size/large"}`
	blocks := []vkBlock{{
		Type:    "text",
		Content: `["", "unstyled", [` + cat + `,` + party + `,` + cat + `]]`,
	}}

	_, html := messageLine(blocks)
	if got := strings.Count(html, "<img"); got != 3 {
		t.Fatalf("картинок смайла %d, ожидались все три, включая повтор: %q", got, html)
	}

	firstCat := strings.Index(html, `src="https://images.live.vkvideo.ru/smile/a/icon/size/large"`)
	partyIndex := strings.Index(html, `src="https://images.live.vkvideo.ru/smile/b/icon/size/large"`)
	lastCat := strings.LastIndex(html, `src="https://images.live.vkvideo.ru/smile/a/icon/size/large"`)
	if firstCat < 0 || partyIndex < 0 || lastCat < 0 || !(firstCat < partyIndex && partyIndex < lastCat) {
		t.Fatalf("порядок смайлов изменился или повтор потерян: %q", html)
	}
}

// Текст от живых людей попадает в разметку экранированным: иначе в ленте можно
// было бы вставить свою разметку и сломать страницу.
func TestMessageLineEscapesTextInHTML(t *testing.T) {
	blocks := []vkBlock{{Type: "text", Content: `["<script>alert(1)</script>","unstyled",[]]`}}

	text, html := messageLine(blocks)
	if !strings.Contains(text, "<script>") {
		t.Errorf("читаемый текст должен остаться как есть: %q", text)
	}
	if strings.Contains(html, "<script>") {
		t.Errorf("в разметку попал неэкранированный текст: %q", html)
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Errorf("текст должен быть экранирован: %q", html)
	}
}
