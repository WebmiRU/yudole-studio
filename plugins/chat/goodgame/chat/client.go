package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Тайминги. GoodGame не документирует свои, поэтому берём с запасом: лучше
// подождать лишнее, чем отвалиться от живого соединения.
const (
	DialTimeout    = 15 * time.Second
	WelcomeTimeout = 10 * time.Second
	JoinTimeout    = 10 * time.Second
	RequestTimeout = 10 * time.Second
	WriteTimeout   = 10 * time.Second

	// ReadLimit — потолок одного сообщения. Вдруг придёт история канала.
	ReadLimit = 4 << 20

	// EventBuffer is how many messages we hold while nobody reads them.
	EventBuffer = 256
)

// ErrClosed is returned once the client is done for good.
var ErrClosed = errors.New("соединение закрыто")

// Client is one connection to the GoodGame chat.
//
// A connection is not tied to a channel: it opens, the service greets it with
// its protocol version, and after that we may join as many channels as we want
// over the same socket. Reading happens in the background from the moment the
// client is open, so a message that arrives while we are waiting for an answer
// to our request does not get lost.
type Client struct {
	conn     *websocket.Conn
	endpoint string

	writeMu sync.Mutex

	mu      sync.Mutex
	waiters []*waiter
	events  chan *Message
	closed  bool
	done    chan struct{}

	// dropped counts the messages we had to throw away because nobody read
	// them in time. Worth knowing: it means the consumer is too slow.
	dropped int
}

// waiter is one request waiting for its answer.
type waiter struct {
	match func(*Message) bool
	reply chan *Message
}

// Open connects and waits for the welcome.
//
// The welcome is not a formality: it is the answer that says the connection is
// alive and which version of the protocol we ended up on. Joining before it
// arrives is a guess, and GoodGame does not answer guesses.
func Open(ctx context.Context, endpoint string) (*Client, error) {
	if endpoint == "" {
		endpoint = Endpoint
	}

	dialCtx, cancel := context.WithTimeout(ctx, DialTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("не подключиться к %s: %w", endpoint, err)
	}

	client := &Client{
		conn:     conn,
		endpoint: endpoint,
		events:   make(chan *Message, EventBuffer),
		done:     make(chan struct{}),
	}
	conn.SetReadLimit(ReadLimit)

	// Приветствие приходит в первые же мгновения после рукопожатия. Ждать его
	// надо, стоя в очереди заранее: иначе сообщение пролетит мимо ожидающего и
	// уйдёт в общую очередь, а мы будем ждать того, что уже пришло.
	welcome := client.expect(func(m *Message) bool { return m.Type == TypeWelcome })

	// Чтение живёт своей горутиной: отмена контекста должна разрывать и его,
	// иначе Close зависнет на блокирующем Read.
	readCtx, stopRead := context.WithCancel(ctx)
	go client.read(readCtx, stopRead)

	welcomeCtx, cancelWelcome := context.WithTimeout(ctx, WelcomeTimeout)
	defer cancelWelcome()

	greeting, err := client.await(welcomeCtx, welcome)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("сервер не поздоровался: %w", err)
	}

	var said Welcome
	if err := greeting.Into(&said); err != nil {
		client.Close()
		return nil, fmt.Errorf("не разобрано приветствие: %w", err)
	}
	if said.ProtocolVersion != 2 {
		client.Close()
		return nil, fmt.Errorf("сервер говорит на протоколе %d, а мы умеем только в 2", said.ProtocolVersion)
	}

	slog.Debug("goodgame: соединение открыто", "endpoint", endpoint, "protocol", said.ProtocolVersion)
	return client, nil
}

// Endpoint is where we are connected.
func (c *Client) Endpoint() string { return c.endpoint }

// Events is everything the server says, in the order it said it.
func (c *Client) Events() <-chan *Message { return c.events }

// read decodes messages until the connection dies. One reader for the whole
// client: answers to our requests are picked out here too, otherwise a reply
// would sit in the queue until somebody happened to read it.
func (c *Client) read(ctx context.Context, stop context.CancelFunc) {
	// Поток закрывает поток: читающий — единственный, кто кладёт в events, и
	// он же последний из выходящих. Закрывать events надо именно здесь, а не в
	// close(): того, кто ждёт обрыва, разбудит только закрытый канал, а
	// close() его не трогает.
	defer func() {
		stop()
		c.close()
		close(c.events)
	}()

	for {
		kind, data, err := c.conn.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Debug("goodgame: соединение прервано", "err", err)
			}
			return
		}
		if kind != websocket.MessageText {
			continue
		}

		message, err := Decode(data)
		if err != nil {
			// Мусор в потоке не повод рвать соединение: может прийти и не
			// наш код, когда-нибудь придёт и наше.
			slog.Debug("goodgame: непонятное сообщение", "err", err, "raw", truncate(data))
			continue
		}
		c.deliver(ctx, message)
	}
}

// deliver hands a message to whoever waits for it, or to the queue.
func (c *Client) deliver(ctx context.Context, message *Message) {
	c.mu.Lock()
	for i, w := range c.waiters {
		if w.match(message) {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			c.mu.Unlock()
			w.reply <- message
			return
		}
	}
	c.mu.Unlock()

	select {
	case c.events <- message:
	default:
		// Очередь переполнена. Ждать здесь нельзя: читатель один, и если он
		// встанет, не придёт даже ответ на наш запрос. Лучше потерять
		// старейшее сообщение, чем встать.
		select {
		case <-c.events:
			c.mu.Lock()
			c.dropped++
			c.mu.Unlock()
		default:
		}
		select {
		case c.events <- message:
		case <-ctx.Done():
		}
	}
}

// Dropped is how many messages we threw away because nobody read them in time.
func (c *Client) Dropped() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// expect takes a place in the queue before anything is read. It matters for a
// message that can arrive unasked — like the welcome: by the time we get round
// to waiting, it may already be in the events queue, and nobody will ever look
// there for it again.
func (c *Client) expect(match func(*Message) bool) *waiter {
	w := &waiter{match: match, reply: make(chan *Message, 1)}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		close(w.reply)
		return w
	}
	c.waiters = append(c.waiters, w)
	return w
}

// await is Wait for a place taken by expect earlier.
func (c *Client) await(ctx context.Context, w *waiter) (*Message, error) {
	select {
	case message, ok := <-w.reply:
		if !ok {
			return nil, ErrClosed
		}
		return message, nil
	case <-ctx.Done():
		c.forget(w)
		return nil, ctx.Err()
	case <-c.done:
		c.forget(w)
		return nil, ErrClosed
	}
}

// Wait blocks until a message we are interested in arrives.
func (c *Client) Wait(ctx context.Context, match func(*Message) bool) (*Message, error) {
	return c.await(ctx, c.expect(match))
}

func (c *Client) forget(w *waiter) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for i, other := range c.waiters {
		if other == w {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			return
		}
	}
}

// Send writes one message to the service.
func (c *Client) Send(ctx context.Context, message *Message) error {
	line, err := message.Encode()
	if err != nil {
		return err
	}

	writeCtx, cancel := context.WithTimeout(ctx, WriteTimeout)
	defer cancel()

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.conn.Write(writeCtx, websocket.MessageText, line); err != nil {
		return fmt.Errorf("отправить %s: %w", message.Type, err)
	}
	return nil
}

// Do sends a request and waits for the answer.
//
// match says which message is ours: usually the type the request is answered
// with, plus anything else that must be ruled out, like a channel id.
func (c *Client) Do(ctx context.Context, request *Message, match func(*Message) bool) (*Message, error) {
	// Встаём в очередь до отправки, а не после. Ответ может прийти быстрее,
	// чем мы вернёмся из Write, и тогда он уйдёт в общую очередь, где его
	// уже никто не ищет.
	w := c.expect(match)

	if err := c.Send(ctx, request); err != nil {
		c.forget(w)
		return nil, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	return c.await(requestCtx, w)
}

// Join subscribes to a channel and waits until the service confirms.
//
// It is worth waiting: success_join is the only place the channel's name, its
// streamer and our rights in it come from. A channel we think we are in and are
// not is worse than one we never joined.
func (c *Client) Join(ctx context.Context, id ID) (*Join, error) {
	request, err := New("join", JoinRequest{ChannelID: id})
	if err != nil {
		return nil, err
	}

	joinCtx, cancel := context.WithTimeout(ctx, JoinTimeout)
	defer cancel()

	reply, err := c.Do(joinCtx, request, func(m *Message) bool {
		if m.Type == TypeSuccessJoin {
			return ChannelOf(m) == id
		}
		// Ошибка может назвать канал, а может и не назвать — на то, что она
		// пришла в ответ на наш вход, указывает то, что больше ждать нечего.
		return m.Type == TypeError
	})
	if err != nil {
		return nil, fmt.Errorf("канал %s: %w", id, err)
	}

	if reply.Type == TypeError {
		// Канал не повторяем: о входе в какой и так известно вызывающему.
		return nil, serviceError(reply)
	}

	var joined Join
	if err := reply.Into(&joined); err != nil {
		return nil, fmt.Errorf("не разобрано подтверждение входа: %w", err)
	}
	return &joined, nil
}

// Unjoin leaves a channel. It counts as done the moment the message is sent.
//
// Подтверждения тут нет, и ждать его нельзя: сервер отвечает на выход
// «{"success":true}» вообще без номера канала, так что ответ невозможно
// отнести к нашему запросу. Поэтому успех — это отправка, а что скажет сервер
// потом, придёт отдельным сообщением и никого не касается.
//
// Возвращается ошибка только если отправить не вышло: тогда мы остались в
// канале и вызывающий вправе считать уход неудавшимся.
func (c *Client) Unjoin(ctx context.Context, id ID) error {
	request, err := New("unjoin", UnjoinRequest{ChannelID: id})
	if err != nil {
		return err
	}
	return c.Send(ctx, request)
}

// Channels asks for the list of channels, the busiest first.
func (c *Client) Channels(ctx context.Context, start, count int) (*ChannelsList, error) {
	request, err := New("get_channels_list", ChannelsListRequest{Start: start, Count: count})
	if err != nil {
		return nil, err
	}

	reply, err := c.Do(ctx, request, func(m *Message) bool {
		return m.Type == TypeChannelsList || m.Type == TypeError
	})
	if err != nil {
		return nil, err
	}
	if reply.Type == TypeError {
		return nil, serviceError(reply)
	}

	var list ChannelsList
	if err := reply.Into(&list); err != nil {
		return nil, fmt.Errorf("не разобран список каналов: %w", err)
	}
	return &list, nil
}

// Close shuts the connection down.
func (c *Client) Close() error {
	c.close()
	return nil
}

func (c *Client) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()

	// CloseNow разрывает блокирующий Read, иначе горутина чтения зависнет.
	_ = c.conn.CloseNow()
	close(c.done)
}

// serviceError turns an error message into something to show. The channel is
// left out: whoever asked already knows which channel it was about.
func serviceError(message *Message) error {
	var failure Error
	if err := message.Into(&failure); err != nil || failure.ErrorMsg == "" {
		return errors.New("сервис отказал без объяснения (" + message.Type + ")")
	}
	return errors.New(failure.ErrorMsg)
}

// Diff tells what to join and what to leave to get from current to wanted,
// keeping the order the user gave.
func Diff(current, wanted []ID) (join, unjoin []ID) {
	have := make(map[ID]bool, len(current))
	for _, id := range current {
		have[id] = true
	}
	keep := make(map[ID]bool, len(wanted))

	for _, id := range wanted {
		if keep[id] {
			continue
		}
		keep[id] = true
		if !have[id] {
			join = append(join, id)
		}
	}
	for _, id := range current {
		if !keep[id] {
			unjoin = append(unjoin, id)
		}
	}
	return join, unjoin
}

func truncate(data []byte) string {
	const limit = 200
	if len(data) <= limit {
		return string(data)
	}
	return string(data[:limit]) + "…"
}
