package plugin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"studio/core/protocol"
	"studio/core/settings"
)

// State tells the options page what is going on with a plugin.
const (
	StateOff     = "off"     // not switched on
	StateRunning = "running" // process is up and described itself
	StateFailed  = "failed"  // switched on, but the process or handshake failed
)

// PluginView is what the options page needs about one plugin: whether it is
// switched on, the form the plugin declared, and the values to show in it.
type PluginView struct {
	ID          string                       `json:"id"`
	Title       string                       `json:"title,omitempty"`
	Description string                       `json:"description,omitempty"`
	Version     string                       `json:"version,omitempty"`
	Enabled     bool                         `json:"enabled"`
	State       string                       `json:"state"`
	Problem     string                       `json:"problem,omitempty"`
	Fields      map[string]string            `json:"fields,omitempty"`
	Items       map[string]map[string]string `json:"items,omitempty"`
	Waiting     bool                         `json:"waiting,omitempty"`
	WaitingText string                       `json:"waitingText,omitempty"`
	Options     []protocol.Option            `json:"options,omitempty"`
	Values      map[string]any               `json:"values,omitempty"`
}

// Manager owns the plugin processes and the settings that belong to them.
// It is safe for concurrent use: the options page calls it from handlers while
// the kernel starts and stops plugins in the background.
type Manager struct {
	mu sync.Mutex

	dir        string
	configPath string
	config     *settings.Config

	descriptors map[string]Descriptor
	running     map[string]*Plugin
	infos       map[string]Info
	problems    map[string]string
	fields      map[string]map[string]string

	// lifetime is the context the plugin processes hang on. It is the kernel
	// context, never a request context: a request that ends must not take the
	// process down with it.
	lifetime context.Context
}

// NewManager loads the configuration and finds the plugins on disk. Nothing is
// started yet: plugins run when the user switches them on.
func NewManager(configPath, pluginsDir string) (*Manager, error) {
	config, err := settings.Load(configPath)
	if err != nil {
		return nil, err
	}

	descriptors, err := Discover(pluginsDir)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		dir:         pluginsDir,
		configPath:  configPath,
		config:      config,
		descriptors: make(map[string]Descriptor, len(descriptors)),
		running:     map[string]*Plugin{},
		infos:       map[string]Info{},
		problems:    map[string]string{},
		fields:      map[string]map[string]string{},
	}
	for _, d := range descriptors {
		m.descriptors[d.Name] = d
	}
	return m, nil
}

// Config returns the loaded configuration.
func (m *Manager) Config() *settings.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.config
}

// Start launches the plugins the user switched on and remembers ctx as the
// lifetime of every process. The rest stay off until the options page says
// otherwise.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.lifetime = ctx
	m.mu.Unlock()

	for _, name := range m.config.ChatPlugins() {
		if !m.config.Enabled(name) {
			continue
		}
		if _, ok := m.descriptors[name]; !ok {
			slog.Warn("config has a plugin that is not installed", "plugin", name)
			continue
		}
		if err := m.enable(name); err != nil {
			slog.Error("plugin failed to start", "plugin", name, "err", err)
		}
	}
}

// Shutdown stops every running plugin and writes the configuration back.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	running := make([]*Plugin, 0, len(m.running))
	for _, p := range m.running {
		running = append(running, p)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	for _, p := range running {
		wg.Add(1)
		go func(p *Plugin) {
			defer wg.Done()
			if err := p.Stop(); err != nil {
				p.logger.Error("stop failed", "err", err)
			}
		}(p)
	}
	wg.Wait()

	m.mu.Lock()
	m.running = map[string]*Plugin{}
	m.mu.Unlock()

	if err := m.save(); err != nil {
		slog.Error("cannot save the config", "err", err)
	}
}

// List returns every plugin found on disk, with its current state.
func (m *Manager) List() []PluginView {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.descriptors))
	for name := range m.descriptors {
		names = append(names, name)
	}
	sort.Strings(names)

	views := make([]PluginView, 0, len(names))
	for _, name := range names {
		views = append(views, m.view(name))
	}
	return views
}

// Get returns one plugin.
func (m *Manager) Get(id string) (PluginView, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.descriptors[id]; !ok {
		return PluginView{}, false
	}
	return m.view(id), true
}

// view builds the state of a plugin. The caller holds the lock.
func (m *Manager) view(name string) PluginView {
	stored, _ := m.config.Chat(name)
	view := PluginView{
		ID:      name,
		Title:   name,
		Enabled: stored.Enabled,
		State:   StateOff,
	}

	if problem, failed := m.problems[name]; failed {
		view.State = StateFailed
		view.Problem = problem
	}
	if fields, rejected := m.fields[name]; rejected && len(fields) > 0 {
		view.Fields = fields
		if view.Problem == "" {
			view.Problem = firstOf(fields)
		}
	}

	if process, running := m.running[name]; running {
		view.Waiting, view.WaitingText = process.Waiting()
	}

	if info, ok := m.infos[name]; ok {
		view.Title = info.Title
		view.Description = info.Description
		view.Version = info.Version
		view.Options = info.Options
		view.Values = ValuesFor(info, stored)
		if view.State != StateFailed {
			view.State = StateRunning
		}
	}
	return view
}

// Enable switches a plugin on: the kernel starts the process and waits for the
// plugin to describe itself.
func (m *Manager) Enable(id string) (PluginView, error) {
	m.mu.Lock()
	_, installed := m.descriptors[id]
	if !installed {
		m.mu.Unlock()
		return PluginView{}, fmt.Errorf("plugin %q is not installed", id)
	}
	stored := m.config.EnsureChat(id)
	stored.Enabled = true
	m.config.Plugins.Chat[id] = stored
	alreadyRunning := m.running[id] != nil
	m.mu.Unlock()

	if err := m.save(); err != nil {
		return PluginView{}, err
	}
	if alreadyRunning {
		slog.Info("plugin is already running", "plugin", id)
		view, _ := m.Get(id)
		return view, nil
	}
	err := m.enable(id)
	view, _ := m.Get(id)
	return view, err
}

// enable starts the process and performs the handshake.
func (m *Manager) enable(id string) error {
	descriptor := m.descriptors[id]
	slog.Info("starting plugin", "plugin", id, "path", descriptor.Path)

	process, err := Start(m.processContext(), id, descriptor.Path)
	if err != nil {
		m.setProblem(id, err)
		return err
	}

	info, err := process.Describe()
	if err != nil {
		m.setProblem(id, err)
		_ = process.Stop()
		return err
	}

	stored, _ := m.config.Chat(id)
	values := ValuesFor(info, stored)
	if _, err := protocol.CheckValues(info.Options, values); err != nil {
		// Stored settings do not fit the schema the plugin just announced:
		// fall back to its defaults instead of failing to start.
		slog.Warn("stored options do not fit the schema, using defaults", "plugin", id, "err", err)
		values = protocol.Defaults(info.Options)
	}
	if err := process.Configure(values); err != nil {
		// Плагин может быть жив, а сохранённые настройки ему не нравятся —
		// например, канал удалили. Это не поломка: запускаем как есть и
		// показываем, что поправить.
		var invalid *ValidationError
		if errors.As(err, &invalid) {
			slog.Warn("stored options were rejected", "plugin", id, "fields", invalid.Fields)
			m.setFields(id, invalid.Fields)
		} else {
			m.setProblem(id, err)
			_ = process.Stop()
			return err
		}
	}

	m.mu.Lock()
	m.running[id] = process
	m.infos[id] = info
	delete(m.problems, id)
	m.mu.Unlock()

	slog.Info("plugin ready", "plugin", id, "title", info.Title, "options", len(info.Options))
	return nil
}

// Disable switches a plugin off and stops its process. The settings stay in the
// config file, and the form keeps the last known shape.
func (m *Manager) Disable(id string) (PluginView, error) {
	m.mu.Lock()
	stored, ok := m.config.Chat(id)
	if !ok {
		m.mu.Unlock()
		return PluginView{}, fmt.Errorf("plugin %q is not installed", id)
	}
	stored.Enabled = false
	m.config.Plugins.Chat[id] = stored
	process := m.running[id]
	delete(m.running, id)
	m.mu.Unlock()

	if process != nil {
		if err := process.Stop(); err != nil {
			slog.Error("stop failed", "plugin", id, "err", err)
		}
	}

	if err := m.save(); err != nil {
		return PluginView{}, err
	}
	slog.Info("plugin switched off", "plugin", id)
	view, _ := m.Get(id)
	return view, nil
}

// SaveOptions stores the option values and hands them to the running plugin.
func (m *Manager) SaveOptions(id string, values map[string]any) (PluginView, error) {
	m.mu.Lock()
	if _, ok := m.descriptors[id]; !ok {
		m.mu.Unlock()
		return PluginView{}, fmt.Errorf("plugin %q is not installed", id)
	}

	info, hasInfo := m.infos[id]
	if hasInfo {
		if _, err := protocol.CheckValues(info.Options, values); err != nil {
			m.mu.Unlock()
			return PluginView{}, fmt.Errorf("plugin %q: %w", id, err)
		}
	}

	stored := m.config.EnsureChat(id)
	stored.Options = values
	process := m.running[id]
	m.mu.Unlock()

	// A plugin that is switched off keeps its settings but is not running, so
	// there is nobody to send them to yet.
	if process != nil {
		if err := process.Configure(values); err != nil {
			view, _ := m.Get(id)
			// Отказ с указанием на поля: показываем их, но в файл ничего не
			// пишем — иначе сломанные значения останутся в конфиге.
			// Отказ с указанием на поля — это не поломка плагина: процесс
			// жив и работает, просто значения не годятся.
			var invalid *ValidationError
			if errors.As(err, &invalid) {
				view.Fields = invalid.Fields
				view.Problem = invalid.Message
				view.Items = invalid.Items
				return view, err
			}
			m.setProblem(id, err)
			return view, err
		}
	}

	m.mu.Lock()
	m.config.Plugins.Chat[id] = stored
	delete(m.fields, id)
	m.mu.Unlock()

	if err := m.save(); err != nil {
		return PluginView{}, err
	}
	view, _ := m.Get(id)
	return view, nil
}

// processContext is the lifetime of plugin processes: the kernel context, or
// the background one before Start has been called.
func (m *Manager) processContext() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.lifetime != nil {
		return m.lifetime
	}
	return context.Background()
}

// save writes the configuration, called with the lock released.
func (m *Manager) save() error {
	m.mu.Lock()
	config := m.config
	path := m.configPath
	m.mu.Unlock()

	return config.Save(path)
}

func (m *Manager) setProblem(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.problems[id] = err.Error()
}

func (m *Manager) setFields(id string, fields map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fields[id] = fields
}

// firstOf берёт первую ошибку поля: для заголовка в списке достаточно одной.
func firstOf(fields map[string]string) string {
	for _, message := range fields {
		return message
	}
	return ""
}

// DescribeTimeout is how long the kernel waits for a plugin to introduce
// itself. It is exported for the API tests.
const DescribeTimeout = 5 * time.Second
