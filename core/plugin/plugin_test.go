package plugin

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"studio/core/protocol"
)

// fakePlugin writes a shell script that speaks the NDJSON protocol: it greets,
// answers get_info, and confirms the messages it receives.
func fakePlugin(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake plugins are shell scripts")
	}

	path := filepath.Join(t.TempDir(), "fake")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureLog redirects the default logger into a buffer for the duration of
// the test and returns it.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// echoPlugin answers in NDJSON and mirrors every set_options with an ack.
const echoPlugin = `#!/bin/sh
echo '{"kind":"system.log","payload":{"level":"info","message":{"src":"Hello from fake plugin","text":"Hello from fake plugin","html":"Hello from fake plugin"}}}'
echo '{"kind":"system.plugin.hello","payload":{"title":"Fake","version":"0.1.0","description":"A fake plugin","options":[{"key":"nick","type":"text","label":"Nick","required":true},{"key":"words","type":"text/multiple","label":"Words"}]}}'
while IFS= read -r line; do
  case "$line" in
    *system.plugin.hello*)
      echo '{"kind":"system.plugin.hello","payload":{"title":"Fake","version":"0.1.0","description":"A fake plugin","options":[{"key":"nick","type":"text","label":"Nick","required":true},{"key":"words","type":"text/multiple","label":"Words"}]}}'
      ;;
    *system.plugin.options.set*) echo '{"kind":"system.message.ack"}' ;;
    *system.plugin.quit*)        echo '{"kind":"system.message.ack"}'; exit 0 ;;
  esac
done
`

func TestPluginStreamsMessages(t *testing.T) {
	logs := captureLog(t)

	p, err := Start(context.Background(), "fake", fakePlugin(t, echoPlugin))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := waitForLog(t, logs, "Hello from fake plugin"); err != nil {
		t.Fatalf("greeting never arrived: %v", err)
	}
	if !strings.Contains(logs.String(), `plugin=fake`) {
		t.Errorf("log line is missing the plugin name:\n%s", logs)
	}

	// Log levels chosen by the plugin are honoured.
	if !strings.Contains(logs.String(), "level=WARN") && !strings.Contains(logs.String(), "level=INFO") {
		t.Errorf("no level in the log line:\n%s", logs)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("plugin did not exit after Stop")
	}
}

// Describe and Configure are the requests the kernel makes to a plugin.
func TestPluginDescribeAndConfigure(t *testing.T) {
	logs := captureLog(t)

	p, err := Start(context.Background(), "fake", fakePlugin(t, echoPlugin))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	info, err := p.Describe()
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.ID != "fake" || info.Title != "Fake" {
		t.Errorf("info = %+v, want the fake plugin", info)
	}
	if len(info.Options) != 2 {
		t.Fatalf("options = %v, want two", info.OptionKeys())
	}
	if info.Options[0].Key != "nick" || info.Options[0].Type != protocol.OptionText {
		t.Errorf("first option = %+v", info.Options[0])
	}

	if _, err := p.Configure(map[string]any{"nick": "vasya", "words": []any{"a"}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := waitForLog(t, logs, "Hello from fake plugin"); err != nil {
		t.Error(err)
	}
}

// A plugin may answer an ack with the values it settled on: it turns a nickname
// or a link into a channel id, and the config has to keep that, not the text
// the user typed.
const normalizePlugin = `#!/bin/sh
echo '{"kind":"system.plugin.hello","payload":{"title":"Fake","version":"0.1.0","options":[{"key":"channels","type":"select/multiple","label":"Channels"}]}}'
while IFS= read -r line; do
  case "$line" in
    *system.plugin.hello*)
      echo '{"kind":"system.plugin.hello","payload":{"title":"Fake","version":"0.1.0","options":[{"key":"channels","type":"select/multiple","label":"Channels"}]}}'
      ;;
    *system.plugin.options.set*) echo '{"kind":"system.message.ack","payload":{"values":{"channels":[{"value":"53029","label":"E.Wolf"}]}}}' ;;
    *system.plugin.quit*)        echo '{"kind":"system.message.ack"}'; exit 0 ;;
  esac
done
`

func TestConfigureReturnsWhatThePluginSettledOn(t *testing.T) {
	captureLog(t)

	p, err := Start(context.Background(), "fake", fakePlugin(t, normalizePlugin))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	if _, err := p.Describe(); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	accepted, err := p.Configure(map[string]any{
		"channels": []any{"https://goodgame.ru/E.Wolf"},
	})
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}

	got, ok := accepted["channels"].([]any)
	if !ok || len(got) != 1 {
		t.Fatalf("accepted channels = %#v, want one", accepted["channels"])
	}
	entry, ok := got[0].(map[string]any)
	if !ok {
		t.Fatalf("accepted entry = %#v, want a pair", got[0])
	}
	if entry["value"] != "53029" || entry["label"] != "E.Wolf" {
		t.Errorf("accepted entry = %v, want 53029/E.Wolf", entry)
	}
}

// A plugin that does not bother normalizing just confirms: the kernel keeps what
// the user sent.
func TestConfigureKeepsValuesWhenThePluginSendsNone(t *testing.T) {
	captureLog(t)

	p, err := Start(context.Background(), "fake", fakePlugin(t, echoPlugin))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	if _, err := p.Describe(); err != nil {
		t.Fatalf("Describe: %v", err)
	}

	sent := map[string]any{"nick": "vasya"}
	accepted, err := p.Configure(sent)
	if err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if accepted != nil {
		t.Errorf("accepted = %v, want nil", accepted)
	}
	if merged(sent, accepted)["nick"] != "vasya" {
		t.Errorf("merged lost the value the user sent: %v", merged(sent, accepted))
	}
}

// Text that is not a protocol message must not stop the reader.
func TestPluginIgnoresGarbage(t *testing.T) {
	logs := captureLog(t)

	script := `#!/bin/sh
echo 'not json at all'
echo '{"kind":"system.log","payload":{"level":"info","message":{"src":"still alive","text":"still alive","html":"still alive"}}}'
sleep 0.3
`
	p, err := Start(context.Background(), "chatty", fakePlugin(t, script))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop() })

	if err := waitForLog(t, logs, "still alive"); err != nil {
		t.Errorf("reader stopped after garbage: %v", err)
	}
	if !strings.Contains(logs.String(), "not a protocol message") {
		t.Errorf("garbage was not reported:\n%s", logs)
	}
}

// A plugin that ignores the quit message must still be killed.
func TestPluginKilledOnStop(t *testing.T) {
	captureLog(t)

	path := fakePlugin(t, "#!/bin/sh\ntrap '' TERM\nwhile true; do sleep 0.2; done\n")
	p, err := Start(context.Background(), "stubborn", path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- p.Stop() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(stopTimeout + 3*time.Second):
		t.Fatal("Stop did not give up on the plugin")
	}
}

func TestStartFailsForMissingBinary(t *testing.T) {
	captureLog(t)

	if _, err := Start(context.Background(), "ghost", filepath.Join(t.TempDir(), "ghost")); err == nil {
		t.Fatal("expected an error for a missing binary")
	}
}

func TestSupervisorKeepsGoingAfterAFailure(t *testing.T) {
	captureLog(t)
	supervisor := &Supervisor{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	supervisor.StartAll(ctx, []Descriptor{
		{Name: "broken", Path: filepath.Join(t.TempDir(), "nope")},
		{Name: "fake", Path: fakePlugin(t, echoPlugin)},
	})

	if got := supervisor.Names(); len(got) != 1 || got[0] != "fake" {
		t.Fatalf("running plugins = %v, want [fake]", got)
	}
	if _, ok := supervisor.Find("fake"); !ok {
		t.Error("Find did not return the running plugin")
	}
	if _, ok := supervisor.Find("broken"); ok {
		t.Error("Find returned a plugin that failed to start")
	}

	// set_options обязан нести значения: без payload ядро такой вызов теперь
// не собирает, и MustNew паникует. Раньше сюда передавался nil, и тест проходил
// только потому, что проверки не было.
supervisor.Broadcast(protocol.MustNew(protocol.KindSetOptions, protocol.SetOptionsPayload{}))
	supervisor.StopAll()
	if len(supervisor.Names()) != 0 {
		t.Errorf("plugins after StopAll = %v, want none", supervisor.Names())
	}
}

func waitForLog(t *testing.T, buf *bytes.Buffer, want string) error {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &timeoutError{want: want, log: buf.String()}
}

type timeoutError struct {
	want string
	log  string
}

func (e *timeoutError) Error() string {
	return "timed out waiting for " + e.want + " in log:\n" + e.log
}
