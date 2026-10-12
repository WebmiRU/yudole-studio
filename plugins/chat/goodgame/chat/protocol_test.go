package chat

import (
	"encoding/json/v2"
	"testing"
)

// Сервер пишет один и тот же флаг по-разному: булевым, числом, словом — а
// иногда просто не пишет.
func TestFlagTakesEveryForm(t *testing.T) {
	cases := []struct {
		raw   string
		known bool
		value bool
	}{
		{raw: `true`, known: true, value: true},
		{raw: `false`, known: true, value: false},
		{raw: `1`, known: true, value: true},
		{raw: `0`, known: true, value: false},
		{raw: `"1"`, known: true, value: true},
		{raw: `"true"`, known: true, value: true},
		{raw: `""`, known: true, value: false},
		{raw: `null`, known: false, value: false},
	}

	for _, c := range cases {
		var flag Flag
		if err := json.Unmarshal([]byte(c.raw), &flag); err != nil {
			t.Errorf("%s: %v", c.raw, err)
			continue
		}
		if flag.Known != c.known || flag.Value != c.value {
			t.Errorf("%s → %+v, want known=%v value=%v", c.raw, flag, c.known, c.value)
		}
	}
}

// Поле может и вовсе не прийти — это не то же самое, что false.
func TestFlagMissingIsNotFalse(t *testing.T) {
	var joined Join
	if err := json.Unmarshal([]byte(`{"channel_id":"5"}`), &joined); err != nil {
		t.Fatal(err)
	}
	if joined.Jobs.Known {
		t.Error("отсутствующий jobs не должен считаться известным")
	}
	if joined.Jobs.True() {
		t.Error("неизвестный флаг не должен считаться включённым")
	}
}

// Настоящий success_join: jobs приходит числом, а не булевым.
func TestJoinSurvivesTheRealServer(t *testing.T) {
	const raw = `{"type":"success_join","data":{
		"channel_id":"5","channel_name":"Марафон","channel_key":"Miker",
		"channel_streamer":{"baninfo":0,"banned":0,"hidden":0,"id":1,"name":"Miker",
			"username":"Miker","avatar":"1_mtmA.png","payments":7,"premium":1,
			"premiums":["5"],"resubs":{"5":7},"rights":50,"staff":0,
			"regtime":1201532455,"ggPlusTier":0,"role":""},
		"motd":"Сообщение дня","room_type":"stream","room_premium":true,
		"premium_price":0,"donate_buttons":"","clients_in_channel":187,
		"users_in_channel":143,"user_id":0,"name":"","access_rights":0,
		"premium_only":0,"premium":false,"premiums":["94123"],
		"notifies":{"94123":0},"resubs":{"94123":7},"gg_plus_tier":0,
		"is_banned":false,"banned_time":0,"reason":"","permanent":false,
		"payments":1,"paymentsAll":{"5":1,"94123":7},"jobs":1,
		"smile_only_mode":false}}`

	message, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var joined Join
	if err := message.Into(&joined); err != nil {
		t.Fatalf("настоящий success_join не разобрался: %v", err)
	}
	if joined.ChannelID != "5" || joined.ChannelKey != "Miker" {
		t.Errorf("joined = %+v", joined)
	}
	if !joined.Jobs.True() {
		t.Error("jobs = 1 должен значить включено")
	}
	if !joined.Streamer.Hidden.Known || joined.Streamer.Hidden.True() {
		t.Errorf("streamer.hidden = %+v, want a known zero", joined.Streamer.Hidden)
	}
	if !joined.Streamer.BanInfo.True() && !joined.Streamer.BanInfo.Known {
		t.Errorf("streamer.baninfo = %+v, want a known zero", joined.Streamer.BanInfo)
	}
}

// Реплика из документа: она приходит всем в канале, и почти всё в ней нам не
// нужно, но разобраться она обязана.
func TestChatLine(t *testing.T) {
	const raw = `{"type":"message","data":{
		"channel_id":"15365","user_id":162675,"user_name":"Resmile",
		"user_rights":0,"premium":1,"premiums":["15365","40756"],
		"resubs":{"15365":4,"40756":2},"staff":0,"color":"silver",
		"icon":"none","role":"","mobile":0,"payments":2,
		"paymentsAll":{"5":2,"6515":2,"15365":2,"183946":1},"gg_plus_tier":0,
		"isStatus":0,"message_id":1693043578692,"timestamp":1693043579,
		"text":"Miker, :panic:","regtime":1371037120}}`

	message, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	var line Line
	if err := message.Into(&line); err != nil {
		t.Fatal(err)
	}
	if line.Channel != "15365" || line.UserName != "Resmile" || line.UserID != "162675" {
		t.Errorf("line = %+v", line)
	}
	if line.MessageID != 1693043578692 || line.Timestamp != 1693043579 {
		t.Errorf("идентификаторы поехали: %d / %d", line.MessageID, line.Timestamp)
	}
	if line.Text != "Miker, :panic:" {
		t.Errorf("text = %q", line.Text)
	}
}
