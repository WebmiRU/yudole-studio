package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"studio/core/protocol"
)

// chatServer отдаёт указанные строки и держит соединение открытым, пока его
// не закрыли.
func chatServer(t *testing.T, lines ...string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		for _, line := range lines {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(line)); err != nil {
				return
			}
		}
		// Держим соединение живым, иначе клиент успеет получить EOF вместо
		// сообщений.
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server
}

func TestSocketDeliversChatLines(t *testing.T) {
	server := chatServer(t,
		`{"type":"message","data":{"message":{"text":"первая","user_name":"vasya"}}}`,
		`{"type":"message","data":{"message":{"text":"вторая","user_name":"petya"}}}`,
	)
	t.Setenv("GOODGAME_WS_URL", strings.Replace(server.URL, "http://", "ws://", 1)+"/%s")

	setChannels([]protocol.ChoiceValue{{Value: "42", Label: "test"}})

	chat := newSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chat.watch(ctx, channelList)

	select {
	case line := <-chat.events:
		if line.channel != "42" {
			t.Errorf("channel = %q, want 42", line.channel)
		}
		chat, ok := parseChat(line.channel, line.raw)
		if !ok {
			t.Fatalf("could not parse %q", line.raw)
		}
		if chat.Message.Text != "первая" || chat.User.Name != "vasya" {
			t.Errorf("chat = %+v", chat)
		}

	case <-time.After(5 * time.Second):
		t.Fatalf("no chat line arrived; log: %v", drain(chat))
	}

	// Второе сообщение тоже должно дойти.
	select {
	case <-chat.events:
	case <-time.After(5 * time.Second):
		t.Error("the second chat line did not arrive")
	}
}

// Смена настроек подхватывается на лету: подключились к одному каналу,
// переключились на другой.
func TestSocketFollowsTheChannelList(t *testing.T) {
	server := chatServer(t, `{"type":"message","data":{"message":{"text":"привет","user_name":"vasya"}}}`)
	t.Setenv("GOODGAME_WS_URL", strings.Replace(server.URL, "http://", "ws://", 1)+"/%s")

	setChannels([]protocol.ChoiceValue{{Value: "1", Label: "test"}})

	chat := newSocket()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chat.watch(ctx, channelList)

	if !waitFor(3*time.Second, func() bool { return len(chat.logs) > 0 }) {
		t.Fatal("nothing was logged about the connection")
	}
	if line := <-chat.logs; !strings.Contains(line, "канал 1: подключено") {
		t.Errorf("log = %q, want a connection to channel 1", line)
	}

	setChannels([]protocol.ChoiceValue{{Value: "2", Label: "test"}})
	if !waitFor(5*time.Second, func() bool {
		for i := 0; i < len(chat.logs); i++ {
			if strings.Contains(<-chat.logs, "канал 2: подключено") {
				return true
			}
		}
		return false
	}) {
		t.Errorf("did not connect to the new channel; log: %v", drain(chat))
	}
}

func drain(chat *socket) []string {
	var lines []string
	for {
		select {
		case line := <-chat.logs:
			lines = append(lines, line)
		default:
			return lines
		}
	}
}

func waitFor(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
