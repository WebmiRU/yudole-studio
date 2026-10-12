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
	{Key:"nick", Type:protocol.OptionText, Label:"Логин Twitch", Section:"Подключение", Placeholder:"your_twitch_login", Hint:"Логин аккаунта Twitch, к которому выпущен токен", Required:true},
	{Key:"token", Type:protocol.OptionPassword, Label:"OAuth-токен", Section:"Подключение", Placeholder:"oauth:...", Hint:"User access token со scope chat:read", Required:true},
	{Key:"channels", Type:protocol.OptionTextMultiple, Label:"Каналы", Section:"Каналы", Placeholder:"имя канала", Hint:"Логины каналов, по одному на строку", Default:[]any{}},
}

type settings struct { Nick, Token string; Channels []string }
var (
	cfgMu sync.RWMutex
	cfg settings
	changed = make(chan struct{}, 1)
	sendMu sync.Mutex
)

func setSettings(s settings) {
	cfgMu.Lock()
	cfg = s
	cfg.Channels = append([]string(nil), s.Channels...)
	cfgMu.Unlock()
	select { case changed <- struct{}{}: default: }
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
		Title:"Twitch", Description:"Twitch chat через IRCv3: tags, commands, membership, смайлы и удаления сообщений.",
		Version:"0.1.0", Options:options,
	})
	commands := make(chan string)
	go readCommands(ctx, commands)
	go supervise(ctx)
	for {
		select {
		case <-ctx.Done(): return
		case line, ok := <-commands:
			if !ok { return }
			if handle(line) { return }
		}
	}
}

func readCommands(ctx context.Context, out chan<- string) {
	defer close(out)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		select { case out <- sc.Text(): case <-ctx.Done(): return }
	}
	if err := sc.Err(); err != nil { logInfo("stdin: " + err.Error()) }
}

func handle(line string) bool {
	m, err := protocol.Decode([]byte(line))
	if err != nil {
		send(protocol.KindError, protocol.ErrorPayload{Message:"Не удалось прочитать команду: " + err.Error()})
		return false
	}
	switch m.Kind {
	case protocol.KindSetOptions:
		var p protocol.SetOptionsPayload
		if err := m.PayloadInto(&p); err != nil {
			send(protocol.KindError, protocol.ErrorPayload{Message:err.Error()})
			return false
		}
		s, fields := parseSettings(p.Values)
		if len(fields) > 0 {
			send(protocol.KindError, protocol.ErrorPayload{Message:"Проверьте настройки Twitch", Fields:fields})
			return false
		}
		setSettings(s)
		send(protocol.KindAck, protocol.AckPayload{Values:map[string]any{"nick":s.Nick, "token":s.Token, "channels":s.Channels}})
	case protocol.KindQuit:
		send(protocol.KindAck, protocol.AckPayload{})
		return true
	default:
		send(protocol.KindError, protocol.ErrorPayload{Message:"Неизвестная команда: " + m.Kind})
	}
	return false
}

func parseSettings(v map[string]any) (settings, map[string]string) {
	s := settings{}
	fields := map[string]string{}
	s.Nick, _ = v["nick"].(string)
	s.Nick = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s.Nick, "@")))
	s.Token, _ = v["token"].(string)
	s.Token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.Token), "oauth:"))
	raw, _ := v["channels"].([]any)
	seen := map[string]bool{}
	for _, item := range raw {
		value := ""
		switch x := item.(type) {
		case string: value = x
		case map[string]any: value, _ = x["value"].(string)
		case protocol.ChoiceValue: value = x.Value
		}
		value = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(value, "#"), "@")))
		if value == "" || seen[value] { continue }
		if !validLogin(value) { fields["channels"] = "Укажите логин канала Twitch, без ссылки и пробелов"; continue }
		seen[value] = true
		s.Channels = append(s.Channels, value)
	}
	if s.Nick != "" && !validLogin(s.Nick) { fields["nick"] = "Некорректный логин Twitch" }
	if s.Token != "" && strings.ContainsAny(s.Token, " \r\n") { fields["token"] = "Токен не должен содержать пробелы" }
	return s, fields
}

func supervise(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil { return }
		s := getSettings()
		if s.Nick == "" || s.Token == "" {
			emitConnection(protocol.ConnectionOffline, "Укажите логин и OAuth-токен Twitch", attempt)
			select { case <-ctx.Done(): return; case <-changed: continue }
		}
		if len(s.Channels) == 0 {
			emitConnection(protocol.ConnectionOffline, "Добавьте хотя бы один канал Twitch", attempt)
			select { case <-ctx.Done(): return; case <-changed: continue }
		}
		state := protocol.ConnectionConnecting
		if attempt > 0 { state = protocol.ConnectionReconnecting }
		emitConnection(state, "", attempt)
		err := runSession(ctx, s)
		if ctx.Err() != nil { return }
		attempt++
		reason := "соединение закрыто"
		if err != nil { reason = err.Error() }
		emitConnection(protocol.ConnectionReconnecting, reason, attempt)
		delay := time.Duration(min(attempt, 10)) * time.Second
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done(): timer.Stop(); return
		case <-changed: timer.Stop(); attempt = 0
		case <-timer.C:
		}
	}
}

var errSettingsChanged = errors.New("настройки Twitch изменились")

func runSession(parent context.Context, s settings) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	d := net.Dialer{Timeout:10*time.Second, KeepAlive:30*time.Second}
	raw, err := d.DialContext(ctx, "tcp", ircAddress)
	if err != nil { return err }
	conn := tls.Client(raw, &tls.Config{ServerName:"irc.chat.twitch.tv", MinVersion:tls.VersionTLS12})
	if err = conn.HandshakeContext(ctx); err != nil { conn.Close(); return err }
	defer conn.Close()
	go func() {
		select {
		case <-ctx.Done(): _ = conn.Close()
		case <-changed:
			cancel()
			_ = conn.Close()
		}
	}()
	reader := textproto.NewReader(bufio.NewReader(conn))
	bw := bufio.NewWriter(conn)
	writer := textproto.NewWriter(bw)
	write := func(command string, args ...string) error {
		if err := writeIRC(writer, command, args...); err != nil { return err }
		return bw.Flush()
	}
	if err = write("CAP", "REQ", "twitch.tv/tags twitch.tv/commands twitch.tv/membership"); err != nil { return err }
	if err = write("PASS", "oauth:"+s.Token); err != nil { return err }
	if err = write("NICK", s.Nick); err != nil { return err }
	joined := false
	for {
		line, err := reader.ReadLine()
		if err != nil {
			if ctx.Err() != nil { return errSettingsChanged }
			return err
		}
		m, err := parseIRC(line)
		if err != nil { logInfo("Некорректная строка IRC: " + err.Error()); continue }
		switch m.Command {
		case "PING":
			if err := write("PONG", m.Trailing); err != nil { return err }
		case "001":
			if !joined {
				for _, ch := range s.Channels {
					if err := write("JOIN", "#"+ch); err != nil { return err }
				}
				joined = true
			}
			emitConnection(protocol.ConnectionOnline, "", 0)
		case "NOTICE":
			lower := strings.ToLower(m.Trailing)
			if m.Tags["msg-id"] == "login_authentication_failed" || strings.Contains(lower, "authentication failed") || strings.Contains(lower, "improperly formatted auth") {
				return errors.New("Twitch отклонил токен или логин; проверьте OAuth-токен со scope chat:read")
			}
			logInfo("Twitch: " + m.Trailing)
		case "RECONNECT":
			return errors.New("Twitch запросил переподключение")
		case "PRIVMSG":
			if len(m.Params) < 1 { continue }
			channel := channelName(m.Params[0])
			login := userLogin(m.Prefix)
			name := m.Tags["display-name"]
			if name == "" { name = login }
			text := m.Trailing
			src := text
			if strings.HasPrefix(text, "ACTION ") && strings.HasSuffix(text, "") {
				text = strings.TrimSuffix(strings.TrimPrefix(text, "ACTION "), "")
			}
			html := renderEmotes(text, m.Tags["emotes"])
			send(protocol.KindMessagePublic, protocol.ChatPayload{
				Channel:channel, User:protocol.ChatUser{Name:name, ID:m.Tags["user-id"], Login:login},
				Message:protocol.ChatLine{Src:src, Text:text, Html:html},
			})
		case "USERNOTICE":
			if len(m.Params) > 0 {
				channel := channelName(m.Params[0])
				login := strings.ToLower(m.Tags["login"])
				name := m.Tags["display-name"]
				if name == "" { name = login }
				text := m.Trailing
				if notice := m.Tags["system-msg"]; notice != "" {
					if text != "" { text = notice + " — " + text } else { text = notice }
				}
				if text != "" {
					send(protocol.KindMessagePublic, protocol.ChatPayload{
						Channel:channel, User:protocol.ChatUser{Name:name, ID:m.Tags["user-id"], Login:login},
						Message:protocol.ChatLine{Src:text, Text:text, Html:renderEmotes(text, m.Tags["emotes"])},
					})
				}
			}
		case "CLEARMSG":
			if len(m.Params) > 0 && m.Tags["target-msg-id"] != "" {
				send(protocol.KindMessageDeleted, protocol.DeletedMessagePayload{Channel:channelName(m.Params[0]), MessageID:m.Tags["target-msg-id"], By:userLogin(m.Prefix)})
			}
		case "JOIN":
			if len(m.Params) > 0 && userLogin(m.Prefix) == strings.ToLower(s.Nick) {
				ch := channelName(m.Params[0])
				send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{Channel:ch, Name:ch})
			}
		case "PART":
			if len(m.Params) > 0 && userLogin(m.Prefix) == strings.ToLower(s.Nick) {
				ch := channelName(m.Params[0])
				send(protocol.KindChannelUnjoinSuccess, protocol.ChannelPayload{Channel:ch, Name:ch})
			}
		}
	}
}

func writeIRC(w *textproto.Writer, command string, args ...string) error {
	line := command
	for i, arg := range args {
		if strings.ContainsAny(arg, "\r\n") { return fmt.Errorf("invalid IRC argument") }
		if i == len(args)-1 && strings.Contains(arg, " ") { line += " :" + arg; break }
		line += " " + arg
	}
	return w.PrintfLine("%s", line)
}
func emitConnection(state, reason string, attempt int) {
	send(protocol.KindConnectionState, protocol.ConnectionPayload{State:state, Service:"Twitch", Reason:reason, Attempt:attempt})
}
func logInfo(s string) { send(protocol.KindLog, protocol.LogPayload{Level:"info", Message:s}) }
func send(kind string, payload any) {
	sendMu.Lock()
	defer sendMu.Unlock()
	line, err := protocol.Encode(protocol.MustNew(kind, payload))
	if err != nil { fmt.Fprintln(os.Stderr, "twitch:", err); return }
	_, _ = os.Stdout.Write(line)
}
