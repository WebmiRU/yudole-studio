package main

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"studio/core/protocol"
	"studio/plugins/chat/goodgame/chat"
)

// Смайлы лежат в файле рядом с плагином и не ездят в интернет при каждом
// запуске: список у GoodGame меняется редко, а качать его на старте — значит
// зависеть от сети при запуске. Обновляет его человек, кнопкой.
//
// Имя файла — data.db, а не smiles.db: база не только про смайлы, и переименуем
// её ещё раз, когда туда ляжет что-то ещё.
const (
	smilesFile = "data.db"

	// Ключи в таблице meta.
	metaSmilesUpdatedAt  = "smiles_updated_at"
	metaSmilesCompiledAt = "smiles_compiled_at"
	metaSmilesTimezone   = "smiles_timezone_offset"
	metaSmilesPacked     = "smiles_packed"

	// smilesMaxSize — потолок ответа. Файл смайлов весит единицы мегабайт;
	// если пришло в разы больше, значит это не он, и мы не будем читать
	// бесконечный поток в память.
	smilesMaxSize = 32 << 20

	smilesTimeout = 60 * time.Second
)

// smilesURL is where we take the list from. Переменная, а не константа: в тесте
// свой сервер подставляется, а вживую — ровно тот адрес, что у GoodGame.
var smilesURL = chat.SmilesURL

// smile — как смайл пишется в тексте и где его картинка. В базе лежит всё,
// что прислал GoodGame, а в оперативку берём только это: остальное — служебные
// поля сайта, и держать их в памяти незачем.
type smile struct {
	// Code is the name as it is written in a message, with the colons:
	// ":jokie5:". Ключ в карте — именно он: в разборе строки мы вырезаем кусок
	// вместе с двоеточиями и идём в карту напрямую. Собирать ключ по кускам на
	// каждое вхождение — лишняя работа на каждой реплике.
	Code string
	// Image is what to show: гифка, если она есть, иначе самая большая
	// статичная. Решено один раз при загрузке, а не на каждое вхождение.
	Image string
}

// smileSet is the base in memory. Common smileys win over channel ones when the
// codes match: in a message we do not know which channel it came from, and a
// smiley that most people can see is the better guess.
type smileSet map[string]smile

// Lookup finds a smiley by the code as written, with the colons.
func (s smileSet) Lookup(code string) (smile, bool) {
	found, ok := s[code]
	return found, ok
}

// smilesMaxCode is how long a piece between two colons may be. Смайлы GoodGame —
// латиница, цифры, тире и подчёркивание, и длиннее тридцати они не бывают.
// Потолок нужен для другого: в обычном тексте двоеточия стоят далеко друг от
// друга («встреча в 12:30»), и без ограничения мы носились бы за такой парой
// через всю оставшуюся строку.
const smilesMaxCode = 64

// Views turns a line into the two the page needs: текст без кодов смайлов и тот
// же текст с картинками вместо них.
//
// Оба получаются за один проход. Идти по строке дважды — значит разбирать её
// дважды, а на каждую реплику это лишняя работа.
//
// Если смайлов не нашлось, вторая строка пустая, а первая — это исходная
// строка целиком, без единого выделения памяти: это подавляющее большинство
// реплик. Собирать второй раз то же самое только для того, чтобы его
// выбросить, — работа на каждое сообщение впустую.
func (s smileSet) Views(src string) (string, string) {
	// Пока смайлов не нашли, обе части остаются nil и мы просто идём по строке.
	var plain, rich []byte
	mark := 0

	for i := 0; i < len(src); {
		open := strings.IndexByte(src[i:], ':')
		if open < 0 {
			break
		}
		open += i

		closing := strings.IndexByte(src[open+1:], ':')
		if closing < 0 {
			break
		}
		closing += open + 1

		// Кусок вместе с двоеточиями — готовый ключ. Состав кода проверять не
		// надо: в карте лежат только настоящие смайлы, а всё остальное просто не
		// найдётся.
		if closing-open <= smilesMaxCode {
			if found, ok := s[src[open:closing+1]]; ok {
				if plain == nil {
					// Запас на разметку смайла — иначе буфер будет расти в
					// геометрической прогрессии.
					plain = make([]byte, 0, len(src))
					rich = make([]byte, 0, len(src)+128)
				}

				plain = append(plain, src[mark:open]...)
				rich = appendEscaped(rich, src[mark:open], false)
				rich = append(rich, `<img class="smile" src="`...)
				rich = appendEscaped(rich, found.Image, true)
				rich = append(rich, `" alt="`...)
				rich = appendEscaped(rich, found.Code, true)
				rich = append(rich, `" />`...)
				mark = closing + 1
			}
		}

		// Идём с закрывающего двоеточия: так мы проверяем каждую пару соседних
		// двоеточий ровно один раз и не возвращаемся назад.
		i = closing
	}

	if plain == nil {
		return src, ""
	}
	plain = append(plain, src[mark:]...)
	rich = appendEscaped(rich, src[mark:], false)
	return string(plain), string(rich)
}

// appendEscaped adds text to the buffer, hiding the characters that would
// otherwise be read as markup. Если экранировать нечего, строка уходит целиком,
// без побайтовой проверки каждого символа по пути.
func appendEscaped(out []byte, s string, quotes bool) []byte {
	start := 0
	for i := 0; i < len(s); i++ {
		var replacement string
		switch s[i] {
		case '&':
			replacement = `&amp;`
		case '<':
			replacement = `&lt;`
		case '>':
			replacement = `&gt;`
		case '"':
			// В тексте кавычка не опасна, в значении атрибута — обрывает его.
			if !quotes {
				continue
			}
			replacement = `&#34;`
		default:
			continue
		}

		out = append(out, s[start:i]...)
		out = append(out, replacement...)
		start = i + 1
	}
	return append(out, s[start:]...)
}

// store is the smiley base on disk.
type store struct {
	db *sql.DB
}

// openStore opens or creates the base next to the plugin. Путь можно
// переопределить — в тестах своя файловая система не нужна.
func openStore() (*store, error) {
	path := os.Getenv("GOODGAME_SMILES_DB")
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("найти свой файл: %w", err)
		}
		path = filepath.Join(filepath.Dir(exe), smilesFile)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("открыть %s: %w", path, err)
	}

	// Один писатель: обновление переписывает таблицу целиком, и два
	// одновременных обновления не должны мешать друг другу.
	db.SetMaxOpenConns(1)

	s := &store{db: db}
	if err := s.prepare(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *store) prepare() error {
	// В базе смайл хранится целиком — таким его прислал GoodGame. Поля сайта
	// нам сейчас не нужны, но перекачивать файл из-за отсутствия базы и
	// держать всё это в памяти смысла нет.
	const schema = `
CREATE TABLE IF NOT EXISTS smiles (
	name        TEXT    NOT NULL,
	channel     TEXT    NOT NULL DEFAULT '',
	id          TEXT    NOT NULL DEFAULT '',
	bind        TEXT    NOT NULL DEFAULT '',
	donat       INTEGER NOT NULL DEFAULT 0,
	premium     INTEGER NOT NULL DEFAULT 0,
	internal_id INTEGER NOT NULL DEFAULT 0,
	paid        INTEGER NOT NULL DEFAULT 0,
	animated    TEXT    NOT NULL DEFAULT '',
	tag         TEXT    NOT NULL DEFAULT '',
	tags        TEXT    NOT NULL DEFAULT '',
	img         TEXT    NOT NULL DEFAULT '',
	img_big     TEXT    NOT NULL DEFAULT '',
	img_gif     TEXT    NOT NULL DEFAULT '',
	PRIMARY KEY (name, channel)
) STRICT;

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("создать таблицы смайлов: %w", err)
	}
	return nil
}

func (s *store) Close() error { return s.db.Close() }

// Replace rewrites the whole base and notes when it happened. Смайлов, которых
// больше нет, не остаётся: в базе должно быть ровно то, что прислал GoodGame.
func (s *store) Replace(file chat.Smiles, list []chat.Smiley, at time.Time) (err error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("начать замену смайлов: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.ExecContext(context.Background(), `DELETE FROM smiles`); err != nil {
		return fmt.Errorf("очистить базу смайлов: %w", err)
	}

	statement, err := tx.PrepareContext(context.Background(), `
INSERT INTO smiles (name, channel, id, bind, donat, premium, internal_id, paid,
                    animated, tag, tags, img, img_big, img_gif)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("подготовить вставку смайла: %w", err)
	}
	defer statement.Close()

	for _, item := range list {
		if item.Name == "" {
			continue
		}
		if _, err = statement.ExecContext(context.Background(),
			item.Name, item.ChannelID, item.ID, item.Bind, item.Donat, item.Premium,
			item.InternalID, item.Paid, animatedMark(item.Animated), item.Tag, item.Tags,
			item.Img, item.ImgBig, item.ImgGif,
		); err != nil {
			return fmt.Errorf("сохранить смайл %s: %w", item.Name, err)
		}
	}

	// Служебное — в той же транзакции: если база переписалась, дата обязана
	// соответствовать ей, а не остаться от прошлого раза.
	meta := [][2]string{
		{metaSmilesUpdatedAt, at.UTC().Format(time.RFC3339)},
		{metaSmilesCompiledAt, string(file.CompiledAt)},
		{metaSmilesTimezone, string(file.TimezoneOffset)},
		{metaSmilesPacked, file.Packed},
	}
	for _, pair := range meta {
		if _, err = tx.ExecContext(context.Background(),
			`INSERT INTO meta (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
			pair[0], pair[1],
		); err != nil {
			return fmt.Errorf("записать %s: %w", pair[0], err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("сохранить базу смайлов: %w", err)
	}
	return nil
}

// UpdatedAt is when the base was last rewritten. Второе значение — есть ли она
// вообще: у только что созданной базы даты нет.
func (s *store) UpdatedAt() (time.Time, bool) {
	var value string

	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaSmilesUpdatedAt).Scan(&value)
	if err != nil {
		return time.Time{}, false
	}

	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// CompiledAt is when GoodGame assembled the file we took the smileys from.
func (s *store) CompiledAt() string {
	var value string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaSmilesCompiledAt).Scan(&value)
	if err != nil {
		return ""
	}
	return value
}

// Load reads the whole base into memory.
func (s *store) Load() (smileSet, error) {
	// В оперативку берём только то, что нужно для показа: как смайл пишется и
	// где его картинка. Остальные поля базы — служебные, они тут ни при чём.
	//
	// Ключ собираем с двоеточиями и сразу решаем, какую картинку показывать.
	// И то и другое делается здесь, по тысяче с лишним раз за запуск, а не на
	// каждой реплике.
	rows, err := s.db.Query(`
SELECT name, channel, animated, img, img_big, img_gif
FROM smiles
ORDER BY channel`)
	if err != nil {
		return nil, fmt.Errorf("прочитать базу смайлов: %w", err)
	}
	defer rows.Close()

	set := smileSet{}
	for rows.Next() {
		var item smile
		var channel, animated, small, big, gif string
		if err := rows.Scan(&item.Code, &channel, &animated, &small, &big, &gif); err != nil {
			return nil, fmt.Errorf("прочитать смайл: %w", err)
		}
		item.Code = ":" + item.Code + ":"

		// Гифку берём только у анимированных. GoodGame присылает её адрес и
		// там, где файла нет: у :carrot: анимации нет, и по адресу
		// anismiles/carrot-gif.gif отдаётся 404. Браузер на битой картинке
		// показывает alt — и в чате вместо смайла виден его текстовый код.
		item.Image = big
		if animated != "" && gif != "" {
			item.Image = gif
		}
		if item.Image == "" {
			item.Image = small
		}

		// Общие идут раньше канальных: при одинаковом коде останется общий.
		if _, taken := set[item.Code]; taken && channel != "" {
			continue
		}
		set[item.Code] = item
	}
	return set, rows.Err()
}

// Count is how many smileys are in the base.
func (s *store) Count() (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT count(*) FROM smiles`).Scan(&count)
	return count, err
}

// fetchSmiles downloads the smiley list.
func fetchSmiles(ctx context.Context) (chat.Smiles, []chat.Smiley, error) {
	ctx, cancel := context.WithTimeout(ctx, smilesTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, smilesURL, nil)
	if err != nil {
		return chat.Smiles{}, nil, err
	}

	// Адрес отдаёт перенаправление, и без заголовка мы бы получили страницу
	// с редиректом вместо списка смайлов.
	request.Header.Set("Accept", "application/json")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return chat.Smiles{}, nil, fmt.Errorf("скачать смайлы: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return chat.Smiles{}, nil, fmt.Errorf("GoodGame ответил %s", response.Status)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, smilesMaxSize))
	if err != nil {
		return chat.Smiles{}, nil, fmt.Errorf("прочитать смайлы: %w", err)
	}

	var file chat.Smiles
	if err := json.Unmarshal(body, &file); err != nil {
		return chat.Smiles{}, nil, fmt.Errorf("разобрать список смайлов: %w", err)
	}
	return file, flatten(file), nil
}

// animatedMark is how we keep the "has a gif" flag in a text column: единица —
// есть, пусто — нет. В файле это булево, в базе хочется видеть без всяких
// догадок.
func animatedMark(flag chat.Flag) string {
	if flag.True() {
		return "1"
	}
	return ""
}

// flatten turns the file into one list. Канальные смайлы отмечаем номером
// канала: код у них может совпасть с общим, и тогда в базе обе строки, а в
// память попадёт общий.
func flatten(file chat.Smiles) []chat.Smiley {
	out := make([]chat.Smiley, 0, len(file.Common))

	// У общих смайлов channel_id приходит нулём. Оставлять его «0» нельзя:
	// ноль — это не имя канала, и такой смайл потом не отличить от
	// канального.
	for _, one := range file.Common {
		one.ChannelID = ""
		one.Channel = ""
		out = append(out, one)
	}

	for channel, list := range file.Channel {
		for _, one := range list {
			one.ChannelID = chat.ID(channel)
			out = append(out, one)
		}
	}
	return out
}

// smileBook is the base we keep in memory and refresh on command.
type smileBook struct {
	mu     sync.RWMutex
	store  *store
	set    smileSet
	loaded time.Time
}

func (b *smileBook) get() smileSet {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.set
}

func (b *smileBook) put(set smileSet) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.set = set
	b.loaded = time.Now()
}

// openBook opens the base and loads it.
func openBook() (*smileBook, error) {
	st, err := openStore()
	if err != nil {
		return nil, err
	}

	book := &smileBook{store: st}
	set, err := st.Load()
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	book.put(set)
	return book, nil
}

func (b *smileBook) Close() error { return b.store.Close() }

// Update downloads the list and rewrites the base. Плагин при этом считает
// себя занятым: страница ждёт ответа и держит кнопку.
func (b *smileBook) Update(ctx context.Context) error {
	file, list, err := fetchSmiles(ctx)
	if err != nil {
		return err
	}
	if err := b.store.Replace(file, list, time.Now()); err != nil {
		return err
	}

	set, err := b.store.Load()
	if err != nil {
		return err
	}
	b.put(set)
	return nil
}

// UpdatedAt is when the base was last rewritten.
func (b *smileBook) UpdatedAt() (time.Time, bool) {
	if b.store == nil {
		return time.Time{}, false
	}
	return b.store.UpdatedAt()
}

// pluginInfo declares what the smiley button does. Ядро ничего в этой команде
// не понимает — оно только пересылает вид.
var smilesButton = protocol.Button{
	Title:   "Обновить базу смайлов",
	Kind:    protocol.KindSmilesUpdate,
	Section: "Смайлы",
	Hint:    "Скачать список смайлов у GoodGame и переписать базу",
}

var errNoSmilesBase = errors.New("база смайлов не открыта")
