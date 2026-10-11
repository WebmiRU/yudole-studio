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
)

// fakePlugin writes a shell script that behaves like a chat plugin: greets on
// start, answers ping, exits on quit.
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

const echoScript = `#!/bin/sh
echo "Hello from fake plugin"
while read -r line; do
  case "$line" in
    ping) echo "pong" ;;
    quit) echo "bye"; exit 0 ;;
  esac
done
`

func TestPluginStreamsOutput(t *testing.T) {
	logs := captureLog(t)

	p, err := Start(context.Background(), "fake", fakePlugin(t, echoScript))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := waitForLog(t, logs, "Hello from fake plugin"); err != nil {
		t.Fatalf("greeting never arrived: %v", err)
	}
	if !strings.Contains(logs.String(), `plugin=fake`) {
		t.Errorf("log line is missing the plugin name:\n%s", logs)
	}

	// The command channel works too: this is what the options page will use.
	if err := p.Send("ping"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := waitForLog(t, logs, "pong"); err != nil {
		t.Fatalf("answer never arrived: %v", err)
	}

	if err := p.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("plugin did not exit after Stop")
	}
	if err := waitForLog(t, logs, "bye"); err != nil {
		t.Errorf("plugin did not get the quit command: %v", err)
	}
}

// A plugin that ignores the quit command must still be killed.
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
	logs := captureLog(t)
	supervisor := &Supervisor{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	good := fakePlugin(t, echoScript)
	supervisor.StartAll(ctx, []Descriptor{
		{Name: "broken", Path: filepath.Join(t.TempDir(), "nope")},
		{Name: "fake", Path: good},
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

	supervisor.Broadcast("ping")
	if err := waitForLog(t, logs, "pong"); err != nil {
		t.Errorf("broadcast did not reach the plugin: %v", err)
	}

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
