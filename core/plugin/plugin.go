// Package plugin runs chat plugins: separate processes the kernel talks to
// over stdin/stdout, one protocol message per line. The messages themselves
// live in the protocol package, which the plugins import too.
//
// A plugin lives in its own directory with a binary of the same name, for
// example plugins/chat/goodgame/goodgame.
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

	"studio/core/protocol"
)

const (
	scanLimit   = 1 << 20 // 1 MiB per line
	stopTimeout = 3 * time.Second
	replyQueue  = 4   // answers waiting to be read by the kernel
	chatQueue   = 256 // lines of chat held while nobody reads them
)

// A Plugin is a running plugin process.
type Plugin struct {
	Name string
	Path string

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	logger *slog.Logger

	// replies carries the answers the kernel is waiting for: info and ack.
	replies chan protocol.Message

	// chat carries the lines of chat, in the order the plugin sent them.
	chat chan protocol.ChatPayload

	stateMu     sync.Mutex
	waiting     bool
	waitingText string

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
		replies: make(chan protocol.Message, replyQueue),
		chat:    make(chan protocol.ChatPayload, chatQueue),
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
func (p *Plugin) Send(message protocol.Message) error {
	line, err := protocol.Encode(message)
	if err != nil {
		return fmt.Errorf("plugin %q: encode %s: %w", p.Name, message.Kind, err)
	}

	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	if _, err := p.stdin.Write(line); err != nil {
		return fmt.Errorf("plugin %q: send %s: %w", p.Name, message.Kind, err)
	}
	return nil
}

// repliesTo returns the answers waiting to be read by the kernel.
func (p *Plugin) repliesTo() <-chan protocol.Message {
	return p.replies
}

// Stop asks the plugin to quit, then kills it if it ignores the request.
func (p *Plugin) Stop() error {
	if err := p.Send(protocol.MustNew(protocol.KindQuit, nil)); err != nil {
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

// Chat is the stream of chat lines from this plugin.
func (p *Plugin) Chat() <-chan protocol.ChatPayload {
	return p.chat
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

		if p.forward([]byte(line)) {
			continue
		}

		message, err := protocol.Decode([]byte(line))
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

// forward passes a chat line to the consumers. Control messages stay out of
// it: one line, one meaning.
func (p *Plugin) forward(line []byte) bool {
	chat, err := protocol.DecodeChat(line)
	if err != nil {
		return false
	}

	p.logger.Info("chat message",
		"kind", chat.Kind,
		"channel", chat.Channel,
		"user", chat.User.Name,
		"text", chat.Message.Text,
	)

	select {
	case p.chat <- chat:
	default:
		p.logger.Warn("chat queue is full, dropping a line", "channel", chat.Channel)
	}
	return true
}

// Waiting tells whether the plugin is busy and what it is busy with. The
// options page keeps the save button disabled while this is true.
func (p *Plugin) Waiting() (bool, string) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.waiting, p.waitingText
}

func (p *Plugin) setWaiting(waiting bool, text string) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	if p.waiting != waiting {
		if waiting {
			p.logger.Info("busy", "reason", text)
		} else {
			p.logger.Info("idle")
		}
	}
	p.waiting = waiting
	p.waitingText = text
}

// reply hands an answer to whoever is waiting for it.
func (p *Plugin) reply(message protocol.Message) {
	select {
	case p.replies <- message:
	default:
		p.logger.Warn("reply queue is full, dropping a message", "kind", message.Kind)
	}
}

func (p *Plugin) handle(message protocol.Message) {
	switch message.Kind {
	case protocol.KindLog:
		var payload protocol.LogPayload
		if err := message.PayloadInto(&payload); err != nil {
			p.logger.Warn("cannot read the log payload", "err", err)
			return
		}
		p.logger.Log(context.Background(), logLevel(payload.Level), payload.Message)

	case protocol.KindWaiting:
		var payload protocol.WaitingPayload
		if err := message.PayloadInto(&payload); err != nil {
			p.logger.Warn("cannot read the waiting payload", "err", err)
			return
		}
		p.setWaiting(true, payload.Message)

	case protocol.KindError:
		var payload protocol.ErrorPayload
		if err := message.PayloadInto(&payload); err != nil {
			p.logger.Warn("cannot read the error payload", "err", err)
			return
		}
		p.logger.Error(payload.Message)
		// Отказ — тоже ответ на наш запрос: его ждёт Configure.
		p.reply(message)

	case protocol.KindPluginChat, protocol.KindAck:
		p.reply(message)

	default:
		// An older kernel must not choke on what a newer plugin says.
		p.logger.Debug("message not handled here", "kind", message.Kind)
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
