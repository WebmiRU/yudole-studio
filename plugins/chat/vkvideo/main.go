package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"studio/core/protocol"
)

const (
	apiBase       = "https://api.live.vkvideo.ru/v1"
	websocketURL  = "wss://pubsub.live.vkvideo.ru/connection/websocket?cf_protocol_version=v2"
	requestLimit  = 20 * time.Second
	maxBackoff    = 60 * time.Second
	maxMessageIDs = 4096
	userAgent     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36"
	// viewersReportInterval — как часто можно показывать счётчик зрителей.
	// VK шлёт его на каждое изменение, а лента от этого только засоряется.
	viewersReportInterval = time.Minute
)

var (
	// topicRe вытаскивает адрес чата со страницы целиком, вместе со слотом.
	// На странице адресов бывает по несколько — и для нашего канала, и для чужих,
	// — поэтому сверяться будем по номеру канала уже в коде.
	topicRe = regexp.MustCompile(`"wsChat(?:Slot)?Channel":"(channel-chat:[A-Za-z0-9_-]+(?:@[0-9]+)?)"`)
)

var (
	probeDrops int
	probeSmile int

	httpClient          = &http.Client{Timeout: requestLimit}
	errSettingsChanged  = errors.New("настройки VK Видео Live изменились")
	errNoUsableChannels = errors.New("не удалось подключиться ни к одному каналу")
	sendMu              sync.Mutex
	cfgMu               sync.RWMutex
	cfg                 settings
	changed             = make(chan struct{}, 1)
)

type settings struct {
	Channels []string
}

type vkChannel struct {
	Input       string
	Slug        string
	ID          string
	DisplayName string
	OwnerName   string
	OwnerNick   string
}

var options = []protocol.Option{
	{
		Key:         "channels",
		Type:        protocol.OptionTextMultiple,
		Label:       "Каналы",
		Section:     "Каналы",
		Placeholder: "имя_канала или https://live.vkvideo.ru/имя_канала",
		Hint:        "Ссылки или имена каналов VK Видео Live, по одному на строку. Авторизация и API-ключ не нужны",
		Default:     []any{},
	},
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	send(protocol.KindPluginChat, protocol.InfoPayload{
		Title:       "VK Видео Live",
		Description: "Чтение публичных чатов трансляций VK Видео Live — без авторизации и API-ключа.",
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
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		logInfo("stdin: " + err.Error())
	}
}

func handle(line string) bool {
	message, err := protocol.Decode([]byte(line))
	if err != nil {
		send(protocol.KindError, protocol.ErrorPayload{Message: "Не удалось прочитать команду: " + err.Error()})
		return false
	}

	switch message.Kind {
	case protocol.KindSetOptions:
		var payload protocol.SetOptionsPayload
		if err := message.PayloadInto(&payload); err != nil {
			send(protocol.KindError, protocol.ErrorPayload{Message: err.Error()})
			return false
		}
		next, fields, items := parseSettings(payload.Values)
		if len(fields) != 0 {
			send(protocol.KindError, protocol.ErrorPayload{
				Message: "Проверьте настройки VK Видео Live",
				Fields:  fields,
				Items:   items,
			})
			return false
		}
		setSettings(next)
		send(protocol.KindAck, protocol.AckPayload{Values: map[string]any{"channels": next.Channels}})
	case protocol.KindQuit:
		send(protocol.KindAck, protocol.AckPayload{})
		return true
	default:
		send(protocol.KindError, protocol.ErrorPayload{Message: "Неизвестная команда: " + message.Kind})
	}
	return false
}

func parseSettings(values map[string]any) (settings, map[string]string, map[string]map[string]string) {
	var result settings
	fields := make(map[string]string)

	raw, exists := values["channels"]
	if !exists || raw == nil {
		return result, fields, nil
	}
	list, ok := raw.([]any)
	if !ok {
		fields["channels"] = "Ожидался список каналов"
		return result, fields, nil
	}

	seen := make(map[string]bool)
	itemErrors := make(map[string]string)
	for _, item := range list {
		input := channelInput(item)
		if input == "" {
			continue
		}
		slug, err := normalizeChannel(input)
		if err != nil {
			itemErrors[input] = err.Error()
			continue
		}
		if !seen[slug] {
			seen[slug] = true
			result.Channels = append(result.Channels, slug)
		}
	}
	if len(itemErrors) != 0 {
		fields["channels"] = "Удалите или исправьте некорректные каналы"
		return settings{}, fields, map[string]map[string]string{"channels": itemErrors}
	}
	return result, fields, nil
}

func channelInput(item any) string {
	switch value := item.(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		if valueString, ok := value["value"].(string); ok && strings.TrimSpace(valueString) != "" {
			return strings.TrimSpace(valueString)
		}
		if label, ok := value["label"].(string); ok {
			return strings.TrimSpace(label)
		}
	case protocol.ChoiceValue:
		if value.Value != "" {
			return strings.TrimSpace(value.Value)
		}
		return strings.TrimSpace(value.Label)
	}
	return ""
}

func normalizeChannel(input string) (string, error) {
	value := strings.TrimSpace(input)
	if value == "" {
		return "", errors.New("укажите имя канала или ссылку")
	}
	if strings.HasPrefix(value, "@") {
		value = strings.TrimPrefix(value, "@")
	}

	// A host/path is a URL even if the user omitted https://.
	if strings.Contains(value, "://") || strings.HasPrefix(value, "live.vkvideo.ru/") ||
		strings.HasPrefix(value, "www.live.vkvideo.ru/") ||
		strings.HasPrefix(value, "vkplay.live/") ||
		strings.HasPrefix(value, "live.vkplay.ru/") {
		candidate := value
		if !strings.Contains(candidate, "://") {
			candidate = "https://" + candidate
		}
		parsed, err := url.Parse(candidate)
		if err != nil {
			return "", errors.New("не удалось разобрать ссылку")
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return "", errors.New("ссылка должна использовать HTTP или HTTPS")
		}
		host := strings.ToLower(parsed.Hostname())
		switch host {
		case "live.vkvideo.ru", "www.live.vkvideo.ru", "vkplay.live", "www.vkplay.live", "live.vkplay.ru":
		default:
			return "", errors.New("ссылка должна вести на live.vkvideo.ru")
		}
		if parsed.User != nil {
			return "", errors.New("ссылка с логином и паролем не поддерживается")
		}
		path := strings.Trim(parsed.EscapedPath(), "/")
		if path == "" {
			return "", errors.New("в ссылке нет имени канала")
		}
		first := strings.Split(path, "/")[0]
		decoded, err := url.PathUnescape(first)
		if err != nil {
			return "", errors.New("некорректное имя канала в ссылке")
		}
		value = strings.TrimPrefix(decoded, "@")
	} else if strings.ContainsAny(value, "/?#") {
		return "", errors.New("укажите имя канала или ссылку live.vkvideo.ru")
	}

	value = strings.TrimSpace(value)
	if value == "" || value == "." || value == ".." {
		return "", errors.New("некорректное имя канала")
	}
	for _, r := range value {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' || r == '.') {
			return "", errors.New("имя канала может содержать буквы, цифры, точку, дефис и подчёркивание")
		}
	}
	return value, nil
}

func setSettings(next settings) {
	next.Channels = append([]string(nil), next.Channels...)
	cfgMu.Lock()
	cfg = next
	cfgMu.Unlock()
	select {
	case changed <- struct{}{}:
	default:
	}
}

func getSettings() settings {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return settings{Channels: append([]string(nil), cfg.Channels...)}
}

func sameSettings(a, b settings) bool {
	if len(a.Channels) != len(b.Channels) {
		return false
	}
	for i := range a.Channels {
		if a.Channels[i] != b.Channels[i] {
			return false
		}
	}
	return true
}

func supervise(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		current := getSettings()
		if len(current.Channels) == 0 {
			emitConnection(protocol.ConnectionOffline, "Добавьте хотя бы один канал", 0)
			select {
			case <-ctx.Done():
				return
			case <-changed:
				attempt = 0
				continue
			}
		}

		state := protocol.ConnectionConnecting
		reason := "Подключаюсь к чату VK Видео Live"
		if attempt > 0 {
			state = protocol.ConnectionReconnecting
			reason = "Восстанавливаю связь с VK Видео Live"
		}
		emitConnection(state, reason, attempt)

		err := runSession(ctx, current)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errSettingsChanged) {
			attempt = 0
			continue
		}
		if errors.Is(err, errNoUsableChannels) {
			emitConnection(protocol.ConnectionOffline, err.Error(), 0)
			select {
			case <-ctx.Done():
				return
			case <-changed:
				attempt = 0
				continue
			}
		}

		attempt++
		if err != nil {
			reason = err.Error()
		} else {
			reason = "соединение закрыто"
		}
		emitConnection(protocol.ConnectionReconnecting, reason, attempt)
		delay := time.Second * time.Duration(1<<min(attempt-1, 6))
		if delay > maxBackoff {
			delay = maxBackoff
		}
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

func runSession(parent context.Context, current settings) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var joinedChannels []vkChannel
	defer func() {
		if ctx.Err() == nil || parent.Err() != nil {
			return
		}
		wanted := make(map[string]bool)
		for _, input := range getSettings().Channels {
			wanted[input] = true
		}
		for _, channel := range joinedChannels {
			if wanted[channel.Input] {
				continue
			}
			send(protocol.KindChannelUnjoinSuccess, protocol.ChannelPayload{
				Channel: channel.Slug, Name: channelName(channel),
			})
		}
	}()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-changed:
				if !sameSettings(getSettings(), current) {
					cancel()
					return
				}
			}
		}
	}()

	channels := make([]vkChannel, 0, len(current.Channels))
	seenChannelIDs := make(map[string]bool)
	retryableFailures := 0
	for _, input := range current.Channels {
		if ctx.Err() != nil {
			return errSettingsChanged
		}
		channel, err := resolveChannel(ctx, httpClient, apiBase, input)
		if err != nil {
			logInfo(fmt.Sprintf("VK Видео Live: не удалось открыть канал %q: %v", input, err))
			send(protocol.KindChannelJoinFailed, protocol.ChannelPayload{Channel: input, Name: input, Reason: err.Error()})
			if retryable(err) {
				retryableFailures++
			}
			continue
		}
		if seenChannelIDs[channel.ID] {
			continue
		}
		seenChannelIDs[channel.ID] = true
		channels = append(channels, channel)
	}
	if ctx.Err() != nil {
		return errSettingsChanged
	}
	if len(channels) == 0 {
		if retryableFailures > 0 {
			return errors.New("API VK Видео Live временно недоступен")
		}
		return errNoUsableChannels
	}

	headers := http.Header{}
	headers.Set("Origin", "https://live.vkvideo.ru")
	headers.Set("Referer", "https://live.vkvideo.ru/")
	headers.Set("User-Agent", userAgent)
	conn, _, err := websocket.Dial(ctx, websocketURL, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		if parent.Err() == nil && ctx.Err() != nil {
			return errSettingsChanged
		}
		return fmt.Errorf("WebSocket VK Видео Live: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "session ended") }()
	go func() {
		<-ctx.Done()
		_ = conn.CloseNow()
	}()

	token, err := getConnectToken(ctx, httpClient, apiBase)
	if err != nil {
		if ctx.Err() != nil && parent.Err() == nil {
			return errSettingsChanged
		}
		return err
	}

	pending := make([]json.RawMessage, 0)
	nextID := int64(0)
	nextID++
	if err := invoke(ctx, conn, nextID, map[string]any{
		"connect": map[string]any{"token": token, "name": "js"},
	}, &pending); err != nil {
		if ctx.Err() != nil && parent.Err() == nil {
			return errSettingsChanged
		}
		return fmt.Errorf("handshake VK Видео Live: %w", err)
	}

	byID := make(map[string]vkChannel, len(channels))
	for _, channel := range channels {
		byID[channel.ID] = channel
	}

	joined := 0
	for _, channel := range channels {
		if ctx.Err() != nil {
			return errSettingsChanged
		}

		// Подписываться надо не на один канал, а на оба, и вот почему.
		//
		// Адрес блога всегда называется public-chat:{id} — он не меняется никогда.
		// Но как только начинается эфир, сообщения идут по другому адресу:
		// channel-chat:{id}. Подписавшись только на public-chat, плагин сидит в
		// тишине на живом канале и честно показывает «на связи» — хуже некуда:
		// вроде всё работает, а реплик нет.
		//
		// Какой из двух сработает — зависит от состояния канала, поэтому
		// берём оба и считаем канал подключённым, если жив хотя бы один.
		chatChannels := []string{"public-chat:" + channel.ID, "channel-chat:" + channel.ID}
		// Адреса со слотом есть только на самой странице канала: в API их нет,
		// а без них часть реплик просто не доходит.
		channelPage := "https://live.vkvideo.ru/" + channel.Slug
		if extra := discoverChatTopics(ctx, httpClient, channelPage, channel.ID); len(extra) > 0 {
			chatChannels = append(chatChannels, extra...)
		}

		chatOK := false
		var chatErrors []string
		for _, name := range chatChannels {
			nextID++
			if err := invoke(ctx, conn, nextID, map[string]any{
				"subscribe": map[string]any{"channel": name},
			}, &pending); err != nil {
				if ctx.Err() != nil && parent.Err() == nil {
					return errSettingsChanged
				}
				chatErrors = append(chatErrors, name+": "+err.Error())
				continue
			}
			chatOK = true
		}
		if !chatOK {
			reason := "подписка на чат не удалась"
			if len(chatErrors) > 0 {
				reason += " (" + strings.Join(chatErrors, "; ") + ")"
			}
			send(protocol.KindChannelJoinFailed, protocol.ChannelPayload{
				Channel: channel.Slug, Name: channelName(channel), Reason: reason,
			})
			logInfo(fmt.Sprintf("VK Видео Live: подписка на чат %q не удалась: %s", channel.Slug, reason))
			delete(byID, channel.ID)
			continue
		}

		nextID++
		if err := invoke(ctx, conn, nextID, map[string]any{
			"subscribe": map[string]any{"channel": "channel-info:" + channel.ID},
		}, &pending); err != nil {
			if ctx.Err() != nil && parent.Err() == nil {
				return errSettingsChanged
			}
			logInfo(fmt.Sprintf("VK Видео Live: метаданные канала %q недоступны: %v", channel.Slug, err))
		}
		if ctx.Err() != nil && parent.Err() == nil {
			return errSettingsChanged
		}
		send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{Channel: channel.Slug, Name: channelName(channel)})
		joinedChannels = append(joinedChannels, channel)
		joined++
	}
	if joined == 0 {
		return errNoUsableChannels
	}
	emitConnection(protocol.ConnectionOnline, "Читаю чат VK Видео Live", 0)

	seen := newSeenIDs()
	states := make(map[string]*channelState)
	for _, raw := range pending {
		processPush(raw, byID, seen, states)
	}

	for {
		raw, err := readFrame(ctx, conn)
		if err != nil {
			if ctx.Err() != nil && parent.Err() == nil {
				return errSettingsChanged
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("чтение чата VK Видео Live: %w", err)
		}
		processPush(raw, byID, seen, states)
	}
}

type httpStatusError struct {
	Status   int
	Endpoint string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s: HTTP %d", e.Endpoint, e.Status)
}

func retryable(err error) bool {
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		return true
	}
	return statusErr.Status == http.StatusTooManyRequests || statusErr.Status >= 500
}

func requestJSON(ctx context.Context, client *http.Client, endpoint string, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", "https://live.vkvideo.ru")
	request.Header.Set("Referer", "https://live.vkvideo.ru/")
	request.Header.Set("User-Agent", userAgent)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("запрос к VK Видео Live: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return &httpStatusError{Status: response.StatusCode, Endpoint: "VK Видео Live API"}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 2<<20))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("ответ VK Видео Live не является корректным JSON: %w", err)
	}
	return nil
}

type vkUser struct {
	ID          int64
	DisplayName string
	Name        string
	Nick        string
}

type vkBlog struct {
	PublicWebSocketChannel string
	BlogURL                string
	Owner                  vkUser
	Data                   *vkBlog
}

func resolveChannel(ctx context.Context, client *http.Client, base, input string) (vkChannel, error) {
	slug, err := normalizeChannel(input)
	if err != nil {
		return vkChannel{}, err
	}
	endpoint := strings.TrimRight(base, "/") + "/blog/" + url.PathEscape(slug)
	var response vkBlog
	if err := requestJSON(ctx, client, endpoint, &response); err != nil {
		return vkChannel{}, err
	}
	if response.Data != nil {
		if response.PublicWebSocketChannel == "" {
			response.PublicWebSocketChannel = response.Data.PublicWebSocketChannel
		}
		if response.BlogURL == "" {
			response.BlogURL = response.Data.BlogURL
		}
		if response.Owner.ID == 0 {
			response.Owner = response.Data.Owner
		}
	}
	id := strings.TrimSpace(response.PublicWebSocketChannel)
	if id == "" {
		return vkChannel{}, errors.New("в ответе VK Видео Live нет publicWebSocketChannel")
	}
	if parts := strings.Split(id, ":"); len(parts) > 1 {
		id = parts[len(parts)-1]
	}
	if id == "" || strings.ContainsAny(id, "/?# \r\n") {
		return vkChannel{}, errors.New("VK Видео Live вернул некорректный ID чата")
	}

	canonical := strings.TrimSpace(response.BlogURL)
	if canonical == "" {
		canonical = slug
	}
	if parsed, parseErr := normalizeChannel(canonical); parseErr == nil {
		canonical = parsed
	}
	return vkChannel{
		Input: input, Slug: canonical, ID: id,
		DisplayName: response.Owner.DisplayName, OwnerName: response.Owner.Name, OwnerNick: response.Owner.Nick,
	}, nil
}

// discoverChatTopics собирает адреса чата со страницы канала.
//
// Одного blog id мало. Пока эфира нет, сообщения идут по public-chat:{id}; когда
// эфир идёт — по channel-chat:{id}, а часть реплик приходит по адресу со
// номером слота: channel-chat:{id}@0. Подписавшись только на первые два, мы
// молча теряли часть сообщений: плагин молчал о потере, и человек решал, что
// просто не все пишут.
//
// Номера слота в публичном API нет — только на самой странице. Поэтому идём туда
// и читаем готовые адреса.
func discoverChatTopics(ctx context.Context, client *http.Client, pageURL, blogID string) []string {
	page, err := fetchPage(ctx, client, pageURL)
	if err != nil || len(page) == 0 {
		return nil
	}
	matches := topicRe.FindAllStringSubmatch(page, -1)

	seen := make(map[string]bool, len(matches))
	topics := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		topic := match[1]
		if topic == "" || seen[topic] || topicBelongsTo(topic, blogID) == false {
			continue
		}
		seen[topic] = true
		topics = append(topics, topic)
	}
	return topics
}

// topicBelongsTo проверяет, что адрес чата относится к нужному каналу.
func topicBelongsTo(topic, blogID string) bool {
	if blogID == "" {
		return false
	}
	id := strings.TrimPrefix(topic, "channel-chat:")
	if at := strings.Index(id, "@"); at >= 0 {
		id = id[:at]
	}
	return id == blogID
}

// fetchPage читает публичную страницу канала целиком.
func fetchPage(ctx context.Context, client *http.Client, pageURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Referer", "https://live.vkvideo.ru/")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("запрос к VK Видео Live: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("страница канала: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func getConnectToken(ctx context.Context, client *http.Client, base string) (string, error) {
	var response struct {
		Token string
		Data  struct {
			Token string
		}
	}
	if err := requestJSON(ctx, client, strings.TrimRight(base, "/")+"/ws/connect", &response); err != nil {
		return "", err
	}
	if response.Token == "" {
		response.Token = response.Data.Token
	}
	if response.Token == "" {
		return "", errors.New("VK Видео Live API не вернул WebSocket token")
	}
	return response.Token, nil
}

func invoke(ctx context.Context, conn *websocket.Conn, id int64, method map[string]any, pending *[]json.RawMessage) error {
	request := make(map[string]any, len(method)+1)
	request["id"] = id
	for key, value := range method {
		request[key] = value
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if err := conn.Write(ctx, websocket.MessageText, encoded); err != nil {
		return err
	}
	for {
		raw, err := readFrame(ctx, conn)
		if err != nil {
			return err
		}
		var envelope struct {
			ID    int64
			Push  json.RawMessage
			Error *struct {
				Code    int
				Message string
			}
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			continue
		}
		if len(envelope.Push) != 0 {
			*pending = append(*pending, append(json.RawMessage(nil), raw...))
			continue
		}
		if envelope.ID != id {
			continue
		}
		if envelope.Error != nil {
			return fmt.Errorf("сервер отклонил команду (%d): %s", envelope.Error.Code, envelope.Error.Message)
		}
		return nil
	}
}

func readFrame(ctx context.Context, conn *websocket.Conn) (json.RawMessage, error) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(data)) == "{}" {
			if err := conn.Write(ctx, websocket.MessageText, []byte("{}")); err != nil {
				return nil, err
			}
			continue
		}
		return append(json.RawMessage(nil), data...), nil
	}
}

type wsPush struct {
	Push struct {
		Channel string
		Pub     struct {
			Data   json.RawMessage
			Offset int64
		}
	}
}

type vkEvent struct {
	Type     string
	Data     json.RawMessage
	BlogURL  string
	Title    string
	IsOnline bool
	Viewers  int
	StreamID string
	Category struct {
		Title string
	}
}

type vkMessage struct {
	ID        int64
	CreatedAt int64
	Author    vkUser
	User      vkUser
	Data      []vkBlock
	IsPrivate bool
}

type vkBlock struct {
	Type        string
	Content     string
	DisplayName string
	Name        string
	Nick        string
	URL         string
}

// channelState помнит, что мы уже показывали по каналу.
//
// Счётчик зрителей VK шлёт постоянно, и на каждое изменение лента получала
// строку вида «HardPlay — название трансляции, 1941 в канале». Три таких строки
// подряд — и человек перестаёт читать ленту вовсе. Поэтому наружу отдаём
// только то, что действительно изменилось, а зрителей — не чаще раза в минуту.
type channelState struct {
	started   int64
	title     string
	viewers   int
	sentAt    time.Time
	announced bool
}

// shouldReport решает, стоит ли показывать канал в ленке снова.
func (st *channelState) shouldReport(title string, viewers int) bool {
	if st.announced && st.title != title {
		return true
	}
	return time.Since(st.sentAt) >= viewersReportInterval
}

func processPush(raw json.RawMessage, byID map[string]vkChannel, seen *seenIDs, states map[string]*channelState) {
	var push wsPush
	if err := json.Unmarshal(raw, &push); err != nil || push.Push.Channel == "" || len(push.Push.Pub.Data) == 0 {
		return
	}

	channel, ok := byID[channelNameFromPush(push.Push.Channel)]
	if !ok {
		return
	}

	var event vkEvent
	if err := json.Unmarshal(push.Push.Pub.Data, &event); err != nil {
		return
	}
	switch event.Type {
	case "message":
		var message vkMessage
		if err := json.Unmarshal(event.Data, &message); err != nil {
			return
		}
		id := strconv.FormatInt(message.ID, 10)
		if message.ID != 0 && !seen.Add(channel.ID+":"+id) {
			return
		}
		author := message.Author
		if author.ID == 0 && author.Nick == "" && author.Name == "" {
			author = message.User
		}
		name := firstNonEmpty(author.DisplayName, author.Name, author.Nick)
		login := firstNonEmpty(author.Nick, author.Name)
		if name == "" {
			name = login
		}
		text := messageText(message.Data)
		if text == "" {
			return
		}
		kind := protocol.KindMessagePublic
		if message.IsPrivate {
			kind = protocol.KindMessagePrivate
		}
		send(kind, protocol.ChatPayload{
			Channel: channel.Slug,
			User:    protocol.ChatUser{Name: name, ID: idForUser(author.ID), Login: login},
			Message: protocol.ChatLine{Src: text, Text: text},
		})
	case "stream_online_status":
		state := states[channel.ID]
		if state == nil {
			state = &channelState{}
			states[channel.ID] = state
		}
		if event.IsOnline && state.started == 0 {
			state.started = time.Now().Unix()
		}
		if !event.IsOnline {
			state.started = 0
		}
		if !state.shouldReport(event.Title, event.Viewers) {
			return
		}
		state.title = event.Title
		state.viewers = event.Viewers
		state.sentAt = time.Now()
		state.announced = true
		send(protocol.KindChannelInfo, protocol.ChannelInfoPayload{
			Channel:  channel.Slug,
			Name:     event.Title,
			Streamer: firstNonEmpty(channel.DisplayName, channel.OwnerName, channel.OwnerNick),
			Started:  state.started,
			Clients:  event.Viewers,
			Users:    event.Viewers,
		})
	case "stream_start":
		state := states[channel.ID]
		if state == nil {
			state = &channelState{}
			states[channel.ID] = state
		}
		if state.started == 0 {
			state.started = time.Now().Unix()
		}
		state.announced = true
		state.sentAt = time.Now()
		send(protocol.KindChannelInfo, protocol.ChannelInfoPayload{
			Channel:  channel.Slug,
			Streamer: firstNonEmpty(channel.DisplayName, channel.OwnerName, channel.OwnerNick),
			Started:  state.started,
		})
	case "stream_end":
		delete(states, channel.ID)
		send(protocol.KindChannelInfo, protocol.ChannelInfoPayload{
			Channel:  channel.Slug,
			Streamer: firstNonEmpty(channel.DisplayName, channel.OwnerName, channel.OwnerNick),
			Clients:  0,
			Users:    0,
		})
	}
}

// channelNameFromPush достаёт id канала из адреса топика.
//
// Адреса приходят в двух видах: public-chat:12345 и channel-chat:12345, а ещё
// бывает channel-chat:12345@67890 — с номером слота. Номер слота к каналу не
// относится, и если его не отбросить, сообщение не найдёт свой канал и молча
// потеряется.
func channelNameFromPush(topic string) string {
	parts := strings.Split(topic, ":")
	if len(parts) < 2 {
		return ""
	}
	id := parts[len(parts)-1]
	if at := strings.Index(id, "@"); at >= 0 {
		id = id[:at]
	}
	return id
}

func messageText(blocks []vkBlock) string {
	var out strings.Builder
	// Один и тот же смайл в одной реплике печатаем один раз. Анимированные
	// приходят десятками одинаковых узлов, и без этого одна реплика
	// превращалась в двадцать одинаковых подписей подряд. Общий набор
	// проходит через все блоки, а не через каждый отдельно: повторы идут
	// именно между блоками.
	seenSmiles := map[string]bool{}
	for _, block := range blocks {
		switch block.Type {
		case "text", "link":
			out.WriteString(vkPlainTextSeen(block.Content, seenSmiles))
		case "mention":
			name := firstNonEmpty(block.DisplayName, block.Name, block.Nick)
			if name != "" {
				if !strings.HasPrefix(name, "@") {
					out.WriteByte('@')
				}
				out.WriteString(name)
			}
		case "smile":
			if block.Name != "" && !seenSmiles[block.Name] {
				seenSmiles[block.Name] = true
				out.WriteString(block.Name)
			}
		default:
			if block.Content != "" {
				out.WriteString(vkPlainTextSeen(block.Content, seenSmiles))
			}
		}
	}
	return strings.TrimSpace(out.String())
}

// vkPlainText достаёт читаемый текст из содержимого блока.
//
// Текст приходит закодированным дважды: в content лежит JSON-строка, а внутри
// неё — сам текст вместе с оформлением: ["привет","unstyled",[]]. Печатать это
// как есть нельзя, в ленте появляется мусор вида ["привет","unstyled",[]].
// Если разобрать не удалось, отдаём как есть: лучше лишние кавычки, чем пустота.
func vkPlainText(raw string) string {
	return vkPlainTextSeen(raw, map[string]bool{})
}

func vkPlainTextSeen(raw string, seen map[string]bool) string {
	if raw == "" || raw[0] != '[' && raw[0] != '{' {
		return raw
	}
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return raw
	}
	var out strings.Builder
	collectVKTextSeen(&out, decoded, seen)
	return out.String()
}

// collectVKText обходит разобранный кусок. Узел устроен как
// [текст, оформление, содержимое]: первый элемент — сам текст, второй — стиль
// («unstyled», «bold» и подобные), его печатать нельзя, третий — вложенные куски.
func collectVKText(out *strings.Builder, node any) {
	collectVKTextSeen(out, node, nil)
}

// collectVKTextSeen обходит разобранный кусок, помня уже выведенные смайлы.
//
// seen передаётся на всё сообщение целиком, а не на отдельный блок: анимированные
// смайлы приходят сразу десятками одинаковых узлов — по одному на кадр, — и без
// этого одна реплика превращалась в двадцать одинаковых подписей подряд.
func collectVKTextSeen(out *strings.Builder, node any, seen map[string]bool) {
	switch value := node.(type) {
	case string:
		out.WriteString(value)
	case []any:
		if len(value) == 0 {
			return
		}
		// Формы две, и по длине их не различить: узел это
		// [текст, оформление, содержимое], а содержимое — снова список узлов.
		// Различает тип второго элемента: у узла на этом месте строка-стиль
		// («unstyled», «bold»), у списка узлов — объект или ничего.
		if len(value) >= 2 {
			if _, isStyle := value[1].(string); isStyle {
				collectVKTextSeen(out, value[0], seen)
				for _, nested := range value[2:] {
					collectVKTextSeen(out, nested, seen)
				}
				return
			}
		}
		for _, nested := range value {
			collectVKTextSeen(out, nested, seen)
		}
	case map[string]any:
		// Смайлы приходят отдельными узлами прямо внутри текста. Без них
		// реплика выглядит обрезанной, а человек по ней не вспомнит, что писал.
		if kind, _ := value["type"].(string); kind == "smile" {
			name := firstNonEmpty(asString(value["name"]), asString(value["displayName"]))
			if name != "" {
				// Один и тот же смайл в одной реплике печатаем один раз.
				if seen[name] {
					return
				}
				if seen != nil {
					seen[name] = true
				}
				out.WriteString(name)
				return
			}
		}
		for _, key := range []string{"text", "content", "title"} {
			if child, ok := value[key]; ok {
				collectVKTextSeen(out, child, seen)
			}
		}
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func idForUser(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

func channelName(channel vkChannel) string {
	return firstNonEmpty(channel.DisplayName, channel.OwnerName, channel.OwnerNick, channel.Slug)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type seenIDs struct {
	mu    sync.Mutex
	set   map[string]struct{}
	order []string
	next  int
}

func newSeenIDs() *seenIDs {
	return &seenIDs{set: make(map[string]struct{}), order: make([]string, 0, maxMessageIDs)}
}

func (s *seenIDs) Add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.set[id]; exists {
		return false
	}
	if len(s.order) < maxMessageIDs {
		s.order = append(s.order, id)
	} else {
		delete(s.set, s.order[s.next])
		s.order[s.next] = id
		s.next = (s.next + 1) % maxMessageIDs
	}
	s.set[id] = struct{}{}
	return true
}

func emitConnection(state, reason string, attempt int) {
	send(protocol.KindConnectionState, protocol.ConnectionPayload{
		State: state, Service: "VK Видео Live", Reason: reason, Attempt: attempt,
	})
}

func logInfo(message string) {
	send(protocol.KindLog, protocol.LogPayload{Level: "info", Message: message})
}

func send(kind string, payload any) {
	sendMu.Lock()
	defer sendMu.Unlock()
	line, err := protocol.Encode(protocol.MustNew(kind, payload))
	if err != nil {
		fmt.Fprintln(os.Stderr, "vkvideo:", err)
		return
	}
	_, _ = os.Stdout.Write(line)
}
