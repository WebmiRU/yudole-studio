// Package settings reads and writes the application configuration file.
//
// The file is a single JSON document describing every plugin and its options:
//
//	{
//	  "kind": "config",
//	  "version": "1.0",
//	  "plugins": {
//	    "chat": {
//	      "goodgame": { "enabled": true, "channels": {} }
//	    }
//	  }
//	}
//
// Options live next to "enabled" inside the plugin object, because their set
// is defined by the plugin itself, not by the kernel. Sections the kernel does
// not know about are kept as they are, so saving never drops someone else's
// data.
package settings

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

const (
	// Kind marks the document as a studio config file.
	Kind = "config"
	// Version is the current format version.
	Version = "1.0"

	// FileName is the default configuration file name.
	FileName = "config.json"

	filePerm  = 0o600 // the file may hold tokens and passwords
	dirPerm   = 0o755
	writeTemp = ".config-*.tmp"
)

// Config is the whole configuration document.
type Config struct {
	Kind    string  `json:"kind"`
	Version string  `json:"version"`
	Plugins Plugins `json:"plugins"`
	extra   map[string]jsontext.Value
}

// Plugins groups plugin settings by kind, for example "chat".
type Plugins struct {
	Chat map[string]Plugin `json:"chat"`
}

// Plugin is the configuration of a single plugin: whether it is on, plus the
// values of the options it declared.
type Plugin struct {
	Enabled bool           `json:"enabled"`
	Options map[string]any `json:"options"`
}

// New returns an empty configuration of the current version.
func New() *Config {
	return &Config{
		Kind:    Kind,
		Version: Version,
		Plugins: Plugins{Chat: map[string]Plugin{}},
	}
}

// Load reads the configuration from path. A missing file is not an error: the
// defaults are returned so the caller can decide whether to create it.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return New(), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(data, path)
}

// Parse reads a configuration document. name is used in error messages.
func Parse(data []byte, name string) (*Config, error) {
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}

	if config.Kind != "" && config.Kind != Kind {
		return nil, fmt.Errorf("parse %s: kind %q, want %q", name, config.Kind, Kind)
	}
	if config.Kind == "" {
		config.Kind = Kind
	}
	if config.Version == "" {
		config.Version = Version
	}
	if config.Plugins.Chat == nil {
		config.Plugins.Chat = map[string]Plugin{}
	}
	for name, plugin := range config.Plugins.Chat {
		if plugin.Options == nil {
			plugin.Options = map[string]any{}
			config.Plugins.Chat[name] = plugin
		}
	}

	return &config, nil
}

// Save writes the configuration to path atomically: the file is replaced in one
// step, so a crash mid-write cannot leave a half-written config behind.
func (c *Config) Save(path string) error {
	data, err := c.MarshalIndent()
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	temp, err := os.CreateTemp(dir, writeTemp)
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)

	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write %s: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tempName, err)
	}
	// Tokens and passwords live in this file.
	if err := os.Chmod(tempName, filePerm); err != nil {
		return fmt.Errorf("chmod %s: %w", tempName, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// MarshalIndent renders the config the way it is stored on disk: two-space
// indentation, a trailing newline, and the sections this version of the kernel
// does not understand.
func (c *Config) MarshalIndent() ([]byte, error) {
	// Marshal through an alias so this method cannot call itself.
	type plain Config

	known, err := json.Marshal(plain(*c), json.Deterministic(true))
	if err != nil {
		return nil, err
	}

	fields := map[string]jsontext.Value{}
	if err := json.Unmarshal(known, &fields); err != nil {
		return nil, err
	}
	for key, value := range c.extra {
		if _, taken := fields[key]; !taken {
			fields[key] = value
		}
	}

	var buf bytes.Buffer
	encoder := jsontext.NewEncoder(&buf, jsontext.Multiline(true), jsontext.WithIndent("  "))
	if err := json.MarshalEncode(encoder, fields, json.Deterministic(true)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalJSON reads the known fields and keeps everything else in extra, so
// saving the file does not silently drop sections written by a newer kernel.
func (c *Config) UnmarshalJSON(data []byte) error {
	type plain Config

	var parsed plain
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*c = Config(parsed)

	var raw map[string]jsontext.Value
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range []string{"kind", "version", "plugins"} {
		delete(raw, key)
	}
	if len(raw) > 0 {
		c.extra = raw
	}
	return nil
}

// ChatPlugins returns the names of the configured chat plugins, sorted.
func (c *Config) ChatPlugins() []string {
	names := make([]string, 0, len(c.Plugins.Chat))
	for name := range c.Plugins.Chat {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Chat returns the settings of a chat plugin.
func (c *Config) Chat(name string) (Plugin, bool) {
	plugin, ok := c.Plugins.Chat[name]
	return plugin, ok
}

// EnsureChat returns the settings of a chat plugin, adding it with the default
// options when it is not in the file yet.
func (c *Config) EnsureChat(name string) Plugin {
	if plugin, ok := c.Plugins.Chat[name]; ok {
		return plugin
	}
	plugin := Plugin{Options: map[string]any{}}
	c.Plugins.Chat[name] = plugin
	return plugin
}

// Enabled reports whether a chat plugin is switched on. Unknown plugins are off.
func (c *Config) Enabled(name string) bool {
	plugin, ok := c.Plugins.Chat[name]
	return ok && plugin.Enabled
}

// Summary is a one-line description of a plugin's settings, for logs.
func (p Plugin) Summary() string {
	state := "off"
	if p.Enabled {
		state = "on"
	}
	return fmt.Sprintf("%s (%d options)", state, len(p.Options))
}
