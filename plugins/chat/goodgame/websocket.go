package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
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

// Как часто переспрашиваем каналы, в которые не удалось войти: не сразу,
// чтобы не ловить отказ GoodGame на каждый чих. Переменные, а не константы:
// в тесте ждать полчаса ради проверки никто не подписывался.
var (
	retryMin = 30 * time.Second
	retryMax = 5 * time.Minute
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

	// book — смайлы, чтобы подставлять картинки в реплики. Книгу обновляет
	// человек кнопкой, поэтому читаем её на каждую реплику заново и берём
	// под замком: гоняться за указателем в конкурентном коде нечего.
	book *smileBook
}

func newSocket(book *smileBook) *socket {
	endpoint := os.Getenv("GOODGAME_CHAT_URL")
	if endpoint == "" {
		endpoint = chat.Endpoint
	}
	return &socket{
		events:   make(chan event, 64),
		logs:     make(chan string, 16),
		changed:  channelsChanged,
		endpoint: endpoint,
		book:     book,
	}
}

// watch keeps a connection to GoodGame alive for as long as the plugin runs.
func (s *socket) watch(ctx context.Context, wanted func() []protocol.ChoiceValue) {
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
				Service: serviceName(),
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
func (s *socket) session(ctx context.Context, wanted func() []protocol.ChoiceValue) (map[chat.ID]string, error) {
	s.send(protocol.KindConnectionState, protocol.ConnectionPayload{
		State: protocol.ConnectionConnecting, Service: serviceName(),
	})

	client, err := chat.Open(ctx, s.endpoint)
	if err != nil {
		return nil, fmt.Errorf("GoodGame: %w", err)
	}
	defer func() { _ = client.Close() }()

	s.say("GoodGame: соединение открыто")
	s.send(protocol.KindConnectionState, protocol.ConnectionPayload{
		State: protocol.ConnectionOnline, Service: serviceName(),
	})

	// live — каналы, в которых мы сейчас, и имя каждого. Имя запоминается при
	// входе: как только канал убрали из настроек, искать его имя больше негде,
	// а выйти из него всё равно надо.
	live := map[chat.ID]string{}
	defer func() {
		// Соединение оборвалось: во всех каналах, где мы были, нас больше
		// нет. Ядру нужно это сказать, иначе он будет считать их нашими.
		for _, id := range slices.Sorted(maps.Keys(live)) {
			s.send(protocol.KindChannelUnjoinSuccess, protocol.ChannelPayload{
				Channel: string(id), Name: live[id],
			})
		}
	}()

	// Неудачные попытки помним с отсрочкой: иначе канал, в который не пускают,
	// молчал бы до следующей смены настроек или переподключения.
	failed := map[chat.ID]time.Time{}
	backoff := retryMin

	// Сигнал мог остаться от настроек, пришедших до открытия соединения. Он уже
	// учтён в sync ниже, и второй раз сработать не должен: иначе сессия вошла бы
	// в каналы дважды подряд и стёрла бы отсрочки неудачных попыток.
	select {
	case <-s.changed:
	default:
	}

	s.sync(ctx, client, live, failed, wanted)

	retry := time.NewTicker(retryMin)
	defer retry.Stop()

	for {
		select {
		case <-ctx.Done():
			return live, nil

		case <-s.changed:
			// Человек что-то поменял: про отсрочки забываем, ведь он и ждал.
			clear(failed)
			backoff = retryMin
			s.sync(ctx, client, live, failed, wanted)

		case <-retry.C:
			if len(failed) > 0 {
				backoff = nextBackoff(backoff)
				s.say(fmt.Sprintf("пробуем снова через %s: %s", shortTime(backoff), listChannels(failed)))
				s.sync(ctx, client, live, failed, wanted)
			}

		case message, ok := <-client.Events():
			if !ok {
				return live, chat.ErrClosed
			}
			s.forward(message)
		}
	}
}

// nextBackoff удваивает паузу, но не выше потолка.
func nextBackoff(current time.Duration) time.Duration {
	if current *= 2; current > retryMax {
		return retryMax
	}
	return current
}

// shortTime пишет паузу словами: «1м30с» читается быстрее «90s».
func shortTime(d time.Duration) string {
	if d >= time.Minute {
		return d.Round(time.Second).String()
	}
	return d.Round(time.Second).String()
}

func listChannels(failed map[chat.ID]time.Time) string {
	ids := slices.Sorted(maps.Keys(failed))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, string(id))
	}
	return strings.Join(names, ", ")
}

// sync brings the channels in line with the settings: joins the new ones, leaves
// the ones that are no longer there, and keeps the rest as they are.
func (s *socket) sync(ctx context.Context, client *chat.Client, live map[chat.ID]string, failed map[chat.ID]time.Time, wanted func() []protocol.ChoiceValue) {
	// Список с именами снимаем один раз, на всю синхронизацию. Если читать
	// настройки по ходу, то канал, убранный человеком прямо в этот момент, к
	// своему выходу останется без имени.
	list := wanted()
	names := make(map[chat.ID]string, len(list))
	ids := make([]chat.ID, 0, len(list))
	for _, channel := range list {
		id := chat.ID(channel.Value)
		names[id] = channel.Label
		ids = append(ids, id)
	}

	join, unjoin := chat.Diff(slices.Collect(maps.Keys(live)), ids)

	// Канал, которому недавно отказали, трогать рано: он либо ещё закрыт, либо
	// GoodGame просто не отвечает — и то и другое пройдёт само за паузу.
	ready := func(ids []chat.ID) []chat.ID {
		out := make([]chat.ID, 0, len(ids))
		for _, id := range ids {
			if until, bad := failed[id]; bad && time.Now().Before(until) {
				continue
			}
			out = append(out, id)
		}
		return out
	}

	// Выход засчитывается по отправке: подтверждения сервер не даёт. Не
	// отправилось — значит, мы в канале остались, и это уже неудача.
	for _, id := range ready(unjoin) {
		// Имя берём из того, что запомнили при входе: в списке этого раза
		// канала уже нет, а назвать его в событии надо.
		name := live[id]

		if err := client.Unjoin(ctx, id); err != nil {
			s.say(fmt.Sprintf("канал %s: %v", id, err))
			s.send(protocol.KindChannelUnjoinFailed, protocol.ChannelPayload{
				Channel: string(id), Name: name, Reason: err.Error(),
			})
			continue
		}
		delete(live, id)
		delete(failed, id)
		s.send(protocol.KindChannelUnjoinSuccess, protocol.ChannelPayload{
			Channel: string(id), Name: name,
		})
	}

	for _, id := range ready(join) {
		joined, err := client.Join(ctx, id)
		if err != nil {
			s.say(fmt.Sprintf("канал %s: %v", id, err))
			s.send(protocol.KindChannelJoinFailed, protocol.ChannelPayload{
				Channel: string(id), Name: names[id], Reason: err.Error(),
			})
			// Запомним, когда пробовать в следующий раз, чтобы сессия сама
			// вернулась к этому каналу, а не ждала новой смены настроек.
			failed[id] = time.Now().Add(retryMin)
			continue
		}
		name := names[id]
		live[id] = name
		delete(failed, id)

		s.send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{
			Channel: string(id), Name: name,
		})

		// Отдельно говорим только о том, чего вход не сказал. Название канала
		// уже названо выше, и если название трансляции совпадает с ним —
		// повторять нечего. Счётчики придут сами из update_channel_info.
		info := protocol.ChannelInfoPayload{
			Channel: string(id),
			Name:    joined.ChannelName,
			// ChannelKey — ник стримера, поэтому идёт и в имя, и в логин:
			// раньше здесь было отдельное поле Streamer.
			User:        &protocol.ChatUser{Name: joined.ChannelKey, Login: joined.ChannelKey},
			Clients:     joined.Clients,
			Users:       joined.Users,
			PremiumOnly: joined.PremiumOnly != 0,
			SmileOnly:   joined.SmileOnlyMode,
		}
		if info.Name == name {
			info.Name = ""
		}
		if info.Name != "" || info.PremiumOnly || info.SmileOnly {
			s.send(protocol.KindChannelInfo, info)
		}
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
			Message: s.line(line.Text),
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
			Message: s.line(line.Text),
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
			MessageID: string(removed.MessageID),
			// Раньше moderator сюда клался в поле By, теперь это тот же
			// ChatUser, что и у остальных событий.
			User: &protocol.ChatUser{Name: removed.AdminName},
		})

	case chat.TypeUnjoin:
		// Ответ на наш выход. Номера канала в нём нет, так что отнести его к
		// конкретному запросу нельзя: выход уже засчитан по отправке, и это
		// сообщение ничего не меняет.
		s.say("GoodGame: подтвердил выход из канала")

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

// serviceName is how we call ourselves in our own events. Не id: id — это имя
// каталога, а человек знает плагин как GoodGame.
func serviceName() string {
	return describe().Title
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

// line turns what came from the service into what the page shows.
//
// Разметку вырезаем один раз: и для текста, и для картинок. Если идти по
// источнику дважды, регулярка сработает дважды, а второй раз она нужна только
// когда в строке есть смайлы.
func (s *socket) line(src string) protocol.ChatLine {
	clean := plainText(src)
	text, html := s.book.get().Views(clean)

	return protocol.ChatLine{
		// Src — как пришло, и всегда как пришло. Ни экранирования, ни
		// вырезания: кто-то захочет увидеть именно то, что прислал GoodGame.
		Src: src,
		// Text — только текст: без кодов смайлов и без bbcode.
		Text: text,
		// Html — то же, но с картинками вместо смайлов. Пустой, если смайлов не
		// было: страница покажет Text, и пустая строка скажет ей об этом раньше,
		// чем разбор html.
		Html: html,
	}
}

// plainText убирает разметку источника: GoodGame присылает коды смайлов и
// bbcode вперемешку с текстом.
func plainText(src string) string {
	// Чаще всего в строке нет ни одного квадратного скобочного тега, и искать
	// регуляркой значит гонять машину по целой строке впустую.
	if strings.IndexByte(src, '[') < 0 {
		return src
	}
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
