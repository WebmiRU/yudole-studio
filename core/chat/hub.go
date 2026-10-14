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
	"studio/core/protocol"
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
	history []protocol.Message

	subscribers map[int]*subscriber
	next        int
}

// subscriber is one connected page.
type subscriber struct {
	events chan protocol.Message
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
		message, err := toMessage(event)
		if err != nil {
			slog.Warn("chat: cannot normalize plugin event", "plugin", event.Plugin, "kind", event.Kind(), "err", err)
			continue
		}

		h.mu.Lock()
		h.history = append(h.history, message)
		if len(h.history) > HistoryLimit {
			h.history = append([]protocol.Message(nil), h.history[len(h.history)-HistoryLimit:]...)
		}
		targets := make([]*subscriber, 0, len(h.subscribers))
		for _, sub := range h.subscribers {
			targets = append(targets, sub)
		}
		h.mu.Unlock()

		for _, sub := range targets {
			select {
			case sub.events <- message:
			default:
				slog.Warn("chat page is behind, dropping an event", "kind", message.Kind)
			}
		}
	}
}

// toMessage preserves the plugin's kind and payload and adds source metadata
// inside payload so every websocket frame remains one protocol envelope.
func toMessage(event plugin.Event) (protocol.Message, error) {
	payload := map[string]any{}
	if len(event.Message.Payload) != 0 {
		if err := json.Unmarshal(event.Message.Payload, &payload); err != nil { return protocol.Message{}, err }
	}
	payload["plugin"] = event.Plugin
	payload["at"] = time.Now()
	raw, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil { return protocol.Message{}, err }
	return protocol.Message{Kind: event.Message.Kind, Payload: jsontext.Value(raw)}, nil
}

// Channels tells which plugins are and how they are doing. The page shows it
// before any line has arrived.
//
// Здесь только то, что плагин объявил о себе сам: название и состояние.
// Названий его каналов ядро не знает и знать не должно — это его настройки, и
// оно само присылает их в своих событиях.
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

// Channel is what a plugin is doing.
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

	sub := &subscriber{events: make(chan protocol.Message, sendQueue)}

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
	snapshot := protocol.MustNew(protocol.KindChatSnapshot, struct {
		Plugins []Channel `json:"plugins"`
		History []protocol.Message `json:"history"`
	}{Plugins: h.Channels(), History: history})
	if err := write(ctx, conn, snapshot); err != nil {
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
		case message, ok := <-sub.events:
			if !ok { return }
			if err := write(ctx, conn, message); err != nil {
				return
			}
		}
	}
}

func write(ctx context.Context, conn *websocket.Conn, message protocol.Message) error {
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

