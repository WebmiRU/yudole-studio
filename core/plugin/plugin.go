// Package plugin runs chat plugins: separate processes the kernel talks to
// over stdin/stdout, one JSON message per line.
//
// A plugin lives in its own directory with a binary of the same name, for
// example plugins/chat/goodgame/goodgame. See protocol.go for the messages
// the two sides exchange.
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
	scanLimit   = 1 << 20 // 1 MiB per line
	stopTimeout = 3 * time.Second
	replyQueue  = 4 // answers waiting to be read by the kernel
)

// A Plugin is a running plugin process.
type Plugin struct {
	Name string
	Path string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	logger *slog.Logger

	// replies carries the answers the kernel is waiting for: info and ack.
	replies chan Message

	writeMu sync.Mutex
	done    chan struct{}
	waitErr chan error
}

// Start launches the plugin at path and starts handling its messages. The
// process is stopped when Stop is called or ctx is cancelled.
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
		replies: make(chan Message, replyQueue),
		done:    make(chan struct{}),
		waitErr: make(chan error, 1),
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin %q: start %s: %w", name, path, err)
	}
	logger.Info("plugin started", "pid", cmd.Process.Pid)

	go p.readMessages(stdout)
	go p.readStderr(stderr)
	go p.reap()

	return p, nil
}

// Send writes one message to the plugin.
func (p *Plugin) Send(message Message) error {
	line, err := Encode(message)
	if err != nil {
		return fmt.Errorf("plugin %q: encode %s: %w", p.Name, message.Type, err)
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	if _, err := p.stdin.Write(line); err != nil {
		return fmt.Errorf("plugin %q: send %s: %w", p.Name, message.Type, err)
	}
	return nil
}

// repliesTo returns the answers waiting to be read by the kernel.
func (p *Plugin) repliesTo() <-chan Message {
	return p.replies
}

// Stop asks the plugin to quit, then kills it if it ignores the request.
func (p *Plugin) Stop() error {
	if err := p.Send(Message{Type: TypeQuit}); err != nil {
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

// readMessages turns the protocol lines of the plugin into messages: logs go
// straight to the kernel log, replies wait in the inbox for a request.
func (p *Plugin) readMessages(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), scanLimit)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		message, err := DecodeMessage([]byte(line))
		if err != nil {
			p.logger.Warn("not a protocol message", "line", line, "err", err)
			continue
		}
		p.handle(message)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, fs.ErrClosed) {
		p.logger.Error("read failed", "stream", "stdout", "err", err)
	}
}

func (p *Plugin) handle(message Message) {
	switch message.Type {
	case TypeLog:
		p.logger.Log(context.Background(), logLevel(message.Level), message.Message)
	case TypeError:
		p.logger.Error(message.Message)

	case TypeInfo, TypeAck:
		select {
		case p.replies <- message:
		default:
			p.logger.Warn("reply queue is full, dropping a message", "type", message.Type)
		}

	default:
		// An older kernel must not choke on what a newer plugin says.
		p.logger.Debug("message not handled here", "type", message.Type)
	}
}

// readStderr keeps whatever a plugin writes outside the protocol visible,
// without pretending it is part of it.
func (p *Plugin) readStderr(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 4096), scanLimit)

	for scanner.Scan() {
		if line := scanner.Text(); line != "" {
			p.logger.Warn(line, "stream", "stderr")
		}
	}
}

func logLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
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
