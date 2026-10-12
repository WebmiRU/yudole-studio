package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"studio/core/protocol"
)

const ircAddress = "irc.chat.twitch.tv:6697"

var options = []protocol.Option{
	{
		Key:         "nick",
		Type:        protocol.OptionText,
		Label:       "Логин Twitch",
		Section:     "Подключение",
		Placeholder: "your_twitch_login",
		Hint:        "Логин аккаунта Twitch, к которому выпущен токен",
		Required:    true,
	},
	{
		Key:         "token",
		Type:        protocol.OptionPassword,
		Label:       "OAuth-токен",
		Section:     "Подключение",
		Placeholder: "oauth:...",
		Hint:        "User access token со scope chat:read",
		Required:    true,
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
		if current.Nick == "" || current.Token == "" {
			emitConnection(protocol.ConnectionOffline, "Укажите логин и OAuth-токен Twitch", attempt)
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		if len(current.Channels) == 0 {
			emitConnection(protocol.ConnectionOffline, "Добавьте хотя бы один канал Twitch", attempt)
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}

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

func runSession(parent context.Context, current settings) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Settings updates interrupt both the dial/handshake and a blocked IRC read.
	go func() {
		select {
		case <-ctx.Done():
		case <-changed:
			cancel()
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
	if err := write("PASS", "oauth:"+current.Token); err != nil {
		return err
	}
	if err := write("NICK", current.Nick); err != nil {
		return err
	}

	joined := false
	for {
		line, err := reader.ReadLine()
		if err != nil {
			if ctx.Err() != nil && parent.Err() == nil {
				return errSettingsChanged
			}
			return err
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
			if !joined {
				for _, channel := range current.Channels {
					if err := write("JOIN", "#"+channel); err != nil {
						return err
					}
				}
				joined = true
			}
			emitConnection(protocol.ConnectionOnline, "", 0)

		case "NOTICE":
			lower := strings.ToLower(message.Trailing)
			if message.Tags["msg-id"] == "login_authentication_failed" ||
				strings.Contains(lower, "authentication failed") ||
				strings.Contains(lower, "improperly formatted auth") {
				return errors.New("Twitch отклонил токен или логин; проверьте OAuth-токен со scope chat:read")
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

			text := message.Trailing
			if notice := message.Tags["system-msg"]; notice != "" {
				if text != "" {
					text = notice + " — " + text
				} else {
					text = notice
				}
			}
			if text == "" {
				continue
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
					Html: renderEmotes(text, message.Tags["emotes"]),
				},
			})

		case "CLEARMSG":
			if len(message.Params) > 0 && message.Tags["target-msg-id"] != "" {
				send(protocol.KindMessageDeleted, protocol.DeletedMessagePayload{
					Channel:   channelName(message.Params[0]),
					MessageID: message.Tags["target-msg-id"],
					By:        userLogin(message.Prefix),
				})
			}

		case "JOIN":
			if len(message.Params) > 0 && userLogin(message.Prefix) == current.Nick {
				channel := channelName(message.Params[0])
				send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{
					Channel: channel,
					Name:    channel,
				})
			}

		case "PART":
			if len(message.Params) > 0 && userLogin(message.Prefix) == current.Nick {
				channel := channelName(message.Params[0])
				send(protocol.KindChannelUnjoinSuccess, protocol.ChannelPayload{
					Channel: channel,
					Name:    channel,
				})
			}
		}
	}
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

	line, err := protocol.Encode(protocol.MustNew(kind, payload))
	if err != nil {
		fmt.Fprintln(os.Stderr, "twitch:", err)
		return
	}
	_, _ = os.Stdout.Write(line)
}
