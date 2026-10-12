package protocol

import (
	"errors"
	"fmt"
	"strings"
)

// Option types the options page knows how to render. A type may carry a mode
// after a slash: text/multiple is a text field filled several times.
const (
	OptionText     = "text"
	OptionPassword = "password"
	OptionNumber   = "number"
	OptionBool     = "bool"
	OptionSelect   = "select"

	// OptionTextMultiple is a text field the user can fill several times.
	OptionTextMultiple = "text/multiple"

	// OptionSelectMultiple is a list of the values the plugin offered, with
	// the user free to add their own. Unlike text/multiple, every value knows
	// how to show itself: "53029" and "E.Wolf" are the same channel.
	OptionSelectMultiple = "select/multiple"
)

// ChoiceValue is one picked entry of a select/multiple: what goes to the plugin
// and what the user sees. A plain string is accepted too and reads as both.
type ChoiceValue struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// Values pulls the raw values out of what was stored, so a plugin does not
// have to care whether the config holds strings or pairs.
func Values(entries []any) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		switch value := entry.(type) {
		case string:
			out = append(out, value)
		case ChoiceValue:
			out = append(out, value.Value)
		case map[string]any:
			if v, ok := value["value"].(string); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// Option describes one setting of a plugin: enough for the kernel to render
// the form, and for the plugin to read the answers back.
type Option struct {
	// Key is the field name in the config file and the value the plugin gets.
	Key string `json:"key"`
	// Type is one of the Option* constants.
	Type string `json:"type"`
	// Label is what the user sees.
	Label string `json:"label"`
	// Section groups related fields under a heading. Плагин, у которого есть
	// поля и кнопки, разносит их по разделам — иначе всё в одной куче, и
	// непонятно, что с чем связано.
	Section string `json:"section,omitempty"`
	// Hint is a short explanation under the label.
	Hint string `json:"hint,omitempty"`
	// Placeholder is empty-field text for text and password inputs.
	Placeholder string `json:"placeholder,omitempty"`
	// Required stops the options page from saving an empty value.
	Required bool `json:"required,omitempty"`
	// Default is shown while the config file has no value for this key. It is
	// not written to the file on its own.
	Default any `json:"default,omitempty"`
	// Choices are the allowed values of a select.
	Choices []Choice `json:"choices,omitempty"`
}

// Choice is one option of a select.
type Choice struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// Valid reports whether the option can be rendered and checked.
func (o Option) Valid() error {
	if o.Key == "" {
		return fmt.Errorf("option without a key")
	}

	switch o.Type {
	case OptionText, OptionPassword, OptionNumber, OptionBool, OptionSelect, OptionTextMultiple, OptionSelectMultiple:
	case "":
		return fmt.Errorf("option %q: type is required", o.Key)
	default:
		return fmt.Errorf("option %q: unknown type %q", o.Key, o.Type)
	}

	// A plain select is useless without variants. A select/multiple may have
	// none: the user can still add a value by hand.
	if o.Type == OptionSelect && len(o.Choices) == 0 {
		return fmt.Errorf("option %q: a select needs choices", o.Key)
	}
	return nil
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
		case option.Type == OptionTextMultiple, option.Type == OptionSelectMultiple:
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
// right type. Options the schema does not know about are returned separately:
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
		switch value.(type) {
		case float64, int, int64:
		default:
			return fmt.Errorf("option %q: want a number, got %T", option.Key, value)
		}
	case OptionBool:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("option %q: want true or false, got %T", option.Key, value)
		}
	case OptionTextMultiple:
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("option %q: want a list of strings, got %T", option.Key, value)
		}
		for i, item := range list {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("option %q[%d]: want a string, got %T", option.Key, i, item)
			}
		}

	case OptionSelectMultiple:
		list, ok := value.([]any)
		if !ok {
			return fmt.Errorf("option %q: want a list of choices, got %T", option.Key, value)
		}
		for i, item := range list {
			if err := checkChoice(item); err != nil {
				return fmt.Errorf("option %q[%d]: %w", option.Key, i, err)
			}
		}
	}
	return nil
}

// checkChoice accepts what the config may hold for a picked entry: a plain
// string or a value with a label.
func checkChoice(item any) error {
	switch choice := item.(type) {
	case string:
		if strings.TrimSpace(choice) == "" {
			return errors.New("empty value")
		}
	case ChoiceValue:
		if choice.Value == "" {
			return errors.New("empty value")
		}
	case map[string]any:
		if value, ok := choice["value"].(string); !ok || strings.TrimSpace(value) == "" {
			return errors.New("want a non-empty value")
		}
	default:
		return fmt.Errorf("want a string or a value with a label, got %T", item)
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
