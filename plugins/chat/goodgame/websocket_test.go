package main

import "testing"

// Проверяем разбор ответа GoodGame в реплику протокола.
func TestParseChat(t *testing.T) {
	raw := []byte(`{"type":"message","data":{"message":{"text":"привет :D","user_name":"vasya","user_id":1234,"login":"vasya_pet"}}}`)

	chat, ok := parseChat("42", raw)
	if !ok {
		t.Fatal("a chat line was not recognised")
	}
	if chat.Channel != "42" {
		t.Errorf("channel = %q, want 42", chat.Channel)
	}
	if chat.User.Name != "vasya" || chat.User.Login != "vasya_pet" || chat.User.ID != "1234" {
		t.Errorf("user = %+v", chat.User)
	}
	if chat.Message.Src != "привет :D" {
		t.Errorf("src = %q, want the text as it came", chat.Message.Src)
	}
	if chat.Message.Text == "" {
		t.Error("text must not be empty")
	}
}

func TestParseChatSkipsServiceNoise(t *testing.T) {
	noise := [][]byte{
		[]byte(`{"type":"system_message","data":{}}`),
		[]byte(`{"type":"users_online"}`),
		[]byte(`not json`),
		[]byte(``),
	}
	for _, raw := range noise {
		if _, ok := parseChat("42", raw); ok {
			t.Errorf("%q should not be treated as a chat line", raw)
		}
	}
}

func TestChannelIDRejectsNames(t *testing.T) {
	if _, err := channelID("42"); err != nil {
		t.Errorf("42 should be a valid channel: %v", err)
	}
	for _, value := range []string{"", "general", "  ", "game-1"} {
		if _, err := channelID(value); err == nil {
			t.Errorf("%q should be rejected: GoodGame addresses rooms by number", value)
		}
	}
}

func TestWSEndpointUsesTheChannel(t *testing.T) {
	t.Setenv("GOODGAME_WS_URL", "")
	if got := wsEndpoint("42"); got != "wss://goodgame.ru/websocket/42" {
		t.Errorf("endpoint = %q", got)
	}

	t.Setenv("GOODGAME_WS_URL", "ws://127.0.0.1:9999/ws/%s")
	if got := wsEndpoint("42"); got != "ws://127.0.0.1:9999/ws/42" {
		t.Errorf("endpoint with a template = %q", got)
	}

	t.Setenv("GOODGAME_WS_URL", "ws://127.0.0.1:9999/ws")
	if got := wsEndpoint("42"); got != "ws://127.0.0.1:9999/ws/42" {
		t.Errorf("endpoint without a template = %q", got)
	}
}

func TestPlainTextStripsMarkup(t *testing.T) {
	cases := map[string]string{
		"привет :D":             "привет :D",
		"[b]жирный[/b]":         "жирный",
		"[color=red]да[/color]": "да",
	}
	for src, want := range cases {
		if got := plainText(src); got != want {
			t.Errorf("plainText(%q) = %q, want %q", src, got, want)
		}
	}
}
