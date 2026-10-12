package chat

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeChat is a GoodGame chat that behaves well: it greets, confirms joins and
// then sends whatever it was told to send. It records what was asked of it.
type fakeChat struct {
	url string

	mu       sync.Mutex
	received []*Message
	// refuse are the channels the service turns down.
	refuse map[ID]bool
	// greet is sent before the welcome, to check we do not read too early.
	greet bool
}

func newFakeChat(t *testing.T) *fakeChat {
	t.Helper()

	fake := &fakeChat{refuse: map[ID]bool{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	fake.url = "ws" + strings.TrimPrefix(server.URL, "http")
	return fake
}

func (f *fakeChat) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	write := func(kind string, payload any) bool {
		message, err := New(kind, payload)
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
	greet := f.greet
	f.mu.Unlock()

	if greet {
		write(TypeChannelInfo, ChannelInfo{Channel: "0"})
	}
	write(TypeWelcome, Welcome{ProtocolVersion: 2})

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		message, err := Decode(data)
		if err != nil {
			continue
		}

		f.mu.Lock()
		f.received = append(f.received, message)
		refuse := f.refuse
		f.mu.Unlock()

		switch message.Type {
		case "join":
			var request JoinRequest
			message.Into(&request)
			if refuse[request.ChannelID] {
				write(TypeError, Error{Channel: request.ChannelID, ErrorMsg: "канал закрыт"})
				continue
			}
			write(TypeSuccessJoin, Join{
				ChannelID:   request.ChannelID,
				ChannelName: "канал " + string(request.ChannelID),
				ChannelKey:  "streamer",
				Clients:     12,
				Users:       7,
			})
		case "unjoin":
			write(TypeUnjoin, Unjoined{Channel: ChannelOf(message), Success: true})
		case "get_channels_list":
			write(TypeChannelsList, ChannelsList{Channels: []Channel{
				{ID: "53029", Name: "E.Wolf", Clients: 5, Users: 2},
			}})
		}
	}
}

// Refuse makes the service turn the channel down.
func (f *fakeChat) Refuse(id ID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refuse[id] = true
}

// Greet makes the service send something before the welcome.
func (f *fakeChat) Greet() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.greet = true
}

// Sent returns the requests the client made, in order.
func (f *fakeChat) Sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	kinds := make([]string, 0, len(f.received))
	for _, message := range f.received {
		kinds = append(kinds, message.Type)
	}
	return kinds
}

func openFake(t *testing.T, fake *fakeChat) *Client {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	client, err := Open(ctx, fake.url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// Подключение без приветствия не считается подключением: сервер мог ответить
// ошибкой, и мы обязаны это увидеть, а не писать в никуда.
func TestOpenWaitsForWelcome(t *testing.T) {
	fake := newFakeChat(t)
	fake.Greet()

	client := openFake(t, fake)

	select {
	case message := <-client.Events():
		if message.Type != TypeChannelInfo {
			t.Errorf("первым пришло %q, а не то, что было до приветствия", message.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("сообщение до приветствия не дошло")
	}
}

func TestOpenRefusesAnOlderProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		line, _ := mustMessage(t, TypeWelcome, Welcome{ProtocolVersion: 1}).Encode()
		conn.Write(r.Context(), websocket.MessageText, line)
		<-r.Context().Done()
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Open(ctx, "ws"+strings.TrimPrefix(server.URL, "http"))
	if err == nil || !strings.Contains(err.Error(), "протоколе 1") {
		t.Fatalf("err = %v, want a complaint about the protocol version", err)
	}
}

func TestJoinWaitsForSuccess(t *testing.T) {
	fake := newFakeChat(t)
	client := openFake(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	joined, err := client.Join(ctx, "53029")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if joined.ChannelID != "53029" {
		t.Errorf("channel = %q, want 53029", joined.ChannelID)
	}
	if joined.ChannelName != "канал 53029" {
		t.Errorf("name = %q, want it from success_join", joined.ChannelName)
	}
	if joined.Clients != 12 || joined.Users != 7 {
		t.Errorf("counters = %d/%d, want 12/7", joined.Clients, joined.Users)
	}

	// Сервер подтвердил — ждать больше нечего: сообщение не должно остаться
	// висеть в общей очереди.
	select {
	case message := <-client.Events():
		t.Errorf("success_join попал в общую очередь: %s", message.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestJoinTurnsServiceErrorIntoFailure(t *testing.T) {
	fake := newFakeChat(t)
	fake.Refuse("53029")
	client := openFake(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := client.Join(ctx, "53029")
	if err == nil || !strings.Contains(err.Error(), "канал закрыт") {
		t.Fatalf("err = %v, want what the service said", err)
	}
}

func TestUnjoinAndChannels(t *testing.T) {
	fake := newFakeChat(t)
	client := openFake(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := client.Unjoin(ctx, "53029"); err != nil {
		t.Fatalf("Unjoin: %v", err)
	}

	list, err := client.Channels(ctx, 0, 50)
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	if len(list.Channels) != 1 || list.Channels[0].Name != "E.Wolf" {
		t.Errorf("channels = %+v", list.Channels)
	}

	want := []string{"unjoin", "get_channels_list"}
	if got := fake.Sent(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sent = %v, want %v", got, want)
	}
}

// Смена списка каналов: к новым подключаемся, от лишних отключаемся, а те, что
// остались, не трогаем.
func TestDiff(t *testing.T) {
	cases := []struct {
		name            string
		current, wanted []ID
		join, unjoin    []ID
	}{
		{name: "ничего не осталось", current: []ID{"5"}, wanted: nil, unjoin: []ID{"5"}},
		{name: "первый раз", current: nil, wanted: []ID{"5", "53029"}, join: []ID{"5", "53029"}},
		{name: "добавили", current: []ID{"5"}, wanted: []ID{"5", "53029"}, join: []ID{"53029"}},
		{name: "убрали", current: []ID{"5", "53029"}, wanted: []ID{"5"}, unjoin: []ID{"53029"}},
		{name: "поменяли", current: []ID{"5"}, wanted: []ID{"53029"}, join: []ID{"53029"}, unjoin: []ID{"5"}},
		{name: "ничего не поменялось", current: []ID{"5", "53029"}, wanted: []ID{"5", "53029"}},
		{name: "порядок не важен", current: []ID{"5", "53029"}, wanted: []ID{"53029", "5"}},
		{name: "опустели", current: []ID{"5"}, wanted: nil, unjoin: []ID{"5"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			join, unjoin := Diff(c.current, c.wanted)
			if got := ids(join); got != ids(c.join) {
				t.Errorf("join = %v, want %v", got, ids(c.join))
			}
			if got := ids(unjoin); got != ids(c.unjoin) {
				t.Errorf("unjoin = %v, want %v", got, ids(c.unjoin))
			}
		})
	}
}

// ids makes a list of ids comparable as a string, so failures read like a list
// instead of a wall of hex.
func ids(list []ID) string {
	return strings.Join(slices.Collect(func(yield func(string) bool) {
		for _, id := range list {
			if !yield(string(id)) {
				return
			}
		}
	}), ",")
}

func mustMessage(t *testing.T, kind string, payload any) *Message {
	t.Helper()
	message, err := New(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// Ответ на запрос не должен попадать в общую очередь: он уже у того, кто его
// ждал.
func TestWaitTakesTheMessageItAskedFor(t *testing.T) {
	fake := newFakeChat(t)
	client := openFake(t, fake)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Первое, что придёт, — ответ на наш join, и он обязан достаться join, а
	// не кому-то в общей очереди.
	if _, err := client.Join(ctx, "53029"); err != nil {
		t.Fatalf("Join: %v", err)
	}

	select {
	case message := <-client.Events():
		t.Errorf("в общей очереди оказалось %s", message.Type)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestIDReadsBothStringAndNumber(t *testing.T) {
	cases := map[string]ID{
		`"5"`:                "5",
		`5`:                  "5",
		`1693062969443`:      "1693062969443",
		`"cup:642ab9c7cddb"`: "cup:642ab9c7cddb",
	}

	for raw, want := range cases {
		var id ID
		if err := json.Unmarshal([]byte(raw), &id); err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if id != want {
			t.Errorf("%s → %q, want %q", raw, id, want)
		}
	}
}

// Большие номера сообщений не должны превращаться в 1693062969443.0000.
func TestIDKeepsBigNumbersExact(t *testing.T) {
	const raw = `{"message_id":1693062969443}`

	var line Line
	if err := json.Unmarshal([]byte(raw), &line); err != nil {
		t.Fatal(err)
	}
	if line.MessageID != 1693062969443 {
		t.Errorf("message_id = %d, want 1693062969443", line.MessageID)
	}
}

func TestChannelOf(t *testing.T) {
	cases := map[string]ID{
		`{"type":"success_join","data":{"channel_id":"5"}}`:         "5",
		`{"type":"message","data":{"channel_id":"15365"}}`:          "15365",
		`{"type":"ban2","data":{"roomId":"45507","userId":1}}`:      "45507",
		`{"type":"channel_counters","data":{"channel_id":"5"}}`:     "5",
		`{"type":"welcome","data":{"protocolVersion":2}}`:           "",
		`{"type":"new_poll","data":{"channel_id":"5","title":"?"}}`: "5",
	}

	for raw, want := range cases {
		message, err := Decode([]byte(raw))
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if got := ChannelOf(message); got != want {
			t.Errorf("%s → %q, want %q", raw, got, want)
		}
	}
}

func TestJoinChannelID(t *testing.T) {
	good := []ID{"5", "53029", "r128", "cup:642ab9c7cddb590015abe39f.298fcf07"}
	for _, id := range good {
		if !JoinChannelID(id) {
			t.Errorf("%q should be a channel", id)
		}
	}
	bad := []ID{"", "  ", "r", "game-1", "123456789", ":cup"}
	for _, id := range bad {
		if JoinChannelID(id) {
			t.Errorf("%q should not be a channel", id)
		}
	}
}
