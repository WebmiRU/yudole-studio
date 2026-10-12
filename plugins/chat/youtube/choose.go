package main

// Выбор пути чтения: официальный API, если ключ жив, иначе резервный.
//
// Ключ — вещь хрупкая: его можно не вписать, он может отозваться или у него
// может кончиться квота, а квота обнуляется раз в сутки. Чат же нужен всегда, и
// эфир идёт в любое время суток. Поэтому здесь не «ключ или ошибка», а «попробуй
// официально, а если не вышло — читай без него».
//
// Переключение одноразовое в пределах запуска: ключ, который отозвали или
// которому кончилась квота, повторно опрашивать не надо — от этого квота только
// утекает. Вернуться можно будет, когда человек пересохранит настройки.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"studio/core/protocol"
)

// errKeyUnusable говорит: официальный путь больше не годится, работаем без него.
var errKeyUnusable = errors.New("ключ YouTube непригоден, читаем без него")

// errQuotaGone — отдельная причина: ключ верный, но квота кончилась.
var errQuotaGone = errors.New("кончилась квота YouTube API")

// keyUnusable разбирает ошибку официального пути: годен ли ключ вообще.
func keyUnusable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errQuotaGone) {
		return true
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		// 400 — ключ неверен, 403 — отозван или исчерпал квоту, 401 — не авторизован.
		// Всё это лечится только резервным путём.
		return apiErr.Status == 400 || apiErr.Status == 401 || apiErr.Status == 403
	}

	// Чтение идёт по gRPC, и его ошибки приходят не в виде apiError: там свои
	// коды. Без их разбора резервный путь не включался бы никогда, а плагин
	// просто оставался «не в сети» после первой неудачи.
	switch status.Code(err) {
	case codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied,
		codes.ResourceExhausted, codes.Unimplemented, codes.NotFound:
		return true
	}
	return false
}

// quotaExhausted ловит в тексте ответа API слова про исчерпанную квоту: YouTube
// не всегда присылает её отдельным кодом, но всегда пишет об этом в сообщении.
func quotaExhausted(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "quotaexceeded") ||
		strings.Contains(lower, "dailylimitexceeded") ||
		strings.Contains(lower, "quota")
}

// watcher — то, ради чего всё затевалось: умеет искать эфир и читать его чат
// тем способом, который в данный момент доступен.
type watcher struct {
	apiKey    string
	channel   string
	title     string
	channelID string

	// allowFallback разрешает уйти в резерв, когда официальный путь не годится.
	// Выключается настройкой: человек, которому нужен именно официальный
	// режим, не должен молча получить другой.
	allowFallback bool

	// keyless становится истиной, когда официальный путь отказал. Дальше
	// пробовать ключ не будем: истёкшая квота от повторов не восстанавливается,
	// а резервный поиск всё равно работает без неё.
	keyless bool
}

// newWatcher decides, с чего начинать: есть ключ — начинаем официально, нет —
// сразу резервом. Ошибки официального пути обрабатываются лениво, при первой
// попытке: ключ может оказаться битым уже после сохранения настроек.
func newWatcher(apiKey, channel string) *watcher {
	return &watcher{
		apiKey:  strings.TrimSpace(apiKey),
		channel: channel,
		keyless: strings.TrimSpace(apiKey) == "",
	}
}

// useKeyless переводит watcher на резервный путь и говорит об этом вслух.
//
// Если резерв запрещён настройкой, переключения не происходит: человек явно
// попросил официальный режим, и молча выдать ему другой — значит соврать.
func (w *watcher) useKeyless(reason string) bool {
	if w.keyless {
		return true
	}
	if !w.allowFallback {
		logInfo("Резервное чтение запрещено настройкой, ключ непригоден: " + reason)
		return false
	}
	w.keyless = true
	logInfo("Перехожу на чтение без ключа: " + reason)
	emitConnection(protocol.ConnectionReconnecting, "Резервное чтение без ключа", 0)
	return true
}

// resolveAll разрешает в список целей: каналы уходят на channels.list (одна
// единица квоты) или на страницу канала, а стримы разрешать не нужно — у них
// уже есть адрес.
func (w *watcher) resolveAll(ctx context.Context, targets []target) (string, string, error) {
	ids := make([]string, 0, len(targets))
	titles := make([]string, 0, len(targets))

	for _, t := range targets {
		if t.video != "" {
			continue
		}
		id, title, err := w.resolveOne(ctx, t)
		if err != nil {
			return "", "", err
		}
		ids = append(ids, id)
		if title != "" {
			titles = append(titles, title)
		}
	}
	if len(ids) == 0 {
		return "", "", errChannelNotFound
	}

	joined := strings.Join(ids, ",")
	w.channelID = joined
	return joined, strings.Join(titles, ", "), nil
}

func (w *watcher) resolveOne(ctx context.Context, t target) (string, string, error) {
	if !w.keyless {
		id, title, err := resolveChannel(ctx, w.apiKey, t.label)
		if err == nil {
			return id, title, nil
		}
		if !keyUnusable(err) {
			return "", "", err
		}
		if !w.useKeyless(err.Error()) {
			return "", "", err
		}
	}
	return keylessResolve(ctx, t.label)
}

// watchDirect читает чаты стримов, указанных адресом. Поиска тут нет и быть не
// должно: человек дал конкретные ссылки, ждать, пока канал окажется в эфире,
// незачем — и незачем тратить на это квоту.
func (w *watcher) watchDirect(ctx context.Context, videos []string) error {
	lives := make([]liveVideo, 0, len(videos))
	for _, id := range videos {
		lives = append(lives, liveVideo{ID: id, Title: id})
	}
	if w.keyless {
		return keylessWatchChats(ctx, w.channel, w.title, lives)
	}

	// С ключом узнаем настоящие названия трансляций — они идут в ленту.
	// Без этого шага поиск не нужен, а videos.list стоит одну единицу.
	if named, err := w.nameLives(ctx, lives); err == nil {
		lives = named
	}

	err := watchLiveChats(ctx, w.apiKey, w.channel, w.title, lives)
	if err == nil || !w.allowFallback || !pathBroken(err) {
		return err
	}
	if !w.useKeyless(err.Error()) {
		return err
	}
	return keylessWatchChats(ctx, w.channel, w.title, lives)
}

// nameLives добирает названия трансляций. Ошибка здесь не страшна: читать можно
// и с id вместо названия.
func (w *watcher) nameLives(ctx context.Context, lives []liveVideo) ([]liveVideo, error) {
	ids := make([]string, 0, len(lives))
	for _, v := range lives {
		ids = append(ids, v.ID)
	}
	params := url.Values{
		"part": {"snippet,liveStreamingDetails"},
		"id":   {strings.Join(ids, ",")},
	}
	var response struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Title string `json:"title"`
			} `json:"snippet"`
			Streaming struct {
				ChatID string `json:"activeLiveChatId"`
			} `json:"liveStreamingDetails"`
		} `json:"items"`
	}
	if err := apiRequest(ctx, w.apiKey, "videos", params, &response); err != nil {
		return nil, err
	}
	byID := make(map[string]struct {
		title string
		chat  string
	}, len(response.Items))
	for _, item := range response.Items {
		byID[item.ID] = struct {
			title string
			chat  string
		}{item.Snippet.Title, item.Streaming.ChatID}
	}
	for i := range lives {
		if got, ok := byID[lives[i].ID]; ok {
			if got.title != "" {
				lives[i].Title = got.title
			}
			if got.chat != "" {
				lives[i].Chat = got.chat
			}
		}
	}
	return lives, nil
}

// findLives ищет живые эфиры.
//
// В официальном режиме это search.list, и вот он-то и стоит квоты: своя
// корзина в сто вызовов на сутки на весь проект. Поэтому, когда резервный путь
// разрешён, ищем эфир через него — фид канала бесплатен и ничем не ограничен.
// Так ключ тратится только на чтение, а не на поиск.
func (w *watcher) findLives(ctx context.Context) ([]liveVideo, error) {
	if w.keyless {
		return keylessFindLives(ctx, w.channelID)
	}
	if !w.allowFallback {
		return w.findLivesOfficial(ctx)
	}

	// Резерв разрешён: ищем эфир без ключа, потому что search.list стоит
	// отдельную квоту в сто вызовов на сутки, а фид канала бесплатен.
	return keylessFindLives(ctx, w.channelID)
}

func (w *watcher) findLivesOfficial(ctx context.Context) ([]liveVideo, error) {
	return findActiveLives(ctx, w.apiKey, w.channelID)
}

func (w *watcher) canUseFallback() bool {
	return w.allowFallback
}

// watch открывает чат найденных эфиров тем путём, который доступен.
// pathBroken сообщает, что официальный путь читать больше нечем и пора
// переходить на резервный.
//
// Причин две, и они разные: ключ негоден (или кончилась квота) — это keyUnusable;
// либо поток открывается и умирает, не отдав ни слова. Во втором случае ключ
// в порядке, и чинить можно только сменой пути. Смысл один: официально читать
// больше не выйдет.
func pathBroken(err error) bool {
	if err == nil {
		return false
	}
	return keyUnusable(err) || errors.Is(err, errOfficialReadBroken)
}

func (w *watcher) watch(ctx context.Context, lives []liveVideo) error {
	if w.keyless {
		return keylessWatchChats(ctx, w.channel, w.title, lives)
	}

	err := watchLiveChats(ctx, w.apiKey, w.channel, w.title, lives)
	if err == nil {
		return nil
	}
	if !w.canUseFallback() {
		return err
	}
	if !pathBroken(err) {
		return err
	}
	if !w.useKeyless(err.Error()) {
		return err
	}
	lives, findErr := keylessFindLives(ctx, w.channelID)
	if findErr != nil {
		return findErr
	}
	if len(lives) == 0 {
		return errChatEnded
	}
	return keylessWatchChats(ctx, w.channel, w.title, lives)
}

// searchLoop ищет эфир, пока не найдёт.
//
// Официальный поиск стоит отдельную квоту — сто вызовов search.list на сутки на
// весь проект, а это примерно час ожидания. Дальше поиск официальным способом
// бессмыслен: он либо упрётся в сутки, либо потратит чужую квоту. Поэтому после
// maxOfficialChecks переходим на фид канала — он бесплатен и ничем не ограничен,
// а ждать он умеет сколько угодно.
func (w *watcher) searchLoop(ctx context.Context) ([]liveVideo, error) {
	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil, errSettingsChanged
		}

		lives, err := w.findLives(ctx)
		if err == nil && len(lives) > 0 {
			return lives, nil
		}
		if err != nil && !errors.Is(err, errNoLive) && ctx.Err() != nil {
			return nil, errSettingsChanged
		}

		// Попытки официального поиска кончились — дальше только фид.
		if !w.keyless && attempt >= maxOfficialChecks {
			logInfo(fmt.Sprintf(
				"Официальный поиск израсходовал %d попыток, перехожу на фид канала — он без квоты",
				maxOfficialChecks))
			if w.useKeyless("исчерпан лимит официального поиска") {
				return nil, errNoLive // вернёмся к ожидаению и найдём эфир фидом
			}
		}

		if !waitInterval(ctx, searchInterval) {
			return nil, errSettingsChanged
		}
	}
}

// maxOfficialChecks — сколько раз ищем эфир официальным search.list, прежде чем
// перейти на фид. Пятнадцать минут ожидания: эфир либо уже идёт и находится с
// первой попытки, либо человек его ещё не запустил и ждать незачем.
const maxOfficialChecks = 15

// searchWindow остаётся для совместимости с прежним поведением: по нему
// решаем, что канал действительно молчит.
const searchWindow = time.Hour

func (w *watcher) String() string {
	if w.keyless {
		return "без ключа"
	}
	return fmt.Sprintf("с ключом %s…", lastFour(w.apiKey))
}

func lastFour(s string) string {
	if len(s) <= 4 {
		return s
	}
	return s[len(s)-4:]
}
