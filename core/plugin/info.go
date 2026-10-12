package plugin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"studio/core/protocol"
	"studio/core/settings"
)

// Info is what a plugin told us about itself.
type Info struct {
	ID          string
	Title       string
	Version     string
	Description string
	Options     []protocol.Option
	Buttons     []protocol.Button
}

// OptionKeys lists the settings the plugin understands.
func (i Info) OptionKeys() []string {
	keys := make([]string, 0, len(i.Options))
	for _, option := range i.Options {
		keys = append(keys, option.Key)
	}
	return keys
}

// Плагин объявляет себя сразу, ждать долго незачем. А вот на настройки
// время уходит: там может быть поход в сеть за списком каналов.
const (
	describeTimeout  = 5 * time.Second
	configureTimeout = 20 * time.Second
	// actionTimeout is for the commands that reach out to the network: обновить
	// базу смайлов — это скачать файл и переписать таблицу.
	actionTimeout = 90 * time.Second
)

// Describe waits for the plugin to introduce itself: a plugin announces on
// start and the kernel reads that announcement. A plugin that stays quiet
// still runs, it just gets no settings page.
func (p *Plugin) Describe() (Info, error) {
	select {
	case message := <-p.repliesTo():
		if message.Kind != protocol.KindPluginChat {
			return Info{}, fmt.Errorf("plugin %q introduced itself with %q", p.Name, message.Kind)
		}

		var payload protocol.InfoPayload
		if err := message.PayloadInto(&payload); err != nil {
			return Info{}, fmt.Errorf("plugin %q sent an unreadable description: %w", p.Name, err)
		}

		return Info{
			ID:          p.Name,
			Title:       payload.Title,
			Version:     payload.Version,
			Description: payload.Description,
			Options:     payload.Options,
			Buttons:     payload.Buttons,
		}, nil

	case <-time.After(describeTimeout):
		return Info{}, fmt.Errorf("plugin %q did not introduce itself within %s", p.Name, describeTimeout)
	}
}

// ValidationError means the plugin looked at the values and refused them,
// pointing at the fields the user has to fix. The kernel must not save them.
type ValidationError struct {
	Message string
	Fields  map[string]string
	// Items narrows Fields down to individual entries of a list field.
	Items map[string]map[string]string
}

func (e *ValidationError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "the plugin rejected the options"
}

// Configure sends the options to the plugin and returns the values the plugin
// ended up with. A plugin may normalize them — turn a nickname or a link into a
// channel id — and then the returned map is what belongs in the config.
// Unknown keys are kept: they belong to a plugin version that knows more than
// the current schema.
func (p *Plugin) Configure(values map[string]any) (map[string]any, error) {
	// Ответ снимет ожидание: и подтверждение, и отказ означают, что работа
	// закончена.
	defer p.setWaiting(false, "")

	message, err := protocol.New(protocol.KindSetOptions, protocol.SetOptionsPayload{Values: values})
	if err != nil {
		return nil, err
	}
	if err := p.Send(message); err != nil {
		return nil, err
	}

	select {
	case reply := <-p.repliesTo():
		switch reply.Kind {
		case protocol.KindAck:
			var payload protocol.AckPayload
			if err := reply.PayloadInto(&payload); err != nil {
				// Подтверждение без читаемого содержимого — просто подтверждение.
				return nil, nil
			}
			return payload.Values, nil
		case protocol.KindError:
			var payload protocol.ErrorPayload
			if err := reply.PayloadInto(&payload); err != nil {
				return nil, fmt.Errorf("plugin %q refused the options and sent an unreadable reason", p.Name)
			}
			if len(payload.Fields) > 0 {
				return nil, &ValidationError{
					Message: payload.Message,
					Fields:  payload.Fields,
					Items:   payload.Items,
				}
			}
			return nil, fmt.Errorf("plugin %q refused the options: %s", p.Name, payload.Message)
		default:
			return nil, fmt.Errorf("plugin %q answered set_options with %q", p.Name, reply.Kind)
		}
	case <-time.After(configureTimeout):
		return nil, fmt.Errorf("plugin %q did not confirm the options within %s", p.Name, configureTimeout)
	}
}

// Run asks the plugin to do something that is not about its options: refresh its
// base of smileys, rebuild a cache. Ядро ничего в команде не понимает — оно
// только пересылает вид и ждёт ответа.
//
// Команда может быть долгой, поэтому ждём дольше, чем обычный ответ на
// set_options: скачивание и переписывание базы занимают секунды, а не
// миллисекунды.
func (p *Plugin) Run(ctx context.Context, kind string) (string, error) {
	// Ответ снимет ожидание: и подтверждение, и отказ означают, что работа
	// закончена.
	defer p.setWaiting(false, "")

	if kind == "" {
		return "", fmt.Errorf("plugin %q: команда без вида", p.Name)
	}

	request, err := protocol.New(kind, struct{}{})
	if err != nil {
		return "", err
	}
	if err := p.Send(request); err != nil {
		return "", err
	}

	select {
	case reply := <-p.repliesTo():
		switch reply.Kind {
		case protocol.KindAck:
			var payload protocol.AckPayload
			if err := reply.PayloadInto(&payload); err != nil {
				// Подтверждение без читаемого содержимого — просто подтверждение.
				return "", nil
			}
			return payload.Message, nil
		case protocol.KindError:
			var payload protocol.ErrorPayload
			if err := reply.PayloadInto(&payload); err != nil || payload.Message == "" {
				return "", fmt.Errorf("plugin %q refused %s without saying why", p.Name, kind)
			}
			return "", errors.New(payload.Message)
		default:
			return "", fmt.Errorf("plugin %q answered %s with %q", p.Name, kind, reply.Kind)
		}
	case <-time.After(actionTimeout):
		return "", fmt.Errorf("plugin %q did not finish %s within %s", p.Name, kind, actionTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ValuesFor builds the option values of a plugin: the stored settings, with
// the schema defaults filling in whatever the config file does not have.
func ValuesFor(info Info, stored settings.Plugin) map[string]any {
	values := protocol.Defaults(info.Options)
	for key, value := range stored.Options {
		values[key] = value
	}
	return values
}
