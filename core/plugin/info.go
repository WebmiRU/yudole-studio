package plugin

import (
	"context"
	"fmt"
	"time"

	"studio/core/settings"
)

// Info is what a plugin told us about itself.
type Info struct {
	ID          string
	Name        string
	Version     string
	Description string
	Options     []Option
}

// OptionKeys lists the settings the plugin understands.
func (i Info) OptionKeys() []string {
	keys := make([]string, 0, len(i.Options))
	for _, option := range i.Options {
		keys = append(keys, option.Key)
	}
	return keys
}

const infoTimeout = 5 * time.Second

// Describe asks the plugin who it is and what it can be configured with. A
// plugin that does not answer in time still runs, just without a form.
func (p *Plugin) Describe(ctx context.Context) (Info, error) {
	if err := p.Send(Message{Type: TypeGetInfo}); err != nil {
		return Info{}, err
	}

	select {
	case message := <-p.repliesTo():
		if message.Type != TypeInfo {
			return Info{}, fmt.Errorf("plugin %q answered get_info with %q", p.Name, message.Type)
		}
		info := Info{
			ID:          message.ID,
			Name:        message.Name,
			Version:     message.Version,
			Description: message.Description,
			Options:     message.Options,
		}
		if info.ID == "" {
			info.ID = p.Name
		}
		return info, nil

	case <-time.After(infoTimeout):
		return Info{}, fmt.Errorf("plugin %q did not describe itself within %s", p.Name, infoTimeout)
	}
}

// Configure sends the options to the plugin. Unknown keys are kept: they belong
// to a plugin version that knows more than the current schema.
func (p *Plugin) Configure(values map[string]any) error {
	if err := p.Send(Message{Type: TypeSetOptions, Values: values}); err != nil {
		return err
	}

	select {
	case message := <-p.repliesTo():
		switch message.Type {
		case TypeAck:
			return nil
		case TypeError:
			return fmt.Errorf("plugin %q rejected the options: %s", p.Name, message.Message)
		default:
			return fmt.Errorf("plugin %q answered set_options with %q", p.Name, message.Type)
		}
	case <-time.After(infoTimeout):
		return fmt.Errorf("plugin %q did not confirm the options within %s", p.Name, infoTimeout)
	}
}

// ValuesFor builds the option values of a plugin: the stored settings, with
// the schema defaults filling in whatever the config file does not have.
func ValuesFor(info Info, stored settings.Plugin) map[string]any {
	values := Defaults(info.Options)
	for key, value := range stored.Options {
		values[key] = value
	}
	return values
}
