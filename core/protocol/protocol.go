// Package protocol is the contract between the kernel and its plugins.
//
// Every line is one JSON object: a kind and a payload. The envelope has
// exactly two fields and never grows, so adding a new kind of message cannot
// pollute the namespace of the others.
//
//	{"kind":"plugin.chat","payload":{"title":"GoodGame","options":[...]}}
//	{"kind":"set_options","payload":{"values":{"channels":["42"]}}}
//	{"kind":"chat.message.public","payload":{"channel":"42","user":{...},"message":{...}}}
//
// The kernel and the plugins import this package instead of keeping their own
// copies of the shapes: a mistake here then breaks both sides at once, rather
// than one of them quietly sending something the other never reads.
package protocol

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

// Kinds of messages.
const (
	// A plugin introduces itself right after it starts.
	KindPluginChat = "plugin.chat"
	// The kernel hands the plugin the values the user filled in.
	KindSetOptions = "set_options"
	// The plugin confirms it took the options.
	KindAck = "ack"
	// Something went wrong inside the plugin.
	KindError = "error"
	// The plugin is busy: it will answer the request later.
	KindWaiting = "waiting"
	// Anything the plugin wants to say to the kernel log.
	KindLog = "log"
	// The kernel asks the plugin to finish.
	KindQuit = "quit"

	// A line of chat: public goes to the rooms everyone can see, private to a
	// direct conversation.
	KindMessagePublic  = "chat.message.public"
	KindMessagePrivate = "chat.message.private"

	// What the plugin does with the channels it was given. These four are the
	// same for every chat service: Twitch, YouTube, GoodGame — разница только
	// в том, что под каналом понимается.
	KindChannelJoinSuccessful   = "chat.channel.join.successful"
	KindChannelJoinFailed       = "chat.channel.join.failed"
	KindChannelUnjoinSuccessful = "chat.channel.unjoin.successful"
	KindChannelUnjoinFailed     = "chat.channel.unjoin.failed"

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
	case KindChannelJoinSuccessful, KindChannelJoinFailed,
		KindChannelUnjoinSuccessful, KindChannelUnjoinFailed,
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
	raw, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return Message{}, fmt.Errorf("encode the %s payload: %w", kind, err)
	}
	return Message{Kind: kind, Payload: raw}, nil
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
	if len(m.Payload) == 0 {
		return fmt.Errorf("message %q has no payload", m.Kind)
	}
	return json.Unmarshal(m.Payload, v)
}

// Encode renders a message as one protocol line.
func Encode(message Message) ([]byte, error) {
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
	return message, nil
}

// --- Payloads --------------------------------------------------------------

// InfoPayload is what a plugin says about itself.
type InfoPayload struct {
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Version     string   `json:"version,omitempty"`
	Options     []Option `json:"options,omitempty"`
}

// SetOptionsPayload carries the values the user filled in.
type SetOptionsPayload struct {
	Values map[string]any `json:"values"`
}

// AckPayload confirms that the plugin took the options. It may also carry the
// values it actually ended up with: a plugin is allowed to normalize them (turn
// a nickname or a link into a channel id), and the kernel saves what the plugin
// works with, not what the user happened to type.
type AckPayload struct {
	Values map[string]any `json:"values,omitempty"`
}

// LogPayload is a line for the kernel log.
type LogPayload struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// WaitingPayload says what the plugin is busy with, so the options page can
// keep the save button disabled instead of pretending nothing happens.
type WaitingPayload struct {
	Message string `json:"message"`
}

// ErrorPayload explains what went wrong. Fields, when set, point at the exact
// options the user has to fix: the kernel does not save the values and the
// options page highlights those fields. Items narrows it further for list
// fields: which of the entries is the problem, keyed by what the user typed.
type ErrorPayload struct {
	Message string                       `json:"message"`
	Fields  map[string]string            `json:"fields,omitempty"`
	Items   map[string]map[string]string `json:"items,omitempty"`
}

// ChatPayload is one line of chat, as chat.message.public or
// chat.message.private.
type ChatPayload struct {
	// Kind is filled in by DecodeChat: it tells public from private. It is not
	// part of the payload, the envelope already carries it.
	Kind    string   `json:"-"`
	Channel string   `json:"channel"`
	User    ChatUser `json:"user"`
	Message ChatLine `json:"message"`
}

// ChatUser is who wrote the line.
type ChatUser struct {
	Name  string `json:"name"`
	ID    string `json:"id"`
	Login string `json:"login"`
}

// ChatLine is one line of chat in three forms: as the service sent it, as
// plain text, and as html for a rich view.
type ChatLine struct {
	// Src is the text exactly as it came in, with the service's own markup.
	Src string `json:"src"`
	// Text is the plain text: no smilies codes, no markup.
	Text string `json:"text"`
	// Html is the same line with the source's formatting, for a rich view.
	Html string `json:"html"`
}

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

// ChannelPayload is what a join, unjoin or a change in a channel carries. The
// three kinds differ only in the kind itself.
type ChannelPayload struct {
	// Channel is the value the plugin stores in the config, not the service's
	// own id: it is what the user wrote and what we will reconnect to.
	Channel string `json:"channel"`
	// Reason is filled in for the failed kinds only.
	Reason string `json:"reason,omitempty"`
}

// ChannelInfoPayload is what the service tells about a channel we are in.
type ChannelInfoPayload struct {
	Channel string `json:"channel"`
	// Name is the channel title at the service.
	Name string `json:"name,omitempty"`
	// Streamer is who streams there, when the service says.
	Streamer string `json:"streamer,omitempty"`
	// Started is when the stream began, in unix seconds; 0 means it is not
	// running.
	Started int64 `json:"started,omitempty"`
	// Clients and Users are how many people are in the channel and how many of
	// them are signed in. Services send this on their own every few seconds,
	// so a plugin can count on it arriving without asking.
	Clients int `json:"clients,omitempty"`
	Users   int `json:"users,omitempty"`
	// PremiumOnly marks a channel that only its own subscribers may enter.
	PremiumOnly bool `json:"premiumOnly,omitempty"`
	// SmileOnly marks a channel where only smilies may be sent.
	SmileOnly bool `json:"smileOnly,omitempty"`
}

// DeletedMessagePayload is a message a moderator took out of the chat.
type DeletedMessagePayload struct {
	Channel   string `json:"channel"`
	MessageID string `json:"messageId"`
	// By is who removed it, when the service says.
	By string `json:"by,omitempty"`
}

// ConnectionPayload is the state of the plugin's connection to the service.
type ConnectionPayload struct {
	// State is one of the Connection* constants.
	State string `json:"state"`
	// Service is the chat the plugin talks to, for the log.
	Service string `json:"service,omitempty"`
	// Reason says why the connection went down or could not be made.
	Reason string `json:"reason,omitempty"`
	// Attempt counts the reconnects since the last good connection.
	Attempt int `json:"attempt,omitempty"`
}
