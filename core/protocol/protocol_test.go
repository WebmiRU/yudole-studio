package protocol

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"strings"
	"testing"
)

func TestEncodeKeepsTheEnvelopeFlat(t *testing.T) {
	message := MustNew(KindPluginChat, InfoPayload{
		Title:       "GoodGame",
		Description: "Чат игровой площадки goodgame.ru",
		Version:     "0.1.0",
		Options: []Option{{
			Key:      "channels",
			Type:     OptionTextMultiple,
			Label:    "Каналы",
			Hint:     "Сколько нужно — столько и добавьте",
			Required: false,
		}},
	})

	line, err := Encode(message)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Count(string(line), "\n") != 1 || !strings.HasSuffix(string(line), "\n") {
		t.Errorf("a protocol line must be one JSON object and one newline: %q", line)
	}

	var envelope map[string]any
	if err := json.Unmarshal(line, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != 2 {
		t.Errorf("the envelope must have only kind and payload: %s", line)
	}
	if _, ok := envelope["payload"]; !ok {
		t.Errorf("payload is missing: %s", line)
	}
	if envelope["kind"] != KindPluginChat {
		t.Errorf("kind = %v, want %q", envelope["kind"], KindPluginChat)
	}
}

func TestRoundTripChatLine(t *testing.T) {
	want := ChatPayload{
		Kind:    KindMessagePublic,
		Channel: "42",
		User:    ChatUser{Name: "vasya", ID: "1234", Login: "vasya_pet"},
		Message: ChatLine{Src: ":D привет", Text: "привет", Html: "<img> привет"},
	}

	line, err := Encode(MustNew(want.Kind, want))
	if err != nil {
		t.Fatal(err)
	}

	got, err := DecodeChat(line)
	if err != nil {
		t.Fatalf("DecodeChat: %v", err)
	}

	if got.Kind != KindMessagePublic {
		t.Errorf("kind = %q, want %q", got.Kind, KindMessagePublic)
	}
	if got.Channel != want.Channel {
		t.Errorf("channel = %q, want %q", got.Channel, want.Channel)
	}
	if got.User != want.User {
		t.Errorf("user = %+v, want %+v", got.User, want.User)
	}
	if got.Message != want.Message {
		t.Errorf("message = %+v, want %+v", got.Message, want.Message)
	}
}

// A newline inside a payload must not break the line format.
func TestPayloadCannotBreakTheFraming(t *testing.T) {
	line, err := Encode(MustNew(KindLog, LogPayload{Message: "первая\nвторая"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(line), "\n") != 1 {
		t.Errorf("the payload broke the framing: %q", line)
	}

	message, err := Decode(line)
	if err != nil {
		t.Fatal(err)
	}
	var payload LogPayload
	if err := message.PayloadInto(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Message != "первая\nвторая" {
		t.Errorf("message = %q, want it unchanged", payload.Message)
	}
}

func TestDecodeRejectsLinesWithoutKind(t *testing.T) {
	if _, err := Decode([]byte(`{"payload":{"a":1}}`)); err == nil {
		t.Error("expected an error for a message without a kind")
	}
	if _, err := Decode([]byte(`hello`)); err == nil {
		t.Error("expected an error for a non-JSON line")
	}
}

func TestDecodeChatRejectsOtherKinds(t *testing.T) {
	line, err := Encode(MustNew(KindAck, AckPayload{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeChat(line); err == nil {
		t.Error("a control message must not pass as a chat line")
	}
}

func TestPayloadIntoComplainsWhenEmpty(t *testing.T) {
	var payload InfoPayload
	if err := (Message{Kind: KindPluginChat}).PayloadInto(&payload); err == nil {
		t.Error("expected an error when the payload is missing")
	}
}

func TestEnvelopeAllowsKindWithoutPayload(t *testing.T) {
	line, err := Encode(MustNew(KindQuit, nil))
	if err != nil { t.Fatal(err) }
	if strings.Contains(string(line), "payload") { t.Fatalf("unexpected payload: %s", line) }
	if _, err := Decode(line); err != nil { t.Fatalf("Decode: %v", err) }
}

func TestTextMessagesUseSharedMessageContent(t *testing.T) {
	line, err := Encode(MustNew(KindError, ErrorPayload{Message: "bad <value>"}))
	if err != nil { t.Fatal(err) }
	var envelope map[string]any
	if err := json.Unmarshal(line, &envelope); err != nil { t.Fatal(err) }
	payload := envelope["payload"].(map[string]any)
	body := payload["message"].(map[string]any)
	if body["src"] != "bad <value>" || body["text"] != "bad <value>" || body["html"] != "bad &lt;value&gt;" {
		t.Fatalf("message body = %#v", body)
	}
	var decoded ErrorPayload
	if err := (Message{Kind: KindError, Payload: jsontext.Value(payloadJSON(payload))}).PayloadInto(&decoded); err != nil { t.Fatal(err) }
	if decoded.Message != "bad <value>" { t.Errorf("decoded message = %q", decoded.Message) }
}

func payloadJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func TestChatMessageRequiresSharedIdentityAndTextFields(t *testing.T) {
	good := MustNew(KindMessagePublic, ChatPayload{
		Channel: "42",
		User: ChatUser{Name: "Danil"},
		Message: ChatLine{Src: "hi", Text: "hi", Html: "hi"},
	})
	if err := Validate(good); err != nil { t.Fatalf("valid chat message: %v", err) }
	bad := Message{Kind: KindMessagePublic, Payload: jsontext.Value([]byte(`{"channel":"42","user":{"id":"1"},"message":{"text":"hi"}}`))}
	if err := Validate(bad); err == nil { t.Fatal("invalid chat message was accepted") }
}

func TestWarningRequiresText(t *testing.T) {
	if _, err := New(KindSystemMessageWarning, MessagePayload{Message: MessageContent{Text:""}}); err == nil {
		t.Fatal("empty warning was accepted")
	}
	if _, err := New(KindSystemMessageWarning, MessagePayload{Message: PlainMessage("Внимание")}); err != nil {
		t.Fatalf("valid warning: %v", err)
	}
}

func TestIsChatKind(t *testing.T) {
	for _, kind := range []string{KindMessagePublic, KindMessagePrivate} {
		if !IsChatKind(kind) {
			t.Errorf("%q should be a chat kind", kind)
		}
	}
	for _, kind := range []string{KindLog, KindAck, KindPluginChat} {
		if IsChatKind(kind) {
			t.Errorf("%q should not be a chat kind", kind)
		}
	}
}

func TestCheckValuesAcceptsWhatThePluginDeclared(t *testing.T) {
	options := []Option{
		{Key: "login", Type: OptionText, Required: true},
		{Key: "token", Type: OptionPassword},
		{Key: "autoconnect", Type: OptionBool},
		{Key: "retries", Type: OptionNumber},
		{Key: "room", Type: OptionSelect, Choices: []Choice{{Value: "general"}}},
		{Key: "channels", Type: OptionTextMultiple},
	}

	values := map[string]any{
		"login":       "vasya",
		"token":       "secret",
		"autoconnect": true,
		"retries":     float64(3),
		"room":        "general",
		"channels":    []any{"general", "game"},
	}

	unknown, err := CheckValues(options, values)
	if err != nil {
		t.Fatalf("CheckValues: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown keys = %v, want none", unknown)
	}
}

func TestCheckValuesRejectsMistypedValues(t *testing.T) {
	options := []Option{
		{Key: "login", Type: OptionText, Required: true},
		{Key: "autoconnect", Type: OptionBool},
		{Key: "channels", Type: OptionTextMultiple},
	}

	cases := map[string]map[string]any{
		"a number in a text field": {"login": 42},
		"a string in a switch":     {"login": "vasya", "autoconnect": "yes"},
		"a list in a text field":   {"login": []any{"vasya"}},
		"an object in a list":      {"login": "vasya", "channels": map[string]any{}},
		"a missing required field": {"autoconnect": true},
	}

	for name, values := range cases {
		if _, err := CheckValues(options, values); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Options the plugin does not know about are kept, not rejected: they may
// belong to a version of the plugin we do not talk to yet.
func TestCheckValuesKeepsUnknownKeys(t *testing.T) {
	options := []Option{{Key: "channels", Type: OptionTextMultiple}}
	values := map[string]any{"channels": []any{"general"}, "future_option": "x"}

	unknown, err := CheckValues(options, values)
	if err != nil {
		t.Fatalf("CheckValues: %v", err)
	}
	if len(unknown) != 1 || unknown[0] != "future_option" {
		t.Errorf("unknown = %v, want [future_option]", unknown)
	}
}

func TestOptionValid(t *testing.T) {
	good := []Option{
		{Key: "a", Type: OptionText},
		{Key: "b", Type: OptionPassword},
		{Key: "c", Type: OptionNumber},
		{Key: "d", Type: OptionBool},
		{Key: "e", Type: OptionTextMultiple},
		{Key: "f", Type: OptionSelect, Choices: []Choice{{Value: "1"}}},
	}
	for _, option := range good {
		if err := option.Valid(); err != nil {
			t.Errorf("%q should be valid: %v", option.Key, err)
		}
	}

	bad := map[string]Option{
		"no key":          {Type: OptionText},
		"no type":         {Key: "a"},
		"unknown type":    {Key: "a", Type: "color"},
		"select no items": {Key: "a", Type: OptionSelect},
	}
	for name, option := range bad {
		if err := option.Valid(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDefaultsFillTheGaps(t *testing.T) {
	options := []Option{
		{Key: "nick", Type: OptionText},
		{Key: "autoconnect", Type: OptionBool, Default: true},
		{Key: "channels", Type: OptionTextMultiple},
		{Key: "room", Type: OptionSelect, Choices: []Choice{{Value: "general"}}},
	}

	values := Defaults(options)

	if values["nick"] != "" {
		t.Errorf("nick = %v, want an empty string", values["nick"])
	}
	if values["autoconnect"] != true {
		t.Errorf("autoconnect = %v, want the declared default", values["autoconnect"])
	}
	if list, ok := values["channels"].([]any); !ok || len(list) != 0 {
		t.Errorf("channels = %#v, want an empty list", values["channels"])
	}
}

func TestSelectMultipleAcceptsBothShapes(t *testing.T) {
	options := []Option{{Key: "channels", Type: OptionSelectMultiple, Choices: []Choice{{Value: "53029", Label: "E.Wolf"}}}}

	// Ручная правка конфига: просто строки без подписей.
	fromConfig, err := CheckValues(options, map[string]any{
		"channels": []any{"53029", "12"},
	})
	if err != nil {
		t.Fatalf("plain strings must be accepted: %v", err)
	}
	if len(fromConfig) != 0 {
		t.Errorf("unknown = %v, want none", fromConfig)
	}

	// То, что пишет страница настроек: значение с подписью.
	pairs := []any{
		ChoiceValue{Value: "53029", Label: "E.Wolf"},
		ChoiceValue{Value: "12", Label: "Общий чат"},
	}
	if _, err := CheckValues(options, map[string]any{"channels": pairs}); err != nil {
		t.Fatalf("value and label must be accepted: %v", err)
	}

	if got := Values(pairs); len(got) != 2 || got[0] != "53029" || got[1] != "12" {
		t.Errorf("Values = %v, want the raw values only", got)
	}
	if got := Values([]any{"12"}); len(got) != 1 || got[0] != "12" {
		t.Errorf("Values of strings = %v", got)
	}
}

func TestSelectMultipleRejectsEmptyValues(t *testing.T) {
	options := []Option{{Key: "channels", Type: OptionSelectMultiple}}

	for name, values := range map[string]map[string]any{
		"an empty value":  {"channels": []any{ChoiceValue{Value: "", Label: "ничего"}}},
		"a value that is": {"channels": []any{42}},
		"not a list":      {"channels": "53029"},
	} {
		if _, err := CheckValues(options, values); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Плагин может прислать пустой список каналов: список придёт из его API позже,
// а добавить значение руками пользователь должен иметь возможность.
func TestSelectMultipleNeedsNoChoices(t *testing.T) {
	if err := (Option{Key: "channels", Type: OptionSelectMultiple}).Valid(); err != nil {
		t.Errorf("a select/multiple without choices should be valid: %v", err)
	}
	if err := (Option{Key: "room", Type: OptionSelect}).Valid(); err == nil {
		t.Error("a plain select without choices should not be valid")
	}
}

func TestDefaultsForSelectMultiple(t *testing.T) {
	values := Defaults([]Option{{Key: "channels", Type: OptionSelectMultiple}})
	if list, ok := values["channels"].([]any); !ok || len(list) != 0 {
		t.Errorf("channels = %#v, want an empty list", values["channels"])
	}
}
