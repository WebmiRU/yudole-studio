package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"studio/core/protocol"
)

// fakeGoodGame повторяет те два адреса, которыми мы пользуемся.
func fakeGoodGame(t *testing.T) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/api/4/users/", func(w http.ResponseWriter, r *http.Request) {
		login := strings.TrimPrefix(r.URL.Path, "/api/4/users/")
		switch login {
		case "E.Wolf":
			w.Write([]byte(`{"id":119256,"nickname":"E.Wolf","stream":{"id":53029,"key":"EWolf"}}`))
		case "DemoChannel":
			w.Write([]byte(`{"id":77,"nickname":"DemoChannel","stream":{"id":12}}`))
		case "NoChannel":
			w.Write([]byte(`{"id":1,"nickname":"NoChannel"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"success":false,"code":404}`))
		}
	})

	mux.HandleFunc("/api/getchannelstatus", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("id") {
		case "53029":
			w.Write([]byte(`{"53029":{"stream_id":53029,"url":"https://goodgame.ru/E.Wolf","title":"Первый"}}`))
		case "12":
			w.Write([]byte(`{"12":{"stream_id":12,"url":"https://goodgame.ru/DemoChannel"}}`))
		default:
			w.Write([]byte(`[]`))
		}
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func resolverFor(t *testing.T) *resolver {
	t.Helper()
	apiBase = fakeGoodGame(t).URL
	return newResolver()
}

func TestResolveChannelByNumber(t *testing.T) {
	value, label, err := resolverFor(t).resolveChannel(context.Background(), "53029")
	if err != nil {
		t.Fatalf("53029: %v", err)
	}
	if value != "53029" {
		t.Errorf("value = %q, want 53029", value)
	}
	if label != "E.Wolf" {
		t.Errorf("label = %q, want E.Wolf", label)
	}
}

func TestResolveChannelByLogin(t *testing.T) {
	value, label, err := resolverFor(t).resolveChannel(context.Background(), "E.Wolf")
	if err != nil {
		t.Fatalf("E.Wolf: %v", err)
	}
	if value != "53029" {
		t.Errorf("value = %q, want 53029", value)
	}
	if label != "E.Wolf" {
		t.Errorf("label = %q, want E.Wolf", label)
	}
}

func TestResolveChannelByURL(t *testing.T) {
	cases := []string{
		"https://goodgame.ru/E.Wolf",
		"http://goodgame.ru/E.Wolf",
		"goodgame.ru/E.Wolf",
		"https://goodgame.ru/E.Wolf/",
	}

	for _, input := range cases {
		value, label, err := resolverFor(t).resolveChannel(context.Background(), input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if value != "53029" || label != "E.Wolf" {
			t.Errorf("%s → %q/%q, want 53029/E.Wolf", input, value, label)
		}
	}
}

func TestResolveChannelReportsProblems(t *testing.T) {
	cases := map[string]string{
		"999999999":  "Канал не найден", // девять цифр — это уже логин, и его нет
		"1234":       "Канал 1234 не найден",
		"nosuchuser": "Канал не найден", // такого пользователя нет
		"":           "ничего не введено",
		"   ":        "ничего не введено",
	}

	for input, want := range cases {
		_, _, err := resolverFor(t).resolveChannel(context.Background(), input)
		if err == nil {
			t.Errorf("%q: ожидалась ошибка", input)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: ошибка %q, ждали упоминания %q", input, err, want)
		}
	}
}

func TestResolveChannelRejectsURLWithoutNick(t *testing.T) {
	if _, _, err := resolverFor(t).resolveChannel(context.Background(), "https://goodgame.ru/"); err == nil {
		t.Error("ссылка без ника должна отвергаться")
	}
}

func TestIsChannelID(t *testing.T) {
	for _, input := range []string{"1", "53029", "99999999"} {
		if !isChannelID(input) {
			t.Errorf("%q должен считаться номером канала", input)
		}
	}
	for _, input := range []string{"", "123456789", "E.Wolf", "12a", "-5"} {
		if isChannelID(input) {
			t.Errorf("%q не должен считаться номером канала", input)
		}
	}
}

func TestNicknameFromURL(t *testing.T) {
	cases := map[string]string{
		"https://goodgame.ru/E.Wolf":   "E.Wolf",
		"https://goodgame.ru/E.Wolf/":  "E.Wolf",
		"goodgame.ru/DemoChannel":      "DemoChannel",
		"https://goodgame.ru/a/b/Demo": "Demo",
	}
	for input, want := range cases {
		got, err := nicknameFromURL(input)
		if err != nil {
			t.Errorf("%s: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("nicknameFromURL(%q) = %q, want %q", input, got, want)
		}
	}
}

// apply проверяет каналы и только потом берёт их в работу.
func TestApplyResolvesChannels(t *testing.T) {
	server := fakeGoodGame(t)
	previous := currentChannels()
	t.Cleanup(func() { setChannels(previous) })

	apiBase = server.URL

	values := map[string]any{
		"channels": []any{
			"53029",
			"DemoChannel",
			map[string]any{"value": "12", "label": "DemoChannel"},
			// Ссылка не равна своему названию: если помнить только результат,
			// по ссылке потом не найти и канал молча выпадет из списка.
			"https://goodgame.ru/E.Wolf",
		},
	}
	settled, err := apply(context.Background(), values)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	if got := strings.Join(channelLabels(currentChannels()), ","); got != "E.Wolf,DemoChannel" {
		t.Errorf("channels = %q, want E.Wolf,DemoChannel", got)
	}

	// Ядру отдаём «номер — название», а не то, что человек набрал: «DemoChannel»
	// и «12» — один канал, в конфиг он должен лечь один раз, а ссылка — уже
	// как 53029/E.Wolf.
	got, ok := settled["channels"].([]protocol.ChoiceValue)
	if !ok {
		t.Fatalf("settled channels = %#v, want a list", settled["channels"])
	}
	if len(got) != 2 {
		t.Fatalf("settled channels = %v, want 2", channelLabels(got))
	}
	if got[0].Value != "53029" || got[0].Label != "E.Wolf" {
		t.Errorf("первый канал = %+v, want 53029/E.Wolf", got[0])
	}
}

// Каналы, которых не нашлось, не должны молча пропасть: настройки не
// принимаются, ядро их не сохранит, страница подсветит поле.
func TestApplyRefusesUnknownChannels(t *testing.T) {
	server := fakeGoodGame(t)
	previous := currentChannels()
	t.Cleanup(func() { setChannels(previous) })

	apiBase = server.URL
	setChannels([]protocol.ChoiceValue{{Value: "53029", Label: "E.Wolf"}})

	values := map[string]any{"channels": []any{"53029", "nosuchuser"}}

	settled, err := apply(context.Background(), values)
	if err == nil {
		t.Fatal("ожидался отказ: канала nosuchuser не существует")
	}
	if settled != nil {
		t.Errorf("при отказе итог возвращаться не должен: %v", settled)
	}

	var rejected *optionError
	if !errors.As(err, &rejected) {
		t.Fatalf("ошибка не про опцию: %v", err)
	}
	// Под полем — короткое «сколько не добавили», без перечисления: каждая
	// запись подписана сама, в items.
	if message := rejected.Fields()["channels"]; message != "Не удалось добавить 1 канал" {
		t.Errorf("сообщение поля = %q", message)
	}
	if reason := rejected.Items()["channels"]["nosuchuser"]; !strings.Contains(reason, "не найден") {
		t.Errorf("причина %q должна говорить, что канал не найден", reason)
	}

	// Прошлые настройки не должны пострадать от отказа.
	if got := strings.Join(channelLabels(currentChannels()), ","); got != "E.Wolf" {
		t.Errorf("channels = %q, want them untouched", got)
	}
}

func TestPluralPicksTheRightForm(t *testing.T) {
	cases := map[int]string{
		0:   "0 каналов",
		1:   "1 канал",
		2:   "2 канала",
		4:   "4 канала",
		5:   "5 каналов",
		11:  "11 каналов",
		21:  "21 канал",
		22:  "22 канала",
		25:  "25 каналов",
		101: "101 канал",
	}
	for n, want := range cases {
		if got := plural(n, "канал", "канала", "каналов"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
}

// Канал, уже сохранённый как «номер — название», повторно не спрашивается:
// иначе на каждом запуске GoodGame отвечает по-разному и название в конфиге
// начинает мигать.
func TestApplyKeepsResolvedPairsAsTheyAre(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	previous := currentChannels()
	t.Cleanup(func() { setChannels(previous) })

	apiBase = server.URL
	setChannels(nil)

	values := map[string]any{"channels": []any{
		map[string]any{"value": "53029", "label": "E.Wolf"},
		map[string]any{"value": "5", "label": "miker"},
	}}

	settled, err := apply(context.Background(), values)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if hits != 0 {
		t.Errorf("GoodGame спросили %d раз, а каналы уже были известны", hits)
	}

	got := settled["channels"].([]protocol.ChoiceValue)
	if len(got) != 2 {
		t.Fatalf("settled = %v, want 2", channelLabels(got))
	}
	if got[1].Label != "miker" {
		t.Errorf("название = %q, want miker — его не должны менять", got[1].Label)
	}
}
