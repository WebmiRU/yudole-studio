package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"studio/core/protocol"
	"studio/plugins/chat/goodgame/chat"
)

// GoodGame держит все каналы в одном соединении: подключиться надо один раз,
// а каналы добавляются и убираются прямо на ходу. Поэтому здесь не «соединение
// на канал», а одна сессия: подключились, дождались приветствия, вошли в
// каналы и читаем всё, что в них происходит.
//
//	смена настроек → к новым каналам join, от лишних unjoin
//	обрыв         → сообщаем ядру, что каналы закрыты, и переподключаемся
const (
	// Первая пауза перед переподключением и её потолок.
	reconnectMin = 2 * time.Second
	reconnectMax = 30 * time.Second
)

// event — всё, что плагин хочет сказать ядру: готовое сообщение протокола.
type event struct {
	message protocol.Message
}

// socket держит соединение с GoodGame и переводит его сообщения в протокол
// ядра.
type socket struct {
	events chan event
	logs   chan string

	// changed будит сессию, когда список каналов поменялся.
	changed <-chan struct{}
	// endpoint — адрес чата; в тестах он свой.
	endpoint string
}

func newSocket() *socket {
	endpoint := os.Getenv("GOODGAME_CHAT_URL")
	if endpoint == "" {
		endpoint = chat.Endpoint
	}
	return &socket{
		events:   make(chan event, 64),
		logs:     make(chan string, 16),
		changed:  channelsChanged,
		endpoint: endpoint,
	}
}

// watch keeps a connection to GoodGame alive for as long as the plugin runs.
func (s *socket) watch(ctx context.Context, wanted func() []chat.ID) {
	go func() {
		backoff := reconnectMin
		attempt := 0

		for ctx.Err() == nil {
			_, err := s.session(ctx, wanted)
			attempt++

			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				s.say(err.Error())
			}
			s.send(protocol.KindConnectionState, protocol.ConnectionPayload{
				State:   protocol.ConnectionReconnecting,
				Service: "goodgame",
				Reason:  errText(err),
				Attempt: attempt,
			})

			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > reconnectMax {
				backoff = reconnectMax
			}
		}
	}()
}

// session is one connection from start to finish. It returns the channels we
// got into, so the caller can tell the kernel they are gone.
func (s *socket) session(ctx context.Context, wanted func() []chat.ID) (map[chat.ID]bool, error) {
	s.send(protocol.KindConnectionState, protocol.ConnectionPayload{
		State: protocol.ConnectionConnecting, Service: "goodgame",
	})

	client, err := chat.Open(ctx, s.endpoint)
	if err != nil {
		return nil, fmt.Errorf("GoodGame: %w", err)
	}
	defer func() { _ = client.Close() }()

	s.say("GoodGame: соединение открыто")
	s.send(protocol.KindConnectionState, protocol.ConnectionPayload{State: protocol.ConnectionOnline, Service: "goodgame"})

	live := map[chat.ID]bool{}
	defer func() {
		// Соединение оборвалось: во всех каналах, где мы были, нас больше
		// нет. Ядру нужно это сказать, иначе он будет считать их нашими.
		for _, id := range slices.Sorted(maps.Keys(live)) {
			s.send(protocol.KindChannelUnjoinSuccessful, protocol.ChannelPayload{Channel: string(id)})
		}
	}()

	s.sync(ctx, client, live, wanted)

	for {
		select {
		case <-ctx.Done():
			return live, nil

		case <-s.changed:
			s.sync(ctx, client, live, wanted)

		case message, ok := <-client.Events():
			if !ok {
				return live, chat.ErrClosed
			}
			s.forward(message)
		}
	}
}

// sync brings the channels in line with the settings: joins the new ones, leaves
// the ones that are no longer there, and keeps the rest as they are.
func (s *socket) sync(ctx context.Context, client *chat.Client, live map[chat.ID]bool, wanted func() []chat.ID) {
	list := wanted()
	join, unjoin := chat.Diff(slices.Collect(maps.Keys(live)), list)

	for _, id := range unjoin {
		if err := client.Unjoin(ctx, id); err != nil {
			s.say(fmt.Sprintf("канал %s: %v", id, err))
			s.send(protocol.KindChannelUnjoinFailed, protocol.ChannelPayload{Channel: string(id), Reason: err.Error()})
			continue
		}
		delete(live, id)
		s.send(protocol.KindChannelUnjoinSuccessful, protocol.ChannelPayload{Channel: string(id)})
	}

	for _, id := range join {
		joined, err := client.Join(ctx, id)
		if err != nil {
			s.say(fmt.Sprintf("канал %s: %v", id, err))
			s.send(protocol.KindChannelJoinFailed, protocol.ChannelPayload{Channel: string(id), Reason: err.Error()})
			continue
		}
		live[id] = true
		name := joined.ChannelName
		if name == "" {
			name = string(id)
		}
		s.send(protocol.KindChannelJoinSuccessful, protocol.ChannelPayload{Channel: string(id)})
		s.send(protocol.KindChannelInfo, protocol.ChannelInfoPayload{
			Channel:     string(id),
			Name:        name,
			Streamer:    joined.ChannelKey,
			Clients:     joined.Clients,
			Users:       joined.Users,
			PremiumOnly: joined.PremiumOnly != 0,
			SmileOnly:   joined.SmileOnlyMode,
		})
	}
}

// forward turns what the service said into what the kernel understands.
func (s *socket) forward(message *chat.Message) {
	switch message.Type {
	case chat.TypeMessage:
		var line chat.Line
		if err := message.Into(&line); err != nil {
			s.say("реплика не разобрана: " + err.Error())
			return
		}
		s.send(protocol.KindMessagePublic, protocol.ChatPayload{
			Channel: string(line.Channel),
			User: protocol.ChatUser{
				Name: line.UserName,
				ID:   string(line.UserID),
			},
			Message: protocol.ChatLine{
				Src:  line.Text,
				Text: plainText(line.Text),
			},
		})

	case chat.TypePrivateMessage:
		// Приватные сообщения мы пока не показываем, но разбирать их
		// научились: как только понадобится, дело за отправкой.
		var line chat.PrivateLine
		if err := message.Into(&line); err != nil {
			return
		}
		s.send(protocol.KindMessagePrivate, protocol.ChatPayload{
			Channel: string(line.Channel),
			User: protocol.ChatUser{
				Name: line.UserName,
				ID:   string(line.UserID),
			},
			Message: protocol.ChatLine{
				Src:  line.Text,
				Text: plainText(line.Text),
			},
		})

	case chat.TypeError:
		var failure chat.Error
		if err := message.Into(&failure); err != nil || failure.ErrorMsg == "" {
			return
		}
		s.say("GoodGame: " + failure.Error())

	case chat.TypeRemoveMessage:
		var removed chat.Removed
		if err := message.Into(&removed); err != nil {
			return
		}
		s.send(protocol.KindMessageDeleted, protocol.DeletedMessagePayload{
			Channel:   string(removed.Channel),
			MessageID: strconv.FormatInt(removed.MessageID, 10),
			By:        removed.AdminName,
		})

	// Сервер шлёт счётчики в каждый наш канал каждые секунд двадцать. Сами по
	// себе они сейчас никому не нужны — но разбирать их мы уже умеем, чтобы
	// потом просто включить.
	case chat.TypeChannelCounters:
		var counter chat.Counter
		if err := message.Into(&counter); err != nil {
			return
		}
		_ = counter
	}
}

// send puts a message for the kernel on the way out.
//
// Логи плагина идут в stdout ядра, а не в stderr: stdout занят протоколом, и
// строчка не в том формате сбила бы ядро с толку.
func (s *socket) send(kind string, payload any) {
	message, err := protocol.New(kind, payload)
	if err != nil {
		s.say("не собрать сообщение " + kind + ": " + err.Error())
		return
	}
	select {
	case s.events <- event{message: message}:
	default:
		s.say("очередь переполнена, потеряно сообщение " + kind)
	}
}

// errText is what we tell the kernel about a failure: nothing is fine too.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (s *socket) say(text string) {
	select {
	case s.logs <- text:
	default:
	}
}

// channelID проверяет, что в настройках лежит именно канал GoodGame: у них
// номер, у флудилки на форуме — «r128», у турнира — «cup:…».
func channelID(value string) (chat.ID, error) {
	id := chat.ID(strings.TrimSpace(value))
	if !chat.JoinChannelID(id) {
		return "", fmt.Errorf("%q — это не канал GoodGame", value)
	}
	return id, nil
}

// plainText убирает разметку источника: GoodGame присылает коды смайлов и
// bbcode вперемешку с текстом.
func plainText(src string) string {
	return bbcode.ReplaceAllString(src, "")
}

var bbcode = regexp.MustCompile(`\[/?[a-zA-Z0-9_][^\]]*\]`)

func first(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
