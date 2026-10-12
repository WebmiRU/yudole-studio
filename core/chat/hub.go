// Package chat serves the live chat: everything the plugins say about it goes
// through here.
//
// The page cannot ask for a chat line — it has to receive it, or it would have
// to poll. So the kernel keeps the last few hundred lines in memory, sends them
// to a page the moment it connects, and then pushes whatever comes next.
//
// One websocket serves every plugin and every channel: a page that shows all
// the chats does not care who is talking.
package chat

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"studio/core/plugin"
)

const (
	// HistoryLimit is how many events a page gets when it opens. Enough to
	// fill the screen on a quiet channel, small enough to send at once.
	HistoryLimit = 200

	// WriteTimeout for one event. A page that stopped reading must not keep a
	// goroutine busy forever.
	WriteTimeout = 10 * time.Second

	// sendQueue is how many events we hold for one page before dropping.
	sendQueue = 512
)

// Source is the part of the plugin manager the chat page needs.
type Source interface {
	Subscribe(buffer int) (<-chan plugin.Event, func())
	List() []plugin.PluginView
}

// Hub keeps the recent events and hands them to the pages that connect.
type Hub struct {
	source Source

	mu      sync.Mutex
	history []Entry

	subscribers map[int]*subscriber
	next        int
}

// Entry is one event with the time it happened, ready for the page.
type Entry struct {
	At      time.Time      `json:"at"`
	Plugin  string         `json:"plugin"`
	Kind    string         `json:"kind"`
	Channel string         `json:"channel,omitempty"`
	Payload jsontext.Value `json:"payload"`
	// Text is the line as plain text, for the page to show without decoding.
	Text string `json:"text,omitempty"`
	User string `json:"user,omitempty"`
}

// subscriber is one connected page.
type subscriber struct {
	events chan Entry
}

// New builds a hub over the plugins.
func New(source Source) *Hub {
	hub := &Hub{
		source:      source,
		subscribers: map[int]*subscriber{},
	}
	go hub.watch()
	return hub
}

// watch reads the events of every plugin and remembers them.
func (h *Hub) watch() {
	events, unsubscribe := h.source.Subscribe(sendQueue)
	defer unsubscribe()

	for event := range events {
		entry := toEntry(event)
		if entry == nil {
			continue
		}

		h.mu.Lock()
		h.history = append(h.history, *entry)
		if len(h.history) > HistoryLimit {
			// Подрезаем с начала, но не по кускам: страница получит столько
			// старых строк, сколько поместилось.
			h.history = append([]Entry(nil), h.history[len(h.history)-HistoryLimit:]...)
		}
		targets := make([]*subscriber, 0, len(h.subscribers))
		for _, sub := range h.subscribers {
			targets = append(targets, sub)
		}
		h.mu.Unlock()

		for _, sub := range targets {
			select {
			case sub.events <- *entry:
			default:
				slog.Warn("chat page is behind, dropping an event",
					"kind", entry.Kind, "channel", entry.Channel)
			}
		}
	}
}

// toEntry turns a plugin event into what the page needs, or nil when there is
// nothing to show.
func toEntry(event plugin.Event) *Entry {
	entry := &Entry{
		At:      time.Now(),
		Plugin:  event.Plugin,
		Kind:    event.Kind(),
		Channel: event.Channel(),
		Payload: event.Message.Payload,
	}

	// Строку чата показываем сразу готовой: странице не нужно знать, где в
	// полезной нагрузке лежит текст.
	if chat, err := event.Chat(); err == nil {
		entry.Text = chat.Message.Text
		entry.User = chat.User.Name
	}
	return entry
}

// Channels tells what the plugins are in and how the connection goes: the
// first thing a page shows, before any line has arrived.
func (h *Hub) Channels() []Channel {
	views := h.source.List()

	out := make([]Channel, 0, len(views))
	for _, view := range views {
		entry := Channel{
			Plugin: view.ID,
			Title:  view.Title,
			State:  view.State,
		}
		if view.Problem != "" {
			entry.Problem = view.Problem
		}
		out = append(out, entry)
	}
	return out
}

// Channel is what a plugin is doing, for the sidebar of the page.
type Channel struct {
	Plugin  string `json:"plugin"`
	Title   string `json:"title,omitempty"`
	State   string `json:"state"`
	Problem string `json:"problem,omitempty"`
}

// Handler serves the websocket of the chat page.
func (h *Hub) Handler() http.Handler {
	return http.HandlerFunc(h.serve)
}

func (h *Hub) serve(rw http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(rw, r, &websocket.AcceptOptions{
		// The page can come from the dev server on another port.
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Debug("chat: cannot accept", "err", err)
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sub := &subscriber{events: make(chan Entry, sendQueue)}

	h.mu.Lock()
	id := h.next
	h.next++
	h.subscribers[id] = sub
	history := slices.Clone(h.history)
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.subscribers, id)
		h.mu.Unlock()
	}()

	// Сначала то, что уже было, и только потом новое: иначе между «историей»
	// и подпиской потерялось бы всё, что пришло за эти доли секунды.
	if err := write(ctx, conn, envelope{Hello: &Hello{
		Plugins: h.Channels(),
		History: history,
	}}); err != nil {
		return
	}
	// Соединение надо ещё и читать — и не для сообщений (страница их не шлёт),
	// а чтобы узнать, когда оно умрёт. Пока мы только пишем, закрывшаяся
	// страница заметна не сразу: Ping ждёт ответа, а отвечать некому.
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gone:
			return
		case entry, ok := <-sub.events:
			if !ok {
				return
			}
			if err := write(ctx, conn, envelope{Event: &entry}); err != nil {
				return
			}
		}
	}
}

// Hello is what a page gets when it opens: the plugins and what has already
// been said.
type Hello struct {
	Plugins []Channel `json:"plugins"`
	History []Entry   `json:"history"`
}

// envelope is one message of the page's websocket. Exactly one of the fields is
// filled: what has just happened, or what has just joined.
type envelope struct {
	Hello *Hello `json:"hello,omitempty"`
	Event *Entry `json:"event,omitempty"`
}

func write(ctx context.Context, conn *websocket.Conn, message envelope) error {
	line, err := json.Marshal(message, json.Deterministic(true))
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()

	if err := conn.Write(writeCtx, websocket.MessageText, line); err != nil {
		slog.Debug("chat: cannot write", "err", err)
		return err
	}
	return nil
}

func slicesClone(list []Entry) []Entry {
	return append([]Entry(nil), list...)
}
