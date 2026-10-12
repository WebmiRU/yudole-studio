package plugin

import "studio/core/protocol"

// Event is one thing a plugin said about the chat: a line, a channel joining or
// leaving, the state of the connection.
//
// The plugin it came from is part of the event: on a page that shows every
// chat at once it is not obvious who написал.
type Event struct {
	Plugin  string
	Message protocol.Message
}

// Kind is what kind of event this is.
func (e Event) Kind() string { return e.Message.Kind }

// Channel is the channel the event is about, when it is about one. A chat line
// names its channel; a connection state names none.
func (e Event) Channel() string {
	switch e.Message.Kind {
	case protocol.KindMessagePublic, protocol.KindMessagePrivate:
		var chat protocol.ChatPayload
		if err := e.Message.PayloadInto(&chat); err != nil {
			return ""
		}
		return chat.Channel
	default:
		var payload protocol.ChannelPayload
		if err := e.Message.PayloadInto(&payload); err != nil {
			return ""
		}
		return payload.Channel
	}
}

// Chat reads the event as a chat line. It fails for everything else.
func (e Event) Chat() (protocol.ChatPayload, error) {
	return protocol.DecodeChatMessage(e.Message)
}
