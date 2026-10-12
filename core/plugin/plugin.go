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
	"slices"
	"sync"
	"time"

	"studio/core/protocol"
)

const (
	scanLimit   = 1 << 20 // 1 MiB per line
	stopTimeout = 3 * time.Second
	replyQueue  = 4   // answers waiting to be read by the kernel
	chatQueue   = 256 // events held while nobody reads them
	watchQueue  = 64  // events held for one subscriber before we skip it
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

	// events carries everything the plugin has to say about the chat: the
	// lines themselves, what happened to the channels, how the connection is.
	// Очередь одна на всех: читателей может быть сколько угодно, читателей
	// очереди — один.
	events chan Event
	// watchers read events; the manager reads them and passes them on.
	watchers []chan Event

	stateMu     sync.Mutex
	waiting     bool
	waitingText string

	// connection — что с подключением к чату: один из Connection* из
	// протокола. Живёт здесь, а не только в событиях, потому что странице
	// настроек нужно показать его рядом с названием плагина, а не искать
	// по ленте чата.
	connection     string
	connectionText string

	// onChange зовётся, когда изменилось что-то в жизни плагина: он занялся,
	// освободился, подключился или потерял связь. Ядро вешает его при запуске,
	// чтобы страница узнавала сразу, а не опросом.
	onChange func()

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
		events:  make(chan Event, chatQueue),
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

// WaitErr is why the process exited, if it exited badly. Ждёт завершения, так
// что звать его надо только после Done.
func (p *Plugin) WaitErr() error {
	select {
	case <-p.done:
	default:
		return nil
	}

	// В канале одно значение, и reap() положил его перед закрытием done.
	err, ok := <-p.waitErr
	if !ok {
		return nil
	}
	return err
}

// Events is the stream of chat events from this plugin.
func (p *Plugin) Events() <-chan Event {
	return p.events
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

// forward passes anything the plugin says about the chat to the consumers.
// Control messages stay out of it: one line, one meaning.
func (p *Plugin) forward(line []byte) bool {
	message, err := protocol.Decode(line)
	if err != nil {
		return false
	}
	if !protocol.IsChatKind(message.Kind) && !protocol.IsChannelKind(message.Kind) {
		return false
	}

	// Состояние связи запоминаем наравне с отправкой дальше: по нему страница
	// настроек показывает «онлайн» или «офлайн» рядом с плагином.
	if message.Kind == protocol.KindConnectionState {
		var payload protocol.ConnectionPayload
		if err := message.PayloadInto(&payload); err == nil {
			p.setConnection(payload.State, payload.Reason)
		}
	}

	event := Event{Plugin: p.Name, Message: message}

	// Подписчиков может быть много, и очередь на всех одна: кто не успевает,
	// тот получит меньше. Лучше потерять строчку у одного читателя, чем
	// затормозить всех.
	p.stateMu.Lock()
	watchers := slices.Clone(p.watchers)
	p.stateMu.Unlock()

	sent := false
	for _, watcher := range watchers {
		select {
		case watcher <- event:
			sent = true
		default:
			p.logger.Warn("a subscriber is behind, dropping an event", "kind", message.Kind)
		}
	}
	if !sent {
		select {
		case p.events <- event:
		default:
			p.logger.Warn("event queue is full, dropping", "kind", message.Kind)
		}
	}
	return true
}

// Watch adds a reader for this plugin's events and returns the function that
// takes it away again.
func (p *Plugin) Watch() (<-chan Event, func()) {
	watcher := make(chan Event, watchQueue)

	p.stateMu.Lock()
	p.watchers = append(p.watchers, watcher)
	p.stateMu.Unlock()

	var once sync.Once
	return watcher, func() {
		once.Do(func() {
			p.stateMu.Lock()
			defer p.stateMu.Unlock()

			for i, other := range p.watchers {
				if other == watcher {
					p.watchers = append(p.watchers[:i], p.watchers[i+1:]...)
					return
				}
			}
		})
	}
}

// Waiting tells whether the plugin is busy and what it is busy with. The
// options page keeps the save button disabled while this is true.
func (p *Plugin) Waiting() (bool, string) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.waiting, p.waitingText
}

// OnChange sets the hook that hears anything in the plugin's life change: it
// got busy, went quiet, connected or lost the service. Kernel only: the hook
// runs on the goroutine that read the message, so it must not block.
func (p *Plugin) OnChange(fn func()) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.onChange = fn
}

// Connection tells how the plugin is doing with the chat service.
func (p *Plugin) Connection() (state, text string) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	return p.connection, p.connectionText
}

func (p *Plugin) setWaiting(waiting bool, text string) {
	p.stateMu.Lock()

	if p.waiting != waiting {
		if waiting {
			p.logger.Info("busy", "reason", text)
		} else {
			p.logger.Info("idle")
		}
	}
	changed := p.waiting != waiting || p.waitingText != text
	p.waiting = waiting
	p.waitingText = text
	notify := p.onChange
	p.stateMu.Unlock()

	// Молчание — тоже ответ: занятость могла не поменяться, а текст мог, и
	// странице есть что показать.
	if changed && notify != nil {
		notify()
	}
}

// setConnection remembers what the service connection is doing. Само событие
// уходит дальше по ленте чата, но странице настроек оно нужно и здесь: она
// показывает связь рядом с названием плагина.
func (p *Plugin) setConnection(state, text string) {
	p.stateMu.Lock()

	changed := p.connection != state || p.connectionText != text
	p.connection = state
	p.connectionText = text
	notify := p.onChange
	p.stateMu.Unlock()

	if changed && notify != nil {
		notify()
	}
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
