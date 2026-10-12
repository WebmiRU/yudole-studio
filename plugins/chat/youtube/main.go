package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"studio/core/protocol"
	ytproto "studio/plugins/chat/youtube/internal/ytproto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	grpcTarget       = "dns:///youtube.googleapis.com:443"
	searchInterval   = time.Minute
	maxSearchAttempts = 60
	requestTimeout   = 20 * time.Second
	maxChatBackoff   = time.Minute
)

var (
	apiBase = "https://www.googleapis.com/youtube/v3/"
	errSettingsChanged = errors.New("настройки YouTube изменились")
	errSearchExhausted = errors.New("активный эфир не найден за час")
	errChatEnded = errors.New("live chat завершён или недоступен")
	errChannelNotFound = errors.New("YouTube не нашёл канал по указанному @handle/ID")
	channelIDPattern = regexp.MustCompile("^UC[A-Za-z0-9_-]{20,}$")
	httpClient = &http.Client{Timeout: requestTimeout}
)

var options = []protocol.Option{
	{
		Key:         "api_key",
		Type:        protocol.OptionPassword,
		Label:       "YouTube API key",
		Section:     "Подключение",
		Placeholder: "AIza...",
		Hint:        "Ключ проекта Google Cloud с включённым YouTube Data API v3. OAuth для чтения публичного чата не нужен.",
	},
	{
		Key:         "channel",
		Type:        protocol.OptionText,
		Label:       "Канал YouTube",
		Section:     "Канал",
		Placeholder: "https://www.youtube.com/@Paradoxlivechannel",
		Hint:        "Ссылка на канал, @handle или ID UC… Проверка эфира выполняется раз в минуту, максимум 60 раз.",
	},
}

type settings struct {
	APIKey  string
	Channel string
}

var (
	settingsMu sync.RWMutex
	current    settings
	changed    = make(chan struct{}, 1)
	stdoutMu   sync.Mutex
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	send(protocol.KindPluginChat, protocol.InfoPayload{
		Title:       "YouTube",
		Description: "Чат YouTube Live через официальный Data API и потоковый liveChatMessages.streamList.",
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

		next, fields := parseSettings(payload.Values)
		if len(fields) > 0 {
			send(protocol.KindError, protocol.ErrorPayload{
				Message: "Проверьте настройки YouTube",
				Fields:  fields,
			})
			return false
		}
		setSettings(next)
		send(protocol.KindAck, protocol.AckPayload{
			Values: map[string]any{
				"api_key": next.APIKey,
				"channel": next.Channel,
			},
		})

	case protocol.KindQuit:
		send(protocol.KindAck, protocol.AckPayload{})
		return true

	default:
		send(protocol.KindError, protocol.ErrorPayload{Message: "Неизвестная команда: " + message.Kind})
	}
	return false
}

func parseSettings(values map[string]any) (settings, map[string]string) {
	var next settings
	fields := make(map[string]string)

	next.APIKey, _ = values["api_key"].(string)
	next.APIKey = strings.TrimSpace(next.APIKey)
	if strings.ContainsAny(next.APIKey, " \r\n\t") {
		fields["api_key"] = "API key не должен содержать пробелы"
	}

	rawChannel, _ := values["channel"].(string)
	rawChannel = strings.TrimSpace(rawChannel)
	if rawChannel != "" {
		channel, err := normalizeChannelInput(rawChannel)
		if err != nil {
			fields["channel"] = err.Error()
		} else {
			next.Channel = channel
		}
	}
	return next, fields
}

func setSettings(next settings) {
	settingsMu.Lock()
	current = next
	settingsMu.Unlock()
	select {
	case changed <- struct{}{}:
	default:
	}
}

func getSettings() settings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return current
}

type channelRef struct {
	Normalized string
	Handle     string
	ID         string
}

func validHandle(value string) bool {
	length := utf8.RuneCountInString(value)
	if length < 3 || length > 30 {
		return false
	}
	for _, r := range value {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func normalizeChannelInput(raw string) (string, error) {
	ref, err := parseChannelRef(raw)
	if err != nil {
		return "", err
	}
	return ref.Normalized, nil
}

func parseChannelRef(raw string) (channelRef, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return channelRef{}, errors.New("укажите ссылку на канал, @handle или ID канала UC…")
	}

	if !strings.Contains(value, "://") &&
		(strings.HasPrefix(strings.ToLower(value), "youtube.com/") ||
			strings.HasPrefix(strings.ToLower(value), "www.youtube.com/") ||
			strings.HasPrefix(strings.ToLower(value), "m.youtube.com/")) {
		value = "https://" + value
	}
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return channelRef{}, errors.New("не удалось разобрать ссылку на канал")
		}
		host := strings.ToLower(parsed.Hostname())
		if host != "youtube.com" && host != "www.youtube.com" && host != "m.youtube.com" {
			return channelRef{}, errors.New("ожидалась ссылка на youtube.com")
		}
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) == 0 || parts[0] == "" {
			return channelRef{}, errors.New("в ссылке не найден канал")
		}
		if parts[0] == "channel" && len(parts) >= 2 && channelIDPattern.MatchString(parts[1]) {
			return channelRef{Normalized: parts[1], ID: parts[1]}, nil
		}
		if strings.HasPrefix(parts[0], "@") {
			handle := strings.TrimPrefix(parts[0], "@")
			if !validHandle(handle) {
				return channelRef{}, errors.New("некорректный YouTube @handle")
			}
			return channelRef{Normalized: "@" + handle, Handle: handle}, nil
		}
		return channelRef{}, errors.New("используйте ссылку вида youtube.com/@handle или youtube.com/channel/UC…")
	}

	if channelIDPattern.MatchString(value) {
		return channelRef{Normalized: value, ID: value}, nil
	}
	value = strings.TrimPrefix(value, "@")
	if !validHandle(value) {
		return channelRef{}, errors.New("укажите корректный @handle YouTube или ID канала UC…")
	}
	return channelRef{Normalized: "@" + value, Handle: value}, nil
}

type apiError struct {
	Status  int
	Reason  string
	Message string
}

func (e *apiError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("YouTube API (%d, %s): %s", e.Status, e.Reason, e.Message)
	}
	return fmt.Sprintf("YouTube API (%d): %s", e.Status, e.Message)
}

func (e *apiError) Permanent() bool {
	return e.Status == http.StatusBadRequest ||
		e.Status == http.StatusUnauthorized ||
		e.Status == http.StatusForbidden ||
		e.Status == http.StatusNotFound
}

func apiRequest(ctx context.Context, apiKey, method string, params url.Values, result any) error {
	if params == nil {
		params = make(url.Values)
	}
	params.Set("key", apiKey)
	endpoint := apiBase + method + "?" + params.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		// http.Client errors often contain the full URL, including the API key.
		return errors.New(strings.ReplaceAll(err.Error(), apiKey, "[redacted]"))
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Errors  []struct {
					Reason string `json:"reason"`
				} `json:"errors"`
			} `json:"error"`
		}
		_ = json.Unmarshal(body, &payload)
		reason := ""
		if len(payload.Error.Errors) > 0 {
			reason = payload.Error.Errors[0].Reason
		}
		message := strings.ReplaceAll(payload.Error.Message, apiKey, "[redacted]")
		return &apiError{Status: response.StatusCode, Reason: reason, Message: message}
	}
	if err := json.Unmarshal(body, result); err != nil {
		return fmt.Errorf("не удалось разобрать ответ YouTube API: %w", err)
	}
	return nil
}

func resolveChannel(ctx context.Context, apiKey, configured string) (channelID, channelTitle string, err error) {
	ref, err := parseChannelRef(configured)
	if err != nil {
		return "", "", err
	}

	params := url.Values{"part": {"id,snippet"}}
	if ref.ID != "" {
		params.Set("id", ref.ID)
	} else {
		params.Set("forHandle", ref.Handle)
	}

	var response struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := apiRequest(ctx, apiKey, "channels", params, &response); err != nil {
		return "", "", err
	}
	if len(response.Items) == 0 {
		return "", "", errChannelNotFound
	}
	return response.Items[0].ID, response.Items[0].Snippet.Title, nil
}

type liveVideo struct {
	ID    string
	Title string
	Chat  string
}

func findActiveLives(ctx context.Context, apiKey, channelID string) ([]liveVideo, error) {
	searchParams := url.Values{
		"part":       {"snippet"},
		"channelId":  {channelID},
		"type":       {"video"},
		"eventType":  {"live"},
		"maxResults": {"50"},
	}

	var searchResponse struct {
		Items []struct {
			ID struct {
				VideoID string `json:"videoId"`
			} `json:"id"`
			Snippet struct {
				Title       string `json:"title"`
				ChannelID   string `json:"channelId"`
				ChannelTitle string `json:"channelTitle"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := apiRequest(ctx, apiKey, "search", searchParams, &searchResponse); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(searchResponse.Items))
	titles := make(map[string]string, len(searchResponse.Items))
	seen := make(map[string]bool, len(searchResponse.Items))
	for _, item := range searchResponse.Items {
		id := strings.TrimSpace(item.ID.VideoID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		titles[id] = item.Snippet.Title
	}
	if len(ids) == 0 {
		return nil, nil
	}

	videoParams := url.Values{
		"part": {"snippet,liveStreamingDetails"},
		"id":   {strings.Join(ids, ",")},
	}
	var videoResponse struct {
		Items []struct {
			ID string `json:"id"`
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
			LiveStreamingDetails struct {
				ActiveLiveChatID string `json:"activeLiveChatId"`
			} `json:"liveStreamingDetails"`
		} `json:"items"`
	}
	if err := apiRequest(ctx, apiKey, "videos", videoParams, &videoResponse); err != nil {
		return nil, err
	}

	lives := make([]liveVideo, 0, len(videoResponse.Items))
	seenChats := make(map[string]bool)
	for _, item := range videoResponse.Items {
		chatID := strings.TrimSpace(item.LiveStreamingDetails.ActiveLiveChatID)
		if chatID == "" || seenChats[chatID] {
			continue
		}
		seenChats[chatID] = true
		title := item.Snippet.Title
		if title == "" {
			title = titles[item.ID]
		}
		lives = append(lives, liveVideo{ID: item.ID, Title: title, Chat: chatID})
	}
	return lives, nil
}

func supervise(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		drainChanges()
		config := getSettings()

		if config.APIKey == "" {
			emitConnection(protocol.ConnectionOffline, "Укажите YouTube API key", 0)
			waitForChange(ctx)
			continue
		}
		if config.Channel == "" {
			emitConnection(protocol.ConnectionOffline, "Укажите канал YouTube", 0)
			waitForChange(ctx)
			continue
		}

		runCtx, cancel := context.WithCancel(ctx)
		watcherDone := make(chan struct{})
		go func() {
			defer close(watcherDone)
			select {
			case <-runCtx.Done():
			case <-changed:
				cancel()
			}
		}()

		err := runConfiguration(runCtx, config)
		cancel()
		<-watcherDone

		if ctx.Err() != nil {
			return
		}
		// The configuration update may race with a completed search/stream
		// and be consumed by the cancellation watcher. Compare the snapshot
		// as well, so we never sleep waiting for an already-consumed signal.
		if getSettings() != config {
			continue
		}
		if errors.Is(err, errSettingsChanged) {
			continue
		}
		if errors.Is(err, errSearchExhausted) {
			waitForChange(ctx)
			continue
		}
		if errors.Is(err, errChatEnded) {
			emitConnection(protocol.ConnectionReconnecting, "Эфир или чат завершились; ищу следующий активный эфир", 0)
			continue
		}
		if err != nil {
			logInfo(err.Error())
			emitConnection(protocol.ConnectionOffline, err.Error(), 0)
			waitForChange(ctx)
		}
	}
}

func drainChanges() {
	for {
		select {
		case <-changed:
		default:
			return
		}
	}
}

func waitForChange(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-changed:
	}
}

func runConfiguration(ctx context.Context, config settings) error {
	emitConnection(protocol.ConnectionConnecting, "Проверяю канал YouTube", 0)
	var channelID, channelTitle string
	var err error
	for resolveAttempt := 0; resolveAttempt < 4; resolveAttempt++ {
		channelID, channelTitle, err = resolveChannel(ctx, config.APIKey, config.Channel)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return errSettingsChanged
		}
		if errors.Is(err, errChannelNotFound) {
			return err
		}
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Permanent() {
			return err
		}
		if resolveAttempt == 3 {
			return fmt.Errorf("не удалось разрешить YouTube-канал после нескольких попыток: %w", err)
		}
		logInfo(fmt.Sprintf("Не удалось получить ID канала (попытка %d/4): %v", resolveAttempt+1, err))
		if !waitInterval(ctx, searchInterval) {
			return errSettingsChanged
		}
	}
	if channelTitle == "" {
		channelTitle = config.Channel
	}
	logInfo(fmt.Sprintf("Канал YouTube разрешён: %s (%s)", channelTitle, channelID))

	for attempt := 1; attempt <= maxSearchAttempts; attempt++ {
		if ctx.Err() != nil {
			return errSettingsChanged
		}

		lives, err := findActiveLives(ctx, config.APIKey, channelID)
		if err != nil {
			if ctx.Err() != nil {
				return errSettingsChanged
			}
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Permanent() {
				return err
			}
			logInfo(fmt.Sprintf("Проверка эфира %d/%d не удалась: %v", attempt, maxSearchAttempts, err))
		} else if len(lives) > 0 {
			send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{
				Channel: config.Channel,
				Name:    channelTitle,
			})
			logInfo(fmt.Sprintf("Найдено активных чатов YouTube: %d", len(lives)))
			return watchLiveChats(ctx, config.APIKey, config.Channel, lives)
		} else {
			logInfo(fmt.Sprintf("Активный эфир не найден: проверка %d/%d", attempt, maxSearchAttempts))
		}

		if attempt == maxSearchAttempts {
			emitConnection(protocol.ConnectionOffline, "Эфир не найден за 60 проверок (около часа). Поиск остановлен до изменения настроек.", attempt)
			return errSearchExhausted
		}
		if !waitInterval(ctx, searchInterval) {
			return errSettingsChanged
		}
	}
	return errSearchExhausted
}

func watchLiveChats(ctx context.Context, apiKey, channel string, lives []liveVideo) error {
	emitConnection(protocol.ConnectionConnecting, "Подключаю поток liveChatMessages.streamList", 0)

	var conn *grpc.ClientConn
	var err error
	for attempt := 0; ; attempt++ {
		dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		conn, err = grpc.DialContext(
			dialCtx,
			grpcTarget,
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
			grpc.WithBlock(),
		)
		cancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return errSettingsChanged
		}
		logInfo("Не удалось подключиться к YouTube gRPC: " + err.Error())
		if !waitInterval(ctx, retryDelay(attempt)) {
			return errSettingsChanged
		}
	}
	defer conn.Close()

	client := ytproto.NewV3DataLiveChatMessageServiceClient(conn)
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		video liveVideo
		err   error
	}
	results := make(chan result, len(lives))
	var wg sync.WaitGroup
	var onlineOnce sync.Once

	for _, video := range lives {
		video := video
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := readOneChat(childCtx, client, apiKey, channel, video, func() {
				onlineOnce.Do(func() {
					emitConnection(protocol.ConnectionOnline, "Читаю live chat YouTube", 0)
				})
			})
			results <- result{video: video, err: err}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	remaining := len(lives)
	for remaining > 0 {
		select {
		case <-ctx.Done():
			cancel()
			return errSettingsChanged
		case item, ok := <-results:
			if !ok {
				return errChatEnded
			}
			remaining--
			if errors.Is(item.err, errSettingsChanged) {
				cancel()
				return errSettingsChanged
			}
			if errors.Is(item.err, errChatEnded) {
				logInfo("Чат трансляции завершён: " + item.video.Title)
				continue
			}
			if item.err != nil {
				cancel()
				return item.err
			}
		}
	}
	return errChatEnded
}

func readOneChat(ctx context.Context, client ytproto.V3DataLiveChatMessageServiceClient, apiKey, channel string, video liveVideo, onConnected func()) error {
	pageToken := ""
	attempt := 0
	seen := make(map[string]struct{})
	seenOrder := make([]string, 0, 1024)

	remember := func(id string) bool {
		if id == "" {
			return true
		}
		if _, ok := seen[id]; ok {
			return false
		}
		seen[id] = struct{}{}
		seenOrder = append(seenOrder, id)
		if len(seenOrder) > 4096 {
			delete(seen, seenOrder[0])
			seenOrder = seenOrder[1:]
		}
		return true
	}

	for {
		if ctx.Err() != nil {
			return errSettingsChanged
		}
		request := &ytproto.LiveChatMessageListRequest{
			LiveChatId: stringPtr(video.Chat),
			Part:       []string{"id", "snippet", "authorDetails"},
			MaxResults: uint32Ptr(500),
		}
		if pageToken != "" {
			request.PageToken = stringPtr(pageToken)
		}
		streamCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("x-goog-api-key", apiKey))
		stream, err := client.StreamList(streamCtx, request)
		if err != nil {
			if ctx.Err() != nil {
				return errSettingsChanged
			}
			if status.Code(err) == codes.NotFound || status.Code(err) == codes.FailedPrecondition {
				return errChatEnded
			}
			if permanentGRPCError(err) {
				return fmt.Errorf("YouTube live chat %q: %w", video.Title, err)
			}
			logInfo("Поток чата прерван, переподключаюсь: " + err.Error())
			if !waitInterval(ctx, retryDelay(attempt)) {
				return errSettingsChanged
			}
			attempt++
			continue
		}

		onConnected()
		received := false
		for {
			response, recvErr := stream.Recv()
			if recvErr != nil {
				if ctx.Err() != nil {
					return errSettingsChanged
				}
				if permanentGRPCError(recvErr) {
					if status.Code(recvErr) == codes.NotFound ||
						status.Code(recvErr) == codes.FailedPrecondition ||
						strings.Contains(strings.ToLower(recvErr.Error()), "ended") ||
						strings.Contains(strings.ToLower(recvErr.Error()), "disabled") {
						return errChatEnded
					}
					return fmt.Errorf("YouTube live chat %q: %w", video.Title, recvErr)
				}
				logInfo("Соединение с чатом потеряно, восстанавливаю поток: " + recvErr.Error())
				break
			}
			received = true
			if response.GetOfflineAt() != "" {
				return errChatEnded
			}
			// Save the continuation token so the stream can resume after a break.
			// The official client reconnects when the server has no nextPageToken.
			pageToken = response.GetNextPageToken()
			for _, item := range response.GetItems() {
				if item == nil || !remember(item.GetId()) {
					continue
				}
				if err := emitChatMessage(channel, item); err != nil {
					logInfo("Не удалось обработать сообщение YouTube: " + err.Error())
				}
				if item.GetSnippet() != nil &&
					item.GetSnippet().GetType() == ytproto.LiveChatMessageSnippet_TypeWrapper_CHAT_ENDED_EVENT {
					return errChatEnded
				}
			}
			if pageToken == "" {
				break
			}
		}

		if ctx.Err() != nil {
			return errSettingsChanged
		}
		if !received {
			logInfo("YouTube завершил поток без данных; пробую возобновить")
		}
		if !waitInterval(ctx, retryDelay(attempt)) {
			return errSettingsChanged
		}
		attempt++
	}
}

func emitChatMessage(channel string, message *ytproto.LiveChatMessage) error {
	snippet := message.GetSnippet()
	if snippet == nil {
		return nil
	}
	author := message.GetAuthorDetails()
	login, name := "", "YouTube"
	if author != nil {
		login = author.GetChannelId()
		name = author.GetDisplayName()
		if name == "" {
			name = login
		}
	}

	switch snippet.GetType() {
	case ytproto.LiveChatMessageSnippet_TypeWrapper_MESSAGE_DELETED_EVENT:
		if details := snippet.GetMessageDeletedDetails(); details != nil && details.GetDeletedMessageId() != "" {
			send(protocol.KindMessageDeleted, protocol.DeletedMessagePayload{
				Channel:   channel,
				MessageID: details.GetDeletedMessageId(),
				By:        login,
			})
		}
		return nil
	case ytproto.LiveChatMessageSnippet_TypeWrapper_MESSAGE_RETRACTED_EVENT:
		if details := snippet.GetMessageRetractedDetails(); details != nil && details.GetRetractedMessageId() != "" {
			send(protocol.KindMessageDeleted, protocol.DeletedMessagePayload{
				Channel:   channel,
				MessageID: details.GetRetractedMessageId(),
				By:        login,
			})
		}
		return nil
	}

	text := snippet.GetDisplayMessage()
	if text == "" {
		if details := snippet.GetTextMessageDetails(); details != nil {
			text = details.GetMessageText()
		}
	}
	if text == "" {
		if details := snippet.GetSuperChatDetails(); details != nil {
			text = details.GetAmountDisplayString()
			if comment := details.GetUserComment(); comment != "" {
				if text != "" {
					text += " — "
				}
				text += comment
			}
		}
	}
	if text == "" {
		if details := snippet.GetSuperStickerDetails(); details != nil {
			text = details.GetAmountDisplayString()
			if sticker := details.GetSuperStickerMetadata(); sticker != nil && sticker.GetAltText() != "" {
				if text != "" {
					text += " — "
				}
				text += sticker.GetAltText()
			}
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}

	send(protocol.KindMessagePublic, protocol.ChatPayload{
		Channel: channel,
		User: protocol.ChatUser{
			Name:  name,
			ID:    login,
			Login: login,
		},
		Message: protocol.ChatLine{
			Src:  text,
			Text: text,
			Html: "",
		},
	})
	return nil
}

func permanentGRPCError(err error) bool {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied, codes.NotFound, codes.FailedPrecondition:
		return true
	default:
		return false
	}
}

func retryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := 5 * time.Second
	for i := 0; i < attempt && delay < maxChatBackoff; i++ {
		delay *= 2
	}
	if delay > maxChatBackoff {
		return maxChatBackoff
	}
	return delay
}

func waitInterval(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-changed:
		return false
	case <-timer.C:
		return true
	}
}

func stringPtr(value string) *string { return &value }
func uint32Ptr(value uint32) *uint32 { return &value }

func emitConnection(state, reason string, attempt int) {
	send(protocol.KindConnectionState, protocol.ConnectionPayload{
		State:   state,
		Service: "YouTube",
		Reason:  reason,
		Attempt: attempt,
	})
}

func logInfo(text string) {
	send(protocol.KindLog, protocol.LogPayload{Level: "info", Message: text})
}

func send(kind string, payload any) {
	stdoutMu.Lock()
	defer stdoutMu.Unlock()

	line, err := protocol.Encode(protocol.MustNew(kind, payload))
	if err != nil {
		fmt.Fprintln(os.Stderr, "youtube:", err)
		return
	}
	_, _ = os.Stdout.Write(line)
}
