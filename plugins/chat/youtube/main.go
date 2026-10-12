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
	grpcTarget     = "dns:///youtube.googleapis.com:443"
	searchInterval = time.Minute
	requestTimeout = 20 * time.Second
	maxChatBackoff        = time.Minute
	grpcDialTimeout       = 5 * time.Second
	streamRestartDelay    = 1 * time.Second
)

var (
	apiBase            = "https://www.googleapis.com/youtube/v3/"
	errSettingsChanged = errors.New("настройки YouTube изменились")
	errSearchExhausted = errors.New("активный эфир не найден за час")
	errChatEnded       = errors.New("live chat завершён или недоступен")
	errChannelNotFound = errors.New("YouTube не нашёл канал по указанному @handle/ID")
	// errOfficialReadBroken — официальный поток открывается и тут же умирает,
	// не отдав ни одного сообщения. Ключ тут ни при чём: чинить это можно
	// только сменой пути чтения, и для этого есть резервный.
	errOfficialReadBroken = errors.New("официальный поток live chat не отдаёт сообщений")

	// maxStreamRestarts — сколько обрывов подряд терпим, прежде чем признать
	// официальный путь нерабочим. Три попытки за полторы минуты: мигание сети
	// обрывы дают, а вот «открылся и сразу умер» — это уже не сеть.
	maxStreamRestarts = 3

	// backlogWindow — сколько истории показывать при первом подключении.
	// Эфир идёт часами, и без этого окна плагин при каждом старте выкладывал в
	// ленту всё, что было сказано за последние сорок минут.
	backlogWindow = 2 * time.Minute

	// chatEndedInterval — пауза после завершившегося чата. Раунд без паузы
	// превращается в тысячу одинаковых попыток за час, а это уже не чтение,
	// а долбёжка чужого сервиса.
	chatEndedInterval = 30 * time.Second

	// errorRetryInterval — пауза перед новой попыткой после неожиданной ошибки.
	// Секунд пять: сеть обычно возвращается быстро, а вот долбить YouTube
	// после каждой неудачи нельзя.
	errorRetryInterval = 5 * time.Second
	channelIDPattern   = regexp.MustCompile("^UC[A-Za-z0-9_-]{20,}$")
	httpClient         = &http.Client{Timeout: requestTimeout}
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
		Type:        protocol.OptionTextMultiple,
		Label:       "Каналы и стримы YouTube",
		Section:     "Канал",
		Placeholder: "https://www.youtube.com/@digitalmediacorp",
		Hint:        "По строке: ссылка на канал, @handle, ID UC… или ссылка на конкретный стрим. Их может быть несколько — каналов и стримов поровну.",
		Default:     []any{},
	},
	{
		Key:     "fallback",
		Type:    protocol.OptionBool,
		Label:   "Резервное чтение без ключа",
		Section: "Канал",
		Hint:    "Если включено: работает без ключа и квоты — через внутренний API самого YouTube.",
		Default: true,
	},
}

type settings struct {
	APIKey string
	// Targets — всё, на что человек подписал плагин: каналы, которые надо
	// ждать, и стримы, к которым надо идти сразу. Порядок сохраняется: он
	// виден человеку в форме и должен совпадать с тем, что он написал.
	Targets []target
	// Fallback разрешает читать без ключа, когда официальный путь недоступен:
	// ключ не введён, отозван или кончилась квота.
	Fallback bool
}

// target is one line of the settings: либо канал, либо конкретный стрим.
//
// Разница не косметическая. Канал надо искать: он «очнётся» когда-нибудь сам,
// и ждать приходится. Стрим уже известен — и искать нечего, к нему идут сразу,
// хоть он и не в эфире: человек дал адрес и ждёт именно его.
type target struct {
	ref   channelRef
	video string
	label string
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
		Description: "Чат YouTube Live: официальный Data API, а без ключа — внутренний API самого YouTube.",
		Version:     "0.2.0",
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
		labels := make([]any, 0, len(next.Targets))
		for _, t := range next.Targets {
			labels = append(labels, t.label)
		}
		send(protocol.KindAck, protocol.AckPayload{
			Values: map[string]any{
				"api_key":  next.APIKey,
				"channel":  labels,
				"fallback": next.Fallback,
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

	// Список целей: в строке может быть и канал, и стрим — разбираем каждую
	// строку отдельно, потому что ведут они себя по-разному.
	rawList, _ := values["channel"].([]any)
	seen := map[string]bool{}
	for _, item := range rawList {
		text := ""
		switch entry := item.(type) {
		case string:
			text = entry
		case map[string]any:
			text, _ = entry["value"].(string)
		case protocol.ChoiceValue:
			text = entry.Value
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}

		ref, err := parseChannelRef(text)
		if err != nil {
			fields["channel"] = err.Error()
			continue
		}
		if seen[ref.Normalized] {
			continue
		}
		seen[ref.Normalized] = true

		next.Targets = append(next.Targets, target{
			ref:   ref,
			video: ref.Video,
			label: ref.Normalized,
		})
	}

	// Резервное чтение — по умолчанию включено: без него плагин умирал бы
	// вместе с ключом, а читать чат нужно всегда.
	if flag, ok := values["fallback"].(bool); ok {
		next.Fallback = flag
	} else {
		next.Fallback = true
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
	// Video is set when the address points at one concrete stream rather than a
	// channel. Такой адрес не ищется: у него и так всё известно, и ждать, пока
	// канал окажется в эфире, незачем.
	Video string
}

// videoIDPattern узнаёт id трансляции в голом виде и в ссылке.
var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

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
		return channelRef{}, errors.New("укажите ссылку на канал или на стрим, @handle или ID канала UC…")
	}

	// Голый id видео — частый случай: его копируют из адресной строки.
	if videoIDPattern.MatchString(value) {
		return channelRef{Normalized: "https://www.youtube.com/watch?v=" + value, Video: value}, nil
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
		if host != "youtube.com" && host != "www.youtube.com" && host != "m.youtube.com" &&
			host != "youtu.be" && host != "www.youtu.be" {
			return channelRef{}, errors.New("ожидалась ссылка на youtube.com")
		}

		// Ссылка на конкретный стрим. Такой адрес — не канал, а готовое видео:
		// искать по нему нечего, к нему идут сразу.
		if id := videoFromURL(parsed); id != "" {
			return channelRef{Normalized: "https://www.youtube.com/watch?v=" + id, Video: id}, nil
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
		// Псевдонимы /c/имя и /user/имя намеренно не принимаем: это не @handle, и
		// канал по такому имени может быть совсем другим. Считать их одним и тем
		// же — значит тихо подписаться не на то.
		return channelRef{}, errors.New("используйте ссылку на канал (@handle) или на стрим (watch?v=…)")
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

// videoFromURL достаёт id трансляции из адреса: и из youtube.com/watch?v=…, и из
// короткого youtu.be/….
func videoFromURL(u *url.URL) string {
	host := strings.ToLower(u.Hostname())

	if host == "youtu.be" || host == "www.youtu.be" {
		id := strings.Trim(u.Path, "/")
		if videoIDPattern.MatchString(id) {
			return id
		}
		return ""
	}

	if u.Path != "/watch" && u.Path != "/watch/" {
		return ""
	}
	id := u.Query().Get("v")
	if !videoIDPattern.MatchString(id) {
		return ""
	}
	return id
}

type apiError struct {
	Status  int
	Reason  string
	Message string
}

func redactAPIKey(value, apiKey string) string {
	if apiKey == "" {
		return value
	}
	return strings.ReplaceAll(value, apiKey, "[redacted]")
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
		return errors.New(redactAPIKey(err.Error(), apiKey))
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
		message := redactAPIKey(payload.Error.Message, apiKey)
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
	// Channel — канал, который вещает эту трансляцию. В ленте подписываться
	// надо на него, а не на id видео: человек в ленте видит названия каналов, и
	// одиннадцать символов вместо названия там неуместны.
	Channel string
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
				Title        string `json:"title"`
				ChannelID    string `json:"channelId"`
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
			ID      string `json:"id"`
			Snippet struct {
				Title        string `json:"title"`
				ChannelTitle string `json:"channelTitle"`
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
		lives = append(lives, liveVideo{
			ID:      item.ID,
			Title:   title,
			Chat:    chatID,
			Channel: item.Snippet.ChannelTitle,
		})
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

		// Ключ теперь необязателен: без него плагин читает резервным путём,
		// и останавливать его незачем. Раньше здесь был просто выход с
		// требованием ключа, и резервный путь был недостижим вовсе.
		if len(config.Targets) == 0 {
			emitConnection(protocol.ConnectionOffline, "Укажите канал или стрим YouTube", 0)
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
		// Список целей сравнить нельзя — это слайс, и структура с ним не сравнивается.
		// Поэтому своё состояние сравниваем по ключу и по подпискам: того, что на
		// самом деле решает, читаем ли мы то же самое, достаточно.
		if sameTargets(getSettings(), config) {
			// Ничего не изменилось: продолжаем ждать следующего эфира.
		} else {
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
			// Паузу здесь пропустить нельзя: чат может быть завершён мгновенно,
			// и без неё цикл станет крутиться раз в секунду, наскоро перезаходя
			// на один и тот же адрес и отправляя YouTube запрос за запросом.
			if !waitInterval(ctx, chatEndedInterval) {
				return
			}
			continue
		}
		if err != nil {
			logInfo(err.Error())
			emitConnection(protocol.ConnectionOffline, err.Error(), 0)
			if keyUnusable(err) {
				// Ключ без смены настроек не починится: ни новый не придёт,
				// ни отозванный не оживёт. Стучаться каждые пять секунд
				// бессмысленно — это только расход квоты и шум в журнале.
				waitForChange(ctx)
				continue
			}
			// А вот сетевые обрывы и смену формы ответа ждать нельзя: плагин
			// обязан вернуться сам. Поэтому ждём и настройки, и подсказку времени.
			if !waitChangeOrTimeout(ctx, errorRetryInterval) {
				return
			}
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

// waitChangeOrTimeout — ждём либо смены настроек, либо подсказки времени.
// Вернуться по часам нужно всегда: сетевые ошибки и обрывы приходят без
// участия человека, и ждать его реакции тут не на что.
func waitChangeOrTimeout(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-changed:
		return true
	case <-timer.C:
		return true
	}
}

func runConfiguration(ctx context.Context, config settings) error {
	// Ключ может быть не вписан, отозван или исчерпал квота — во всех трёх
	// случаях работаем, просто без него. Решает watcher, а не этот код.
	w := newWatcher(config.APIKey, "")
	w.allowFallback = config.Fallback

	// Стримы, указанные адресом, читаем сразу: искать по адресу нечего, человек
	// уже сказал, какой именно. Каналы — ждём, пока проснутся.
	direct := directVideos(config.Targets)
	channels := channelTargets(config.Targets)

	if len(direct) > 0 {
		logInfo("Стримы указаны адресом, иду сразу без поиска: " + strings.Join(direct, ", "))
		w.channel = direct[0]
		w.title = "YouTube"
		err := w.watchDirect(ctx, direct)
		// Список может смешивать стримы и каналы. Стрим, который уже
		// закончился, не воскреснет, и крутить его по кругу незачем: если в
		// списке есть ещё и каналы, идут они.
		if err == nil || len(channels) == 0 ||
			errors.Is(err, errSettingsChanged) || ctx.Err() != nil {
			return err
		}
		logInfo("Стримы из списка закончились, перехожу к каналам")
		w.channel = ""
		w.title = ""
	} else if len(channels) == 0 {
		return errChannelNotFound
	}

	channelID, channelTitle, err := w.resolveAll(ctx, channels)
	if err != nil {
		return err
	}
	w.title = channelTitle
	if channelTitle == "" {
		w.title = firstLabel(channels)
	}
	logInfo(fmt.Sprintf("Канал YouTube разрешён: %s (%s), чтение: %s", w.title, channelID, w))

	// Поиск ведёт watcher: он сам решит, официальным способом или резервным, и
	// переключится, если с ключом что-то не так.
	for {
		lives, err := w.searchLoop(ctx)
		if err == nil {
			logInfo(fmt.Sprintf("Найдено активных чатов YouTube: %d (чтение: %s)", len(lives), w))
			return w.watch(ctx, lives)
		}
		if errors.Is(err, errSettingsChanged) || ctx.Err() != nil {
			return errSettingsChanged
		}
		if errors.Is(err, errNoLive) {
			emitConnection(protocol.ConnectionOffline,
				"Эфир не найден. Канал молчит — поиск продолжится автоматически.", 0)
			return errSearchExhausted
		}
		if errors.Is(err, errChannelNotFound) {
			return err
		}
		logInfo("Поиск эфира не удался: " + err.Error())
		if !waitInterval(ctx, searchInterval) {
			return errSettingsChanged
		}
	}
}

// directVideos — из списка целей те, что оказались ссылками на стрим.
func directVideos(targets []target) []string {
	var out []string
	for _, t := range targets {
		if t.video != "" {
			out = append(out, t.video)
		}
	}
	return out
}

// channelTargets — те, что каналы: их надо искать, они просыпаются сами.
func channelTargets(targets []target) []target {
	var out []target
	for _, t := range targets {
		if t.video == "" {
			out = append(out, t)
		}
	}
	return out
}

func firstLabel(targets []target) string {
	if len(targets) == 0 {
		return ""
	}
	return targets[0].label
}

// sameTargets сравнивает настройки по тому, что на самом деле влияет на чтение.
func sameTargets(a, b settings) bool {
	if a.APIKey != b.APIKey || a.Fallback != b.Fallback || len(a.Targets) != len(b.Targets) {
		return false
	}
	for i := range a.Targets {
		if a.Targets[i].label != b.Targets[i].label || a.Targets[i].video != b.Targets[i].video {
			return false
		}
	}
	return true
}

func watchLiveChats(ctx context.Context, apiKey, channel, channelTitle string, lives []liveVideo) error {
	emitConnection(protocol.ConnectionConnecting, "Подключаю поток liveChatMessages.streamList", 0)

	var conn *grpc.ClientConn
	var err error
	for attempt := 0; ; attempt++ {
		dialCtx, cancel := context.WithTimeout(ctx, grpcDialTimeout)
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
		if attempt+1 >= maxStreamRestarts {
			return fmt.Errorf("%w: gRPC handshake: %v", errOfficialReadBroken, err)
		}
		// Дальше API-ключ не поможет: если HTTP(S) работает, но gRPC недоступен,
		// watcher должен быстро перейти на резервный публичный путь.
		if !waitInterval(ctx, time.Duration(attempt+1)*time.Second) {
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
	var joinedOnce sync.Once
	var onlineOnce sync.Once

	for _, video := range lives {
		video := video
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Подписываемся на тот канал, который вещает именно эту трансляцию.
			// Когда в списке смешаны каналы и стримы, общее имя на всех было бы
			// враньём: под одной подписью оказывались бы разные каналы.
			label := video.Channel
			if label == "" {
				label = channelTitle
			}
			if label == "" {
				label = channel
			}
			err := readOneChat(childCtx, client, apiKey, label, video, func() {
				joinedOnce.Do(func() {
					send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{
						Channel: label,
						Name:    label,
					})
				})
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

	// caughtUp означает, что мы дочитали историю и стоим на живом краю. Пока
	// он не установлен, всё, что старше backlogWindow, — это хвост прошлого:
	// эфир идёт часами, и при каждом старте плагин получал сорок минутых
	// сообщений, в которых никто уже не участвует.
	caughtUp := false

	// restarts считает подряд идущие попытки, в которых сервер не вернул
	// ни одного ответа. Если ответ пришёл — соединение работает, даже когда
	// в тихом чате в нём нет сообщений.
	restarts := 0
	gotAny := false
	connectedReported := false

	// Поток, который несколько раз подряд не смог вернуть даже ответ, не
	// держим на экспоненциальных паузах бесконечно: пора попробовать резерв.
	brokeTooOften := func() bool { return restarts >= maxStreamRestarts }

	// Логируем, с чем именно идём в официальный API. Пустой id чата — самая
	// вероятная причина того, что поток открывается и сразу умирает, а по
	// сообщению об ошибке это не разглядеть.
	chatHint := video.Chat
	if len(chatHint) > 12 {
		chatHint = chatHint[:12] + "…"
	}
	logInfo(fmt.Sprintf("Читаю официально: видео %s, id чата %q, страница %q",
		video.ID, chatHint, pageToken))

	for {
		if ctx.Err() != nil {
			return errSettingsChanged
		}
		request := &ytproto.LiveChatMessageListRequest{
			LiveChatId: stringPtr(video.Chat),
			Part:       []string{"id", "snippet", "authorDetails"},
		}
		// MaxResults в streaming RPC не используется. Первое подключение без
		// pageToken получает допустимую серверную порцию истории, которую ниже
		// ограничиваем по времени, а повторное подключение продолжает с курсора.
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

			// Не тратим 5 + 10 + 20 секунд, прежде чем признать, что gRPC
			// вообще не может открыть поток. Это и есть случай для keyless fallback.
			restarts++
			if brokeTooOften() {
				return fmt.Errorf("%w: StreamList: %v", errOfficialReadBroken, err)
			}
			if !gotAny {
				emitConnection(protocol.ConnectionReconnecting, "Восстанавливаю поток YouTube live chat", attempt+1)
			}
			logInfo("Поток чата не открылся, повторяю: " + err.Error())
			if !waitInterval(ctx, retryDelay(attempt)) {
				return errSettingsChanged
			}
			attempt++
			continue
		}

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
				if !received {
					restarts++
				} else {
					restarts = 0
				}
				if brokeTooOften() {
					return fmt.Errorf("%w: обрыв %v", errOfficialReadBroken, recvErr)
				}
				// Пока не пришло ни одного сообщения, молчание с перезаходом —
				// ещё может быть поломкой, и человек должен её видеть. А вот
				// после сообщений перезаход обычен: в тихом чате сервер закрывает
				// опрос каждые несколько секунд, и пугать «переподключением»
				// там, где всё работает, — враньё в интерфейсе.
				if !gotAny {
					emitConnection(protocol.ConnectionReconnecting,
						"Восстанавливаю поток YouTube live chat", attempt+1)
				}
				break
			}
			received = true
			gotAny = true
			// Успешный ответ означает, что канал до сервера живой: сбрасываем
			// exponential backoff и счётчик неудачных открытий. В тихом чате
			// ответ может быть пустым — это не повод считать его ошибкой.
			attempt = 0
			restarts = 0
			if !connectedReported {
				onConnected()
				connectedReported = true
			}
			offline := response.GetOfflineAt() != ""
			// Храним последний непустой continuation token. Пустое поле в одном
			// ответе не должно сбрасывать курсор: иначе при следующем соединении
			// чтение стартует заново и сообщения между попытками можно потерять.
			pageToken, _ = updateContinuationToken(pageToken, response.GetNextPageToken())
			for _, item := range response.GetItems() {
				if item == nil || !remember(item.GetId()) {
					continue
				}
				// Пока мы не дочитали историю, всё старше окна — прошлое. В ленте
				// этому не место: человек открывает чат и видит сорок минутых
				// сообщений, в которых он уже не участвовал.
				if !caughtUp {
					published, err := time.Parse(time.RFC3339Nano, item.GetSnippet().GetPublishedAt())
					if err == nil && time.Since(published) > backlogWindow {
						continue
					}
					caughtUp = true
				}
				if err := emitChatMessage(channel, item); err != nil {
					logInfo("Не удалось обработать сообщение YouTube: " + err.Error())
				}
				if item.GetSnippet() != nil &&
					item.GetSnippet().GetType() == ytproto.LiveChatMessageSnippet_TypeWrapper_CHAT_ENDED_EVENT {
					return errChatEnded
				}
			}
			if offline {
				return errChatEnded
			}
			if pageToken == "" {
				// Токен следующей страницы не выдан — ждать больше нечего, надо
				// просто спросить заново. Это обычный опрос, а не поломка.
				break
			}
		}

		if ctx.Err() != nil {
			return errSettingsChanged
		}
		delay, nextAttempt := streamRetryDelay(attempt, received)
		if !received {
			logInfo("YouTube завершил поток без данных; пробую возобновить")
		}
		if !waitInterval(ctx, delay) {
			return errSettingsChanged
		}
		attempt = nextAttempt
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

// updateContinuationToken preserves the last usable cursor when a
// response omits nextPageToken. Resetting it to empty restarts history retrieval.
func updateContinuationToken(current, next string) (string, bool) {
	next = strings.TrimSpace(next)
	if next == "" {
		return current, false
	}
	return next, true
}

// streamRetryDelay uses exponential backoff only when no response was received.
// After a valid response, an ended RPC is resumed quickly from its page token.
func streamRetryDelay(attempt int, received bool) (time.Duration, int) {
	if received {
		return streamRestartDelay, 0
	}
	return retryDelay(attempt), attempt + 1
}

func stringPtr(value string) *string { return &value }

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
