package plugin

import (
	"context"
	"log/slog"
	"sync"

	"studio/core/protocol"
)

// Supervisor starts every discovered plugin and stops them all on shutdown.
// One plugin failing to start never stops the kernel.
type Supervisor struct {
	plugins []*Plugin
}

// StartAll launches every descriptor in order.
func (s *Supervisor) StartAll(ctx context.Context, descriptors []Descriptor) {
	for _, d := range descriptors {
		p, err := Start(ctx, d.Name, d.Path)
		if err != nil {
			slog.Error("plugin failed to start", "plugin", d.Name, "err", err)
			continue
		}
		s.plugins = append(s.plugins, p)
	}
}

// Names lists the running plugins.
func (s *Supervisor) Names() []string {
	names := make([]string, 0, len(s.plugins))
	for _, p := range s.plugins {
		names = append(names, p.Name)
	}
	return names
}

// Plugins returns the running plugins, in start order.
func (s *Supervisor) Plugins() []*Plugin {
	return s.plugins
}

// Find returns a running plugin by name.
func (s *Supervisor) Find(name string) (*Plugin, bool) {
	for _, p := range s.plugins {
		if p.Name == name {
			return p, true
		}
	}
	return nil, false
}

// Broadcast sends a message to every running plugin.
func (s *Supervisor) Broadcast(message protocol.Message) {
	var wg sync.WaitGroup
	for _, p := range s.plugins {
		wg.Add(1)
		go func(p *Plugin) {
			defer wg.Done()
			if err := p.Send(message); err != nil {
				p.logger.Error("broadcast failed", "type", message.Kind, "err", err)
			}
		}(p)
	}
	wg.Wait()
}

// StopAll stops every plugin in reverse start order and waits for the
// processes to exit.
func (s *Supervisor) StopAll() {
	var wg sync.WaitGroup
	for i := len(s.plugins) - 1; i >= 0; i-- {
		wg.Add(1)
		go func(p *Plugin) {
			defer wg.Done()
			if err := p.Stop(); err != nil {
				p.logger.Error("stop failed", "err", err)
			}
		}(s.plugins[i])
	}
	wg.Wait()
	s.plugins = nil
}
