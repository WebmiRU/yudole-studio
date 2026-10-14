// Package api serves the JSON interface of the kernel to the options page.
//
// The page knows nothing about plugins in advance: it asks for the list, and
// every plugin brings its own settings form along.
package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"studio/core/plugin"
	"studio/core/protocol"
	"studio/core/settings"
	"studio/core/theme"
)

// Manager is the part of the plugin manager the API needs.
type Manager interface {
	List() []plugin.PluginView
	Get(id string) (plugin.PluginView, bool)
	Enable(id string) (plugin.PluginView, error)
	Disable(id string) (plugin.PluginView, error)
	SaveOptions(id string, values map[string]any) (plugin.PluginView, error)
	Run(ctx context.Context, id, kind string) (string, error)
	// WatchPlugins hands out the live plugin states: the options page takes
	// them instead of asking on a timer.
	WatchPlugins(buffer int) (<-chan plugin.PluginView, func())
	// Комнаты. Отдельный раздел настроек, но тот же config.json и тот же
	// писатель, поэтому комнаты идут через Manager, а не мимо него.
	ListRooms() []settings.Room
	GetRoom(id string) (settings.Room, bool)
	CreateRoom() (settings.Room, error)
	SaveRoom(room settings.Room) (settings.Room, error)
	DeleteRoom(id string) error
}

// RoomsNotifier сообщает открытым лентам, что комната изменилась.
//
// Узкий интерфейс вместо указателя на chat.Hub: маршруты настроек не знают
// ничего про сокеты ленты, и не должны. Нужно от них ровно одно — «скажи
// страницам».
type RoomsNotifier interface {
	RoomsChanged(id string)
}

// Handler builds the routes of the options page.
func Handler(manager Manager, themes *theme.Catalog, notifier RoomsNotifier) http.Handler {
	mux := http.NewServeMux()

	// Живое состояние плагинов. Страница настроек подписывается на него вместо
	// опроса: спрашивать каждые две секунды значило затирать наполовину
	// набранное в форме.
	mux.HandleFunc("GET /api/plugins/events", watchHandler(manager))

	// Темы. Список снят при старте, поэтому ответ мгновенный: страница
	// спрашивает его один раз и держит у себя.
	mux.HandleFunc("GET /api/themes", func(w http.ResponseWriter, r *http.Request) {
		write(w, protocol.KindThemesList, map[string]any{"themes": themes.List()})
	})

	// --- Комнаты -------------------------------------------------------------
	//
	// Комнаты живут в том же config.json, что и плагины, и пишет их Manager.
	// Формы настроек с ними общается так же, как с плагинами: список, а потом
	// адрес конкретной комнаты.

	mux.HandleFunc("GET /api/rooms", func(w http.ResponseWriter, r *http.Request) {
		write(w, protocol.KindRoomsList, map[string]any{"rooms": manager.ListRooms()})
	})

	mux.HandleFunc("POST /api/rooms", func(w http.ResponseWriter, r *http.Request) {
		if _, err := decodeRequest(r, protocol.KindRoomCreate, nil); err != nil {
			writeError(w, http.StatusBadRequest, "cannot read the request: "+err.Error())
			return
		}
		room, err := manager.CreateRoom()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Сообщаем лентам после ответа: страница перечитает комнату, и к этому
		// моменту она уже должна быть записана.
		write(w, protocol.KindRoomCreated, room)
		notifier.RoomsChanged(room.ID)
	})

	mux.HandleFunc("PUT /api/rooms/{id}", func(w http.ResponseWriter, r *http.Request) {
		room, ok := manager.GetRoom(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "room not found")
			return
		}

		// Тело приходит целиком: так сохраняется всё, что есть, и не нужно
		// различать «поле сброшено» и «поле не прислали».
		var incoming settings.Room
		if _, err := decodeRequest(r, protocol.KindRoomSave, &incoming); err != nil {
			writeError(w, http.StatusBadRequest, "cannot read the room: "+err.Error())
			return
		}

		// id приходит из адреса, а не из тела: тело ему не доверяем.
		room.ID = r.PathValue("id")
		room.Title = incoming.Title
		room.Font = incoming.Font
		room.Theme = incoming.Theme
		room.MessageSize = incoming.MessageSize
		room.AuthorSize = incoming.AuthorSize
		room.Events = incoming.Events

		saved, err := manager.SaveRoom(room)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		write(w, protocol.KindRoomSaved, saved)
		// Лента, открытая на эту комнату, применяет настройки сразу: тема,
		// шрифт и размеры не должны ждать перезагрузки страницы.
		notifier.RoomsChanged(saved.ID)
	})

	mux.HandleFunc("DELETE /api/rooms/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := decodeRequest(r, protocol.KindRoomDelete, nil); err != nil {
			writeError(w, http.StatusBadRequest, "cannot read the request: "+err.Error())
			return
		}
		if err := manager.DeleteRoom(r.PathValue("id")); err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		write(w, protocol.KindRoomDeleted, map[string]bool{"deleted": true})
		notifier.RoomsChanged(r.PathValue("id"))
	})

	mux.HandleFunc("GET /api/plugins/chat", func(w http.ResponseWriter, r *http.Request) {
		write(w, protocol.KindPluginsList, map[string]any{"plugins": manager.List()})
	})

	mux.HandleFunc("GET /api/plugins/chat/{id}", func(w http.ResponseWriter, r *http.Request) {
		view, ok := manager.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "plugin not found")
			return
		}
		write(w, protocol.KindPluginView, view)
	})

	mux.HandleFunc("PUT /api/plugins/chat/{id}/options", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Values map[string]any `json:"values"`
		}
		if _, err := decodeRequest(r, protocol.KindPluginOptionsSave, &body); err != nil {
			writeError(w, http.StatusBadRequest, "cannot read the options: "+err.Error())
			return
		}
		if body.Values == nil {
			body.Values = map[string]any{}
		}

		view, err := manager.SaveOptions(r.PathValue("id"), body.Values)
		if err != nil {
			var invalid *plugin.ValidationError
			if errors.As(err, &invalid) {
				// Плагин указал на конкретные поля: страница подсветит их и
				// не потеряет то, что уже ввёл.
				slog.Info("api: options rejected", "plugin", r.PathValue("id"), "fields", invalid.Fields)
				writeStatus(w, http.StatusUnprocessableEntity, protocol.KindError, protocol.ErrorPayload{
					Message: invalid.Message, Fields: invalid.Fields, Items: invalid.Items,
				})
				return
			}
			slog.Warn("api: saving options failed", "plugin", r.PathValue("id"), "err", err)
		}
		write(w, protocol.KindPluginOptionsSaved, view)
	})

	// Кнопка на странице настроек: «обновить базу смайлов» и всё прочее, что
	// плагин умеет делать по команде. Ядро в команде ничего не понимает: оно
	// пересылает вид и ждёт ответа.
	mux.HandleFunc("POST /api/plugins/chat/{id}/actions", func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeRequest(r, "", nil)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cannot read the action: "+err.Error())
			return
		}
		id := r.PathValue("id")
		slog.Info("api: action", "plugin", id, "kind", request.Kind)
		done, err := manager.Run(r.Context(), id, request.Kind)
		if err != nil {
			slog.Warn("api: action failed", "plugin", id, "kind", request.Kind, "err", err)
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		var body *protocol.MessageContent
		if done != "" {
			content := protocol.PlainMessage(done)
			body = &content
		}
		write(w, protocol.KindPluginActionComplete, struct {
			Done    bool                     `json:"done"`
			Message *protocol.MessageContent `json:"message,omitempty"`
		}{Done: true, Message: body})
	})

	mux.HandleFunc("POST /api/plugins/chat/{id}/enable", func(w http.ResponseWriter, r *http.Request) {
		if _, err := decodeRequest(r, protocol.KindPluginEnable, nil); err != nil {
			writeError(w, http.StatusBadRequest, "cannot read the request: "+err.Error())
			return
		}
		// A plugin that refuses to start still answers with a view: it carries
		// the state and the reason, which is exactly what the page shows.
		view, err := manager.Enable(r.PathValue("id"))
		if err != nil {
			slog.Warn("api: enabling failed", "plugin", r.PathValue("id"), "err", err)
		}
		write(w, protocol.KindPluginEnabled, view)
	})

	mux.HandleFunc("POST /api/plugins/chat/{id}/disable", func(w http.ResponseWriter, r *http.Request) {
		if _, err := decodeRequest(r, protocol.KindPluginDisable, nil); err != nil {
			writeError(w, http.StatusBadRequest, "cannot read the request: "+err.Error())
			return
		}
		view, err := manager.Disable(r.PathValue("id"))
		if err != nil {
			writeError(w, statusFor(err), err.Error())
			return
		}
		write(w, protocol.KindPluginDisabled, view)
	})

	return mux
}

func decodeRequest(r *http.Request, expectedKind string, payload any) (protocol.Message, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil {
		return protocol.Message{}, err
	}
	if len(body) > 1<<20 {
		return protocol.Message{}, fmt.Errorf("request exceeds 1 MiB")
	}
	message, err := protocol.Decode(body)
	if err != nil {
		return protocol.Message{}, err
	}
	if expectedKind != "" && message.Kind != expectedKind {
		return protocol.Message{}, fmt.Errorf("kind %q is not %q", message.Kind, expectedKind)
	}
	if payload != nil {
		if err := message.PayloadInto(payload); err != nil {
			return protocol.Message{}, err
		}
	}
	return message, nil
}

func write(w http.ResponseWriter, kind string, payload any) {
	writeStatus(w, http.StatusOK, kind, payload)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeStatus(w, code, protocol.KindError, protocol.ErrorPayload{Message: message})
}

func writeStatus(w http.ResponseWriter, status int, kind string, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	message, err := protocol.New(kind, payload)
	if err != nil {
		slog.Error("api: cannot encode response", "kind", kind, "err", err)
		http.Error(w, "cannot encode response", http.StatusInternalServerError)
		return
	}
	data, err := json.Marshal(message, json.Deterministic(true))
	if err != nil {
		slog.Error("api: cannot marshal response", "kind", kind, "err", err)
		http.Error(w, "cannot marshal response", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	if _, err := w.Write(data); err != nil {
		slog.Debug("api: write response", "kind", kind, "err", err)
	}
}

// statusFor maps a manager error to a status code. A plugin that refuses its
// options is the user's problem, not a broken server.
func statusFor(err error) int {
	message := err.Error()
	switch {
	case strings.Contains(message, "is not installed"):
		return http.StatusNotFound
	case strings.Contains(message, "rejected"), strings.Contains(message, "want a"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
