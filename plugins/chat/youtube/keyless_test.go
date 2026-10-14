package main

import "testing"

// Страница /live разбирается тем же разбором, что и страница эфира. Проверяем
// на кусках настоящей разметки: главное — взять id идущей трансляции, а не
// первого попавшегося из прошлых.
func TestKeylessParseWatchPageLive(t *testing.T) {
	// Порядок как в настоящей странице: id идущей трансляции, потом
	// перечисленные ниже прошлые эфиры.
	page := `{
		"videoDetails":{"videoId":"RPAd9WBKM6c","author":"FIKUS_TV"},
		"playabilityStatus":{"isLive":true},
		"videoId":"ji3g1SWX7lc",
		"INNERTUBE_API_KEY":"AIzaSyTEST",
		"continuation":"CgtVQ2h0dHlwZS4xMjM0NTY3ODkwQ2hpZDEyMzQ1Njc4OTAxMg"
	}`

	got := keylessParseWatchPage(page)
	if got.videoID != "RPAd9WBKM6c" {
		t.Errorf("videoID = %q, а нужен RPAd9WBKM6c", got.videoID)
	}
	if !got.isLive {
		t.Error("isLive = false, а эфир идёт")
	}
	if got.token == "" {
		t.Error("токен не прочитан")
	}
	if got.apiKey != "AIzaSyTEST" {
		t.Errorf("apiKey = %q", got.apiKey)
	}
}

// Запись, а не эфир: токен в разметке есть, а признака эфира нет.
func TestKeylessParseWatchPageArchive(t *testing.T) {
	page := `{"videoId":"1dk3AMwx0_w","lengthSeconds":"1200","isLive":false}`

	got := keylessParseWatchPage(page)
	if got.isLive {
		t.Error("isLive = true у записи")
	}
	if got.videoID != "1dk3AMwx0_w" {
		t.Errorf("videoID = %q", got.videoID)
	}
}

// Эфир без токена — не эфир, а запись с пустым продолжением.
func TestKeylessParseWatchPageNoLive(t *testing.T) {
	page := `{"videoId":"aaaaaaaaaaa","lengthSeconds":"0","liveStreamability":{}}`

	got := keylessParseWatchPage(page)
	if !got.isLive {
		t.Error("нулевая длина при liveStreamability должна считаться эфиром")
	}
	if got.token != "" {
		t.Errorf("token = %q, а токена на странице нет", got.token)
	}
}

func TestKeylessParseWatchPageEmpty(t *testing.T) {
	got := keylessParseWatchPage("")
	if got.videoID != "" || got.isLive || got.token != "" {
		t.Errorf("пустая разметка разобрана не как пустая: %+v", got)
	}
}
