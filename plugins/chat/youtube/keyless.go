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

	lives := make([]liveVideo, 0, len(order))
	for _, id := range order {
		page, err := keylessWatchPage(ctx, id)
		if err != nil {
			logInfo(fmt.Sprintf("Страница %s не открылась: %v", id, err))
			continue
		}
		if !page.isLive {
			continue
		}
		title := titles[id]
		if title == "" {
			title = page.title
		}
		lives = append(lives, liveVideo{ID: id, Title: title})
	}
	return lives, nil
}

var feedVideoRe = regexp.MustCompile(`<yt:videoId>([A-Za-z0-9_-]{11})</yt:videoId>(?s).*?<title>([^<]*)</title>`)

type watchPage struct {
	isLive bool
	title  string
	token  string
	apiKey string
	ver    string
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
}

type keylessMessage struct {
	ID     string
	Author struct {
		Name  string `json:"simpleText"`
		ID    string `json:"id"`
		Login string `json:"login"`
	}
	Runs []struct {
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
}

// keylessChatPage asks for the next batch of messages.
func keylessChatPage(ctx context.Context, page watchPage, continuation string) (chatPage, error) {
	body, err := json.Marshal(map[string]any{
		"context": map[string]any{"client": map[string]any{
			"clientName": "WEB", "clientVersion": page.ver, "hl": "en",
		}},
		"continuation": continuation,
	})
	if err != nil {
		return chatPage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		ytBase+ytChatPath+"?key="+page.apiKey, strings.NewReader(string(body)))
	if err != nil {
		return chatPage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Chrome/120")
	req.Header.Set("Cookie", consentCookie)

	resp, err := httpClient.Do(req)
	if err != nil {
		return chatPage{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, ytPageLimit))
	if err != nil {
		return chatPage{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return chatPage{}, fmt.Errorf("live chat отдал HTTP %d", resp.StatusCode)
	}

	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return chatPage{}, fmt.Errorf("ответ live chat не разобран: %w", err)
	}

	live := digMap(root, "continuationContents", "liveChatContinuation")
	out := chatPage{waitMS: 5000}

	actions, _ := live["actions"].([]any)
	for _, item := range actions {
		entry := digMap(item, "addChatItemAction", "item")
		if node := entry["liveChatTextMessageRenderer"]; node != nil {
			blob, _ := json.Marshal(node)
			var msg keylessMessage
			json.Unmarshal(blob, &msg)
			if msg.Author.Name == "" {
				continue
			}
			out.messages = append(out.messages, msg)
		}
	}

	conts, _ := live["continuations"].([]any)
	for _, c := range conts {
		for _, v := range digSlice(c) {
			m, _ := v.(map[string]any)
			if tok, _ := m["continuation"].(string); tok != "" {
				out.continue_ = tok
			}
			if ms, ok := m["timeoutMs"].(float64); ok && ms > 0 {
				out.waitMS = int(ms)
			}
		}
	}
	return out, nil
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
			results <- result{video: video, err: keylessReadOne(ctx, channel, video, func() {
				send(protocol.KindChannelJoinSuccess, protocol.ChannelPayload{
					Channel: channel,
					Name:    channelTitle,
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
func keylessReadOne(ctx context.Context, channel string, video liveVideo, onConnected func()) error {
	page, err := keylessWatchPage(ctx, video.ID)
	if err != nil {
		return err
	}
	if page.token == "" || page.apiKey == "" {
		return errNoLive
	}

	seen := map[string]bool{}
	connected := false
	token := page.token

	for {
		if ctx.Err() != nil {
			return errSettingsChanged
		}
		batch, err := keylessChatPage(ctx, page, token)
		if err != nil {
			if ctx.Err() != nil {
				return errSettingsChanged
			}
			return err
		}

		for _, msg := range batch.messages {
			if msg.ID != "" {
				if seen[msg.ID] {
					continue
				}
				seen[msg.ID] = true
			}
			if !connected {
				connected = true
				onConnected()
			}
			emitKeylessMessage(channel, msg)
		}

		if batch.continue_ == "" {
			return errChatEnded
		}
		token = batch.continue_

		if !waitInterval(ctx, time.Duration(batch.waitMS)*time.Millisecond) {
			return errSettingsChanged
		}
	}
}

// emitKeylessMessage turns one message into what the kernel and the page expect.
//
// Кастомные эмоти приходят отдельными кусками и в тексте их нет: без подстановки
// они молча исчезли бы из реплики. Поэтому в html попадает всё, а в text —
// то, что человек написал буквами.
func emitKeylessMessage(channel string, msg keylessMessage) {
	var text, html strings.Builder
	for _, run := range msg.Runs {
		if run.Emoji.EmojiID != "" {
			if run.Emoji.Custom.Thumbnails != nil {
				pick := run.Emoji.Custom.Thumbnails
				best := pick[len(pick)-1]
				html.WriteString(fmt.Sprintf(
					`<img class="smile" src="%s" alt="%s" loading="lazy">`,
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
			Name:  msg.Author.Name,
			ID:    msg.Author.ID,
			Login: msg.Author.Login,
		},
		Message: line,
	})
}
