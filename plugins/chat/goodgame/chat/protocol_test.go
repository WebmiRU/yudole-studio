package chat

import (
	"encoding/json/v2"
	"strings"
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

// Смайлы приезжают отдельным файлом. Берём кусок настоящего ответа.
func TestSmiles(t *testing.T) {
	const raw = `{"Smiles":[{
		"id":"874","bind":"26","name":"thup","donat":0,"premium":0,
		"internal_id":104,"paid":0,"animated":"1","tag":"?tuvney",
		"img":"https://goodgame.ru/images/smiles/thup.png",
		"img_big":"https://goodgame.ru/images/smiles/thup-big.png",
		"img_gif":"https://goodgame.ru/images/anismiles/thup-gif.gif",
		"channel":"","channel_id":0}],
	"Channel_Smiles":{"1577":[{
		"id":"95","name":"shimoro2","premium":1,"channel":"SHIMORO",
		"channel_id":"1577","animated":"","tag":"?tuvney"}]}}`

	var smiles Smiles
	if err := json.Unmarshal([]byte(raw), &smiles); err != nil {
		t.Fatalf("global.json не разобрался: %v", err)
	}
	if len(smiles.Common) != 1 || smiles.Common[0].Name != "thup" {
		t.Fatalf("общие = %+v", smiles.Common)
	}
	if !smiles.Common[0].IsAnimated() {
		t.Error("animated: \"1\" должен значить гифку")
	}
	if len(smiles.Channel["1577"]) != 1 {
		t.Fatalf("смайлы канала = %+v", smiles.Channel)
	}

	set := NewSet(smiles.Common...)
	set.Add(smiles.Channel["1577"])

	for _, code := range []string{"thup", ":thup:"} {
		if _, ok := set.Lookup(code); !ok {
			t.Errorf("смайл %q не нашёлся", code)
		}
	}
	if _, ok := set.Lookup("nosmile"); ok {
		t.Error("несуществующий смайл нашёлся")
	}
	if set.Len() != 2 {
		t.Errorf("в наборе %d смайлов, want 2", set.Len())
	}
}

// Ответ на запрос токена — приходит на сайт, не в чат.
func TestLogin(t *testing.T) {
	const raw = `{"code":4,"user_id":"12345","login_page":"",
		"settings":"{\"alignType\":0}","token":"fa2e9010cb9a4228215be14fbde13226",
		"result":true,"return":false,"response":"Вы успешно вошли на сайт"}`

	var login Login
	if err := json.Unmarshal([]byte(raw), &login); err != nil {
		t.Fatal(err)
	}
	if login.UserID != "12345" || login.Token == "" || !login.Result {
		t.Errorf("login = %+v", login)
	}
}

// Подписчика сервер шлёт то с userId, то с user_id — читаем оба.
func TestFollowerTakesBothSpellings(t *testing.T) {
	cases := map[string]struct {
		id   ID
		name string
	}{
		`{"type":"follower","data":{"userId":58559,"userName":"MyDream","channel_id":158456}}`:  {"58559", "MyDream"},
		`{"type":"follower","data":{"user_id":58559,"username":"MyDream","channel_id":158456}}`: {"58559", "MyDream"},
	}

	for raw, want := range cases {
		message, err := Decode([]byte(raw))
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		var follower Follower
		if err := message.Into(&follower); err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if follower.User() != want.id || follower.Name() != want.name {
			t.Errorf("%s → %s/%s, want %s/%s", raw, follower.User(), follower.Name(), want.id, want.name)
		}
		if follower.Channel != "158456" {
			t.Errorf("канал = %q", follower.Channel)
		}
	}
}

// get_poll спрашивает отдельно от get_poll_results: первое отдаёт само
// голосование, второе — результаты.
func TestGetPollRequest(t *testing.T) {
	message, err := New("get_poll", GetPollRequest{ChannelID: "5"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := message.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != `{"type":"get_poll","data":{"channel_id":"5"}}` {
		t.Errorf("get_poll = %s", line)
	}
}

// Удаление реплики: номер приходит строкой в запросе и числом в ответе. Если
// объявить его целым, запрос перестанет разбираться вовсе.
func TestRemovedMessageIDTakesBothForms(t *testing.T) {
	cases := map[string]string{
		`{"channel_id":"5","message_id":"100"}`:                                 "100",
		`{"channel_id":"163568","message_id":1693044447764,"adminName":"Homo"}`: "1693044447764",
	}

	for raw, want := range cases {
		var removed Removed
		if err := json.Unmarshal([]byte(raw), &removed); err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if string(removed.MessageID) != want {
			t.Errorf("%s → %q, want %q", raw, removed.MessageID, want)
		}
	}

	// И обратно: то, что мы читаем, должно уходить обратно в обоих видах.
	message, err := New("remove_message", Removed{Channel: "5", MessageID: "100"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := message.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), `"message_id":"100"`) {
		t.Errorf("remove_message = %s", line)
	}
}
