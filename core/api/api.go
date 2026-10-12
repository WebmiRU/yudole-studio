// Package api serves the JSON interface of the kernel to the options page.
//
// The page knows nothing about plugins in advance: it asks for the list, and
// every plugin brings its own settings form along.
package api

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"studio/core/plugin"
)

// Manager is the part of the plugin manager the API needs.
type Manager interface {
	List() []plugin.PluginView
	Get(id string) (plugin.PluginView, bool)
	Enable(id string) (plugin.PluginView, error)
	Disable(id string) (plugin.PluginView, error)
	SaveOptions(id string, values map[string]any) (plugin.PluginView, error)
	Run(ctx context.Context, id, kind string) (string, error)
}

// Handler builds the routes of the options page.
func Handler(manager Manager) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/plugins/chat", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"plugins": manager.List()})
	})

	mux.HandleFunc("GET /api/plugins/chat/{id}", func(w http.ResponseWriter, r *http.Request) {
		view, ok := manager.Get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, "plugin not found")
			return
		}
		write(w, view)
	})

	mux.HandleFunc("PUT /api/plugins/chat/{id}/options", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Values map[string]any `json:"values"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
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
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusUnprocessableEntity)
				if err := json.MarshalEncode(jsontext.NewEncoder(w), map[string]any{
					"error":  invalid.Message,
					"fields": invalid.Fields,
					"items":  invalid.Items,
				}); err != nil {
					slog.Error("api: cannot write the response", "err", err)
				}
				return
			}
			slog.Warn("api: saving options failed", "plugin", r.PathValue("id"), "err", err)
		}
		write(w, view)
	})

	// Кнопка на странице настроек: «обновить базу смайлов» и всё прочее, что
	// плагин умеет делать по команде. Ядро в команде ничего не понимает: оно
	// пересылает вид и ждёт ответа.
	mux.HandleFunc("POST /api/plugins/chat/{id}/actions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Kind string `json:"kind"`
		}
		if err := json.UnmarshalRead(r.Body, &body); err != nil {
			writeError(w, http.StatusBadRequest, "cannot read the action: "+err.Error())
			return
		}
		if body.Kind == "" {
			writeError(w, http.StatusBadRequest, "no kind in the action")
			return
		}

		id := r.PathValue("id")
		slog.Info("api: action", "plugin", id, "kind", body.Kind)

		done, err := manager.Run(r.Context(), id, body.Kind)
		if err != nil {
			slog.Warn("api: action failed", "plugin", id, "kind", body.Kind, "err", err)
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		// Что именно вышло, знает только плагин: он и пишет, ядро пересылает.
		write(w, map[string]any{"kind": body.Kind, "done": true, "message": done})
	})

	mux.HandleFunc("POST /api/plugins/chat/{id}/enable", func(w http.ResponseWriter, r *http.Request) {
		// A plugin that refuses to start still answers with a view: it carries
		// the state and the reason, which is exactly what the page shows.
		view, err := manager.Enable(r.PathValue("id"))
		if err != nil {
			slog.Warn("api: enabling failed", "plugin", r.PathValue("id"), "err", err)
		}
		write(w, view)
	})

	mux.HandleFunc("POST /api/plugins/chat/{id}/disable", func(w http.ResponseWriter, r *http.Request) {
		view, err := manager.Disable(r.PathValue("id"))
		if err != nil {
			writeError(w, statusFor(err), err.Error())
			return
		}
		write(w, view)
	})

	return mux
}

func write(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if err := json.MarshalEncode(jsontext.NewEncoder(w), payload, json.Deterministic(true)); err != nil {
		slog.Error("api: cannot write the response", "err", err)
	}
}

func writeError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.MarshalEncode(jsontext.NewEncoder(w), map[string]string{"error": message}); err != nil {
		slog.Error("api: cannot write the error", "err", err)
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
