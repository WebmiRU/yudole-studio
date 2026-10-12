package settings

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The template the studio config is meant to look like.
const template = `{
  "kind": "config",
  "version": "1.0",
  "plugins": {
    "chat": {
      "goodgame": {
        "enabled": true,
        "options": {
          "channels": []
        }
      },
      "twitch": {

      },
      "trovo": {

      },
      "youtube": {

      }
    }
  }
}`

func TestParseTemplate(t *testing.T) {
	config, err := Parse([]byte(template), "config.json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if config.Kind != Kind {
		t.Errorf("kind = %q, want %q", config.Kind, Kind)
	}
	if config.Version != Version {
		t.Errorf("version = %q, want %q", config.Version, Version)
	}

	want := []string{"goodgame", "trovo", "twitch", "youtube"}
	if got := config.ChatPlugins(); !reflect.DeepEqual(got, want) {
		t.Errorf("chat plugins = %v, want %v (sorted)", got, want)
	}

	goodgame, ok := config.Chat("goodgame")
	if !ok {
		t.Fatal("goodgame is missing")
	}
	if !goodgame.Enabled {
		t.Error("goodgame should be enabled")
	}
	channels, ok := goodgame.Options["channels"]
	if !ok {
		t.Fatalf("goodgame options = %v, want a channels key", goodgame.Options)
	}
	if list, ok := channels.([]any); !ok || len(list) != 0 {
		t.Errorf("channels = %#v, want an empty list", channels)
	}

	// A plugin object without "enabled" is valid and simply off.
	twitch, _ := config.Chat("twitch")
	if twitch.Enabled {
		t.Error("twitch should be off")
	}
	if len(twitch.Options) != 0 {
		t.Errorf("twitch options = %v, want none", twitch.Options)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	config, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.Kind != Kind || config.Version != Version {
		t.Errorf("defaults = %+v, want kind %q version %q", config, Kind, Version)
	}
	if len(config.ChatPlugins()) != 0 {
		t.Errorf("chat plugins = %v, want none", config.ChatPlugins())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("Load must not create the file on its own")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	config, err := Parse([]byte(template), "config.json")
	if err != nil {
		t.Fatal(err)
	}
	plugin := config.EnsureChat("goodgame")
	plugin.Enabled = true
	plugin.Options["login"] = "vasya"
	plugin.Options["games"] = []any{"csgo", "dota2"}
	plugin.Options["autoconnect"] = true
	config.Plugins.Chat["goodgame"] = plugin

	if err := config.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Compare the rendered documents: that is what a user and a diff see.
	got, want := mustMarshal(t, loaded), mustMarshal(t, config)
	if string(got) != string(want) {
		t.Errorf("round trip differs:\n got: %s\nwant: %s", got, want)
	}

	// Option values live in the nested "options" object.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"options": {`) {
		t.Errorf("options must be nested under their own key:\n%s", data)
	}
	if !strings.Contains(string(data), `"enabled": true`) {
		t.Errorf("enabled is missing:\n%s", data)
	}
}

// The file may hold tokens and passwords, so it belongs to the user only.
func TestSaveUsesPrivatePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := New().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}
}

func TestSaveLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := New().Save(path); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only config.json", names)
	}
}

// Sections written by a newer kernel must survive a load/save cycle.
func TestUnknownSectionsSurvive(t *testing.T) {
	input := `{
  "kind": "config",
  "version": "1.0",
  "ui": { "theme": "dark", "sidebar": true },
  "plugins": { "chat": { "goodgame": { "enabled": true, "options": {} } } }
}`

	config, err := Parse([]byte(input), "config.json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"theme": "dark"`) {
		t.Errorf("unknown section was dropped:\n%s", data)
	}
}

func TestParseRejectsBrokenFiles(t *testing.T) {
	cases := map[string]string{
		"invalid json": `{"kind":`,
		"wrong kind":   `{"kind": "profile", "version": "1.0"}`,
		"bad enabled":  `{"plugins": {"chat": {"goodgame": {"enabled": "yes"}}}}`,
	}

	for name, input := range cases {
		if _, err := Parse([]byte(input), "config.json"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestParseFillsInDefaults(t *testing.T) {
	config, err := Parse([]byte(`{"plugins": {"chat": {"goodgame": null}}}`), "config.json")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if config.Kind != Kind || config.Version != Version {
		t.Errorf("kind/version = %q/%q, want defaults", config.Kind, config.Version)
	}

	plugin, ok := config.Chat("goodgame")
	if !ok {
		t.Fatal("goodgame is missing")
	}
	if plugin.Options == nil {
		t.Error("options must never be nil, or writing them panics")
	}
}

func TestEnabledHelper(t *testing.T) {
	config, err := Parse([]byte(template), "config.json")
	if err != nil {
		t.Fatal(err)
	}

	if !config.Enabled("goodgame") {
		t.Error("goodgame should be enabled")
	}
	if config.Enabled("twitch") {
		t.Error("twitch should be off")
	}
	if config.Enabled("unknown") {
		t.Error("an unconfigured plugin must count as off")
	}
}

func TestEnsureChatIsIdempotent(t *testing.T) {
	config := New()

	plugin := config.EnsureChat("goodgame")
	plugin.Options["login"] = "vasya"
	config.Plugins.Chat["goodgame"] = plugin

	again := config.EnsureChat("goodgame")
	if again.Options["login"] != "vasya" {
		t.Errorf("EnsureChat reset existing options: %v", again.Options)
	}
	if got := config.ChatPlugins(); len(got) != 1 {
		t.Errorf("chat plugins = %v, want one", got)
	}
}

func TestPluginMarshalsOptionsUnderOptions(t *testing.T) {
	plugin := Plugin{Enabled: true, Options: map[string]any{"channels": []any{"csgo"}}}

	data, err := json.Marshal(plugin)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back["enabled"] != true {
		t.Errorf("enabled = %v, want true", back["enabled"])
	}

	options, ok := back["options"].(map[string]any)
	if !ok {
		t.Fatalf("options = %#v, want an object", back["options"])
	}
	if _, ok := options["channels"]; !ok {
		t.Errorf("options = %v, want a channels key", options)
	}
	if _, ok := back["channels"]; ok {
		t.Error("option values must not be lifted next to enabled")
	}
}

func mustMarshal(t *testing.T, config *Config) []byte {
	t.Helper()
	data, err := config.MarshalIndent()
	if err != nil {
		t.Fatal(err)
	}
	return data
}
