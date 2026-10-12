package plugin

import (
	"encoding/json/v2"
	"fmt"
)

// The kernel and the plugins exchange one JSON object per line. Every object
// carries a type, so both sides can ignore what they do not know yet.
//
//	kernel -> plugin   {"type":"get_info"}
//	plugin -> kernel   {"type":"info","name":"GoodGame","options":[...]}
//	kernel -> plugin   {"type":"set_options","values":{"login":"vasya"}}
//	plugin -> kernel   {"type":"ack"}
//	plugin -> kernel   {"type":"log","level":"info","message":"hello"}
//
// Anything else is logged as an unknown message rather than treated as an
// error, which keeps an older kernel working with a newer plugin.
const (
	TypeGetInfo    = "get_info"
	TypeInfo       = "info"
	TypeSetOptions = "set_options"
	TypeAck        = "ack"
	TypeError      = "error"
	TypeLog        = "log"
	TypeQuit       = "quit"
)

// Option types understood by the options page.
const (
	OptionText     = "text"
	OptionPassword = "password"
	OptionNumber   = "number"
	OptionBool     = "bool"
	OptionSelect   = "select"
	OptionTags     = "tags"
)

// Message is one line of the plugin protocol.
type Message struct {
	Type string `json:"type"`

	// Answer to get_info.
	ID          string   `json:"id,omitempty"`
	Name        string   `json:"name,omitempty"`
	Version     string   `json:"version,omitempty"`
	Description string   `json:"description,omitempty"`
	Options     []Option `json:"options,omitempty"`

	// Answer to set_options: the values the user filled in.
	Values map[string]any `json:"values,omitempty"`

	// Text of log and error messages.
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`
}

// Option describes a single setting of a plugin: enough for the kernel to
// render the form, and for the plugin to read the answers back.
type Option struct {
	// Key is the field name in the config file and the value the plugin gets.
	Key string `json:"key"`
	// Type is one of the Option* constants.
	Type string `json:"type"`
	// Label is what the user sees.
	Label string `json:"label"`
	// Hint is a short explanation under the label.
	Hint string `json:"hint,omitempty"`
	// Placeholder is empty-field text for text and password inputs.
	Placeholder string `json:"placeholder,omitempty"`
	// Required stops the options page from saving an empty value.
	Required bool `json:"required,omitempty"`
	// Default is used while the config file has no value for this key. It is
	// shown in the form but never written to the file on its own.
	Default any `json:"default,omitempty"`
	// Choices are the allowed values of a select.
	Choices []Choice `json:"choices,omitempty"`
}

// Choice is one option of a select.
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Valid reports whether the option can be rendered and checked.
func (o Option) Valid() error {
	if o.Key == "" {
		return fmt.Errorf("option without a key")
	}

	switch o.Type {
	case OptionText, OptionPassword, OptionNumber, OptionBool, OptionSelect, OptionTags:
	case "":
		return fmt.Errorf("option %q: type is required", o.Key)
	default:
		return fmt.Errorf("option %q: unknown type %q", o.Key, o.Type)
	}

	if o.Type == OptionSelect && len(o.Choices) == 0 {
		return fmt.Errorf("option %q: a select needs choices", o.Key)
	}
	return nil
}

// Secret reports whether the value must be masked: never sent to the browser
// and never written to the log.
func (o Option) Secret() bool {
	return o.Type == OptionPassword
}

// Defaults returns the default value of every option, so the kernel can hand a
// complete set of values to a plugin that has never been configured.
func Defaults(options []Option) map[string]any {
	values := make(map[string]any, len(options))
	for _, option := range options {
		switch {
		case option.Default != nil:
			values[option.Key] = option.Default
		case option.Type == OptionBool:
			values[option.Key] = false
		case option.Type == OptionTags:
			values[option.Key] = []any{}
		case option.Type == OptionNumber:
			values[option.Key] = float64(0)
		default:
			values[option.Key] = ""
		}
	}
	return values
}

// CheckValues verifies that every option in the schema holds a value of the
// right type. Options the schema does not know about are reported separately:
// they are kept in the config file and handed to the plugin as they are.
func CheckValues(options []Option, values map[string]any) (unknown []string, err error) {
	for _, option := range options {
		if verr := option.Valid(); verr != nil {
			return nil, verr
		}

		value, ok := values[option.Key]
		if !ok {
			if option.Required {
				return nil, fmt.Errorf("option %q: required", option.Key)
			}
			continue
		}
		if verr := checkValue(option, value); verr != nil {
			return nil, verr
		}
	}

	for key := range values {
		if !known(options, key) {
			unknown = append(unknown, key)
		}
	}
	return unknown, nil
}

func checkValue(option Option, value any) error {
	// A required option may be left empty while the user is still typing, but
	// a value that is there has to make sense.
	if value == nil {
		if option.Required {
			return fmt.Errorf("option %q: required", option.Key)
		}
		return nil
	}

	switch option.Type {
	case OptionText, OptionPassword, OptionSelect:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("option %q: want a string, got %T", option.Key, value)
		}
	case OptionNumber:
		if _, ok := value.(float64); !ok {
			if _, ok := value.(int); !ok {
				return fmt.Errorf("option %q: want a number, got %T", option.Key, value)
			}
		}
	case OptionBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("option %q: want true or false, got %T", option.Key, value)
		}
	case OptionTags:
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("option %q: want a list of strings, got %T", option.Key, value)
		}
		for i, item := range list {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("option %q[%d]: want a string, got %T", option.Key, i, item)
			}
		}
	}
	return nil
}

func known(options []Option, key string) bool {
	for _, option := range options {
		if option.Key == key {
			return true
		}
	}
	return false
}

// DecodeMessage reads one protocol line.
func DecodeMessage(line []byte) (Message, error) {
	var message Message
	if err := json.Unmarshal(line, &message); err != nil {
		return Message{}, err
	}
	if message.Type == "" {
		return Message{}, fmt.Errorf("message without a type: %s", line)
	}
	return message, nil
}

// Encode renders a message as a protocol line. The output is deterministic, so
// the same state always produces the same bytes.
func Encode(message Message) ([]byte, error) {
	data, err := json.Marshal(message, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
