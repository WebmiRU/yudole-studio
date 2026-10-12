package api

// Живые состояния плагинов для страницы настроек.
//
// Страница ничего не спрашивает по таймеру: как только подключилась — получила
// все плагины разом, а дальше ядро само сообщает, что с каждым из них
// происходит. Причина простая: опрос затирал наполовину набранное в форме,
// и страница выглядела так, будто человек печатает мимо.

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"studio/core/plugin"
)

const (
	// stateWriteTimeout for one message. Страница, которая перестала читать,
	// не должна держать горутину ядра.
	stateWriteTimeout = 10 * time.Second

	// stateQueue is how many states we hold for one page before dropping.
	stateQueue = 16
)

// stateMessage is one message of the options page's websocket: what is going on
// with the plugins right now.
type stateMessage struct {
	// Hello is the full picture on connect: every plugin found on disk.
	Hello *stateHello `json:"hello,omitempty"`
	// Plugin is what changed: one plugin's state, right after the change.
	Plugin *plugin.PluginView `json:"plugin,omitempty"`
}

// stateHello is what a page gets when it opens.
type stateHello struct {
	Plugins []plugin.PluginView `json:"plugins"`
}

// watchHandler serves the websocket of the options page.
func watchHandler(manager Manager) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(rw, r, &websocket.AcceptOptions{
			// Страница может прийти с дев-сервера с другого порта.
			InsecureSkipVerify: true,
		})
		if err != nil {
			slog.Debug("options: cannot accept", "err", err)
			return
		}
		defer conn.CloseNow()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		// Подписка до первого ответа: иначе между «вот все плагины» и
		// подпиской потерялось бы то, что с ними случилось.
		views, unwatch := manager.WatchPlugins(stateQueue)
		defer unwatch()

		hello := stateMessage{Hello: &stateHello{Plugins: manager.List()}}
		if err := writeState(ctx, conn, hello); err != nil {
			return
		}

		// Соединение надо ещё и читать — не ради сообщений (страница их не
		// шлёт), а чтобы узнать, когда оно умрёт. Пока мы только пишем,
		// закрывшаяся страница заметна не сразу.
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
			case view, ok := <-views:
				if !ok {
					return
				}
				if err := writeState(ctx, conn, stateMessage{Plugin: &view}); err != nil {
					return
				}
			}
		}
	}
}

func writeState(ctx context.Context, conn *websocket.Conn, message stateMessage) error {
	line, err := json.Marshal(message, json.Deterministic(true))
	if err != nil {
		slog.Error("options: cannot encode a state", "err", err)
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, stateWriteTimeout)
	defer cancel()

	if err := conn.Write(writeCtx, websocket.MessageText, line); err != nil {
		slog.Debug("options: cannot write a state", "err", err)
		return err
	}
	return nil
}
