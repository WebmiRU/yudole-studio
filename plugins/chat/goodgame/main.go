// Command goodgame is the GoodGame chat plugin for studio.
//
// The kernel launches it and talks over stdin/stdout with NDJSON: one JSON
// message per line. On start the plugin says hello, describes itself when the
// kernel asks, and takes the channel list the user filled in.
package main

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	announce  = 3 * time.Second
	scanLimit = 64 * 1024
	id        = "goodgame"
)

// message mirrors the kernel protocol. Unknown fields are ignored on purpose:
// a newer kernel may send more than we understand.
type message struct {
	Type   string         `json:"type"`
	Values map[string]any `json:"values,omitempty"`
}

type option struct {
	Key         string   `json:"key"`
	Type        string   `json:"type"`
	Label       string   `json:"label"`
	Hint        string   `json:"hint,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Default     any      `json:"default,omitempty"`
	Choices     []choice `json:"choices,omitempty"`
}

type choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type info struct {
	Type        string   `json:"type"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Options     []option `json:"options"`
}

// options describes the settings page the kernel renders for us.
var options = []option{{
	Key:         "channels",
	Type:        "tags",
	Label:       "Каналы",
	Hint:        "Enter — добавить канал",
	Placeholder: "канал",
	Default:     []string{},
}}

// channels are the rooms to listen to. The kernel fills them from config.json.
var channels []string

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	commands := commandsFrom(ctx, os.Stdin)
	ticker := time.NewTicker(announce)
	defer ticker.Stop()

	sayLog("Hello from goodgame plugin")

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			sayLog("Hello from goodgame plugin")

		case line, ok := <-commands:
			if !ok {
				// The kernel closed stdin: nothing left to talk to.
				return
			}
			if handle(line) {
				return
			}
		}
	}
}

// handle processes one command and reports whether the plugin should stop.
func handle(line string) bool {
	var msg message
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		sayError(fmt.Sprintf("cannot parse %q: %v", line, err))
		return false
	}

	switch msg.Type {
	case "get_info":
		send(info{
			Type:        "info",
			ID:          id,
			Name:        "GoodGame",
			Version:     "0.1.0",
			Description: "Чат игровой площадки goodgame.ru",
			Options:     options,
		})

	case "set_options":
		apply(msg.Values)
		sendAck()

	case "quit":
		sendAck()
		return true

	default:
		sayError("unknown command " + msg.Type)
	}
	return false
}

// apply takes the channels out of the values sent by the kernel.
func apply(values map[string]any) {
	if values == nil {
		return
	}

	raw, ok := values["channels"]
	if !ok {
		return
	}

	list, ok := raw.([]any)
	if !ok {
		sayError(`option "channels" must be a list of strings`)
		return
	}

	next := make([]string, 0, len(list))
	for _, item := range list {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			sayError(`option "channels" must hold strings only`)
			return
		}
		next = append(next, strings.TrimSpace(name))
	}

	channels = next
	sayLog(fmt.Sprintf("following %d channels: %s", len(channels), strings.Join(channels, ", ")))
}

// commandsFrom reads the kernel's NDJSON lines. The channel is closed when
// stdin is closed or the context is cancelled.
func commandsFrom(ctx context.Context, in *os.File) <-chan string {
	out := make(chan string)

	go func() {
		defer close(out)

		lines := make(chan string)
		go func() {
			defer close(lines)

			scanner := bufio.NewScanner(in)
			scanner.Buffer(make([]byte, 0, 4096), scanLimit)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
			_ = scanner.Err()
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case line, ok := <-lines:
				if !ok {
					return
				}
				select {
				case out <- line:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out
}

func send(payload any) {
	line, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		sayError(fmt.Sprintf("cannot encode: %v", err))
		return
	}
	fmt.Fprintln(os.Stdout, string(line))
}

type ack struct {
	Type string `json:"type"`
}

type logMessage struct {
	Type    string `json:"type"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type errorMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func sendAck() {
	send(ack{Type: "ack"})
}

func sayLog(text string) {
	send(logMessage{Type: "log", Level: "info", Message: text})
}

func sayError(text string) {
	send(errorMessage{Type: "error", Message: text})
}
