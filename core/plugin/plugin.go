// Package plugin runs chat plugins: separate processes the kernel talks to
// over stdin/stdout, one message per line.
//
// A plugin lives in its own directory with a binary of the same name, for
// example plugins/chat/goodgame/goodgame. Anything the plugin prints on stdout
// is a message for the kernel; anything the kernel writes to stdin is a
// command for the plugin.
package plugin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os/exec"
	"sync"
	"time"
)

const (
	scanLimit     = 1 << 20 // 1 MiB per line
	stopTimeout   = 3 * time.Second
	writeErrorGap = 100 * time.Millisecond
)

// A Plugin is a running plugin process.
type Plugin struct {
	Name string
	Path string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	logger *slog.Logger

	writeMu sync.Mutex
	done    chan struct{}
	waitErr chan error
}

// Start launches the plugin at path and starts logging its output. The process
// is stopped when ctx is cancelled or Stop is called.
func Start(ctx context.Context, name, path string) (*Plugin, error) {
	logger := slog.Default().With("plugin", name)

	cmd := exec.CommandContext(ctx, path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: stdin: %w", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: stdout: %w", name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: stderr: %w", name, err)
	}

	p := &Plugin{
		Name:    name,
		Path:    path,
		cmd:     cmd,
		stdin:   stdin,
		logger:  logger,
		done:    make(chan struct{}),
		waitErr: make(chan error, 1),
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin %q: start %s: %w", name, path, err)
	}
	logger.Info("plugin started", "pid", cmd.Process.Pid)

	go p.readLines(stdout, "stdout", slog.LevelInfo)
	go p.readLines(stderr, "stderr", slog.LevelWarn)
	go p.reap()

	return p, nil
}

// Send writes a command to the plugin's stdin. The trailing newline is added.
func (p *Plugin) Send(command string) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	if _, err := fmt.Fprintf(p.stdin, "%s\n", command); err != nil {
		return fmt.Errorf("plugin %q: send %q: %w", p.Name, command, err)
	}
	return nil
}

// Stop asks the plugin to quit, then kills it if it ignores the request.
func (p *Plugin) Stop() error {
	if err := p.Send("quit"); err != nil {
		// The plugin may already be gone; killing it below is enough.
		p.logger.Debug("quit was not delivered", "err", err)
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(stopTimeout):
	}

	p.logger.Warn("did not stop in time, killing", "timeout", stopTimeout)
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	<-p.done
	return nil
}

// Done is closed once the process has exited.
func (p *Plugin) Done() <-chan struct{} {
	return p.done
}

// readLines logs every line the plugin writes to a stream.
func (p *Plugin) readLines(r io.Reader, stream string, level slog.Level) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), scanLimit)

	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			p.logger.Log(context.Background(), level, line, "stream", stream)
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, fs.ErrClosed) {
		p.logger.Error("read failed", "stream", stream, "err", err)
	}
}

// reap waits for the process and closes Done.
func (p *Plugin) reap() {
	err := p.cmd.Wait()
	if err != nil && !errors.Is(err, context.Canceled) {
		p.logger.Warn("exited", "err", err)
	} else {
		p.logger.Info("exited")
	}
	p.waitErr <- err
	close(p.waitErr)
	close(p.done)
}
