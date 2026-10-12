package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"studio/core/protocol"
	"studio/plugins/chat/goodgame/chat"
)

// goodgame is a chat service that behaves: it greets, confirms joins and then
// sends whatever the test tells it to. What it was asked, it remembers.
type goodgame struct {
	url string

	mu       sync.Mutex
	conn     *websocket.Conn
	joined   []string
	refused  map[string]string
	notices  []func(*chat.Message)
	accepted chan struct{}
}

func newGoodgame(t *testing.T) *goodgame {
	t.Helper()

	fake := &goodgame{refused: map[string]string{}, accepted: make(chan struct{}, 32)}
	t.Setenv("GOODGAME_CHAT_URL", "")

	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)

	// Адрес сервера подставляем через переменную окружения: он читается один
	// раз, при создании сокета.
	fake.url = "ws" + strings.TrimPrefix(server.URL, "http")
	t.Setenv("GOODGAME_CHAT_URL", fake.url)
	return fake
}

func (f *goodgame) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	send := func(kind string, payload any) bool {
		message, err := chat.New(kind, payload)
		if err != nil {
			return false
		}
		line, err := message.Encode()
		if err != nil {
			return false
		}
		return conn.Write(r.Context(), websocket.MessageText, line) == nil
	}

	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()

	send(chat.TypeWelcome, chat.Welcome{ProtocolVersion: 2})

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		message, err := chat.Decode(data)
		if err != nil {
			continue
		}

		f.mu.Lock()
		f.joined = append(f.joined, message.Type)
		reason := f.refused[message.Type]
		notices := slices.Clone(f.notices)
		f.mu.Unlock()

		if reason != "" {
			send(chat.TypeError, chat.Error{Channel: chat.ChannelOf(message), ErrorMsg: reason})
			select {
			case f.accepted <- struct{}{}:
			default:
			}
			continue
		}

		switch message.Type {
		case "join":
			id := chat.ChannelOf(message)
			send(chat.TypeSuccessJoin, chat.Join{
				ChannelID: id, ChannelName: "канал " + string(id), ChannelKey: "streamer",
				Clients: 12, Users: 7,
			})
		case "unjoin":
			send(chat.TypeUnjoin, chat.Unjoined{Channel: chat.ChannelOf(message), Success: true})
		}

		for _, notice := range notices {
			notice(message)
		}
	}
}

// Say makes the service send a message of its own after the next request.
func (f *goodgame) Say(send func(*chat.Message)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, send)
}

// Refuse makes the service turn every request of that kind down.
func (f *goodgame) Refuse(kind, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refused[kind] = reason
}

// Saw reports which requests the service has received.
func (f *goodgame) Saw(want ...string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.joined, ",") == strings.Join(want, ",")
}

// waitAccepted waits for the service to have dealt with one more request.
func (f *goodgame) waitAccepted(t *testing.T) {
	t.Helper()
	select {
	case <-f.accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("сервис не ответил")
	}
}

// watchSocket starts the socket against the fake service.
func watchSocket(t *testing.T) (*socket, context.CancelFunc) {
	t.Helper()

	socket := newSocket()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	socket.watch(ctx, channelList)
	return socket, cancel
}

// waitKind waits for a message of that kind and returns it.
func waitKind(t *testing.T, socket *socket, kind string) protocol.Message {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-socket.events:
			if event.message.Kind == kind {
				return event.message
			}
		case <-deadline:
			t.Fatalf("не дождались %s; пока видели: %v", kind, seen(socket))
			return protocol.Message{}
		}
	}
}

func seen(socket *socket) []string {
	var kinds []string
	for {
		select {
		case event := <-socket.events:
			kinds = append(kinds, event.message.Kind)
		default:
			return kinds
		}
	}
}

func TestSocketJoinsConfiguredChannels(t *testing.T) {
	fake := newGoodgame(t)
	restore := currentChannels()
	t.Cleanup(func() { setChannels(restore) })
	setChannels([]protocol.ChoiceValue{{Value: "53029", Label: "E.Wolf"}})

	socket, _ := watchSocket(t)

	joined := waitKind(t, socket, protocol.KindChannelJoinSuccessful)
	var payload protocol.ChannelPayload
	if err := joined.PayloadInto(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Channel != "53029" {
		t.Errorf("канал = %q, want 53029", payload.Channel)
	}

	info := waitKind(t, socket, protocol.KindChannelInfo)
	var shown protocol.ChannelInfoPayload
	info.PayloadInto(&shown)
	if shown.Name != "канал 53029" || shown.Clients != 12 {
		t.Errorf("info = %+v, want the name and counters from success_join", shown)
	}

	if !fake.Saw("join") {
		t.Errorf("сервис получил %v, а ждал один join", fake.Saw("join"))
	}
}

func TestSocketForwardsChatLines(t *testing.T) {
	fake := newGoodgame(t)
	restore := currentChannels()
	t.Cleanup(func() { setChannels(restore) })
	setChannels([]protocol.ChoiceValue{{Value: "53029", Label: "E.Wolf"}})

	fake.Say(func(*chat.Message) {
		fake.push(t, chat.TypeMessage, chat.Line{
			Channel: "53029", UserID: "162675", UserName: "Resmile",
			Text: "Miker, :panic:", MessageID: 1693043578692,
		})
	})

	socket, _ := watchSocket(t)
	waitKind(t, socket, protocol.KindChannelJoinSuccessful)

	line := waitKind(t, socket, protocol.KindMessagePublic)
	var payload protocol.ChatPayload
	if err := line.PayloadInto(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Channel != "53029" {
		t.Errorf("channel = %q, want 53029", payload.Channel)
	}
	if payload.User.Name != "Resmile" || payload.User.ID != "162675" {
		t.Errorf("user = %+v", payload.User)
	}
	if payload.Message.Src != "Miker, :panic:" {
		t.Errorf("src = %q, want the text as it came", payload.Message.Src)
	}
	if payload.Message.Text == "" {
		t.Error("text must not be empty")
	}
}

// push is how a test makes the service say something on its own. It is only
// useful while the fake holds a connection, so it sends through the last one.
func (f *goodgame) push(t *testing.T, kind string, payload any) {
	t.Helper()

	message, err := chat.New(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	line, err := message.Encode()
	if err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		t.Fatalf("сервис ещё не подключён, нечего слать %s", kind)
	}
	_ = conn.Write(context.Background(), websocket.MessageText, line)
}

// Смена настроек: к новым каналам подключаемся, от лишних отключаемся.
func TestSocketFollowsTheChannelList(t *testing.T) {
	fake := newGoodgame(t)
	restore := currentChannels()
	t.Cleanup(func() { setChannels(restore) })
	setChannels([]protocol.ChoiceValue{{Value: "53029", Label: "E.Wolf"}})

	socket, _ := watchSocket(t)
	waitKind(t, socket, protocol.KindChannelJoinSuccessful)

	setChannels([]protocol.ChoiceValue{
		{Value: "53029", Label: "E.Wolf"},
		{Value: "5", Label: "miker"},
	})

	joined := waitKind(t, socket, protocol.KindChannelJoinSuccessful)
	var payload protocol.ChannelPayload
	joined.PayloadInto(&payload)
	if payload.Channel != "5" {
		t.Errorf("подключились к %q, want 5", payload.Channel)
	}

	if !waitFor(5*time.Second, func() bool { return fake.Saw("join", "join") }) {
		t.Errorf("сервис получил не два join: %v", fake.Saw("join", "join"))
	}
}

// Отказ сервиса — это не поломка плагина: сообщаем ядру, в какой канал не
// удалось, и продолжаем.
func TestSocketReportsARefusedChannel(t *testing.T) {
	newGoodgame(t).Refuse("join", "канал закрыт")

	restore := currentChannels()
	t.Cleanup(func() { setChannels(restore) })
	setChannels([]protocol.ChoiceValue{{Value: "53029", Label: "E.Wolf"}})

	socket, _ := watchSocket(t)

	failed := waitKind(t, socket, protocol.KindChannelJoinFailed)
	var payload protocol.ChannelPayload
	if err := failed.PayloadInto(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Channel != "53029" {
		t.Errorf("канал = %q, want 53029", payload.Channel)
	}
	if payload.Reason != "канал закрыт" {
		t.Errorf("причина = %q, want то, что сказал сервис", payload.Reason)
	}
}

// Обрыв соединения: ядро должно узнать, что каналов больше нет.
func TestSocketReportsUnjoinOnDisconnect(t *testing.T) {
	newGoodgame(t)
	restore := currentChannels()
	t.Cleanup(func() { setChannels(restore) })
	setChannels([]protocol.ChoiceValue{{Value: "53029", Label: "E.Wolf"}})

	socket, cancel := watchSocket(t)
	waitKind(t, socket, protocol.KindChannelJoinSuccessful)

	cancel()

	left := waitKind(t, socket, protocol.KindChannelUnjoinSuccessful)
	var payload protocol.ChannelPayload
	if err := left.PayloadInto(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Channel != "53029" {
		t.Errorf("канал = %q, want 53029", payload.Channel)
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
