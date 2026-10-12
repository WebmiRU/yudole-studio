package plugin

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
echo '{"type":"log","level":"info","message":"Hello from fake plugin"}'
while IFS= read -r line; do
  case "$line" in
    *get_info*)
      echo '{"type":"info","id":"fake","name":"Fake","version":"0.1.0","description":"A fake plugin","options":[{"key":"nick","type":"text","label":"Nick","required":true},{"key":"tags","type":"tags","label":"Tags","default":[]}]}'
      ;;
    *set_options*) echo '{"type":"ack"}' ;;
    *quit*)        echo '{"type":"ack"}'; exit 0 ;;
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

	info, err := p.Describe(context.Background())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if info.ID != "fake" || info.Name != "Fake" {
		t.Errorf("info = %+v, want the fake plugin", info)
	}
	if len(info.Options) != 2 {
		t.Fatalf("options = %v, want two", info.OptionKeys())
	}
	if info.Options[0].Key != "nick" || info.Options[0].Type != OptionText {
		t.Errorf("first option = %+v", info.Options[0])
	}

	if err := p.Configure(map[string]any{"nick": "vasya", "tags": []any{"a"}}); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := waitForLog(t, logs, "Hello from fake plugin"); err != nil {
		t.Error(err)
	}
}

// Text that is not a protocol message must not stop the reader.
func TestPluginIgnoresGarbage(t *testing.T) {
	logs := captureLog(t)

	script := `#!/bin/sh
echo 'not json at all'
echo '{"type":"log","message":"still alive"}'
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

	supervisor.Broadcast(Message{Type: TypeGetInfo})
	supervisor.StopAll()
	if len(supervisor.Names()) != 0 {
		t.Errorf("plugins after StopAll = %v, want none", supervisor.Names())
	}
}

// The kernel and the plugin must agree on the line format.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	want := Message{
		Type:        TypeInfo,
		ID:          "goodgame",
		Name:        "GoodGame",
		Version:     "0.1.0",
		Description: "Чат goodgame.ru",
		Options: []Option{{
			Key:     "channels",
			Type:    OptionTags,
			Label:   "Каналы",
			Hint:    "Enter — добавить канал",
			Default: []string{},
		}},
	}

	line, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Count(string(line), "\n") != 1 || !strings.HasSuffix(string(line), "\n") {
		t.Errorf("NDJSON line must end with exactly one newline: %q", line)
	}

	got, err := DecodeMessage(line)
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if got.Type != want.Type || got.Name != want.Name || len(got.Options) != 1 {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if got.Options[0].Key != "channels" || got.Options[0].Type != OptionTags {
		t.Errorf("option = %+v", got.Options[0])
	}
}

func TestDecodeRejectsLinesWithoutType(t *testing.T) {
	if _, err := DecodeMessage([]byte(`{"name":"GoodGame"}`)); err == nil {
		t.Error("expected an error for a message without a type")
	}
	if _, err := DecodeMessage([]byte(`hello`)); err == nil {
		t.Error("expected an error for a non-JSON line")
	}
}

func TestPluginPayloadStaysOneLine(t *testing.T) {
	// A description with a newline must not break the protocol.
	line, err := Encode(Message{Type: TypeLog, Message: "first\nsecond"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(line), "\n") != 1 {
		t.Errorf("message broke the line format: %q", line)
	}

	var back Message
	if err := json.Unmarshal(line, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Message != "first\nsecond" {
		t.Errorf("message = %q, want it unchanged", back.Message)
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
