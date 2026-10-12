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
	Buttons     []protocol.Button            `json:"buttons,omitempty"`
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

	// subscribers read the events of every plugin. The chat page is one of
	// them: it shows all the chats at once, so it needs the events from all
	// the plugins at the same time.
	subscribers map[int]chan Event
	nextSub     int

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

		subscribers: map[int]chan Event{},
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
	m.subscribers = map[int]chan Event{}
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
		view.Buttons = info.Buttons
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
	if accepted, err := process.Configure(values); err != nil {
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
	} else if accepted != nil {
		// Плагин привёл настройки к своему виду (например, превратил ссылки
		// в номера каналов) — сохраняем то, с чем он реально работает.
		m.remember(id, accepted)
		if err := m.save(); err != nil {
			slog.Warn("cannot save the settings the plugin settled on", "plugin", id, "err", err)
		}
	}

	m.mu.Lock()
	m.running[id] = process
	m.infos[id] = info
	delete(m.problems, id)
	m.mu.Unlock()

	// События плагина кто-то должен читать, иначе очередь переполнится и
	// реплики начнут теряться. Читаем их всегда, даже когда никто не смотрит:
	// подписчики придут и увидят то, что после них.
	events, stopWatching := process.Watch()
	go m.pump(id, events, stopWatching)

	slog.Info("plugin ready", "plugin", id, "title", info.Title, "options", len(info.Options))
	return nil
}

// pump carries the events of one plugin to everybody who is watching.
func (m *Manager) pump(id string, events <-chan Event, stopWatching func()) {
	defer stopWatching()

	for event := range events {
		m.broadcast(event)
	}
}

// Subscribe hands out a stream of events from every plugin and returns the
// function that closes it. Closing matters: a page that closes the tab must not
// leave a channel behind that fills up and makes the kernel drop events.
func (m *Manager) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 64
	}

	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	events := make(chan Event, buffer)
	m.subscribers[id] = events
	m.mu.Unlock()

	var once sync.Once
	return events, func() {
		once.Do(func() {
			m.mu.Lock()
			defer m.mu.Unlock()

			if _, ok := m.subscribers[id]; !ok {
				return
			}
			delete(m.subscribers, id)
			close(events)
		})
	}
}

// broadcast passes an event to every subscriber. A subscriber that is behind
// loses this event rather than holding up the rest: the chat is live, and a
// slow page must not make the kernel drop lines for everyone else.
func (m *Manager) broadcast(event Event) {
	m.mu.Lock()
	subscribers := make([]chan Event, 0, len(m.subscribers))
	for _, events := range m.subscribers {
		subscribers = append(subscribers, events)
	}
	m.mu.Unlock()

	for _, events := range subscribers {
		select {
		case events <- event:
		default:
			slog.Warn("subscriber is behind, dropping an event",
				"plugin", event.Plugin, "kind", event.Kind())
		}
	}
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

	process := m.running[id]
	m.mu.Unlock()

	// A plugin that is switched off keeps its settings but is not running, so
	// there is nobody to send them to yet.
	accepted := values
	if process != nil {
		result, err := process.Configure(values)
		if err != nil {
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
		// Плагин мог привести значения к своему виду: ссылка или ник вместо
		// номера канала. В конфиг идёт то, с чем он реально работает.
		accepted = merged(values, result)
	}

	m.remember(id, accepted)

	if err := m.save(); err != nil {
		return PluginView{}, err
	}
	view, _ := m.Get(id)
	return view, nil
}

// Run asks a running plugin to do something that is not about its options.
func (m *Manager) Run(ctx context.Context, id, kind string) (string, error) {
	m.mu.Lock()
	process := m.running[id]
	_, known := m.descriptors[id]
	m.mu.Unlock()

	if !known {
		return "", fmt.Errorf("plugin %q is not installed", id)
	}
	if process == nil {
		// Нет процесса — некому выполнять. Проще сказать прямо, чем вернуть
		// ошибку из ниоткуда.
		return "", fmt.Errorf("plugin %q is not running", id)
	}
	return process.Run(ctx, kind)
}

// remember puts the values a plugin settled on into the config. Until the file
// is saved they live only in memory.
func (m *Manager) remember(id string, values map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored := m.config.EnsureChat(id)
	stored.Options = values
	m.config.Plugins.Chat[id] = stored
	delete(m.fields, id)
}

// merged overlays what the plugin answered with onto what the user sent. A
// plugin usually only names the options it handled; everything else — including
// keys from a newer plugin version — has to survive.
func merged(sent, accepted map[string]any) map[string]any {
	if accepted == nil {
		return sent
	}
	out := make(map[string]any, len(sent)+len(accepted))
	for key, value := range sent {
		out[key] = value
	}
	for key, value := range accepted {
		out[key] = value
	}
	return out
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
