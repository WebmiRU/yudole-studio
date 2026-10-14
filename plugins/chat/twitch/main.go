package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html"
	"maps"
	"math/rand/v2"
	"net"
	"net/textproto"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"studio/core/protocol"
)

const ircAddress = "irc.chat.twitch.tv:6697"

// readSilence — сколько молчания считаем обрывом. Twitch спрашивает PING
// каждые пять минут, поэтому шесть минут тишины — это точно не «чат молчит».
const readSilence = 6 * time.Minute

var options = []protocol.Option{
	{
		Key:         "nick",
		Type:        protocol.OptionText,
		Label:       "Логин Twitch",
		Section:     "Подключение",
		Placeholder: "оставьте пустым",
		Hint:        "Необязательно. Без него подключаемся анонимно, как гость",
	},
	{
		Key:         "token",
		Type:        protocol.OptionPassword,
		Label:       "OAuth-токен",
		Section:     "Подключение",
		Placeholder: "оставьте пустым",
		Hint:        "Необязательно. Нужен для отправки сообщений, а не для чтения",
	},
	{
		Key:         "channels",
		Type:        protocol.OptionTextMultiple,
		Label:       "Каналы",
		Section:     "Каналы",
		Placeholder: "имя канала",
		Hint:        "Логины каналов, по одному на строку",
		Default:     []any{},
	},
}

type settings struct {
	Nick     string
	Token    string
	Channels []string
}

var (
	cfgMu   sync.RWMutex
	cfg     settings
	changed = make(chan struct{}, 1)
	sendMu  sync.Mutex
)

func setSettings(s settings) {
	cfgMu.Lock()
	cfg = s
	cfg.Channels = append([]string(nil), s.Channels...)
	cfgMu.Unlock()

	select {
	case changed <- struct{}{}:
	default:
	}
}

func getSettings() settings {
	cfgMu.RLock()
	defer cfgMu.RUnlock()

	s := cfg
	s.Channels = append([]string(nil), cfg.Channels...)
	return s
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	send(protocol.KindPluginChat, protocol.InfoPayload{
		Title:       "Twitch",
		Description: "Twitch chat через IRCv3: tags, commands, membership, смайлы и удаления сообщений.",
		Version:     "0.1.0",
		Options:     options,
	})

	commands := make(chan string)
	go readCommands(ctx, commands)
	go supervise(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case line, ok := <-commands:
			if !ok {
				return
			}
			if handle(line) {
				return
			}
		}
	}
}

func readCommands(ctx context.Context, out chan<- string) {
	defer close(out)

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		select {
		case out <- scanner.Text():
		case <-ctx.Done():
			return
		}
	}
	if err := scanner.Err(); err != nil {
		logInfo("stdin: " + err.Error())
	}
}

func handle(line string) bool {
	message, err := protocol.Decode([]byte(line))
	if err != nil {
		send(protocol.KindError, protocol.ErrorPayload{
			Message: "Не удалось прочитать команду: " + err.Error(),
		})
		return false
	}

	switch message.Kind {
	case protocol.KindSetOptions:
		var payload protocol.SetOptionsPayload
		if err := message.PayloadInto(&payload); err != nil {
			send(protocol.KindError, protocol.ErrorPayload{Message: err.Error()})
			return false
		}

		next, fields := parseSettings(payload.Values)
		if len(fields) > 0 {
			send(protocol.KindError, protocol.ErrorPayload{
				Message: "Проверьте настройки Twitch",
				Fields:  fields,
			})
			return false
		}

		setSettings(next)
		send(protocol.KindAck, protocol.AckPayload{
			Values: map[string]any{
				"nick":     next.Nick,
				"token":    next.Token,
				"channels": next.Channels,
			},
		})

	case protocol.KindQuit:
		send(protocol.KindAck, protocol.AckPayload{})
		return true

	default:
		send(protocol.KindError, protocol.ErrorPayload{
			Message: "Неизвестная команда: " + message.Kind,
		})
	}

	return false
}

func parseSettings(values map[string]any) (settings, map[string]string) {
	var result settings
	fields := make(map[string]string)

	result.Nick, _ = values["nick"].(string)
	result.Nick = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(result.Nick, "@")))

	result.Token, _ = values["token"].(string)
	result.Token = strings.TrimSpace(result.Token)
	result.Token = strings.TrimPrefix(result.Token, "oauth:")

	rawChannels, _ := values["channels"].([]any)
	seen := make(map[string]bool)
	for _, item := range rawChannels {
		value := ""
		switch entry := item.(type) {
		case string:
			value = entry
		case map[string]any:
			value, _ = entry["value"].(string)
		case protocol.ChoiceValue:
			value = entry.Value
		}

		value = strings.ToLower(strings.TrimSpace(value))
		value = strings.TrimPrefix(strings.TrimPrefix(value, "#"), "@")
		if value == "" || seen[value] {
			continue
		}
		if !validLogin(value) {
			fields["channels"] = "Укажите логин канала Twitch, без ссылки и пробелов"
			continue
		}

		seen[value] = true
		result.Channels = append(result.Channels, value)
	}

	if result.Nick != "" && !validLogin(result.Nick) {
		fields["nick"] = "Некорректный логин Twitch"
	}
	if result.Token != "" && strings.ContainsAny(result.Token, " \r\n") {
		fields["token"] = "Токен не должен содержать пробелы"
	}

	return result, fields
}

func supervise(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		current := getSettings()

		// Каналов может не быть, и это не беда: подключаемся всё равно и висим,
		// отвечая на пинги. Канал, добавленный позже, встанет на этом же
		// соединении, и страница сразу станет «в сети» — а не «ждёт каналы».
		state := protocol.ConnectionConnecting
		if attempt > 0 {
			state = protocol.ConnectionReconnecting
		}
		emitConnection(state, "", attempt)

		err := runSession(ctx, current)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errSettingsChanged) {
			select {
			case <-changed:
			default:
			}
			attempt = 0
			continue
		}

		attempt++
		reason := "соединение закрыто"
		if err != nil {
			reason = err.Error()
		}
		emitConnection(protocol.ConnectionReconnecting, reason, attempt)

		delay := time.Duration(min(attempt, 10)) * time.Second
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			timer.Stop()
			attempt = 0
		case <-timer.C:
		}
	}
}

var errSettingsChanged = errors.New("настройки Twitch изменились")

// guestName — под каким логином мы входим, если человек ничего не вписал.
// Имена вида justinfanNNNN сервер принимает без пароля и без токена, ровно так
// же, как принимает браузер гостя: читать можно, писать нельзя.
func guestName(configured string) string {
	if configured != "" {
		return configured
	}
	return fmt.Sprintf("justinfan%d", rand.IntN(9000)+1000)
}

func runSession(parent context.Context, current settings) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Settings updates interrupt both the dial/handshake and a blocked IRC read.
	// Смена настроек обычно означает другое: человек добавил или убрал канал.
	// Переподключаться ради этого незачем — IRC умеет входить и выходить из
	// канала на живом соединении. Рвём сессию только когда сменился логин или
	// токен: это наше имя на сервере, и сменить его можно лишь заново.
	//
	// resync несёт новый список каналов. Буфер на один: важно, что список
	// посмотрели свежим, а не сколько раз он менялся.
	resync := make(chan []string, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				next := getSettings()
				if next.Nick != current.Nick || next.Token != current.Token {
					cancel()
					return
				}
				current.Channels = next.Channels
				select {
				case resync <- next.Channels:
				default:
				}
			}
		}
	}()

	dialer := net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	raw, err := dialer.DialContext(ctx, "tcp", ircAddress)
	if err != nil {
		if parent.Err() == nil && ctx.Err() != nil {
			return errSettingsChanged
		}
		return err
	}

	conn := tls.Client(raw, &tls.Config{
		ServerName: "irc.chat.twitch.tv",
		MinVersion: tls.VersionTLS12,
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		if parent.Err() == nil && ctx.Err() != nil {
			return errSettingsChanged
		}
		return err
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	reader := textproto.NewReader(bufio.NewReader(conn))
	bufferedWriter := bufio.NewWriter(conn)
	writer := textproto.NewWriter(bufferedWriter)
	write := func(command string, args ...string) error {
		if err := writeIRC(writer, command, args...); err != nil {
			return err
		}
		return bufferedWriter.Flush()
	}

	if err := write("CAP", "REQ", "twitch.tv/tags twitch.tv/commands twitch.tv/membership"); err != nil {
		return err
	}
	// Токен необязателен: чтение чата Twitch отдаёт и гостю, достаточно
	// придумать логин. Пустой PASS — это ошибка авторизации, поэтому не
	// отправляем его вовсе.
	if current.Token != "" {
		if err := write("PASS", "oauth:"+current.Token); err != nil {
			return err
		}
	}
	// Имя, под которым нас на самом деле приняли: вписать его могли и не
	// могли, но сравнивать JOIN и PART надо с тем, что сервер знает.
	nick := guestName(current.Nick)
	if err := write("NICK", nick); err != nil {
		return err
	}

	// Чтение живёт своей горутиной: пока она висит в ReadLine, настройки могут
	// поменяться, а ждать следующей строки от Twitch ради этого нельзя.
	//
	// Дедлайн на чтение обновляется в этой горутине: пока строки идут, молчание
	// не накапливается, а перестало идти — сработает и мы узнаем об обрыве.
	// Без него ReadLine ждал бы вечно: соединение снаружи выглядело бы живым,
	// плагин перестал бы читать чат, и никто об этом не узнал бы. Так молча
	// завис GoodGame, пока его поток событий не начали закрывать при обрыве.
	lines := make(chan string, 256)
	readErr := make(chan error, 1)
	go func() {
		defer close(lines)
		for {
			if err := conn.SetReadDeadline(time.Now().Add(readSilence)); err != nil {
				readErr <- err
				return
			}
			line, err := reader.ReadLine()
			if err != nil {
				if ctx.Err() == nil {
					readErr <- err
				}
				return
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()

	// joined — где мы сейчас состоим в каналах. Списка настроек для этого мало:
	// он меняется, а состояние приходит от самого сервера, и по нему видно,
	// в каком канале нас на самом деле приняли.
	joined := map[string]bool{}
	var resyncErr error

	for {
		var line string

		select {
		case <-ctx.Done():
			return errSettingsChanged
		case next, ok := <-resync:
			if !ok {
				continue
			}
			if resyncErr = joinDiff(write, joined, next); resyncErr != nil {
				return resyncErr
			}
			continue
		case err := <-readErr:
			if ctx.Err() != nil && parent.Err() == nil {
				return errSettingsChanged
			}
			return err
		case line = <-lines:
		}

		message, err := parseIRC(line)
		if err != nil {
			logInfo("Некорректная строка IRC: " + err.Error())
			continue
		}

		switch message.Command {
		case "PING":
			if err := write("PONG", message.Trailing); err != nil {
				return err
			}

		case "CAP":
			if len(message.Params) >= 2 && message.Params[1] == "NAK" {
				return errors.New("Twitch не принял запрошенные IRC capabilities")
			}

		case "001":
			if err := joinDiff(write, joined, current.Channels); err != nil {
				return err
			}
			emitConnection(protocol.ConnectionOnline, "", 0)

		case "NOTICE":
			lower := strings.ToLower(message.Trailing)
			if message.Tags["msg-id"] == "login_authentication_failed" ||
				strings.Contains(lower, "authentication failed") ||
				strings.Contains(lower, "improperly formatted auth") {
				return errors.New("Twitch отклонил токен или логин; проверьте OAuth-токен со scope chat:read")
			}
			if len(message.Params) > 0 && strings.HasPrefix(message.Params[0], "#") {
				switch message.Tags["msg-id"] {
				case "msg_channel_suspended", "msg_channel_not_found":
					channel := channelName(message.Params[0])
					send(protocol.KindChannelJoinFailed, protocol.ChannelPayload{
						Channel: channel,
						Name:    channel,
						Reason:  message.Trailing,
					})
				}
			}
			logInfo("Twitch: " + message.Trailing)

		case "RECONNECT":
			return errors.New("Twitch запросил переподключение")

		case "PRIVMSG":
			if len(message.Params) == 0 {
				continue
			}
			channel := channelName(message.Params[0])
			login := userLogin(message.Prefix)
			name := message.Tags["display-name"]
			if name == "" {
				name = login
			}

			text := message.Trailing
			source := text
			if strings.HasPrefix(text, "\x01ACTION ") && strings.HasSuffix(text, "\x01") {
				text = strings.TrimSuffix(strings.TrimPrefix(text, "\x01ACTION "), "\x01")
			}

			send(protocol.KindMessagePublic, protocol.ChatPayload{
				Channel: channel,
				User: protocol.ChatUser{
					Name:  name,
					ID:    message.Tags["user-id"],
					Login: login,
				},
				Message: protocol.ChatLine{
					Src:  source,
					Text: text,
					Html: renderEmotes(text, message.Tags["emotes"]),
				},
			})

		case "USERNOTICE":
			if len(message.Params) == 0 {
				continue
			}
			channel := channelName(message.Params[0])
			login := strings.ToLower(message.Tags["login"])
			name := message.Tags["display-name"]
			if name == "" {
				name = login
			}

			body := message.Trailing
			notice := message.Tags["system-msg"]
			text := body
			if notice != "" {
				if body != "" {
					text = notice + " — " + body
				} else {
					text = notice
				}
			}
			if text == "" {
				continue
			}

			rendered := ""
			if body != "" {
				bodyHTML := renderEmotes(body, message.Tags["emotes"])
				if bodyHTML != "" {
					if notice != "" {
						rendered = html.EscapeString(notice) + " — " + bodyHTML
					} else {
						rendered = bodyHTML
					}
				}
			}
			send(protocol.KindMessagePublic, protocol.ChatPayload{
				Channel: channel,
				User: protocol.ChatUser{
					Name:  name,
					ID:    message.Tags["user-id"],
					Login: login,
				},
				Message: protocol.ChatLine{
					Src:  text,
					Text: text,
					Html: rendered,
				},
				// Подписка или рейд — это писал Twitch, а не человек. В ленте
				// оно стоит среди реплик, как у самой Twitch, но помечено.
				Notice: true,
			})

		case "CLEARMSG":
			if len(message.Params) > 0 && message.Tags["target-msg-id"] != "" {
				send(protocol.KindMessageDeleted, protocol.DeletedMessagePayload{
					Channel:   channelName(message.Params[0]),
					MessageID: message.Tags["target-msg-id"],
					By:        userLogin(message.Prefix),
				})
			}

		case "CLEARCHAT":
			if len(message.Params) == 0 {
				continue
			}
			channel := channelName(message.Params[0])
			switch {
			case message.Trailing == "":
				logInfo("Twitch очистил чат канала #" + channel)
			case message.Tags["ban-duration"] != "":
				logInfo("Twitch выдал тайм-аут в канале #" + channel + ": " + message.Trailing)
			default:
				logInfo("Twitch заблокировал пользователя в канале #" + channel + ": " + message.Trailing)
			}

		case "JOIN":
			if len(message.Params) > 0 && userLogin(message.Prefix) == nick {
				channel := channelName(message.Params[0])
				joined[channel] = true
				send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{
					Channel: channel,
					Name:    channel,
				})
			}

		case "PART":
			if len(message.Params) > 0 && userLogin(message.Prefix) == nick {
				channel := channelName(message.Params[0])
				delete(joined, channel)
				send(protocol.KindChannelUnjoinSuccess, protocol.ChannelPayload{
					Channel: channel,
					Name:    channel,
				})
			}
		}
	}
}

// joinDiff brings the channels in line with the settings: JOIN the new ones,
// PART the ones that are no longer there, keep the rest.
//
// Соединение при этом остаётся прежним.
//
// Карта здесь означает «мы послали JOIN или PART», а не «сервер подтвердил».
// Разница видна на старте: настройки меняются ещё до подключения, и первый же
// joinDiff успевает отправить JOIN, а подтверждение придёт через мгновение —
// второй joinDiff, случившийся в промежутке, не отправил бы ничего зря. Если
// сервер канал не принял, придёт NOTICE, и канала в карте не будет: следующая
// правка настроек попробует снова.
func joinDiff(write func(command string, args ...string) error, joined map[string]bool, wanted []string) error {
	want := make(map[string]bool, len(wanted))
	for _, channel := range wanted {
		want[channel] = true
	}

	// Сначала выходим: так список каналов придёт в порядке настроек, а не в
	// порядке истории подключений.
	for _, channel := range slices.Sorted(maps.Keys(joined)) {
		if want[channel] {
			continue
		}
		if err := write("PART", "#"+channel); err != nil {
			return err
		}
		delete(joined, channel)
	}

	for _, channel := range wanted {
		if joined[channel] {
			continue
		}
		if err := write("JOIN", "#"+channel); err != nil {
			return err
		}
		joined[channel] = true
	}
	return nil
}

func writeIRC(writer *textproto.Writer, command string, args ...string) error {
	line := command
	for i, arg := range args {
		if strings.ContainsAny(arg, "\r\n") {
			return fmt.Errorf("invalid IRC argument")
		}
		if i == len(args)-1 && strings.Contains(arg, " ") {
			line += " :" + arg
			break
		}
		line += " " + arg
	}
	return writer.PrintfLine("%s", line)
}

func emitConnection(state, reason string, attempt int) {
	send(protocol.KindConnectionState, protocol.ConnectionPayload{
		State:   state,
		Service: "Twitch",
		Reason:  reason,
		Attempt: attempt,
	})
}

func logInfo(text string) {
	send(protocol.KindLog, protocol.LogPayload{
		Level:   "info",
		Message: text,
	})
}

func send(kind string, payload any) {
	sendMu.Lock()
	defer sendMu.Unlock()

	message, err := protocol.New(kind, payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "twitch: invalid %s message: %v\n", kind, err)
		return
	}
	line, err := protocol.Encode(message)
	if err != nil {
		fmt.Fprintln(os.Stderr, "twitch:", err)
		return
	}
	_, _ = os.Stdout.Write(line)
}
