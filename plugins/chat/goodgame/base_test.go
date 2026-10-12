package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"studio/plugins/chat/goodgame/chat"
)

// Прерванное обновление не должно оставлять после себя половину базы. Человек
// жмёт кнопку, отваливается сеть или кончается место — и база, которая была
// вчера, обязана остаться вчерашней.
func TestReplaceThatFailsKeepsTheOldBase(t *testing.T) {
	st := openTestStore(t)
	file, list := parsedSmiles(t)

	if err := st.Replace(file, list, time.Now()); err != nil {
		t.Fatal(err)
	}
	before, err := st.Count()
	if err != nil {
		t.Fatal(err)
	}
	beforeAt, _ := st.UpdatedAt()

	// Тот же список, но с двумя одинаковыми смайлами: второй нарушает
	// первичный ключ, и запись падает уже на середине. Именно этот момент и
	// проверяем — половина базы успела записаться.
	poisoned := append(append([]chat.Smiley(nil), list...), list[0])

	if err := st.Replace(file, poisoned, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("Replace должен был упасть, а он нет")
	}

	after, err := st.Count()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("после сорванного обновления смайлов %d, было %d", after, before)
	}

	afterAt, ok := st.UpdatedAt()
	if !ok {
		t.Fatal("дата обновления пропала")
	}
	if !afterAt.Equal(beforeAt) {
		t.Errorf("дата обновления = %v, want %v — база не тронута, значит и дата прежняя",
			afterAt, beforeAt)
	}

	// И содержимое целое: не половина смайлов, а все.
	set, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != before {
		t.Errorf("в памяти %d смайлов, want %d", len(set), before)
	}
}

// База не должна браться из-под ног: пока идёт перезапись, читать нельзя.
func TestReplaceHoldsTheWrite(t *testing.T) {
	st := openTestStore(t)
	file, list := parsedSmiles(t)

	if err := st.Replace(file, list, time.Now()); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		for range 50 {
			if _, err := st.Load(); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	for range 20 {
		if err := st.Replace(file, list, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Errorf("чтение во время записи: %v", err)
	}
}

// Если базу не открыть вовсе, плагин всё равно должен работать: смайлы просто не
// подставятся.
func TestBookWithoutStore(t *testing.T) {
	book := &smileBook{}

	if _, ok := book.UpdatedAt(); ok {
		t.Error("пустая книга не должна помнить дату")
	}

	text, html := book.get().Views("текст :sing: текст2")
	if text != "текст :sing: текст2" {
		t.Errorf("text = %q, want исходную строку", text)
	}
	if html != "" {
		t.Errorf("html = %q, want пусто — базы нет", html)
	}
}

// Неудачное обновление не должно оставлять следов: база, которая была до
// попытки, обязана пережить её целиком, вместе с датой.
func TestUpdateThatFailsChangesNothing(t *testing.T) {
	t.Setenv("GOODGAME_SMILES_DB", filepath.Join(t.TempDir(), "data.db"))

	// Готовим базу, как будто человек обновлял её в прошлый раз и всё вышло.
	before, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	file, list := parsedSmiles(t)
	if err := before.Replace(file, list, time.Now()); err != nil {
		t.Fatal(err)
	}
	beforeAt, _ := before.UpdatedAt()
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	// Теперь сеть обрывается на середине.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Smiles": [`))
		// Дальше клиент увидит обрыв и вернёт ошибку.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	was := smilesURL
	smilesURL = server.URL + "/global.json"
	defer func() { smilesURL = was }()

	st, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	book := &smileBook{store: st}

	if err := book.Update(context.Background()); err == nil {
		t.Fatal("Update должен был упасть, а он нет")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Открываем заново и смотрим, что пережило неудачную попытку.
	after, err := openStore()
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()

	got, err := after.Count()
	if err != nil {
		t.Fatal(err)
	}
	if got != len(flatten(file)) {
		t.Errorf("после неудачного обновления смайлов %d, want %d", got, len(flatten(file)))
	}

	gotAt, ok := after.UpdatedAt()
	if !ok {
		t.Fatal("дата пропала после неудачного обновления")
	}
	if !gotAt.Equal(beforeAt) {
		t.Errorf("дата = %v, want %v", gotAt, beforeAt)
	}

	// И в памяти то же, что на диске: загружено по-настоящему, а не осталось
	// от прошлого раза.
	set, err := after.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != len(flatten(file)) {
		t.Errorf("в памяти %d смайлов, want %d", len(set), len(flatten(file)))
	}
}
