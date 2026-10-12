package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"studio/core/protocol"
)

// GoodGame отдаёт события чата по websocket: на каждый канал (у GoodGame они
// называются комнатами) своё соединение. Здесь только транспорт — разбор
// сообщений в parseChat.
//
//	сообщения в комнате → сообщения в stdout
//	обрыв               → переподключение с задержкой
const (
	// wsURL — адрес чата GoodGame, %s подставляется в номер комнаты.
	wsURL = "wss://goodgame.ru/websocket/%s"

	// Первая пауза перед переподключением и её потолок.
	reconnectMin  = 2 * time.Second
	reconnectMax  = 30 * time.Second
	dialTimeout   = 15 * time.Second
	readLimit     = 1 << 20 // 1 MiB на одно сообщение
	shutdownGrace = 3 * time.Second
)

// event — то, что пришло из чата.
type event struct {
	channel string
	raw     []byte
}

// socket держит соединения с каналами и отдаёт их сообщения в одну очередь.
type socket struct {
	events chan event
	logs   chan string
}

func newSocket() *socket {
	return &socket{
		events: make(chan event, 64),
		logs:   make(chan string, 16),
	}
}

// watch переподключает каналы из списка и кладёт всё приходящее в очередь.
// Список читается при каждом переподключении, чтобы смена опций применялась
// без перезапуска.
func (s *socket) watch(ctx context.Context, channels func() []string) {
	go func() {
		// По одному соединению на канал: переподключаем упавшее, новые
		// каналы подключаем, исчезнувшие закрываем.
		type channel struct {
			cancel context.CancelFunc
		}
		live := map[string]*channel{}

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			wanted := channels()
			keep := map[string]bool{}
			for _, id := range wanted {
				keep[id] = true
			}

			for id, r := range live {
				if !keep[id] {
					r.cancel()
					delete(live, id)
				}
			}

			for _, id := range wanted {
				if _, ok := live[id]; ok {
					continue
				}
				channelCtx, cancel := context.WithCancel(ctx)
				live[id] = &channel{cancel: cancel}
				go s.connect(channelCtx, id)
			}

			select {
			case <-ctx.Done():
				for _, r := range live {
					r.cancel()
				}
				return
			case <-ticker.C:
			}
		}
	}()
}

// connect держит одно соединение с каналом, пока не придёт отмена.
func (s *socket) connect(ctx context.Context, id string) {
	backoff := reconnectMin

	for ctx.Err() == nil {
		if err := s.session(ctx, id); err != nil && !errors.Is(err, context.Canceled) {
			s.say("канал " + id + ": " + err.Error())
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > reconnectMax {
			backoff = reconnectMax
		}
	}
}

// session — одна попытка: подключиться и читать, пока соединение живо.
func (s *socket) session(ctx context.Context, id string) error {
	target := wsEndpoint(id)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, target, nil)
	if err != nil {
		return fmt.Errorf("не подключиться к %s: %w", target, err)
	}
	defer conn.CloseNow()

	// Читаем в отдельной горутине: отмена контекста должна разрывать и чтение.
	readCtx, stopRead := context.WithCancel(ctx)
	defer stopRead()

	go func() {
		<-readCtx.Done()
		// CloseNow разрывает блокирующее Read, иначе goroutine зависнет.
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	s.say("канал " + id + ": подключено")
	defer s.say("канал " + id + ": отключено")

	conn.SetReadLimit(readLimit)

	for {
		kind, data, err := conn.Read(readCtx)
		if err != nil {
			return err
		}
		if kind != websocket.MessageText {
			continue
		}

		select {
		case s.events <- event{channel: id, raw: data}:
		case <-readCtx.Done():
			return readCtx.Err()
		}
	}
}

func (s *socket) say(text string) {
	select {
	case s.logs <- text:
	default:
	}
}

// wsEndpoint собирает адрес канала: у GoodGame у канала есть числовой номер.
// Адрес можно переопределить переменной окружения — в тестах нужен свой.
func wsEndpoint(channel string) string {
	template := os.Getenv("GOODGAME_WS_URL")
	if template == "" {
		template = wsURL
	}
	if strings.Contains(template, "%s") {
		return fmt.Sprintf(template, url.PathEscape(channel))
	}
	return strings.TrimRight(template, "/") + "/" + url.PathEscape(channel)
}

// channelID проверяет, что в настройках лежит именно номер канала: GoodGame
// адресует комнаты числами, а не именами.
func channelID(value string) (string, error) {
	id := strings.TrimSpace(value)
	if id == "" {
		return "", errors.New("пустое значение")
	}
	if _, err := strconv.Atoi(id); err != nil {
		return "", fmt.Errorf("%q — это не номер канала (у GoodGame он числовой)", value)
	}
	return id, nil
}

// Формат реплики — как chat.message.public.json: та же структура, что ждёт
// ядро.
// parseChat достаёт реплику из сообщения GoodGame. Формат ответа у них
// менялся, поэтому берём первое осмысленное поле и не падаем на неизвестном.
// src — канал, из которого пришла реплика.
func parseChat(channel string, raw []byte) (protocol.ChatPayload, bool) {
	var envelope struct {
		Type string `json:"type"`
		Data struct {
			Message *struct {
				Text     string `json:"text"`
				UserName string `json:"user_name"`
				Name     string `json:"name"`
				UserID   int    `json:"user_id"`
				Login    string `json:"login"`
				HTML     string `json:"html"`
			} `json:"message"`
			MessageText string `json:"message_text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return protocol.ChatPayload{}, false
	}

	if envelope.Data.Message != nil {
		m := envelope.Data.Message
		user := protocol.ChatUser{
			Name:  first(m.UserName, m.Name),
			Login: m.Login,
		}
		if m.UserID != 0 {
			user.ID = strconv.Itoa(m.UserID)
		}
		return protocol.ChatPayload{
			Channel: channel,
			User:    user,
			Message: protocol.ChatLine{Src: m.Text, Text: plainText(m.Text), Html: m.HTML},
		}, true
	}
	if envelope.Data.MessageText != "" {
		return protocol.ChatPayload{
			Channel: channel,
			Message: protocol.ChatLine{Src: envelope.Data.MessageText, Text: plainText(envelope.Data.MessageText)},
		}, true
	}

	slog.Debug("goodgame: неизвестное сообщение", "kind", envelope.Type)
	return protocol.ChatPayload{}, false
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
