// Command goodgame is the GoodGame chat plugin for studio.
//
// The kernel launches it and talks over stdin/stdout in the protocol format:
// one JSON message per line. On start the plugin introduces itself and takes
// the channel list the user filled in.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"

	"studio/core/protocol"
)

const (
	scanLimit = 64 * 1024
)

// Настройки объявляются теми же типами, что и в ядре.
type option = protocol.Option

// options describes the settings page the kernel renders for us.
// Каналы приходят выбором: значение — то, что человек написал, а подпись —
// название, которое GoodGame знает про этот канал.
var options = []protocol.Option{{
	Key:     "channels",
	Type:    protocol.OptionSelectMultiple,
	Label:   "Каналы",
	Section: "Каналы",
	Hint:    "Номер канала, ник или ссылка goodgame.ru — плагин сам найдёт, что это",
	// Что подставить в поле, пока не знаем: у GoodGame нет списка каналов в
	// одном месте, а лазить по сайту за каждым каналом — дело не ядра.
	Placeholder: "53029",
	Default:     []any{},
}}

// channels — каналы, которые плагин читает: номер и название. Ядро присылает
// их из config.json, читать их будет горутина вебсокетов, поэтому доступ
// защищён.
var (
	channelsMu sync.RWMutex
	channels   []protocol.ChoiceValue
	// connection — соединение с чатом.
	connection *socket
	// channelsChanged будит соединение, когда список каналов изменился. Один
	// элемент достаточно: важно, что список посмотрели свежим, а не сколько
	// раз он менялся.
	channelsChanged = make(chan struct{}, 1)
)

func setChannels(next []protocol.ChoiceValue) {
	channelsMu.Lock()
	channels = next
	channelsMu.Unlock()

	// Список изменился: соединение должно это увидеть и пересобрать каналы.
	select {
	case channelsChanged <- struct{}{}:
	default:
	}
}

func currentChannels() []protocol.ChoiceValue {
	channelsMu.RLock()
	defer channelsMu.RUnlock()
	return slices.Clone(channels)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// База смайлов: открывается при запуске и целиком лежит в памяти. Плагин
	// должен иметь её всегда, поэтому отсутствие файла — не поломка, а пустая
	// база: обновить можно кнопкой.
	book, err := openBook()
	if err != nil {
		sayLog("база смайлов не открыта: " + err.Error())
		book = &smileBook{}
	} else {
		defer book.Close()
		sayLog(fmt.Sprintf("смайлов в базе: %d", len(book.get())))
	}

	commands := commandsFrom(ctx, os.Stdin)

	// Плагин сам объявляет себя: ядро читает это первым, без запроса.
	send(protocol.KindPluginChat, describe())

	connection = newSocket(book)
	connection.watch(ctx, channelList)

	for {
		select {
		case <-ctx.Done():
			return

		case text := <-connection.logs:
			sayLog(text)

		case event := <-connection.events:
			write(event.message)

		case line, ok := <-commands:
			if !ok {
				// Ядро закрыло stdin — или его вообще не было, например при
				// запуске руками. Дальше работать не с кем.
				sayLog("stdin closed, exiting")
				return
			}
			if handle(ctx, book, line) {
				return
			}
		}
	}
}

// updateSmiles переписывает базу смайлов и отвечает ядру. Пока идёт скачивание,
// плагин считается занятым: страница держит кнопку и ждёт.
func updateSmiles(ctx context.Context, book *smileBook) {
	if book.store == nil {
		sayError(errNoSmilesBase.Error())
		return
	}

	sayWaiting("Скачиваю смайлы")

	if err := book.Update(ctx); err != nil {
		sayError("смайлы не обновились: " + err.Error())
		return
	}

	count := len(book.get())
	sayLog(fmt.Sprintf("смайлов в базе: %d", count))

	// Число — главное, время второстепенно. Раньше строка читалась «12.10 17:28
	// в 5879»: предлог остался, а слово после него потерялось.
	if at, ok := book.UpdatedAt(); ok {
		sendDone(fmt.Sprintf("База смайлов обновлена: %d (%s)", count, at.Local().Format("02.01 в 15:04")))
		return
	}
	sendDone(fmt.Sprintf("База смайлов обновлена: %d", count))
}

// channelList — каналы, к которым надо быть подключены прямо сейчас, вместе с
// именами: имя нужно и чтобы войти, и чтобы потом сказать, из чего вышли.
func channelList() []protocol.ChoiceValue {
	var out []protocol.ChoiceValue
	for _, channel := range currentChannels() {
		if _, err := channelID(channel.Value); err != nil {
			sayLog(fmt.Sprintf("канал %q пропущен: %v", channel.Label, err))
			continue
		}
		out = append(out, channel)
	}
	return out
}

// describe is what we tell about ourselves: the kind of plugin, its title and
// the form of its settings.
func describe() protocol.InfoPayload {
	return protocol.InfoPayload{
		Title:       "GoodGame",
		Description: "Чат игровой площадки goodgame.ru",
		Version:     "0.1.0",
		Options:     options,
		Buttons:     []protocol.Button{smilesButton},
	}
}

// handle processes one command and reports whether the plugin should stop.
func handle(ctx context.Context, book *smileBook, line string) bool {
	message, err := protocol.Decode([]byte(line))
	if err != nil {
		sayError(fmt.Sprintf("cannot parse %q: %v", line, err))
		return false
	}

	switch message.Kind {
	case protocol.KindSetOptions:
		var payload protocol.SetOptionsPayload
		if err := message.PayloadInto(&payload); err != nil {
			sayError("cannot read the options: " + err.Error())
			return false
		}
		settled, err := apply(ctx, payload.Values)
		if err != nil {
			// Настройки не приняты: ядро их не сохранит, а страница
			// подсветит поля, на которые указывает Fields.
			var rejected *optionError
			if errors.As(err, &rejected) {
				sayFields(rejected.Error(), rejected.Fields(), rejected.Items())
			} else {
				sayError(err.Error())
			}
			return false
		}
		sendAck(settled)

	case protocol.KindSmilesUpdate:
		updateSmiles(ctx, book)

	case protocol.KindQuit:
		sendAck(nil)
		return true

	default:
		sayError("unknown command " + message.Kind)
	}
	return false
}

// apply проверяет настройки и, только если всё в порядке, берёт их в работу.
// Возвращает то, с чем плагин работает на самом деле: человек мог ввести ник
// или ссылку, а в конфиг должно лечь «номер — название», иначе при следующем
// запуске придётся разрешать то же самое заново.
//
// Каналы, которые уже проверены и не менялись, повторно не спрашиваем у
// GoodGame: это и медленно, и результат всё равно тот же.
func apply(ctx context.Context, values map[string]any) (map[string]any, error) {
	if values == nil {
		return nil, nil
	}

	raw, ok := values["channels"]
	if !ok {
		return nil, nil
	}

	list, ok := raw.([]any)
	if !ok {
		return nil, &optionError{fields: map[string]string{
			"channels": "ожидался список каналов",
		}}
	}

	resolved, reused, todo := resolveKnown(list)
	// Что именно не получилось: страница отметит эту запись, а не всё поле.
	bad := map[string]string{}
	if len(todo) > 0 {
		// Живой запрос к GoodGame: говорим ядру, что ждать. Сколько именно
		// каналов ищем — детали, человеку они ничего не говорят.
		sayWaiting("Спрашиваю GoodGame")

		resolver := newResolver()
		for _, input := range todo {
			value, label, err := resolver.resolveChannel(ctx, input)
			if err != nil {
				bad[input] = err.Error()
				continue
			}
			entry := protocol.ChoiceValue{Value: value, Label: label}
			// Ключи — и результат, и то, что человек ввёл: ссылка
			// «https://goodgame.ru/E.Wolf» сама в себя не превращается,
			// и по одному только результату её потом не найти.
			resolved[input] = entry
			resolved[value] = entry
			resolved[label] = entry
		}
	}

	if len(bad) > 0 {
		// Каждая проблемная запись подписана своим текстом — повторять их
		// здесь списком незачем, хватит самого количества.
		return nil, &optionError{
			fields: map[string]string{
				"channels": fmt.Sprintf("Не удалось добавить %s", plural(len(bad), "канал", "канала", "каналов")),
			},
			items: map[string]map[string]string{"channels": bad},
		}
	}

	// Один канал может прийти разными словами — «miker» и «5» — это одно и то
	// же, поэтому сверяемся по номеру, а не по тому, что ввёл человек.
	next := make([]protocol.ChoiceValue, 0, len(list))
	added := map[string]bool{}
	for _, input := range orderedInput(list) {
		entry, ok := lookupResolved(resolved, input)
		if !ok || added[entry.Value] {
			continue
		}
		added[entry.Value] = true
		next = append(next, entry)
	}
	if len(next) == 0 && len(list) == 0 {
		next = nil
	}

	setChannels(next)

	if reused > 0 {
		sayLog(fmt.Sprintf("каналов без изменений: %d, проверено заново: %d", reused, len(todo)))
	}
	sayLog(fmt.Sprintf("following %d channels: %s", len(next), strings.Join(channelLabels(next), ", ")))

	// Ядро сохранит именно это: номер канала и его название, а не то, что
	// человек набрал в поле.
	return map[string]any{"channels": next}, nil
}

// resolveKnown раскладывает вход по полам: то, что уже знаем, берём из
// прошлой проверки, остальное отдаём на сверку с GoodGame.
func resolveKnown(list []any) (resolved map[string]protocol.ChoiceValue, reused int, todo []string) {
	resolved = map[string]protocol.ChoiceValue{}
	for _, channel := range currentChannels() {
		resolved[channel.Value] = channel
		resolved[channel.Label] = channel
	}

	seen := map[string]bool{}
	for _, item := range list {
		input := channelInput(item)
		if input == "" || seen[input] {
			continue
		}
		seen[input] = true

		if _, ok := resolved[input]; ok {
			reused++
			continue
		}

		// Пара «номер — название» — это уже проверенный канал: он приходит
		// из конфига, куда его положил сам плагин. Спрашивать GoodGame
		// заново незачем, а его название может в любой момент сменить
		// написание — и конфиг начнёт дёргаться на каждом запуске.
		if entry, ok := knownPair(item); ok {
			resolved[entry.Value] = entry
			resolved[entry.Label] = entry
			resolved[input] = entry
			reused++
			continue
		}

		todo = append(todo, input)
	}
	return resolved, reused, todo
}

// knownPair достаёт канал из пары, которую когда-то уже разрешили: номер
// канала плюс его название. Голое число или строка — это то, что человек ввёл
// сам, такие проверять надо.
func knownPair(item any) (protocol.ChoiceValue, bool) {
	entry, ok := item.(map[string]any)
	if !ok {
		choice, isPair := item.(protocol.ChoiceValue)
		if !isPair {
			return protocol.ChoiceValue{}, false
		}
		entry = map[string]any{"value": choice.Value, "label": choice.Label}
	}

	value, _ := entry["value"].(string)
	label, _ := entry["label"].(string)
	value = strings.TrimSpace(value)
	label = strings.TrimSpace(label)

	if label == "" || label == value || !isChannelID(value) {
		return protocol.ChoiceValue{}, false
	}
	return protocol.ChoiceValue{Value: value, Label: label}, true
}

// lookupResolved находит уже проверенный канал по тому, что прислали.
func lookupResolved(resolved map[string]protocol.ChoiceValue, input string) (protocol.ChoiceValue, bool) {
	if entry, ok := resolved[input]; ok {
		return entry, true
	}
	// Прислали пару «номер — название»: подходит любой из ключей.
	trimmed := strings.TrimSpace(input)
	if entry, ok := resolved[trimmed]; ok {
		return entry, true
	}
	return protocol.ChoiceValue{}, false
}

// orderedInput сохраняет порядок, в котором пользователь перечислил каналы.
func orderedInput(list []any) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		if input := channelInput(item); input != "" {
			out = append(out, input)
		}
	}
	return out
}

func channelLabels(channels []protocol.ChoiceValue) []string {
	out := make([]string, 0, len(channels))
	for _, channel := range channels {
		out = append(out, channel.Label)
	}
	return out
}

// channelInput вытаскивает то, что ввёл пользователь: строку или значение из
// пары «номер — название».
func channelInput(item any) string {
	switch entry := item.(type) {
	case string:
		return strings.TrimSpace(entry)
	case map[string]any:
		if label, ok := entry["label"].(string); ok && strings.TrimSpace(label) != "" {
			return strings.TrimSpace(label)
		}
		if value, ok := entry["value"].(string); ok {
			return strings.TrimSpace(value)
		}
	case protocol.ChoiceValue:
		if entry.Label != "" {
			return entry.Label
		}
		return entry.Value
	}
	return ""
}

// optionError — настройки не приняты, и вот почему: по полям и, где это
// возможно, по конкретным значениям внутри поля.
type optionError struct {
	fields map[string]string
	items  map[string]map[string]string
}

func (e *optionError) Error() string {
	for _, message := range e.fields {
		return message
	}
	return "настройки отклонены"
}

// Fields возвращает ошибки по полям: ядро их покажет у нужных полей.
func (e *optionError) Fields() map[string]string {
	return e.fields
}

// Items возвращает ошибки по отдельным значениям поля.
func (e *optionError) Items() map[string]map[string]string {
	return e.items
}

// commandsFrom reads the kernel's NDJSON lines. The channel is closed when
// stdin is closed or the context is cancelled.
func commandsFrom(ctx context.Context, in *os.File) <-chan string {
	out := make(chan string)

	go func() {
		defer close(out)

		lines := make(chan string)
		go func() {
			defer close(lines)

			scanner := bufio.NewScanner(in)
			scanner.Buffer(make([]byte, 0, 4096), scanLimit)
			for scanner.Scan() {
				lines <- scanner.Text()
			}
			_ = scanner.Err()
		}()

		for {
			select {
			case <-ctx.Done():
				return
			case line, ok := <-lines:
				if !ok {
					return
				}
				select {
				case out <- line:
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out
}

func write(message protocol.Message) {
	line, err := protocol.Encode(message)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goodgame: cannot encode %s: %v\n", message.Kind, err)
		return
	}
	os.Stdout.Write(line)
}

func send(kind string, payload any) {
	line, err := protocol.Encode(protocol.MustNew(kind, payload))
	if err != nil {
		fmt.Fprintf(os.Stderr, "goodgame: cannot encode %s: %v\n", kind, err)
		return
	}
	os.Stdout.Write(line)
}

// sendAck подтверждает, что настройки приняты. Если плагин что-то привёл к
// своему виду, он кладёт итог в values — ядро сохранит именно его.
func sendAck(values map[string]any) {
	send(protocol.KindAck, protocol.AckPayload{Values: values})
}

// sendDone подтверждает команду и говорит, что именно вышло. Словами: человек
// нажал кнопку и должен понять результат, а не листать лог.
func sendDone(message string) {
	send(protocol.KindAck, protocol.AckPayload{Message: message})
}

func sayLog(text string) {
	send(protocol.KindLog, protocol.LogPayload{Level: "info", Message: text})
}

func sayError(text string) {
	send(protocol.KindError, protocol.ErrorPayload{Message: text})
}

// plural подставляет нужную форму слова: 1 канал, 2 канала, 5 каналов.
func plural(n int, one, few, many string) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return fmt.Sprintf("%d %s", n, one)
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		return fmt.Sprintf("%d %s", n, few)
	default:
		return fmt.Sprintf("%d %s", n, many)
	}
}

// sayWaiting сообщает ядру, что плагин занят и ответит не сразу.
func sayWaiting(text string) {
	send(protocol.KindWaiting, protocol.WaitingPayload{Message: text})
}

// sayFields отвечает отказом с указанием на конкретные поля и, если удалось,
// на конкретные значения внутри них.
func sayFields(text string, fields map[string]string, items map[string]map[string]string) {
	send(protocol.KindError, protocol.ErrorPayload{Message: text, Fields: fields, Items: items})
}
