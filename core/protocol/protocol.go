// Package protocol is the contract between the kernel and its plugins.
//
// Every line is one JSON object: kind is required; payload may be omitted. Any text
// uses the shared message.src/text/html shape, while channel, user and message
// keep the same meaning across services and transports.
//
//	{"kind":"system.plugin.hello","payload":{"title":"GoodGame","options":[...]}}
//	{"kind":"system.plugin.options.set","payload":{"values":{"channels":["42"]}}}
//	{"kind":"chat.message.public","payload":{"channel":"42","user":{...},"message":{"src":"...","text":"...","html":"..."}}}
//
// The kernel and the plugins import this package instead of keeping their own
// copies of the shapes: a mistake here then breaks both sides at once, rather
// than one of them quietly sending something the other never reads.
package protocol

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	stdhtml "html"
	"strings"
)

// Kinds of messages.
const (
	// A plugin introduces itself right after it starts.
	KindPluginChat = "system.plugin.hello"
	// The kernel hands the plugin the values the user filled in.
	KindSetOptions = "system.plugin.options.set"
	// The plugin confirms it took the options.
	KindAck = "system.message.ack"
	// Something went wrong inside the plugin.
	KindError = "system.message.error"
	// The plugin is busy: it will answer the request later.
	KindWaiting = "system.message.waiting"
	// Anything the plugin wants to say to the kernel log.
	KindLog = "system.log"
	// The kernel asks the plugin to finish.
	KindQuit = "system.plugin.quit"

	// The kernel asks a plugin to do something that is not about its options:
	// refresh its base of smileys. Начинается с system., потому что команда
	// общая для всех чатов и смысла в имени плагина не несёт.
	KindSmilesUpdate = "system.chat.smiles.update"

	// A line of chat: public goes to the rooms everyone can see, private to a
	// direct conversation.
	KindMessagePublic  = "chat.message.public"
	KindMessagePrivate = "chat.message.private"

	KindSystemMessageWarning = "system.message.warning"
	KindChatSnapshot = "system.chat.snapshot"
	KindPluginsSnapshot = "system.plugins.snapshot"
	KindPluginState = "system.plugin.state"

	KindThemesList = "system.themes.list"
	KindRoomsList = "system.rooms.list"
	KindRoomCreate = "system.room.create"
	KindRoomCreated = "system.room.created"
	KindRoomSave = "system.room.save"
	KindRoomSaved = "system.room.saved"
	KindRoomDelete = "system.room.delete"
	KindRoomDeleted = "system.room.deleted"
	KindPluginsList = "system.plugins.list"
	KindPluginView = "system.plugin.view"
	KindPluginOptionsSave = "system.plugin.options.save"
	KindPluginOptionsSaved = "system.plugin.options.saved"
	KindPluginActionComplete = "system.plugin.action.completed"
	KindPluginEnable = "system.plugin.enable"
	KindPluginEnabled = "system.plugin.enabled"
	KindPluginDisable = "system.plugin.disable"
	KindPluginDisabled = "system.plugin.disabled"

	// What the plugin does with the channels it was given. These four are the
	// same for every chat service: Twitch, YouTube, GoodGame — разница только
	// в том, что под каналом понимается.
	KindChannelJoinSuccess   = "chat.channel.join.success"
	KindChannelJoinFailed    = "chat.channel.join.failed"
	KindChannelUnjoinSuccess = "chat.channel.unjoin.success"
	KindChannelUnjoinFailed  = "chat.channel.unjoin.failed"

	// The service told us something about a channel we are in: its name, who
	// is streaming there, how many people are watching.
	KindChannelInfo = "chat.channel.info"
	// A message was taken out of the chat by a moderator.
	KindMessageDeleted = "chat.message.deleted"
	// The connection to the service is up, down or being retried. A plugin that
	// sits quietly on a dead connection looks like a hung one.
	KindConnectionState = "chat.connection.state"
)

// Connection states a plugin reports.
const (
	ConnectionConnecting   = "connecting"
	ConnectionOnline       = "online"
	ConnectionReconnecting = "reconnecting"
	ConnectionOffline      = "offline"
)

// IsChatKind reports whether a kind carries a chat line.
func IsChatKind(kind string) bool {
	return kind == KindMessagePublic || kind == KindMessagePrivate
}

// IsChannelKind reports whether a kind talks about a channel rather than about
// what happens in it.
func IsChannelKind(kind string) bool {
	switch kind {
	case KindChannelJoinSuccess, KindChannelJoinFailed,
		KindChannelUnjoinSuccess, KindChannelUnjoinFailed,
		KindChannelInfo, KindMessageDeleted, KindConnectionState:
		return true
	default:
		return false
	}
}

// Message is the envelope of one protocol line.
type Message struct {
	Kind    string         `json:"kind"`
	Payload jsontext.Value `json:"payload,omitempty"`
}

// New builds an envelope with a payload of any shape.
func New(kind string, payload any) (Message, error) {
	if kind == "" { return Message{}, fmt.Errorf("message without a kind") }
	kind = canonicalKind(kind)
	if payload == nil {
		message := Message{Kind: kind}
		if err := Validate(message); err != nil { return Message{}, err }
		return message, nil
	}
	raw, err := json.Marshal(wirePayload(payload), json.Deterministic(true))
	if err != nil { return Message{}, fmt.Errorf("encode the %s payload: %w", kind, err) }
	message := Message{Kind: kind, Payload: raw}
	if err := Validate(message); err != nil { return Message{}, err }
	return message, nil
}

// MustNew is New for payloads that cannot fail to encode.
func MustNew(kind string, payload any) Message {
	message, err := New(kind, payload)
	if err != nil {
		panic("protocol: " + err.Error())
	}
	return message
}

// PayloadInto decodes the payload of a message into v.
func (m Message) PayloadInto(v any) error {
	if len(m.Payload) == 0 { return fmt.Errorf("message %q has no payload", m.Kind) }
	switch out := v.(type) {
	case *AckPayload:
		var wire struct { Values map[string]any `json:"values"`; Message jsontext.Value `json:"message"` }
		if err := json.Unmarshal(m.Payload, &wire); err != nil { return err }
		text, err := readMessageText(wire.Message); if err != nil { return err }; out.Values, out.Message = wire.Values, text; return nil
	case *LogPayload:
		var wire struct { Level string `json:"level"`; Message jsontext.Value `json:"message"` }
		if err := json.Unmarshal(m.Payload, &wire); err != nil { return err }
		text, err := readMessageText(wire.Message); if err != nil { return err }; out.Level, out.Message = wire.Level, text; return nil
	case *WaitingPayload:
		var wire struct { Message jsontext.Value `json:"message"` }
		if err := json.Unmarshal(m.Payload, &wire); err != nil { return err }
		text, err := readMessageText(wire.Message); if err != nil { return err }; out.Message = text; return nil
	case *ErrorPayload:
		var wire struct { Message jsontext.Value `json:"message"`; Fields map[string]string `json:"fields"`; Items map[string]map[string]string `json:"items"` }
		if err := json.Unmarshal(m.Payload, &wire); err != nil { return err }
		text, err := readMessageText(wire.Message); if err != nil { return err }; out.Message, out.Fields, out.Items = text, wire.Fields, wire.Items; return nil
	case *ChannelPayload:
		var wire struct { Channel string `json:"channel"`; Name string `json:"name"`; Message jsontext.Value `json:"message"`; Reason string `json:"reason"` }
		if err := json.Unmarshal(m.Payload, &wire); err != nil { return err }
		text, err := readMessageText(wire.Message); if err != nil { return err }; if text == "" { text = wire.Reason }; out.Channel, out.Name, out.Reason = wire.Channel, wire.Name, text; return nil
	case *ConnectionPayload:
		var wire struct { State string `json:"state"`; Service string `json:"service"`; Message jsontext.Value `json:"message"`; Reason string `json:"reason"`; Attempt int `json:"attempt"` }
		if err := json.Unmarshal(m.Payload, &wire); err != nil { return err }
		text, err := readMessageText(wire.Message); if err != nil { return err }; if text == "" { text = wire.Reason }; out.State, out.Service, out.Reason, out.Attempt = wire.State, wire.Service, text, wire.Attempt; return nil
	default:
		return json.Unmarshal(m.Payload, v)
	}
}

func readMessageText(raw jsontext.Value) (string, error) {
	if len(raw) == 0 { return "", nil }
	var body MessageContent
	if err := json.Unmarshal(raw, &body); err == nil { if body.Text != "" { return body.Text, nil }; return body.Src, nil }
	var legacy string
	if err := json.Unmarshal(raw, &legacy); err == nil { return legacy, nil }
	return "", fmt.Errorf("payload.message must be an object with src/text/html")
}

// wirePayload normalizes all human-readable text into the shared message body.
func wirePayload(payload any) any {
	switch value := payload.(type) {
	case AckPayload:
		return struct { Values map[string]any `json:"values,omitempty"`; Message *MessageContent `json:"message,omitempty"` }{value.Values, messageBody(value.Message)}
	case LogPayload:
		return struct { Level string `json:"level"`; Message *MessageContent `json:"message,omitempty"` }{value.Level, messageBody(value.Message)}
	case WaitingPayload:
		return struct { Message *MessageContent `json:"message,omitempty"` }{messageBody(value.Message)}
	case ErrorPayload:
		return struct { Message *MessageContent `json:"message,omitempty"`; Fields map[string]string `json:"fields,omitempty"`; Items map[string]map[string]string `json:"items,omitempty"` }{messageBody(value.Message), value.Fields, value.Items}
	case ChannelPayload:
		return struct { Channel string `json:"channel"`; Name string `json:"name,omitempty"`; Message *MessageContent `json:"message,omitempty"` }{value.Channel, value.Name, messageBody(value.Reason)}
	case ConnectionPayload:
		return struct { State string `json:"state"`; Service string `json:"service,omitempty"`; Message *MessageContent `json:"message,omitempty"`; Attempt int `json:"attempt,omitempty"` }{value.State, value.Service, messageBody(value.Reason), value.Attempt}
	default: return payload
	}
}

func messageBody(text string) *MessageContent {
	if text == "" { return nil }; body := PlainMessage(text); return &body
}

// Encode renders a message as one protocol line.
func Encode(message Message) ([]byte, error) {
	message.Kind = canonicalKind(message.Kind)
	if err := Validate(message); err != nil { return nil, err }
	data, err := json.Marshal(message, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Decode reads one protocol line. The kind must be there: a line without it
// cannot be routed.
func Decode(line []byte) (Message, error) {
	var message Message
	if err := json.Unmarshal(line, &message); err != nil {
		return Message{}, err
	}
	if message.Kind == "" {
		return Message{}, fmt.Errorf("message without a kind: %s", line)
	}
	message.Kind = canonicalKind(message.Kind)
	if err := Validate(message); err != nil { return Message{}, err }
	return message, nil
}

// canonicalKind accepts legacy short control kinds on input. Encode always uses
// the current constants, so all newly emitted messages share the same namespace.
func canonicalKind(kind string) string {
	switch kind {
	case "plugin.chat": return KindPluginChat
	case "set_options": return KindSetOptions
	case "ack": return KindAck
	case "error": return KindError
	case "waiting": return KindWaiting
	case "log": return KindLog
	case "quit": return KindQuit
	default: return kind
	}
}

// Validate enforces only the semantics that are intrinsic to a kind. Unknown
// kinds remain extensible; their payload schemas belong to their producers.
func Validate(message Message) error {
	if message.Kind == "" {
		return fmt.Errorf("message without a kind")
	}

	requiresPayload := IsChatKind(message.Kind) ||
		message.Kind == KindSystemMessageWarning ||
		message.Kind == KindError || message.Kind == KindWaiting ||
		message.Kind == KindLog || message.Kind == KindPluginChat ||
		message.Kind == KindSetOptions

	if len(message.Payload) == 0 || string(message.Payload) == "null" {
		if requiresPayload {
			return fmt.Errorf("message %q requires a payload", message.Kind)
		}
		return nil
	}

	var payload map[string]any
	if err := json.Unmarshal(message.Payload, &payload); err != nil {
		return fmt.Errorf("message %q payload must be an object: %w", message.Kind, err)
	}

	textBody := func(required bool) (map[string]any, error) {
		body, ok := payload["message"].(map[string]any)
		if !ok {
			if required { return nil, fmt.Errorf("message %q requires payload.message", message.Kind) }
			return nil, nil
		}
		for _, key := range []string{"src", "text", "html"} {
			if _, ok := body[key]; !ok {
				return nil, fmt.Errorf("message %q requires payload.message.%s", message.Kind, key)
			}
		}
		text, _ := body["text"].(string)
		if required && strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("message %q requires non-empty payload.message.text", message.Kind)
		}
		return body, nil
	}
	stringField := func(name string) string {
		value, _ := payload[name].(string)
		return value
	}
	requireString := func(name string) error {
		if strings.TrimSpace(stringField(name)) == "" {
			return fmt.Errorf("message %q requires payload.%s", message.Kind, name)
		}
		return nil
	}

	switch message.Kind {
	case KindMessagePublic, KindMessagePrivate:
		if _, err := textBody(true); err != nil { return err }
		if err := requireString("channel"); err != nil { return err }
		user, _ := payload["user"].(map[string]any)
		name, _ := user["name"].(string)
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("message %q requires payload.user.name", message.Kind)
		}
	case KindSystemMessageWarning, KindError, KindWaiting, KindLog:
		if _, err := textBody(true); err != nil { return err }
	case KindChannelJoinFailed, KindChannelUnjoinFailed:
		if err := requireString("channel"); err != nil { return err }
		if _, err := textBody(true); err != nil { return err }
	case KindChannelJoinSuccess, KindChannelUnjoinSuccess, KindChannelInfo, KindMessageDeleted:
		if err := requireString("channel"); err != nil { return err }
		if message.Kind == KindMessageDeleted && strings.TrimSpace(stringField("messageId")) == "" {
			return fmt.Errorf("message %q requires payload.messageId", message.Kind)
		}
	case KindConnectionState:
		if err := requireString("state"); err != nil { return err }
	case KindPluginChat:
		if err := requireString("title"); err != nil { return err }
	case KindSetOptions:
		if _, ok := payload["values"].(map[string]any); !ok {
			return fmt.Errorf("message %q requires payload.values", message.Kind)
		}
	}
	return nil
}

// --- Payloads --------------------------------------------------------------

// InfoPayload is what a plugin says about itself.
type InfoPayload struct {
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	Options     []Option `json:"options,omitempty"`
	// Buttons are the actions the page offers: the kernel knows only how to
	// forward a kind to the plugin, and nothing about what it means.
	Buttons []Button `json:"buttons,omitempty"`
}

// Button is an action on the options page: not a field, а то, что нужно
// сделать — обновить базу, пересобрать кеш.
type Button struct {
	// Title is what is written on the button.
	Title string `json:"title"`
	// Kind is the command the kernel sends the plugin when it is pressed.
	Kind string `json:"kind"`
	// Hint is a short explanation under the button.
	Hint string `json:"hint,omitempty"`
	// Section is the heading it belongs to, the same as an option's.
	Section string `json:"section,omitempty"`
}

// SetOptionsPayload carries the values the user filled in.
type SetOptionsPayload struct {
	Values map[string]any `json:"values"`
}

// MessageContent is the common representation of human-readable text.
type MessageContent struct {
	Src string `json:"src"`
	Text string `json:"text"`
	Html string `json:"html"`
}

// PlainMessage builds a text-only message. HTML text is escaped by default.
func PlainMessage(text string) MessageContent {
	return MessageContent{Src: text, Text: text, Html: stdhtml.EscapeString(text)}
}

type MessagePayload struct { Message MessageContent `json:"message"` }

type AckPayload struct { Values map[string]any `json:"values,omitempty"`; Message string `json:"-"` }
type LogPayload struct { Level string `json:"level"`; Message string `json:"-"` }
type WaitingPayload struct { Message string `json:"-"` }
type ErrorPayload struct { Message string `json:"-"`; Fields map[string]string `json:"fields,omitempty"`; Items map[string]map[string]string `json:"items,omitempty"` }

// ChatPayload is one line of chat, as chat.message.public or
// chat.message.private.
type ChatPayload struct {
	// Kind is filled in by DecodeChat: it tells public from private. It is not
	// part of the payload, the envelope already carries it.
	Kind    string   `json:"-"`
	Channel string   `json:"channel"`
	User    ChatUser `json:"user"`
	Message ChatLine `json:"message"`
	// Notice marks a message the service posted itself rather than a person:
	// Twitch writes into the chat «X subscribed» and «X raided the channel».
	// Само по себе это реплика, и в ленте она стоит наравне с остальными —
	// так же, как у самой Twitch. Но по написанному это не отличить, а
	// странице полезно знать: человек смотрит на чат, а это писал сервис.
	Notice bool `json:"notice,omitempty"`
}

// ChatUser is who wrote the line.
type ChatUser struct {
	Name  string `json:"name"`
	ID    string `json:"id,omitempty"`
	Login string `json:"login,omitempty"`
}

// ChatLine is an alias for the shared text representation.
type ChatLine = MessageContent

// DecodeChat reads one chat line and checks that the kind is a chat kind.
func DecodeChat(line []byte) (ChatPayload, error) {
	message, err := Decode(line)
	if err != nil {
		return ChatPayload{}, err
	}
	if !IsChatKind(message.Kind) {
		return ChatPayload{}, fmt.Errorf("not a chat line: kind %q", message.Kind)
	}

	var chat ChatPayload
	if err := message.PayloadInto(&chat); err != nil {
		return ChatPayload{}, err
	}
	chat.Kind = message.Kind
	return chat, nil
}

// DecodeChatMessage reads an already decoded envelope as a chat line and fills
// in the kind.
func DecodeChatMessage(message Message) (ChatPayload, error) {
	if !IsChatKind(message.Kind) {
		return ChatPayload{}, fmt.Errorf("not a chat line: kind %q", message.Kind)
	}

	var chat ChatPayload
	if err := message.PayloadInto(&chat); err != nil {
		return ChatPayload{}, err
	}
	chat.Kind = message.Kind
	return chat, nil
}

// ChannelPayload describes a channel transition. Failure explanations are
// encoded in payload.message using the shared text shape.
type ChannelPayload struct {
	Channel string `json:"channel"`
	Name string `json:"name,omitempty"`
	Reason string `json:"-"`
}

// ChannelInfoPayload reports information about a channel. A person involved in
// the event always uses the common payload.user.name/id/login structure.
type ChannelInfoPayload struct {
	Channel string `json:"channel"`
	Name string `json:"name,omitempty"`
	User *ChatUser `json:"user,omitempty"`
	Started int64 `json:"started,omitempty"`
	Clients int `json:"clients,omitempty"`
	Users int `json:"users,omitempty"`
	PremiumOnly bool `json:"premiumOnly,omitempty"`
	SmileOnly bool `json:"smileOnly,omitempty"`
}

// DeletedMessagePayload describes a message removed from a channel.
type DeletedMessagePayload struct {
	Channel string `json:"channel"`
	MessageID string `json:"messageId"`
	User *ChatUser `json:"user,omitempty"`
}

// ConnectionPayload describes the plugin connection. Human-readable context
// is encoded via the shared payload.message object.
type ConnectionPayload struct {
	State string `json:"state"`
	Service string `json:"service,omitempty"`
	Reason string `json:"-"`
	Attempt int `json:"attempt,omitempty"`
}
