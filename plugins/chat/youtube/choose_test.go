package main

// Выбор пути чтения: официальный API, если ключ жив, иначе резервный.
// Проверяем ровно то, что ломается на практике: битый ключ, конченная квота и
// запрет на резерв.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNewWatcherWithoutKeyGoesKeyless(t *testing.T) {
	for _, key := range []string{"", "   "} {
		w := newWatcher(key, "@handle")
		if !w.keyless {
			t.Errorf("с ключом %q ждали резервный путь, а получили официальный", key)
		}
	}
}

func TestNewWatcherWithKeyStartsOfficial(t *testing.T) {
	w := newWatcher("AIzaSyTest", "@handle")
	if w.keyless {
		t.Error("с ключом ждали официальный путь, а получили резервный")
	}
}

// Ключ может быть неверен, отозван или исчерпал квоту — все три случаи ведут
// в резерв, потому что официальным путём дальше читать нечем.
func TestKeyUnusable(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
	} {
		err := &apiError{Status: status, Message: "whatever"}
		if !keyUnusable(err) {
			t.Errorf("ошибка %d должна вести в резерв", status)
		}
	}

	if keyUnusable(nil) {
		t.Error("без ошибки резерв включаться не должен")
	}
	// Прочие сбои — сеть, 5xx — резервным не считаются: возможно, ключ цел.
	if keyUnusable(errors.New("соединение разорвано")) {
		t.Error("сетевая ошибка не должна включать резерв")
	}
	if keyUnusable(&apiError{Status: http.StatusNotFound, Message: "нет такого метода"}) {
		t.Error("404 не должен включать резерв: тут дело в запросе, а не в ключе")
	}
}

// Однажды перейдя на резерв, обратно не возвращаемся: ключ, которому кончилась
// квота, от повторных попыток её не вернёт — только растратит остаток.
func TestWatcherSwitchesToKeylessOnce(t *testing.T) {
	w := newWatcher("AIzaSyTest", "@handle")
	w.allowFallback = true

	w.useKeyless("кончилась квота")
	if !w.keyless {
		t.Fatal("резервный путь не включился")
	}

	// Повторный вызов не должен ничего ломать и не должен шуметь.
	w.useKeyless("ещё раз")
	if !w.keyless {
		t.Error("повторный вызов сбросил выбор пути")
	}
}

// Если человек выключил резерв, официальный путь не смешается тихо на другой:
// это его выбор, и нарушать его молча нельзя.
func TestWatcherKeepsOfficialWhenFallbackForbidden(t *testing.T) {
	w := newWatcher("AIzaSyTest", "@handle")
	w.allowFallback = false

	if w.canUseFallback() {
		t.Fatal("резерв не должен быть разрешён")
	}
	if w.useKeyless("ключ отозван") {
		t.Error("переключиться нельзя: резерв запрещён настройкой")
	}
	if w.keyless {
		t.Error("путь переключился вопреки запрету")
	}
}

// Официальный путь надо покидать по двум разным причинам: ключ негоден либо
// поток не отдаёт сообщений. Второе — не про ключ, но чинить его иначе нечем.
func TestPathBrokenCoversKeyAndDeadStream(t *testing.T) {
	if !pathBroken(&apiError{Status: 403}) {
		t.Error("отозванный ключ должен считаться сломанным путём")
	}
	if !pathBroken(fmt.Errorf("обрыв: %w", errOfficialReadBroken)) {
		t.Error("мёртвый поток должен считаться сломанным путём")
	}
	if pathBroken(errors.New("сеть моргнула")) {
		t.Error("обычная ошибка не должна считаться сломанным путём")
	}
	if pathBroken(nil) {
		t.Error("без ошибки путь не сломан")
	}
}

// Эфир, найденный фидом канала, идёт дальше без id чата: там его и быть не
// может. Если не заметить этого и позвать официальное чтение, streamList
// получит пустоту и откажет — а выглядит это так, будто сломан ключ.
func TestAnyWithoutChat(t *testing.T) {
	if anyWithoutChat([]liveVideo{{ID: "a", Chat: "c"}, {ID: "b", Chat: "d"}}) {
		t.Error("у всех эфиров есть id чата")
	}
	if !anyWithoutChat([]liveVideo{{ID: "a", Chat: "c"}, {ID: "b"}}) {
		t.Error("пустой id чата должен замечаться")
	}
	if anyWithoutChat(nil) {
		t.Error("пустой список нечего проверять")
	}
}

// Путь чтения не должен светить ключ в журнале: журнал видно в интерфейсе и он
// уезжает в файлы.
func TestWatcherStringKeepsKeySecret(t *testing.T) {
	withKey := &watcher{apiKey: "AIzaSyDhv7eykPRiszrRtgF0iN8lzj6_bpIiFl8"}
	if got := withKey.String(); strings.Contains(got, "Fl8") || strings.Contains(got, "AIza") {
		t.Errorf("ключ утёк в описание пути: %q", got)
	}
	if !strings.Contains(withKey.String(), "с ключом") {
		t.Errorf("не сказано, что путь официальный: %q", withKey.String())
	}
	if got := (&watcher{keyless: true}).String(); got != "без ключа" {
		t.Errorf("резервный путь назван неверно: %q", got)
	}
}

func TestQuotaExhaustedText(t *testing.T) {
	for _, message := range []string{
		"Quota exceeded for quota metric 'Queries'",
		"dailyLimitExceeded",
		"QUOTA_EXCEEDED",
	} {
		if !quotaExhausted(message) {
			t.Errorf("«%s» должно читаться как исчерпанная квота", message)
		}
	}
	if quotaExhausted("channel not found") {
		t.Error("«channel not found» — не про квоту")
	}
}

func TestSelectChatContinuationPrefersInvalidationRegardlessOfOrder(t *testing.T) {
	live := map[string]any{
		"continuations": []any{
			map[string]any{
				"timedContinuationData": map[string]any{
					"continuation": "timed-cursor",
					"timeoutMs":    float64(12000),
				},
			},
			map[string]any{
				"invalidationContinuationData": map[string]any{
					"continuation": "invalidation-cursor",
					"timeoutMs":    float64(15000),
				},
			},
		},
	}

	got, wait := selectChatContinuation(live)
	if got != "invalidation-cursor" || wait != 15000 {
		t.Fatalf("selected continuation = %q / %d ms; want invalidation-cursor / 15000 ms", got, wait)
	}
}

func TestSelectChatContinuationFallsBackToTimed(t *testing.T) {
	live := map[string]any{
		"continuations": []any{
			map[string]any{
				"timedContinuationData": map[string]any{
					"continuation": "timed-cursor",
					"timeoutMs":    float64(8000),
				},
			},
		},
	}

	got, wait := selectChatContinuation(live)
	if got != "timed-cursor" || wait != 8000 {
		t.Fatalf("selected continuation = %q / %d ms; want timed-cursor / 8000 ms", got, wait)
	}
}

func TestSelectChatContinuationIgnoresUnknownKinds(t *testing.T) {
	live := map[string]any{
		"continuations": []any{
			map[string]any{
				"unknownContinuationData": map[string]any{
					"continuation": "wrong-cursor",
					"timeoutMs":    float64(10000),
				},
			},
		},
	}

	got, wait := selectChatContinuation(live)
	if got != "" || wait != 5000 {
		t.Fatalf("selected continuation = %q / %d ms; want empty / 5000 ms", got, wait)
	}
}

func TestKeylessRetryDelayIsBounded(t *testing.T) {
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 0, want: time.Second},
		{failures: 1, want: time.Second},
		{failures: 2, want: 2 * time.Second},
		{failures: 3, want: 5 * time.Second},
		{failures: 4, want: 10 * time.Second},
		{failures: 6, want: 15 * time.Second},
		{failures: 8, want: 30 * time.Second},
		{failures: 100, want: 30 * time.Second},
	}
	for _, test := range tests {
		if got := keylessRetryDelay(test.failures); got != test.want {
			t.Errorf("keylessRetryDelay(%d) = %s; want %s", test.failures, got, test.want)
		}
	}
}
