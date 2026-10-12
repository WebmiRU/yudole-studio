// Package chat speaks the GoodGame chat protocol v2.
//
// The whole thing is one websocket connection to wss://chat-1.goodgame.ru/chat2/
// and a stream of JSON objects:
//
//	{"type":"join","data":{"channel_id":"5"}}
//	{"type":"message","data":{"channel_id":"5","user_name":"Василий","text":"привет"}}
//
// The envelope has exactly two fields and never grows, so a message we do not
// know about costs nothing: we read the type, and only look inside when it is
// one we asked for.
//
// GoodGame addresses channels by a string that is not always a number:
// "5" is a chat room, "r128" is a flood chat on the forum, and
// "cup:…" is a tournament. So the id is kept as a string everywhere.
//
// Source: https://github.com/GoodGame/API/blob/master/Chat/protocol2.md
package chat

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Endpoint is the address of the GoodGame chat. One connection serves every
// channel: it is not addressed by channel at all.
const Endpoint = "wss://chat-1.goodgame.ru/chat2/"

// MaxTextLength is the limit GoodGame puts on a message, JSON markup included.
const MaxTextLength = 4096

// Types of messages the server sends.
const (
	TypeWelcome          = "welcome"
	TypeSuccessAuth      = "success_auth"
	TypeChannelsList     = "channels_list"
	TypeSuccessJoin      = "success_join"
	TypeUnjoin           = "unjoin"
	TypeUsersList        = "users_list"
	TypeStreamModersList = "stream_moders_list"
	TypeChannelCounters  = "channel_counters"
	TypeChannelInfo      = "update_channel_info"
	TypeChannelInfoJobs  = "update_channel_info_jobs"
	TypeSetting          = "setting"
	TypeRandomResult     = "random_result"
	TypeIgnoreList       = "ignore_list"
	TypeChannelHistory   = "channel_history"
	TypeMessage          = "message"
	TypePrivateMessage   = "private_message"
	TypeFollower         = "follower"
	TypePayment          = "payment"
	TypePremium          = "premium"
	TypeGiftedPremiums   = "gifted_premiums"
	TypeRemoveMessage    = "remove_message"
	TypeUserBan          = "user_ban"
	TypeUserWarn         = "user_warn"
	TypeNewPoll          = "new_poll"
	TypePollResults      = "poll_results"
	TypeTeleportUser     = "teleport_user"
	TypeTeleportAim      = "teleport_aim"
	TypeSmileOnlyMode    = "set_smile_only_mode"
	TypeError            = "error"
)

// Message is one line of the protocol: what it is, and its contents.
//
// Data stays unread until somebody asks what is inside: one type can carry
// either a list of users or an empty object, and we do not want to pay for
// decoding what we are not going to use.
type Message struct {
	Type string         `json:"type"`
	Data jsontext.Value `json:"data,omitempty"`
}

// Into decodes the contents of a message into v.
func (m Message) Into(v any) error {
	if len(m.Data) == 0 {
		return fmt.Errorf("сообщение %q без содержимого", m.Type)
	}
	return json.Unmarshal(m.Data, v)
}

// New wraps a payload into the envelope. Every request in this package is built
// through it, so no request can forget its type.
func New(kind string, payload any) (*Message, error) {
	data, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("собрать %s: %w", kind, err)
	}
	return &Message{Type: kind, Data: data}, nil
}

// Encode renders a message as one line of the wire format.
func (m *Message) Encode() ([]byte, error) {
	return json.Marshal(m, json.Deterministic(true))
}

// Decode reads one line of the protocol.
func Decode(line []byte) (*Message, error) {
	var message Message
	if err := json.Unmarshal(line, &message); err != nil {
		return nil, err
	}
	if message.Type == "" {
		return nil, fmt.Errorf("сообщение без типа: %s", line)
	}
	return &message, nil
}

// ID is an identifier that GoodGame sends as a string in one message and as a
// number in another. Both are read as strings.
type ID string

func (id ID) String() string { return string(id) }

// UnmarshalJSONFrom takes either form. Returning errors.ErrUnsupported instead
// of a failure lets the decoder try its own rules, which is what we want for
// anything that is not a string or a number.
func (id *ID) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	switch dec.PeekKind() {
	case '"':
		var text string
		if err := json.UnmarshalDecode(dec, &text); err != nil {
			return err
		}
		*id = ID(text)
		return nil
	case '0':
		// Identifiers are big enough to lose precision in a float64, so the
		// number is read as text.
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		*id = ID(raw)
		return nil
	}
	return errors.ErrUnsupported
}

// Flag is a switch that GoodGame writes in three ways at once: as true, as 1
// and as "1". Some fields are also simply absent, and that is not the same as
// false — Known says whether we were told anything.
//
// The paper says jobs is a three-valued boolean: on, off, and null for a
// channel we know nothing about. The server does send 1 there.
type Flag struct {
	Known bool
	Value bool
}

// UnmarshalJSONFrom takes a boolean, a number or a word.
func (f *Flag) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	switch dec.PeekKind() {
	case 'n':
		// null: сказали, что не знаем.
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		if len(raw) != 4 || string(raw) != "null" {
			return fmt.Errorf("не понял %q", raw)
		}
		*f = Flag{Known: false}
		return nil
	case 't', 'f':
		var value bool
		if err := json.UnmarshalDecode(dec, &value); err != nil {
			return err
		}
		*f = Flag{Known: true, Value: value}
		return nil
	case '"':
		var text string
		if err := json.UnmarshalDecode(dec, &text); err != nil {
			return err
		}
		switch strings.ToLower(text) {
		case "1", "true", "on", "yes":
			*f = Flag{Known: true, Value: true}
		case "0", "false", "off", "no", "":
			*f = Flag{Known: true, Value: false}
		default:
			return fmt.Errorf("не понял %q", text)
		}
		return nil
	case '0':
		raw, err := dec.ReadValue()
		if err != nil {
			return err
		}
		number, err := strconv.ParseFloat(string(raw), 64)
		if err != nil {
			return err
		}
		*f = Flag{Known: true, Value: number != 0}
		return nil
	}
	return errors.ErrUnsupported
}

// MarshalJSONFrom writes back what the service would have written.
func (f Flag) MarshalJSONFrom(enc *jsontext.Encoder) error {
	if !f.Known {
		return enc.WriteToken(jsontext.Null)
	}
	if f.Value {
		return enc.WriteToken(jsontext.Bool(true))
	}
	return enc.WriteToken(jsontext.Bool(false))
}

// True reports whether the switch is on. An unknown switch counts as off.
func (f Flag) True() bool { return f.Known && f.Value }

// Rights levels GoodGame gives to a user. The numbers are fixed by the service.
const (
	RightsCasual         = 0
	RightsStreamModer    = 10
	RightsStreamer       = 20
	RightsModerator      = 30
	RightsSuperModerator = 40
	RightsAdmin          = 50
)

// User is a person seen in a channel or in the channel list.
type User struct {
	ID          ID             `json:"id"`
	Name        string         `json:"name"`
	Avatar      string         `json:"avatar"`
	Rights      int            `json:"rights"`
	Staff       int            `json:"staff"`
	Hidden      Flag           `json:"hidden"`
	Banned      Flag           `json:"banned"`
	BanInfo     Flag           `json:"baninfo"`
	Premium     int            `json:"premium"`
	Premiums    []string       `json:"premiums"`
	Payments    int            `json:"payments"`
	PaymentsAll map[string]int `json:"paymentsAll"`
	Resubs      map[string]int `json:"resubs"`
	GGPlusTier  int            `json:"ggPlusTier"`
	Role        string         `json:"role"`
	RegTime     int64          `json:"regtime"`
}

// Streamer is the owner of a channel.
type Streamer struct {
	ID         ID             `json:"id"`
	Name       string         `json:"name"`
	Username   string         `json:"username"`
	Avatar     string         `json:"avatar"`
	Rights     int            `json:"rights"`
	Staff      int            `json:"staff"`
	Hidden     Flag           `json:"hidden"`
	Banned     Flag           `json:"banned"`
	BanInfo    Flag           `json:"baninfo"`
	Premium    int            `json:"premium"`
	Premiums   []string       `json:"premiums"`
	Payments   int            `json:"payments"`
	Resubs     map[string]int `json:"resubs"`
	GGPlusTier int            `json:"ggPlusTier"`
	Role       string         `json:"role"`
	RegTime    int64          `json:"regtime"`
}

// Channel is one entry of the channel list.
type Channel struct {
	ID      ID     `json:"channel_id"`
	Name    string `json:"channel_name"`
	Clients int    `json:"clients_in_channel"`
	Users   int    `json:"users_in_channel"`
}

// Counter is the number of people in a channel. The server sends it on its own
// every twenty seconds or so to every channel we are in.
type Counter struct {
	Channel ID  `json:"channel_id"`
	Clients int `json:"clients_in_channel"`
	Users   int `json:"users_in_channel"`
}

// Jobs are the tasks a streamer can turn on in a channel.
type Jobs struct {
	Enabled              bool   `json:"enabled"`
	MinimumAmount        int    `json:"minimumAmount"`
	MaxActiveJobsPerUser int    `json:"maxActiveJobsPerUser"`
	Filter               string `json:"filter"`
	ColorSkin            int    `json:"colorSkin"`
	BackgroundImage      string `json:"backgroundImage"`
	CanCreate            bool   `json:"canCreate"`
}

// Answer is one option of a vote.
type Answer struct {
	ID     int    `json:"id"`
	Text   string `json:"text"`
	Voters int    `json:"voters"`
}

// --- What the server sends ---------------------------------------------------

// Welcome answers the connection itself: it says which version of the protocol
// we ended up on. It is the first message on every connection.
type Welcome struct {
	ProtocolVersion int `json:"protocolVersion"`
}

// Auth is what success_auth says about us.
type Auth struct {
	UserID   ID     `json:"user_id"`
	UserName string `json:"user_name"`
}

// ChannelsList is the answer to get_channels_list, sorted by how many people
// are in a channel, the busiest first.
type ChannelsList struct {
	Channels []Channel `json:"channels"`
}

// Join is what success_join says about a channel we got into. It is the richest
// message in the protocol: rights, bans, the streamer, counters.
type Join struct {
	ChannelID   ID     `json:"channel_id"`
	ChannelName string `json:"channel_name"`
	// ChannelKey is the name the streamer goes by, which is not always the
	// channel name.
	ChannelKey string   `json:"channel_key"`
	Streamer   Streamer `json:"channel_streamer"`
	// Motd is the message of the day.
	Motd          string `json:"motd"`
	RoomType      string `json:"room_type"`
	RoomPremium   bool   `json:"room_premium"`
	PremiumPrice  int    `json:"premium_price"`
	DonateButtons string `json:"donate_buttons"`

	Clients int `json:"clients_in_channel"`
	Users   int `json:"users_in_channel"`

	// UserID and Name are who we are in this channel. For a guest they are 0
	// and empty: we are nobody until we sign in.
	UserID       ID     `json:"user_id"`
	Name         string `json:"name"`
	AccessRights int    `json:"access_rights"`

	PremiumOnly int            `json:"premium_only"`
	Premium     bool           `json:"premium"`
	Premiums    []string       `json:"premiums"`
	Notifies    map[string]int `json:"notifies"`
	Resubs      map[string]int `json:"resubs"`
	GGPlusTier  int            `json:"gg_plus_tier"`
	Payments    int            `json:"payments"`
	PaymentsAll map[string]int `json:"paymentsAll"`

	IsBanned   bool   `json:"is_banned"`
	BannedTime int64  `json:"banned_time"`
	Reason     string `json:"reason"`
	Permanent  bool   `json:"permanent"`

	// Jobs says whether the streamer's tasks are on. The paper calls it a
	// three-valued boolean and promises null for a channel we know nothing
	// about.
	Jobs Flag `json:"jobs"`

	// SmileOnlyMode is set when the channel takes only smilies.
	SmileOnlyMode bool `json:"smile_only_mode"`
}

// Unjoined is the answer to unjoin, and also the channel that was left.
type Unjoined struct {
	Channel ID   `json:"channel_id"`
	Success bool `json:"success"`
}

// Users is the answer to get_users_list2. Guests are not in the list.
type Users struct {
	Channel ID     `json:"channel_id"`
	Clients int    `json:"clients_in_channel"`
	Users   int    `json:"users_in_channel"`
	People  []User `json:"users"`
}

// Moder is a helper of the streamer, one of those who can manage the chat even
// when they are not in it.
type Moder struct {
	ID     ID     `json:"id"`
	Name   string `json:"name"`
	Rights string `json:"rights"`
}

// Moders is the answer to get_stream_moders_list.
type Moders struct {
	Channel ID      `json:"channel_id"`
	Moders  []Moder `json:"moders"`
}

// ChannelInfo is update_channel_info: whether the stream has started, and when.
type ChannelInfo struct {
	Channel     ID    `json:"channel_id"`
	PremiumOnly int   `json:"premium_only"`
	Started     int64 `json:"started"`
}

// ChannelInfoJobs is update_channel_info_jobs.
type ChannelInfoJobs struct {
	Channel ID   `json:"channel_id"`
	Jobs    Jobs `json:"jobs"`
}

// Setting is a change of a channel setting, for example motd or slowmod. The
// same type is both the request and the answer.
type Setting struct {
	Channel      ID     `json:"channel_id"`
	Name         string `json:"name"`
	Value        string `json:"value"`
	ModerID      ID     `json:"moder_id"`
	ModerName    string `json:"moder_name"`
	ModerRights  int    `json:"moder_rights"`
	ModerPremium Flag   `json:"moder_premium"`
	Silent       int    `json:"silent"`
}

// Random is random_result: a viewer picked out of the channel, or nothing when
// nobody matched.
type Random struct {
	Channel ID     `json:"channel_id"`
	Success bool   `json:"success"`
	Data    []User `json:"data"`
}

// Ignored is one entry of the ignore list.
type Ignored struct {
	ID   ID     `json:"id"`
	Name string `json:"name"`
}

// IgnoreList is the answer to any of the three ignore list commands. The list
// is the same for every channel.
type IgnoreList struct {
	People []Ignored `json:"users"`
}

// History is the answer to get_channel_history, oldest first.
type History struct {
	Channel  ID        `json:"channel_id"`
	Messages []Message `json:"messages"`
}

// Line is a chat line. The text arrives with the smiley codes the service uses
// in it and with its own markup: both are ours to render, not the server's.
type Line struct {
	Channel     ID             `json:"channel_id"`
	UserID      ID             `json:"user_id"`
	UserName    string         `json:"user_name"`
	UserRights  int            `json:"user_rights"`
	Premium     int            `json:"premium"`
	Premiums    []string       `json:"premiums"`
	Resubs      map[string]int `json:"resubs"`
	Staff       int            `json:"staff"`
	Color       string         `json:"color"`
	Icon        string         `json:"icon"`
	Role        string         `json:"role"`
	Mobile      int            `json:"mobile"`
	Payments    int            `json:"payments"`
	PaymentsAll map[string]int `json:"paymentsAll"`
	GGPlusTier  int            `json:"gg_plus_tier"`
	IsStatus    int            `json:"isStatus"`
	// MessageID is what we would answer with to delete the line.
	MessageID int64 `json:"message_id"`
	// Timestamp is when the line reached the server, in unix seconds.
	Timestamp int64 `json:"timestamp"`
	// Text is the line as it was sent, without any markup from the server.
	Text    string `json:"text"`
	RegTime int64  `json:"regtime"`
}

// PrivateLine is a direct message. Everything a Line has, plus who it was for.
type PrivateLine struct {
	Channel    ID       `json:"channel_id"`
	UserID     ID       `json:"user_id"`
	UserName   string   `json:"user_name"`
	UserRights int      `json:"user_rights"`
	UserGroups []string `json:"user_groups"`
	HideIcon   bool     `json:"hideIcon"`
	Mobile     bool     `json:"mobile"`
	TargetID   ID       `json:"target_id"`
	TargetName string   `json:"target_name"`
	Timestamp  int64    `json:"timestamp"`
	Text       string   `json:"text"`
}

// Follower is somebody who subscribed to a channel.
type Follower struct {
	Channel  ID     `json:"channel_id"`
	UserID   ID     `json:"user_id"`
	UserName string `json:"username"`
}

// Payment is a donation. Total and Title are filled in only when the money went
// to the pool of a tournament.
type Payment struct {
	Channel             ID     `json:"channel_id"`
	UserID              ID     `json:"userId"`
	UserName            string `json:"userName"`
	Amount              string `json:"amount"`
	Message             string `json:"message"`
	Voice               string `json:"voice"`
	Total               string `json:"total"`
	Title               string `json:"title"`
	Link                string `json:"link"`
	Gift                int    `json:"gift"`
	IsCommissionCovered bool   `json:"is_commission_covered"`
}

// Premium is somebody who turned on premium here.
type Premium struct {
	Channel  ID     `json:"channel_id"`
	UserID   ID     `json:"userId"`
	UserName string `json:"userName"`
	Payment  int    `json:"payment"`
	Resub    string `json:"resub"`
}

// Gifted is premium somebody was given. PayerID is empty when it was anonymous.
type Gifted struct {
	Channel ID     `json:"channel_id"`
	Title   string `json:"title"`
	Payer   string `json:"payer"`
	PayerID ID     `json:"payerId"`
	Users   []struct {
		ID       ID     `json:"id"`
		Username string `json:"username"`
	} `json:"users"`
}

// Removed is a message a moderator took out. The same type is the request to
// remove one.
type Removed struct {
	Channel   ID     `json:"channel_id"`
	MessageID int64  `json:"message_id"`
	AdminName string `json:"adminName"`
}

// Ban is a ban shown to the whole channel. The same type, with RoomID instead
// of ChannelID, is what ban2 is asked with.
type Ban struct {
	Channel      ID     `json:"channel_id"`
	UserID       ID     `json:"user_id"`
	UserName     string `json:"user_name"`
	ModerID      ID     `json:"moder_id"`
	ModerName    string `json:"moder_name"`
	ModerRights  int    `json:"moder_rights"`
	ModerPremium Flag   `json:"moder_premium"`
	// Duration is in seconds, unlike the request, which asks in minutes.
	Duration int64  `json:"duration"`
	Type     int    `json:"type"`
	Reason   string `json:"reason"`
	Show     bool   `json:"show"`
}

// Warn is a warning shown to the whole channel.
type Warn struct {
	Channel      ID     `json:"channel_id"`
	UserID       ID     `json:"user_id"`
	UserName     string `json:"user_name"`
	ModerID      ID     `json:"moder_id"`
	ModerName    string `json:"moder_name"`
	ModerRights  int    `json:"moder_rights"`
	ModerPremium Flag   `json:"moder_premium"`
	Reason       string `json:"reason"`
}

// Poll is a vote. The same type creates it and announces it to the channel.
type Poll struct {
	Channel   ID       `json:"channel_id"`
	Title     string   `json:"title"`
	Answers   []Answer `json:"answers"`
	ModerID   ID       `json:"moder_id"`
	ModerName string   `json:"moder_name"`
}

// PollResults is the answer to get_poll_results.
type PollResults struct {
	Channel ID       `json:"channel_id"`
	Title   string   `json:"title"`
	Voters  int      `json:"voters"`
	Answers []Answer `json:"answers"`
}

// Teleport is a request from the service to move us to another channel, because
// the streamer we came for is now streaming somewhere else.
type Teleport struct {
	ChannelSrc  ID     `json:"channelSrc"`
	ChannelDest ID     `json:"channelDest"`
	Name        string `json:"name"`
}

// TeleportAim is what the channel we were teleported from says about it.
type TeleportAim struct {
	ChannelSrc           ID     `json:"channelSrc"`
	StreamerUsernameSrc  string `json:"streamerUsernameSrc"`
	StreamerNicknameSrc  string `json:"streamerNicknameSrc"`
	StreamerIDSrc        ID     `json:"streamerIdSrc"`
	ChannelDest          ID     `json:"channelDest"`
	StreamerUsernameDest string `json:"streamerUsernameDest"`
	StreamerNicknameDest string `json:"streamerNicknameDest"`
	StreamerIDDest       ID     `json:"streamerIdDest"`
	UsersCnt             int    `json:"usersCnt"`
}

// SmileOnlyMode turns the "smilies only" channel mode on or off. The same type
// asks for it and reports it to the channel.
type SmileOnlyMode struct {
	Channel ID   `json:"channel_id"`
	Enabled bool `json:"enabled"`
}

// Error is the only way the service says no. The text is ready to be shown to a
// person as is.
type Error struct {
	Channel  ID     `json:"channel_id"`
	ErrorMsg string `json:"errorMsg"`
}

func (e Error) Error() string {
	if e.Channel == "" {
		return e.ErrorMsg
	}
	return fmt.Sprintf("канал %s: %s", e.Channel, e.ErrorMsg)
}

// --- What we send ------------------------------------------------------------

// AuthRequest signs in. Without a token the service treats us as a guest, which
// is read-only: we can listen, but not talk.
type AuthRequest struct {
	UserID ID     `json:"user_id"`
	Token  string `json:"token,omitempty"`
}

// ChannelsListRequest asks for the channel list, a page at a time.
type ChannelsListRequest struct {
	Start int `json:"start,omitempty"`
	Count int `json:"count,omitempty"`
}

// JoinRequest asks for a channel. One connection can be in many at once.
type JoinRequest struct {
	ChannelID ID `json:"channel_id"`
	// Hidden keeps us out of the user list. Moderators only.
	Hidden int  `json:"hidden,omitempty"`
	Mobile bool `json:"mobile,omitempty"`
	// Reload asks the service to send the channel again from the start.
	Reload bool `json:"reload,omitempty"`
}

// UnjoinRequest leaves a channel.
type UnjoinRequest struct {
	ChannelID ID `json:"channel_id"`
}

// UsersListRequest asks who is in a channel.
type UsersListRequest struct {
	ChannelID ID `json:"channel_id"`
}

// ModersRequest asks for the helpers of the streamer, those in the chat and
// those not.
type ModersRequest struct {
	ChannelID ID `json:"channel_id"`
}

// CountersRequest asks how many people are in a channel.
type CountersRequest struct {
	ChannelID ID `json:"channel_id"`
}

// RandomRequest picks a viewer out of the channel.
type RandomRequest struct {
	ChannelID ID   `json:"channel_id"`
	Already   []ID `json:"already,omitempty"`
	Version   int  `json:"version,omitempty"`
	Premiums  bool `json:"premiums,omitempty"`
	Donaters  bool `json:"donaters,omitempty"`
	Others    bool `json:"others,omitempty"`
	NoModers  bool `json:"noModers,omitempty"`
}

// IgnoreListRequest asks for the ignore list.
type IgnoreListRequest struct{}

// IgnoreAddRequest puts somebody on the ignore list.
type IgnoreAddRequest struct {
	UserID ID `json:"user_id"`
}

// IgnoreDelRequest takes somebody off the ignore list.
type IgnoreDelRequest struct {
	UserID ID `json:"user_id"`
}

// HistoryRequest asks for what was said in a channel. From is the id of a
// message to start from, empty for the newest.
type HistoryRequest struct {
	ChannelID ID     `json:"channel_id"`
	From      string `json:"from,omitempty"`
}

// SendRequest writes a line to a channel. A guest may not: the service takes
// the message and answers with an error.
type SendRequest struct {
	ChannelID ID     `json:"channel_id"`
	Text      string `json:"text"`
	Color     string `json:"color,omitempty"`
	Icon      string `json:"icon,omitempty"`
	Role      string `json:"role,omitempty"`
	Mobile    int    `json:"mobile,omitempty"`
}

// TextOverlong reports whether a line is longer than the service allows.
func TextOverlong(text string) bool {
	return len(text) > MaxTextLength
}

// BanRequest bans somebody in a channel. Duration is in minutes, while the ban
// the channel sees counts seconds.
type BanRequest struct {
	UserID  ID     `json:"userId"`
	RoomID  ID     `json:"roomId"`
	Reason  string `json:"reason,omitempty"`
	Comment string `json:"comment,omitempty"`
	// Type is 0 for a ban for a while, 1 until the stream ends, 2 forever.
	Type          int  `json:"type"`
	Duration      int  `json:"duration,omitempty"`
	DeleteMessage bool `json:"deleteMessage,omitempty"`
}

// WarnRequest warns somebody in a channel.
type WarnRequest struct {
	ChannelID ID     `json:"channel_id"`
	UserID    ID     `json:"user_id"`
	Reason    string `json:"reason,omitempty"`
}

// ModeratorRequest gives a helper of the streamer their rights, or takes them
// away. The same type, with a different type name, does both.
type ModeratorRequest struct {
	ChannelID ID `json:"channel_id"`
	UserID    ID `json:"user_id"`
}

// VoteRequest picks an answer in a running vote.
type VoteRequest struct {
	ChannelID ID  `json:"channel_id"`
	AnswerID  int `json:"answer_id"`
}

// PollRequest starts a vote. There may be no more than six answers.
type PollRequest struct {
	ChannelID ID       `json:"channel_id"`
	Title     string   `json:"title"`
	Answers   []Answer `json:"answers"`
}

// PollResultsRequest asks how the vote went.
type PollResultsRequest struct {
	ChannelID ID `json:"channel_id"`
}

// PollMaxAnswers is how many answers a vote may have.
const PollMaxAnswers = 6

// ChannelOf pulls the channel out of any message that names one. The service
// puts it under different names depending on the message, and for some — like
// a ban — only under the one it was asked with.
func ChannelOf(message *Message) ID {
	switch message.Type {
	case TypeSuccessJoin, TypeUsersList, TypeStreamModersList,
		TypeChannelCounters, TypeChannelInfo, TypeChannelInfoJobs,
		TypeSetting, TypeRandomResult, TypeChannelHistory, TypeMessage,
		TypePrivateMessage, TypeFollower, TypePayment, TypePremium,
		TypeGiftedPremiums, TypeNewPoll, TypePollResults, TypeSmileOnlyMode,
		TypeError,
		// Запросы тоже называют канал: по нему удобно проверить, что
		// ответили именно на наш, а не на чужой.
		"join", "unjoin", "ban2":
		var named struct {
			Channel    ID `json:"channel_id"`
			ChannelAlt ID `json:"channelId"`
			RoomID     ID `json:"roomId"`
		}
		if err := message.Into(&named); err != nil {
			return ""
		}
		for _, id := range []ID{named.Channel, named.ChannelAlt, named.RoomID} {
			if id != "" {
				return id
			}
		}
	}
	return ""
}

// JoinChannelID checks an id against what GoodGame uses. Numbers are chat
// rooms, "r128" is a forum flood chat, "cup:…" is a tournament.
func JoinChannelID(id ID) bool {
	text := strings.TrimSpace(string(id))
	if text == "" {
		return false
	}
	if strings.HasPrefix(text, "r") {
		// Флудилка на форуме: «r128».
		return len(text) > 1
	}
	if prefix, _, ok := strings.Cut(text, ":"); ok {
		// Турнир: «cup:…» — префикс без содержимого ничего не значит.
		return prefix != ""
	}
	for _, char := range text {
		if char < '0' || char > '9' {
			return false
		}
	}
	return len(text) <= 8
}
