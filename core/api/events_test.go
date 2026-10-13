package api

// Канал состояний плагинов для страницы настроек.

import (
	"context"
	"encoding/json/v2"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"studio/core/plugin"
	"studio/core/settings"
)

// fakeManager plays the part of the plugin manager for the states page needs.
// Один канал на всё время, как у настоящего: иначе событие, отправленное до
// подписки, ушло бы в пустоту.
type fakeManager struct {
	views []plugin.PluginView

	states   chan plugin.PluginView
	mu       sync.Mutex
	subbed   int
	closedIn bool
}

func newFakeManager() *fakeManager {
	return &fakeManager{states: make(chan plugin.PluginView, 64)}
}

func (f *fakeManager) List() []plugin.PluginView { return f.views }

func (f *fakeManager) WatchPlugins(int) (<-chan plugin.PluginView, func()) {
	f.mu.Lock()
	f.subbed++
	f.mu.Unlock()

	var once sync.Once
	return f.states, func() {
		once.Do(func() {
			f.mu.Lock()
			f.closedIn = true
			f.mu.Unlock()
		})
	}
}

func (f *fakeManager) Get(string) (plugin.PluginView, bool) { return plugin.PluginView{}, false }
func (f *fakeManager) Enable(string) (plugin.PluginView, error) {
	return plugin.PluginView{}, nil
}
func (f *fakeManager) Disable(string) (plugin.PluginView, error) {
	return plugin.PluginView{}, nil
}
func (f *fakeManager) SaveOptions(string, map[string]any) (plugin.PluginView, error) {
	return plugin.PluginView{}, nil
}
func (f *fakeManager) Run(context.Context, string, string) (string, error) { return "", nil }

// Комнаты. Эти тесты проверяют ленту состояний, а не комнаты, поэтому подменя
// их пустыми: методы нужны лишь потому, что Manager требует полный набор.
func (f *fakeManager) ListRooms() []settings.Room { return nil }
func (f *fakeManager) GetRoom(string) (settings.Room, bool) {
	return settings.Room{}, false
}
func (f *fakeManager) CreateRoom() (settings.Room, error)                 { return settings.Room{}, nil }
func (f *fakeManager) SaveRoom(room settings.Room) (settings.Room, error) { return room, nil }
func (f *fakeManager) DeleteRoom(string) error                            { return nil }

// readState waits for one message of the states page.
func readState(t *testing.T, conn *websocket.Conn) stateMessage {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var message stateMessage
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatalf("not a message of the states page: %v", err)
	}
	return message
}

// При подключении страница получает все плагины разом: с этого начинается её
// работа, спрашивать список отдельно незачем.
func TestStatesPageGetsEveryPluginOnOpen(t *testing.T) {
	fake := newFakeManager()
	fake.views = []plugin.PluginView{
		{ID: "goodgame", Title: "GoodGame", State: "running"},
		{ID: "twitch", Title: "Twitch", State: "off"},
	}

	server := httptest.NewServer(watchHandler(fake))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	hello := readState(t, conn)
	if hello.Hello == nil {
		t.Fatal("первым пришло не приветствие")
	}
	if len(hello.Hello.Plugins) != 2 {
		t.Fatalf("плагинов %d, want 2", len(hello.Hello.Plugins))
	}
	if hello.Hello.Plugins[0].ID != "goodgame" || hello.Hello.Plugins[0].Title != "GoodGame" {
		t.Errorf("плагины = %+v", hello.Hello.Plugins)
	}
}

// Изменение приходит сразу после того, как произошло, — без опроса.
func TestStatesPageGetsChangesAsTheyHappen(t *testing.T) {
	fake := newFakeManager()
	server := httptest.NewServer(watchHandler(fake))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	readState(t, conn) // приветствие

	fake.states <- plugin.PluginView{ID: "goodgame", State: "running", Waiting: true, WaitingText: "Спрашиваю GoodGame"}

	live := readState(t, conn)
	if live.Plugin == nil {
		t.Fatal("пришло не состояние плагина")
	}
	if live.Plugin.ID != "goodgame" || live.Plugin.State != "running" {
		t.Errorf("состояние = %+v", live.Plugin)
	}
	if !live.Plugin.Waiting || live.Plugin.WaitingText != "Спрашиваю GoodGame" {
		t.Errorf("занятость не доехала: %+v", live.Plugin)
	}
}

// Настроек, а с ними и паролей, в потоке быть не должно: он живёт весь день и
// уходит в браузер десятки раз, а значения нужны один раз и по делу.
func TestStatesStreamCarriesNoValues(t *testing.T) {
	fake := newFakeManager()
	fake.views = []plugin.PluginView{{
		ID:     "twitch",
		State:  "running",
		Values: map[string]any{"nick": "vasya", "token": "oauth:secret"},
	}}

	server := httptest.NewServer(watchHandler(fake))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	hello := readState(t, conn)
	if hello.Hello == nil || len(hello.Hello.Plugins) != 1 {
		t.Fatalf("приветствие = %+v", hello.Hello)
	}

	line, err := marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(line, "secret") || strings.Contains(line, "oauth:") {
		t.Errorf("пароль попал в поток состояний: %s", line)
	}
	if strings.Contains(line, "values") {
		t.Errorf("настройки попали в поток состояний: %s", line)
	}
}

// marshal is how the page would see one message.
func marshal(message stateMessage) (string, error) {
	data, err := json.Marshal(message, json.Deterministic(true))
	return string(data), err
}

// Закрывшаяся страница не должна оставлять подписку: её очередь перестанет
// читать, и ядро начнёт терять состояния.
func TestStatesPageLeavesNoSubscription(t *testing.T) {
	fake := newFakeManager()
	server := httptest.NewServer(watchHandler(fake))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	readState(t, conn)

	if err := conn.CloseNow(); err != nil {
		t.Fatalf("close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fake.mu.Lock()
		closed := fake.closedIn
		fake.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("подписка осталась после закрытия страницы")
}
