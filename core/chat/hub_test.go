package chat

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"studio/core/plugin"
	"studio/core/protocol"
)

// fakeSource plays the part of the plugin manager: hands out subscriptions and
// lets a test push events into them.
type fakeSource struct {
	events   chan plugin.Event
	subbed   int
	views    []plugin.PluginView
	subsDone bool
}

func newFakeSource() *fakeSource {
	// Канал один на всё время: подписка отдаёт тот же самый, иначе события,
	// отправленные до неё, ушли бы в пустоту.
	return &fakeSource{events: make(chan plugin.Event, 64)}
}

func (f *fakeSource) Subscribe(int) (<-chan plugin.Event, func()) {
	f.subbed++

	var closed bool
	return f.events, func() {
		if !closed {
			closed = true
			f.subsDone = true
		}
	}
}

func (f *fakeSource) List() []plugin.PluginView { return f.views }

func (f *fakeSource) push(t *testing.T, pluginName, kind string, payload any) {
	t.Helper()

	message, err := protocol.New(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	f.events <- plugin.Event{Plugin: pluginName, Message: message}
}

func line(kind, channel, user, text string) plugin.Event {
	message := protocol.MustNew(kind, protocol.ChatPayload{
		Channel: channel,
		User:    protocol.ChatUser{Name: user},
		Message: protocol.ChatLine{Text: text},
	})
	return plugin.Event{Plugin: "goodgame", Message: message}
}

// dial opens a page against the hub and returns the connection.
func dial(t *testing.T, hub *Hub) *websocket.Conn {
	t.Helper()

	server := httptest.NewServer(hub.Handler())
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	conn.SetReadLimit(1 << 20)
	return conn
}

// read waits for one protocol envelope from the chat websocket.
func read(t *testing.T, conn *websocket.Conn) protocol.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	message, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("not a protocol message: %v", err)
	}
	return message
}

type snapshotPayload struct {
	Plugins []Channel          `json:"plugins"`
	History []protocol.Message `json:"history"`
}

// Страница, открытая позже, должна увидеть то, что было до неё: иначе чат на
// второй вкладке начинался бы с пустоты.
func TestPageGetsTheHistoryOnOpen(t *testing.T) {
	source := newFakeSource()
	source.views = []plugin.PluginView{{ID: "goodgame", Title: "GoodGame", State: "running"}}
	hub := New(source)
	t.Cleanup(func() { waitFor(2*time.Second, func() bool { return source.subbed == 1 }) })

	source.push(t, "goodgame", protocol.KindConnectionState, protocol.ConnectionPayload{State: protocol.ConnectionOnline})
	source.push(t, "goodgame", protocol.KindChannelJoinSuccess, protocol.ChannelPayload{Channel: "53029"})
	source.push(t, "goodgame", protocol.KindMessagePublic, protocol.ChatPayload{
		Channel: "53029",
		User:    protocol.ChatUser{Name: "E.Wolf"},
		Message: protocol.ChatLine{Text: "привет"},
	})

	// Ждём, пока хаб всё это примет: подписка начинается в отдельной горутине.
	if !waitFor(2*time.Second, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.history) == 3
	}) {
		t.Fatal("хаб не запомнил события")
	}

	conn := dial(t, hub)
	hello := read(t, conn)
	if hello.Kind != protocol.KindChatSnapshot {
		t.Fatalf("kind = %q", hello.Kind)
	}
	var snapshot snapshotPayload
	if err := hello.PayloadInto(&snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.History) != 3 {
		t.Fatalf("в истории %d событий, want 3", len(snapshot.History))
	}
	if len(snapshot.Plugins) != 1 || snapshot.Plugins[0].Title != "GoodGame" {
		t.Errorf("плагины = %+v", snapshot.Plugins)
	}
	last := snapshot.History[2]
	var payload struct {
		Channel string            `json:"channel"`
		User    protocol.ChatUser `json:"user"`
		Message protocol.ChatLine `json:"message"`
	}
	if err := last.PayloadInto(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Message.Text != "привет" || payload.User.Name != "E.Wolf" || payload.Channel != "53029" {
		t.Errorf("реплика = %+v", payload)
	}
}

// Что пришло после подключения — приходит сразу.
func TestPageGetsNewEvents(t *testing.T) {
	source := newFakeSource()
	hub := New(source)
	t.Cleanup(func() { waitFor(2*time.Second, func() bool { return source.subbed == 1 }) })

	conn := dial(t, hub)
	snapshot := read(t, conn)
	if snapshot.Kind != protocol.KindChatSnapshot {
		t.Fatalf("kind = %q", snapshot.Kind)
	}

	source.events <- line(protocol.KindMessagePublic, "53029", "E.Wolf", "строка")

	live := read(t, conn)
	if live.Kind != protocol.KindMessagePublic {
		t.Fatalf("kind = %q", live.Kind)
	}
	var payload struct {
		Plugin  string            `json:"plugin"`
		Channel string            `json:"channel"`
		User    protocol.ChatUser `json:"user"`
		Message protocol.ChatLine `json:"message"`
	}
	if err := live.PayloadInto(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Message.Text != "строка" || payload.Channel != "53029" {
		t.Errorf("событие = %+v", payload)
	}
	if payload.Plugin != "goodgame" {
		t.Errorf("плагин = %q, want goodgame", payload.Plugin)
	}
}

// Закрывшаяся страница не должна оставлять после себя подписку: иначе её
// очередь переполнится и ядро начнёт терять события для всех.
func TestUnsubscribeIsEnoughOnce(t *testing.T) {
	source := newFakeSource()
	hub := New(source)
	t.Cleanup(func() { waitFor(2*time.Second, func() bool { return source.subbed == 1 }) })

	conn := dial(t, hub)
	_ = read(t, conn)
	_ = conn.CloseNow()

	if !waitFor(3*time.Second, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.subscribers) == 0
	}) {
		t.Fatal("страница осталась в списке подписчиков")
	}
}

// История не растёт бесконечно.
func TestHistoryIsCapped(t *testing.T) {
	source := newFakeSource()
	hub := New(source)
	t.Cleanup(func() { waitFor(2*time.Second, func() bool { return source.subbed == 1 }) })

	for i := range HistoryLimit + 50 {
		source.events <- line(protocol.KindMessagePublic, "53029", "vasya", strings.Repeat("x", i%7+1))
	}

	if !waitFor(5*time.Second, func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.history) == HistoryLimit
	}) {
		hub.mu.Lock()
		got := len(hub.history)
		hub.mu.Unlock()
		t.Fatalf("в истории %d событий, want %d", got, HistoryLimit)
	}
}

func waitFor(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
