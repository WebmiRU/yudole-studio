package plugin

import (
	"context"
	"log/slog"
	"sync"
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

// Find returns a running plugin by name.
func (s *Supervisor) Find(name string) (*Plugin, bool) {
	for _, p := range s.plugins {
		if p.Name == name {
			return p, true
		}
	}
	return nil, false
}

// Broadcast sends a command to every running plugin.
func (s *Supervisor) Broadcast(command string) {
	var wg sync.WaitGroup
	for _, p := range s.plugins {
		wg.Add(1)
		go func(p *Plugin) {
			defer wg.Done()
			if err := p.Send(command); err != nil {
				p.logger.Error("broadcast failed", "command", command, "err", err)
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
