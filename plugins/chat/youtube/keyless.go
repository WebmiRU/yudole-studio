package main

// Запасной путь: чтение чата YouTube без ключа и без регистрации.
//
// Официальный путь через Data API хорош, пока ключ живой. Но ключи кончаются,
// а чат нужен всегда: квота обнуляется раз в сутки, а эфир идёт в любое время.
// Поэтому рядом живёт второй способ — тот самый, которым пользуется сам сайт.
//
// Отличия от официального пути, которые стоит держать в голове:
//
//   - Страница трансляции отдаётся только с кукой согласия. Без неё YouTube
//     отвечает страницей вовсе без чата, и кажется, что эфира не существует.
//   - Токен в разметке есть и у записей: у завершённой трансляции он тоже
//     находится. Значит, «живой» узнаётся только по isLive, а не по наличию
//     токена.
//   - Скорость чтения задаёт сервер полем timeoutMs — обычно десять секунд.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"studio/core/protocol"
)

// htmlEscape is what goes into the markup we build ourselves. Пользовательский
// текст сюда попадает через наш же html.EscapeString, иначе реплика может
// принести с собой теги.
func htmlEscape(text string) string { return html.EscapeString(text) }

const (
	ytBase      = "https://www.youtube.com"
	ytWatch     = "https://www.youtube.com/watch?v="
	ytFeeds     = "https://www.youtube.com/feeds/videos.xml?channel_id="
	ytChatPath  = "/youtubei/v1/live_chat/get_live_chat"
	ytPageLimit = 2 << 20 // страница эфира с вложениями
)

// consentCookie нужна каждому запросу к youtube.com. Без неё сервер отдаёт
// страницу без чата — тихо, без всякой ошибки, и понять, что дело не в чтении,
// а в куке, можно только долго удивляться.
const consentCookie = "CONSENT=YES+cb; SOCS=CAI"

var (
	innertubeKeyRe = regexp.MustCompile(`"INNERTUBE_API_KEY":"([^"]+)"`)
	innertubeVerRe = regexp.MustCompile(`"INNERTUBE_CLIENT_VERSION":"([^"]+)"`)
	// tokenRe ищет токен продолжения. Их в разметке несколько: от того, какой
	// именно вкладки чата, и годятся все, но берём первый.
	tokenRe   = regexp.MustCompile(`"continuation"\s*:\s*"([^"]{40,})"`)
	videoIDRe = regexp.MustCompile(`"videoId":"([A-Za-z0-9_-]{11})"`)
	// titleRe — название для ленты: сперва идёт то, что в <title> страницы.
	titleRe = regexp.MustCompile(`<title>([^<]*)</title>`)
	// ownerRe и ownerIDRe — кто вещает. Без них в ленте вместо названия канала
	// светится пустое «Канал», а это ни о чём не говорит.
	ownerRe    = regexp.MustCompile(`"ownerChannelName":"([^"]+)"`)
	ownerIDRe  = regexp.MustCompile(`"(?:externalChannelId|channelId)":"(UC[A-Za-z0-9_-]{20,})"`)
	legacyName = regexp.MustCompile(`<link itemprop="name" content="([^"]+)">`)
)

// errNoLive означает, что эфира сейчас нет. Это не поломка: канал молчит.
var errNoLive = fmt.Errorf("активных эфиров нет")

// ytGet is a request to youtube.com with the consent cookie attached.
func ytGet(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Chrome/120")
	req.Header.Set("Cookie", consentCookie)
	req.Header.Set("Accept-Language", "en")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("youtube отдал HTTP %d на %s", resp.StatusCode, rawURL)
	}
	return io.ReadAll(io.LimitReader(resp.Body, ytPageLimit))
}

// keylessResolve turns an @handle, a channel URL or a UC… id into the channel
// id. Страница канала отдаёт его в externalId — там же, где лежит всё, что
// знает о канале сама страница.
func keylessResolve(ctx context.Context, configured string) (channelID, title string, err error) {
	if channelIDPattern.MatchString(configured) {
		return configured, "", nil
	}

	handle := configured
	if strings.Contains(handle, "youtube.com/") || strings.Contains(handle, "youtu.be/") {
		handle = handleFromURL(handle)
	}
	handle = strings.TrimPrefix(handle, "@")
	if handle == "" {
		return "", "", errChannelNotFound
	}

	body, err := ytGet(ctx, ytBase+"/@"+handle)
	if err != nil {
		return "", "", err
	}
	page := string(body)

	if id := externalIDRe.FindStringSubmatch(page); id != nil {
		title = channelTitleRe.FindStringSubmatch(page)[1]
		return id[1], title, nil
	}
	return "", "", errChannelNotFound
}

var (
	externalIDRe   = regexp.MustCompile(`"externalId":"(UC[A-Za-z0-9_-]{20,})"`)
	channelTitleRe = regexp.MustCompile(`"channelMetadataRenderer":\{"title":"([^"]+)"`)
)

// handleFromURL takes the handle out of any of YouTube's address shapes:
// /@name, /channel/UC…, /c/name, /user/name, /live/UC….
func handleFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	path := strings.Trim(u.Path, "/")

	if strings.HasPrefix(path, "@") {
		return strings.TrimPrefix(path, "@")
	}
	for _, prefix := range []string{"channel/", "c/", "user/", "live/"} {
		if strings.HasPrefix(path, prefix) {
			return strings.TrimPrefix(path, prefix)
		}
	}
	return path
}

// keylessFindLives looks for live streams through the channel's own feed.
//
// Фид отдаёт последние пятнадцать видео и стоит копейки: страницу эфира качать
// приходится только для тех, что в фиде появились впервые. Проверять «живой
// или запись» приходится на странице: в самом фиде этого нет, а токен чата
// находится и у завершённых трансляций.
func keylessFindLives(ctx context.Context, channelID string) ([]liveVideo, error) {
	body, err := ytGet(ctx, ytFeeds+channelID)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	order := []string{}
	titles := map[string]string{}
	for _, m := range feedVideoRe.FindAllStringSubmatch(string(body), -1) {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		order = append(order, id)
		titles[id] = m[2]
	}

	// Свежие сверху, и смотреть дальше смысла нет: эфир, если он идёт, это
	// одно из последних видео.
	if len(order) > 4 {
		order = order[:4]
	}

	// Проверяем несколько свежих видео параллельно. Последовательная проверка
	// четырёх страниц добавляла задержки друг к другу: при медленном ответе
	// YouTube первые сообщения могли ждать суммарное время всех запросов.
	type pageResult struct {
		page watchPage
		err  error
	}
	pages := make([]pageResult, len(order))
	var wg sync.WaitGroup
	for i, id := range order {
		i, id := i, id
		wg.Add(1)
		go func() {
			defer wg.Done()
			pages[i].page, pages[i].err = keylessWatchPage(ctx, id)
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	lives := make([]liveVideo, 0, len(order))
	for i, id := range order {
		result := pages[i]
		if result.err != nil {
			logInfo(fmt.Sprintf("Страница %s не открылась: %v", id, result.err))
			continue
		}
		if !result.page.isLive {
			continue
		}
		title := titles[id]
		if title == "" {
			title = result.page.title
		}
		lives = append(lives, liveVideo{ID: id, Title: title, Page: &result.page})
	}
	return lives, nil
}

var feedVideoRe = regexp.MustCompile(`<yt:videoId>([A-Za-z0-9_-]{11})</yt:videoId>(?s).*?<title>([^<]*)</title>`)

type watchPage struct {
	isLive bool
	title  string
	// owner и ownerID — канал, который вещает. Их показываем в ленте, иначе
	// человек видит вместо канала пустое слово «Канал».
	owner   string
	ownerID string
	token   string
	apiKey  string
	ver     string
}

// keylessWatchPage opens the stream page and pulls out of it everything needed
// to read the chat: the liveness, the continuation token and the public
// InnerTube key, which lies in the page in plain sight.
func keylessWatchPage(ctx context.Context, videoID string) (watchPage, error) {
	var page watchPage

	body, err := ytGet(ctx, ytWatch+videoID+"&hl=en")
	if err != nil {
		return page, err
	}
	html := string(body)

	// Живой эфир отличается двумя признаками сразу: длина нулевая, а рядом
	// стоит broadcastId. Одного токена мало — он есть и у записей.
	page.isLive = strings.Contains(html, `"isLive":true`) ||
		(strings.Contains(html, `"lengthSeconds":"0"`) && strings.Contains(html, "liveStreamability"))
	page.token = firstSubmatch(tokenRe, html)
	page.apiKey = firstSubmatch(innertubeKeyRe, html)
	page.ver = firstSubmatch(innertubeVerRe, html)
	if m := titleRe.FindStringSubmatch(html); m != nil {
		page.title = strings.TrimSpace(m[1])
	}
	page.owner = firstSubmatch(ownerRe, html)
	if page.owner == "" {
		page.owner = firstSubmatch(legacyName, html)
	}
	page.ownerID = firstSubmatch(ownerIDRe, html)
	return page, nil
}

func firstSubmatch(re *regexp.Regexp, text string) string {
	if m := re.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// chatPage is the answer to one get_live_chat call.
type chatPage struct {
	messages  []keylessMessage
	continue_ string
	waitMS    int
	// offline — YouTube прямо сказал, что трансляция закончилась. Только это
	// и означает конец чата: пустой continuation после долгого ожидания —
	// обычное дело, а не прощание.
	offline bool
}

// keylessRun — кусок реплики: либо текст, либо эмодзи.
type keylessRun struct {
	Text  string `json:"text"`
	Emoji struct {
		EmojiID string `json:"emojiId"`
		Custom  struct {
			Thumbnails []struct {
				URL    string `json:"url"`
				Width  int    `json:"width"`
				Height int    `json:"height"`
			} `json:"thumbnails"`
		} `json:"customEmojiRendered"`
	} `json:"emoji"`
}

// keylessMessage — одна реплика.
//
// Форму YouTube менял: сейчас имя автора лежит в authorName, а текст — внутри
// message.runs. Прежняя разметка держала их на верхнем уровне, и на ней реплики
// разбирались, а на новой молча терялись все. Потерянные реплики не видно нигде:
// эфир выглядит просто пустым, и кажется, что читать нечего. Поэтому понимаем
// обе формы.
type keylessMessage struct {
	ID       string `json:"id"`
	TimeUSec string `json:"timestampUsec"`

	Author struct {
		Name string `json:"simpleText"`
	} `json:"authorName"`
	AuthorID string `json:"authorExternalChannelId"`

	Message struct {
		Runs []keylessRun `json:"runs"`
	} `json:"message"`

	// Прежняя форма, оставлена на всякий случай.
	OldAuthor struct {
		Name string `json:"simpleText"`
	} `json:"author"`
	OldRuns []keylessRun `json:"runs"`
}

// authorName достаёт имя автора из любой из форм.
func (m keylessMessage) authorName() string {
	if m.Author.Name != "" {
		return m.Author.Name
	}
	return m.OldAuthor.Name
}

// runs достаёт куски реплики из любой из форм.
func (m keylessMessage) runs() []keylessRun {
	if len(m.Message.Runs) > 0 {
		return m.Message.Runs
	}
	return m.OldRuns
}

// keylessChatRaw спрашивает у InnerTube следующую порцию и отдаёт ответ как есть,
// разбирать его — дело вызывающего.
func keylessChatRaw(ctx context.Context, page watchPage, continuation string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"context": map[string]any{"client": map[string]any{
			"clientName": "WEB", "clientVersion": page.ver, "hl": "en",
		}},
		"continuation": continuation,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		ytBase+ytChatPath+"?key="+page.apiKey, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Chrome/120")
	req.Header.Set("Cookie", consentCookie)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ytPageLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("live chat отдал HTTP %d", resp.StatusCode)
	}
	return raw, nil
}

// keylessChatPage asks for the next batch of messages.
func keylessChatPage(ctx context.Context, page watchPage, continuation string) (chatPage, error) {
	raw, err := keylessChatRaw(ctx, page, continuation)
	if err != nil {
		return chatPage{}, err
	}

	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return chatPage{}, fmt.Errorf("ответ live chat не разобран: %w", err)
	}

	// continuationContents встречается и объектом, и массивом — в зависимости от
	// того, чему именно продолжение продолжено. Искать только одним способом
	// значит либо читать пустоту, либо падать на смене формы; поэтому пробуем
	// оба.
	live := digMap(root, "continuationContents", "liveChatContinuation")
	if len(live) == 0 {
		for _, section := range digSlice(root["continuationContents"]) {
			if cont := digMap(digMapAny(section), "liveChatContinuation"); len(cont) > 0 {
				live = cont
				break
			}
		}
	}
	out := chatPage{waitMS: 5000}

	actions, _ := live["actions"].([]any)
	for _, item := range actions {
		entry := digMap(item, "addChatItemAction", "item")
		if node := entry["liveChatTextMessageRenderer"]; node != nil {
			blob, _ := json.Marshal(node)
			var msg keylessMessage
			json.Unmarshal(blob, &msg)
			if msg.authorName() == "" {
				continue
			}
			out.messages = append(out.messages, msg)
		}
	}

	out.continue_, out.waitMS = selectChatContinuation(live)

	out.offline = hasOfflineMarker(root)
	return out, nil
}

// selectChatContinuation picks one usable cursor in a stable order. YouTube can
// return several continuation kinds together; choosing the last item (or ranging
// over a map) can select a seek/reload cursor instead of the live chat cursor,
// causing gaps, repeated history, or long waits.
func selectChatContinuation(live map[string]any) (string, int) {
	const defaultWait = 5000
	priority := []string{
		"invalidationContinuationData",
		"timedContinuationData",
		"reloadContinuationData",
		"liveChatReplayContinuationData",
		"playerSeekContinuationData",
	}
	type candidate struct {
		token string
		wait  int
	}
	found := make(map[string]candidate)
	continuations, _ := live["continuations"].([]any)
	for _, item := range continuations {
		wrapper := digMapAny(item)
		for _, key := range priority {
			data := digMapAny(wrapper[key])
			token, _ := data["continuation"].(string)
			if token == "" {
				continue
			}
			wait := defaultWait
			if ms, ok := numberOf(data["timeoutMs"]); ok && ms > 0 {
				wait = int(ms)
			}
			if _, exists := found[key]; !exists {
				found[key] = candidate{token: token, wait: wait}
			}
		}
	}
	for _, key := range priority {
		if selected, ok := found[key]; ok {
			return selected.token, selected.wait
		}
	}
	return "", defaultWait
}

// hasOfflineMarker ищет в ответе прямое указание, что трансляция закончилась.
//
// Искать надо именно маркер, а не отсутствие сообщений: после долгого ожидания
// пустой ответ — норма, в эфире может быть тихо десять минут подряд, и это не
// конец. Обрывать чтение по тишине значит объявить живой эфир мёртвым.
func hasOfflineMarker(v any) bool {
	switch node := v.(type) {
	case map[string]any:
		for key, child := range node {
			if strings.Contains(key, "offlineSlate") || key == "isLiveNow" {
				if b, ok := child.(bool); !ok || !b {
					return true
				}
				continue
			}
			if hasOfflineMarker(child) {
				return true
			}
		}
	case []any:
		for _, child := range node {
			if hasOfflineMarker(child) {
				return true
			}
		}
	}
	return false
}

// digMap walks a decoded JSON tree, returning an empty map rather than panicking
// on a missing key: the shape of the answer changes, и падать тут не на что.
func digMap(v any, keys ...string) map[string]any {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return map[string]any{}
		}
		if v = m[k]; v == nil {
			return map[string]any{}
		}
	}
	m, _ := v.(map[string]any)
	return m
}

func digSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// digMapAny достаёт объект, а отсутствие ключа отличает пустым значением: так
// видно, ветку мы нашли или промахнулись.
func digMapAny(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// numberOf читает число, которое YouTube может прислать и числом, и строкой.
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		parsed, err := strconv.ParseFloat(n, 64)
		return parsed, err == nil
	}
	return 0, false
}

// keylessWatchChats reads the chat of every live stream, one goroutine per
// stream, and ends when all of them are over.
func keylessWatchChats(ctx context.Context, channel, channelTitle string, lives []liveVideo) error {
	type result struct {
		video liveVideo
		err   error
	}
	results := make(chan result, len(lives))
	var wg sync.WaitGroup

	for _, video := range lives {
		video := video
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Название канала выясняется уже на странице трансляции, поэтому
			// подписаться на канал мы можем только оттуда — и подписываемся
			// именно на тот канал, который реально вещает этот эфир.
			results <- result{video: video, err: keylessReadOne(ctx, channelTitle, video, func(name string) {
				label := name
				if label == "" {
					label = channelTitle
				}
				send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{
					Channel: name,
					Name:    label,
				})
				emitConnection(protocol.ConnectionOnline, "Читаю чат YouTube без ключа", 0)
			})}
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
			return errSettingsChanged
		case item, ok := <-results:
			if !ok {
				return errChatEnded
			}
			remaining--
			if errors.Is(item.err, errSettingsChanged) {
				return errSettingsChanged
			}
			if errors.Is(item.err, errChatEnded) || errors.Is(item.err, errNoLive) {
				logInfo("Чат трансляции завершён: " + item.video.Title)
				continue
			}
			if item.err != nil {
				return item.err
			}
		}
	}
	return errChatEnded
}

// keylessReadOne is the read loop of a single stream: ask, wait as long as the
// server said, ask again.
// onConnected вызывается именем канала, который выяснился на странице.
func keylessReadOne(ctx context.Context, channelTitle string, video liveVideo, onConnected func(name string)) error {
	var page watchPage
	if video.Page != nil {
		page = *video.Page
	} else {
		var err error
		page, err = keylessWatchPage(ctx, video.ID)
		if err != nil {
			return err
		}
	}
	if page.token == "" || page.apiKey == "" {
		return errNoLive
	}

	// Название канала берём со страницы самой трансляции: там оно точно есть и
	// точно правильное. Заглушка вроде «YouTube» или пустое «Канал» человеку
	// ничего не объясняет, а id канала хотя бы что-то значит.
	channel := page.owner
	if channel == "" {
		channel = page.ownerID
	}
	if channel == "" {
		channel = channelTitle
	}

	seen := map[string]bool{}
	token := page.token

	// Токен получен — значит чат уже читается, и можно честно сказать «в сети».
	// Ждать первого сообщения для этого нельзя: в эфире бывает тихо по минуте,
	// и человек в это время видит «офлайн» у живого плагина.
	onConnected(channel)
	// lastMessage — когда последний раз что-то приходило. Тишина сама по себе
	// ни о чём не говорит, но бесконечное молчание стоит перепроверить: вдруг
	// эфир всё-таки закончился, а плагин об этом не узнал.
	lastMessage := time.Now()
	consecutiveFailures := 0
	reportedDegraded := false

	for {
		if ctx.Err() != nil {
			return errSettingsChanged
		}
		batch, err := keylessChatPage(ctx, page, token)
		if err != nil {
			if ctx.Err() != nil {
				return errSettingsChanged
			}

			// Единичный сбой опроса не должен завершать всю сессию: тогда
			// supervisor заново искал эфир, а каждый повтор печатал в чате
			// «Подключились к YouTube». Остаёмся на месте и повторяем тот же
			// continuation; иначе сообщения между двумя сессиями теряются.
			consecutiveFailures++
			if consecutiveFailures == 1 {
				logInfo("Временная ошибка чтения чата YouTube; повторяю запрос")
			}
			if consecutiveFailures >= 2 && !reportedDegraded {
				emitConnection(protocol.ConnectionReconnecting,
					"Опрос чата YouTube временно не отвечает", consecutiveFailures)
				reportedDegraded = true
			}

			// Если токен протух или формат ответа изменился, периодически
			// обновляем страницу трансляции и continuation. Ошибка обновления
			// не завершает чтение: следующая попытка повторится с backoff.
			if consecutiveFailures == 3 || consecutiveFailures%5 == 0 {
				if fresh, refreshErr := keylessWatchPage(ctx, video.ID); refreshErr == nil {
					if fresh.token != "" && fresh.apiKey != "" {
						page = fresh
						token = fresh.token
					}
				}
			}

			if !waitInterval(ctx, keylessRetryDelay(consecutiveFailures)) {
				return errSettingsChanged
			}
			continue
		}
		if consecutiveFailures > 0 {
			logInfo("Чтение чата YouTube восстановлено")
			if reportedDegraded {
				emitConnection(protocol.ConnectionOnline, "Читаю чат YouTube без ключа", 0)
			}
			consecutiveFailures = 0
			reportedDegraded = false
		}

		for _, msg := range batch.messages {
			if msg.ID != "" {
				if seen[msg.ID] {
					continue
				}
				seen[msg.ID] = true
			}
			lastMessage = time.Now()
			emitKeylessMessage(channel, msg)
		}

		if batch.offline {
			return errChatEnded
		}
		if batch.continue_ != "" {
			token = batch.continue_
		}
		// Токен пустой — просто продолжаем с прежним: после ожидания сервер
		// имеет право не прислать ничего нового.

		if time.Since(lastMessage) >= silenceRecheck {
			fresh, err := keylessWatchPage(ctx, video.ID)
			if err == nil && fresh.token != "" {
				page = fresh
				token = fresh.token
				logInfo(fmt.Sprintf(
					"Молчит уже %s, эфир %s — продолжаю",
					time.Since(lastMessage).Round(time.Minute), describeLiveness(fresh)))
				lastMessage = time.Now()
			} else if err == nil && fresh.token == "" {
				return errNoLive
			}
		}

		if !waitInterval(ctx, time.Duration(batch.waitMS)*time.Millisecond) {
			return errSettingsChanged
		}
	}
}

// keylessRetryDelay bounds retries for the public chat endpoint. Keep these
// delays short enough not to lose many messages but long enough to avoid hammering
// YouTube when its chat endpoint temporarily rejects requests.
func keylessRetryDelay(failures int) time.Duration {
	switch {
	case failures <= 1:
		return time.Second
	case failures == 2:
		return 2 * time.Second
	case failures == 3:
		return 5 * time.Second
	case failures == 4:
		return 10 * time.Second
	case failures < 8:
		return 15 * time.Second
	default:
		return 30 * time.Second
	}
}

// silenceRecheck — сколько можно молчать, прежде чем перепроверить эфир. В
// эфире бывают паузы и по десять минут, но час молчания — уже повод спросить
// у самой страницы, не закончилось ли всё.
const silenceRecheck = 10 * time.Minute

// describeLiveness пишет по-человечески, живой эфир или нет.
func describeLiveness(page watchPage) string {
	if page.isLive {
		return "всё ещё идёт"
	}
	return "больше не идёт"
}

// emitKeylessMessage turns one message into what the kernel and the page expect.
//
// Кастомные эмоти приходят отдельными кусками и в тексте их нет: без подстановки
// они молча исчезли бы из реплики. Поэтому в html попадает всё, а в text —
// то, что человек написал буквами.
func emitKeylessMessage(channel string, msg keylessMessage) {
	var text, html strings.Builder
	for _, run := range msg.runs() {
		if run.Emoji.EmojiID != "" {
			if run.Emoji.Custom.Thumbnails != nil {
				pick := run.Emoji.Custom.Thumbnails
				best := pick[len(pick)-1]
				html.WriteString(fmt.Sprintf(
					`<img class="smile" src="%s" alt="%s">`,
					htmlEscape(best.URL), htmlEscape(run.Emoji.EmojiID)))
				continue
			}
			// Обычный эмодзи-юникод: в тексте он остаётся символом.
			text.WriteString(run.Emoji.EmojiID)
			html.WriteString(htmlEscape(run.Emoji.EmojiID))
			continue
		}
		text.WriteString(run.Text)
		html.WriteString(htmlEscape(run.Text))
	}

	if text.Len() == 0 && html.Len() == 0 {
		return
	}

	line := protocol.ChatLine{Src: text.String(), Text: text.String()}
	if strings.Contains(html.String(), "<img") {
		line.Html = html.String()
	}

	send(protocol.KindMessagePublic, protocol.ChatPayload{
		Channel: channel,
		User: protocol.ChatUser{
			Name:  msg.authorName(),
			ID:    msg.AuthorID,
			Login: msg.AuthorID,
		},
		Message: line,
	})
}
