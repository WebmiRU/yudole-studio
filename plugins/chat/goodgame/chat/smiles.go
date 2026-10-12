package chat

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// Смайлы GoodGame приезжают отдельным файлом, а не в чате:
//
//	https://goodgame.ru/js/minified/global.json
//
// В тексте реплики смайл — это код вида :jokie5:, и подставить вместо него
// картинку можно только зная, что за код. Состав смайлов зависит от канала:
// у части каналов свои, доступные только с подпиской.
//
// Разбирать это рано: смайлы в тексте приходят уже подставленными, а сами
// файлы нужны, когда дойдёт очередь до показа. Но раз structures разбираются
// один раз, пусть они будут готовы.

// SmilesURL is where the list of smileys lives.
const SmilesURL = "https://goodgame.ru/js/minified/global.json"

// SmilesURLLegacy is the older address, still mentioned in the paper.
const SmilesURLLegacy = "http://goodgame.ru/js/minified/global.js"

// Text is a string that the service sometimes sends as null: у общих смайлов
// поле channel приходит именно так.
type Text string

func (t *Text) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	if dec.PeekKind() == 'n' {
		if _, err := dec.ReadValue(); err != nil {
			return err
		}
		*t = ""
		return nil
	}

	var text string
	if err := json.UnmarshalDecode(dec, &text); err != nil {
		return err
	}
	*t = Text(text)
	return nil
}

// Smiley is one smiley.
//
// Типы — по настоящему файлу, а не по документе: там id и bind приходят
// числами, animated — булевым, а не строкой "1", как обещает описание.
type Smiley struct {
	ID ID `json:"id"`
	// Bind is for the service's own bookkeeping; we do not know what it means.
	Bind ID `json:"bind"`
	// Name is the code written in a message, without the colons.
	Name string `json:"name"`
	// Donat means the smiley is one of those unlocked by a donation.
	Donat int `json:"donat"`
	// Premium means it is for subscribers only.
	Premium int `json:"premium"`
	// Paid means it costs real money; who may use it is in payments.
	Paid int `json:"paid"`
	// InternalID is for the service's own bookkeeping.
	InternalID int `json:"internal_id"`
	// Animated says whether the smiley has a gif.
	Animated Flag `json:"animated"`
	// Tag is the template the code is built from, for example "?tuvney".
	Tag string `json:"tag"`

	Img    string `json:"img"`
	ImgBig string `json:"img_big"`
	ImgGif string `json:"img_gif"`

	// Channel is the name of the channel the smiley belongs to, empty for the
	// ones available everywhere. Бывает и null.
	Channel Text `json:"channel"`
	// Tags are the words the site's search finds this smiley by. Нам не
	// нужны, но в базу кладём: файл должен лежать целиком.
	Tags string `json:"tags"`
	// ChannelID is the same by number. Здесь он приходит то строкой, то
	// числом — как и channel_id в чате, поэтому берём тот же тип.
	ChannelID ID `json:"channel_id"`
}

// IsAnimated reports whether the smiley has a gif.
func (s Smiley) IsAnimated() bool { return s.Animated.True() }

// Smiles is the whole file: the smileys everyone has, and the ones bound to
// particular channels.
type Smiles struct {
	Common  []Smiley            `json:"Smiles"`
	Channel map[string][]Smiley `json:"Channel_Smiles"`

	// Packed is every smiley name in one line through a comma — дубль того,
	// что мы и так храним поимённо.
	Packed string `json:"SmilesPacked"`
	// CompiledAt and TimezoneOffset are the file's own words: когда GoodGame
	// его собрал и с каким часовым поясом считал.
	CompiledAt     Text `json:"_compiled_at_"`
	TimezoneOffset Text `json:"timezone_offset"`
}

// Set is the smileys we know, by the code they are written by. Собирать его
// надо один раз на канал, а не на каждую реплику.
type Set struct {
	byName map[string]Smiley
}

// NewSet indexes the smileys we were given.
func NewSet(list ...Smiley) *Set {
	set := &Set{byName: make(map[string]Smiley, len(list))}
	for _, smiley := range list {
		set.byName[smiley.Name] = smiley
	}
	return set
}

// Add puts the smileys of one channel into the set, so the codes can be looked
// up without knowing which channel they came from.
func (s *Set) Add(list []Smiley) {
	for _, smiley := range list {
		s.byName[smiley.Name] = smiley
	}
}

// Lookup finds a smiley by the code written in a message, with or without the
// colons.
func (s *Set) Lookup(code string) (Smiley, bool) {
	smiley, ok := s.byName[trimColons(code)]
	return smiley, ok
}

// Len is how many smileys the set knows.
func (s *Set) Len() int { return len(s.byName) }

func trimColons(code string) string {
	if len(code) > 1 && code[0] == ':' && code[len(code)-1] == ':' {
		return code[1 : len(code)-1]
	}
	return code
}

// LoginRequest is the form sent to https://goodgame.ru/ajax/chatlogin for a
// token.
//
// The paper itself does not recommend this way: it is here because it is what
// the protocol describes, and the proper one is OAuth2 through the api4 client.
type LoginRequest struct {
	Login    string
	Password string
}

// LoginURL is where a token is asked for.
const LoginURL = "https://goodgame.ru/ajax/chatlogin"

// Login is what the service answers with.
//
// Token is what we later send in auth; UserID is who we then are. Without
// either we stay a guest and can only listen.
type Login struct {
	Code int `json:"code"`
	// UserID arrives as a string.
	UserID ID `json:"user_id"`
	// Settings is a JSON document inside a string, and it is of no use to a
	// chat client: it is the site's own page settings. Kept raw so that
	// nothing is lost, not parsed so that nothing depends on it.
	Settings string `json:"settings"`
	Token    string `json:"token"`
	Result   bool   `json:"result"`
	// Return and Response are the service's own fields for the site page.
	Return   bool   `json:"return"`
	Response string `json:"response"`
	// LoginPage says where to send the person if the login needs a second step.
	LoginPage string `json:"login_page"`
}
