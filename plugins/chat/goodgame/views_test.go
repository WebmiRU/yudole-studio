package main

import (
	"strings"
	"testing"
)

// Подстановка смайлов — единственное место, где текст превращается в разметку,
// и ошибиться тут легко: строка в реплике может быть какой угодно. Каждый
// случай ниже — то, что приходит из чата по-настоящему.
func TestViews(t *testing.T) {
	base := smileSet{
		":sing:":   {Code: ":sing:", Image: "https://gg/sing.gif"},
		":jokie5:": {Code: ":jokie5:", Image: "https://gg/jokie5-big.png"},
	}

	cases := []struct {
		name string
		src  string
		text string
		html string
	}{
		{
			// Самый частый случай: смайлов нет. Вторая строка пустая, первая —
			// исходная целиком.
			name: "без смайлов",
			src:  "просто слова",
			text: "просто слова",
			html: "",
		},
		{
			name: "смайл в середине",
			src:  "текст :sing: текст2",
			text: "текст  текст2",
			html: `текст <img class="smile" src="https://gg/sing.gif" alt=":sing:" /> текст2`,
		},
		{
			name: "только смайл",
			src:  ":sing:",
			text: "",
			html: `<img class="smile" src="https://gg/sing.gif" alt=":sing:" />`,
		},
		{
			name: "два подряд",
			src:  ":sing::jokie5:",
			text: "",
			html: `<img class="smile" src="https://gg/sing.gif" alt=":sing:" />` +
				`<img class="smile" src="https://gg/jokie5-big.png" alt=":jokie5:" />`,
		},
		{
			// Кода в базе нет — значит это не смайл, а просто текст с
			// двоеточиями. Есть такой код не должен.
			name: "неизвестный код",
			src:  "встреча в :нет: такого",
			text: "встреча в :нет: такого",
			html: "",
		},
		{
			// Обычный текст с двоеточиями далеко друг от друга. Потолок на длину
			// кода спасает от того, чтобы искать пару через всю строку.
			name: "двоеточия в обычном тексте",
			src:  "собрание в 12:30, а потом в 18:45 и снова в 20:00",
			text: "собрание в 12:30, а потом в 18:45 и снова в 20:00",
			html: "",
		},
		{
			name: "незакрытое двоеточие",
			src:  "текст :sing",
			text: "текст :sing",
			html: "",
		},
		{
			// Текст реплики не должен становиться разметкой.
			name: "спецсимволы в тексте",
			src:  `5 < 6 & "кавычки" :sing: <script>`,
			text: `5 < 6 & "кавычки"  <script>`,
			html: `5 &lt; 6 &amp; "кавычки" ` +
				`<img class="smile" src="https://gg/sing.gif" alt=":sing:" /> ` +
				`&lt;script&gt;`,
		},
		{
			name: "смайл в начале",
			src:  ":sing: привет",
			text: " привет",
			html: `<img class="smile" src="https://gg/sing.gif" alt=":sing:" /> привет`,
		},
		{
			name: "смайл в конце",
			src:  "привет :sing:",
			text: "привет ",
			html: `привет <img class="smile" src="https://gg/sing.gif" alt=":sing:" />`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, html := base.Views(c.src)
			if text != c.text {
				t.Errorf("text =\n  %q\nwant\n  %q", text, c.text)
			}
			if html != c.html {
				t.Errorf("html =\n  %q\nwant\n  %q", html, c.html)
			}
		})
	}
}

// Разметка в строке реплики — это код, а не смайл, даже если рядом двоеточие.
// Проверяем, что в alt не может просочиться ничего, обрывающее атрибут.
func TestViewsEscapesAttribute(t *testing.T) {
	base := smileSet{
		`:onmouseover="alert(1):`: {Code: `:onmouseover="alert(1):`, Image: `x" onerror="alert(2)`},
	}

	_, html := base.Views(`:onmouseover="alert(1):`)

	if strings.Contains(html, `onmouseover="alert(1):"`) {
		t.Errorf("кавычка пролезла в alt: %s", html)
	}
	if !strings.Contains(html, `&#34;`) {
		t.Errorf("кавычки не экранированы: %s", html)
	}
}

// Пустая база — обычное дело: плагин запустился, а кнопку никто ещё не жал.
func TestViewsEmptyBase(t *testing.T) {
	var empty smileSet

	text, html := empty.Views("текст :sing: текст2")
	if text != "текст :sing: текст2" {
		t.Errorf("text = %q, want исходную строку", text)
	}
	if html != "" {
		t.Errorf("html = %q, want пусто", html)
	}
}

// Строка без смайлов не должна ни выделять памяти, ни копироваться. Это
// подавляющее большинство реплик, и ради них весь разбор и написан так, чтобы
// не трогать кучу.
func TestViewsWithoutSmilesDoesNotAllocate(t *testing.T) {
	base := smileSet{
		":sing:": {Code: ":sing:", Image: "https://gg/sing.gif"},
	}

	one := testing.AllocsPerRun(100, func() {
		base.Views("обычная реплика без единого смайла")
	})
	if one != 0 {
		t.Errorf("выделений на реплику без смайлов = %v, want 0", one)
	}
}

// Три строки реплики — три разных обещания. Src обязан остаться ровно тем, что
// прислал сервис, даже если там угловые скобки и кавычки.
func TestLineKeepsSrcUntouched(t *testing.T) {
	book := &smileBook{}
	book.put(smileSet{
		":sing:": {Code: ":sing:", Image: "https://gg/sing.gif"},
	})
	s := &socket{book: book}

	const src = `[b]текст[/b] :sing: <script>alert(1)</script> &quot;`
	got := s.line(src)

	if got.Src != src {
		t.Errorf("src = %q,\nwant %q\nисходник не трогаем вообще", got.Src, src)
	}
	if strings.Contains(got.Html, "<script>") {
		t.Errorf("разметка реплики попала в html: %s", got.Html)
	}
	if strings.Contains(got.Text, ":sing:") {
		t.Errorf("код смайла остался в тексте: %q", got.Text)
	}
	if !strings.Contains(got.Html, "<img class=\"smile\"") {
		t.Errorf("в html нет картинки: %s", got.Html)
	}
}
