package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Каналы GoodGame можно назвать тремя способами, и пользователю не придётся
// знать разницу:
//
//	53029                    номер канала — берём как есть
//	E.Wolf                   логин       — ищем номер по логину
//	https://goodgame.ru/E.Wolf   ссылка     — достаём ник, потом как логин
//
// Номер и название — разные вещи: «53029» — фактическое значение,
// «E.Wolf» — то, что видит человек. Настройки хранят оба.
const (
	channelStatusURL = "https://goodgame.ru/api/getchannelstatus?id=%s&fmt=json"
	channelHost      = "goodgame.ru"

	maxChannelIDDigits = 8
	requestTimeout     = 10 * time.Second
)

// Сообщения об ошибках уходят прямо в форму, поэтому с заглавной буквы:
// это текст для человека, а не для разработчика.
var (
	errNoChannel = errors.New("Канал не найден")
)

// resolver ходит в API GoodGame и превращает то, что ввёл пользователь, в
// номер канала и его название.
type resolver struct {
	client *http.Client
	base   string // куда ходим; в тестах подменяется
}

// apiBase — адрес API GoodGame. Подменяется в тестах.
var apiBase = "https://goodgame.ru"

func newResolver() *resolver {
	return &resolver{
		client: &http.Client{Timeout: requestTimeout},
		base:   apiBase,
	}
}

// resolveChannel догадывается, что ввёл пользователь, и возвращает номер
// канала и его название.
func (r *resolver) resolveChannel(ctx context.Context, input string) (value, label string, err error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", errors.New("ничего не введено")
	}

	nickname := input
	switch {
	case looksLikeURL(input):
		nickname, err = nicknameFromURL(input)
		if err != nil {
			return "", "", err
		}
		// Из ссылки получаем ник, но номер у GoodGame не в ней — идём в профиль.
	case isChannelID(input):
		value = input
	}
	return r.resolve(ctx, value, nickname, input)
}

// resolve достраивает недостающее: номер известен — берём название из ссылки,
// название известно — ищем номер по логину.
func (r *resolver) resolve(ctx context.Context, value, nickname, input string) (string, string, error) {
	if value == "" {
		if nickname == "" {
			nickname = input
		}
		if err := r.idByNickname(ctx, nickname, &value); err != nil {
			return "", "", err
		}
	}
	if nickname == "" || isChannelID(nickname) {
		// Название придётся взять из ссылки самого канала.
		if err := r.nameByID(ctx, value, &nickname); err != nil {
			return "", "", err
		}
	}
	return value, nickname, nil
}

// idByNickname спрашивает профиль и берёт номер канала оттуда.
func (r *resolver) idByNickname(ctx context.Context, nickname string, into *string) error {
	var user struct {
		Stream *struct {
			ID int `json:"id"`
		} `json:"stream"`
	}
	endpoint := r.base + "/api/4/users/" + url.PathEscape(nickname)
	if err := r.get(ctx, endpoint, &user); err != nil {
		return err
	}
	if user.Stream == nil || user.Stream.ID == 0 {
		return errNoChannel
	}
	*into = strconv.Itoa(user.Stream.ID)
	return nil
}

// nameByID спрашивает статус канала и достаёт название из ссылки: отдельного
// поля с именем у них нет.
func (r *resolver) nameByID(ctx context.Context, id string, into *string) error {
	endpoint := strings.Replace(channelStatusURL, "https://goodgame.ru", r.base, 1)

	// На несуществующий канал GoodGame отвечает пустым массивом, а не объектом,
	// поэтому сначала смотрим, что именно пришло.
	var raw any
	if err := r.get(ctx, fmt.Sprintf(endpoint, id), &raw); err != nil {
		return err
	}

	status, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("Канал %s не найден", id)
	}

	entry, ok := status[id].(map[string]any)
	if !ok {
		return fmt.Errorf("Канал %s не найден", id)
	}

	rawURL, _ := entry["url"].(string)
	name, err := nicknameFromURL(rawURL)
	if err != nil {
		// Номер мы уже знаем — покажем его, чтобы человек понял, о ком речь.
		*into = id
		return nil
	}
	*into = name
	return nil
}

func (r *resolver) get(ctx context.Context, endpoint string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")

	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("GoodGame не ответил: %w", err)
	}
	defer response.Body.Close()

	switch response.StatusCode {
	case http.StatusNotFound:
		// Имени пользователя нет: значит нет и его канала.
		return errNoChannel
	case http.StatusOK:
	default:
		return fmt.Errorf("GoodGame ответил %s", response.Status)
	}

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("непонятный ответ GoodGame: %w", err)
	}
	return nil
}

// isChannelID — это номер канала: от одной до восьми цифр.
func isChannelID(input string) bool {
	if len(input) == 0 || len(input) > maxChannelIDDigits {
		return false
	}
	for _, r := range input {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// looksLikeURL ловит и «https://goodgame.ru/E.Wolf», и «goodgame.ru/E.Wolf».
func looksLikeURL(input string) bool {
	lower := strings.ToLower(input)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, channelHost+"/")
}

// nicknameFromURL достаёт ник из ссылки на канал.
func nicknameFromURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("пустая ссылка")
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + trimmed
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("это не похоже на ссылку: %w", err)
	}

	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if part := parts[i]; part != "" {
			return part, nil
		}
	}
	return "", errors.New("в ссылке не указан ник")
}
