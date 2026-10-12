package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestParseChannelRef(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		normalized string
		handle     string
		id         string
		wantErr    bool
	}{
		{
			name: "handle URL", input: "https://www.youtube.com/@Paradoxlivechannel",
			normalized: "@Paradoxlivechannel", handle: "Paradoxlivechannel",
		},
		{
			name: "live tab URL", input: "https://youtube.com/@Paradoxlivechannel/live",
			normalized: "@Paradoxlivechannel", handle: "Paradoxlivechannel",
		},
		{
			name: "bare handle", input: "Paradoxlivechannel",
			normalized: "@Paradoxlivechannel", handle: "Paradoxlivechannel",
		},
		{
			name: "handle with at", input: "@Paradoxlivechannel",
			normalized: "@Paradoxlivechannel", handle: "Paradoxlivechannel",
		},
		{
			name: "URL without scheme", input: "www.youtube.com/@Paradoxlivechannel",
			normalized: "@Paradoxlivechannel", handle: "Paradoxlivechannel",
		},
		{
			name: "Unicode handle", input: "@ТестовыйКанал",
			normalized: "@ТестовыйКанал", handle: "ТестовыйКанал",
		},
		{
			name: "channel ID", input: "UC1234567890123456789012",
			normalized: "UC1234567890123456789012", id: "UC1234567890123456789012",
		},
		{
			name: "channel ID URL", input: "https://www.youtube.com/channel/UC1234567890123456789012",
			normalized: "UC1234567890123456789012", id: "UC1234567890123456789012",
		},
		{name: "wrong host", input: "https://example.com/@channel", wantErr: true},
		{name: "custom URL", input: "https://youtube.com/c/LegacyName", wantErr: true},
		{name: "not a handle", input: "two words", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChannelRef(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseChannelRef(%q) succeeded unexpectedly: %#v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Normalized != tt.normalized || got.Handle != tt.handle || got.ID != tt.id {
				t.Fatalf("parseChannelRef(%q) = %#v", tt.input, got)
			}
		})
	}
}

// Ссылка на канал и ссылка на стрим — разные вещи, и плагин обязан их различать:
// канал надо искать, а к стриму идут сразу.
func TestParseChannelRefTellsChannelFromVideo(t *testing.T) {
	videos := []string{
		"https://www.youtube.com/watch?v=l00tBimYE0U",
		"https://youtube.com/watch?v=l00tBimYE0U&t=42s",
		"https://m.youtube.com/watch?v=l00tBimYE0U",
		"https://youtu.be/l00tBimYE0U",
		"l00tBimYE0U",
	}
	for _, input := range videos {
		ref, err := parseChannelRef(input)
		if err != nil {
			t.Errorf("разбор %q: %v", input, err)
			continue
		}
		if ref.Video != "l00tBimYE0U" {
			t.Errorf("%q распознан не как стрим: %#v", input, ref)
		}
	}

	channels := []string{
		"https://www.youtube.com/@digitalmediacorp",
		"@digitalmediacorp",
		"https://www.youtube.com/channel/UCho7zQ31MRzmku1VmXYT_4Q",
		"UCho7zQ31MRzmku1VmXYT_4Q",
	}
	for _, input := range channels {
		ref, err := parseChannelRef(input)
		if err != nil {
			t.Errorf("разбор %q: %v", input, err)
			continue
		}
		if ref.Video != "" {
			t.Errorf("%q принят за стрим: %#v", input, ref)
		}
	}
}

// Каналов и стримов может быть сколько угодно: эфир иногда идёт двумя
// трансляциями сразу — обычно короткой и обычной.
func TestParseSettingsKeepsEveryTarget(t *testing.T) {
	got, fields := parseSettings(map[string]any{
		"api_key": "",
		"channel": []any{
			"https://www.youtube.com/@digitalmediacorp",
			"https://www.youtube.com/watch?v=l00tBimYE0U",
			"@digitalmediacorp", // повтор — должен отсеяться
		},
	})
	if len(fields) != 0 {
		t.Fatalf("ошибки: %#v", fields)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("целей %d, want 2: %#v", len(got.Targets), got.Targets)
	}
	if got.Targets[0].video != "" {
		t.Error("первая цель должна быть каналом")
	}
	if got.Targets[1].video != "l00tBimYE0U" {
		t.Errorf("вторая цель должна быть стримом: %#v", got.Targets[1])
	}

	if got := directVideos(got.Targets); len(got) != 1 || got[0] != "l00tBimYE0U" {
		t.Errorf("directVideos = %v", got)
	}
}

func TestParseSettingsDoesNotRequireFutureAPIKey(t *testing.T) {
	got, fields := parseSettings(map[string]any{"api_key": "", "channel": []any{}})
	if len(fields) != 0 {
		t.Fatalf("unexpected validation errors: %#v", fields)
	}
	if got.APIKey != "" || len(got.Targets) != 0 {
		t.Fatalf("blank settings changed unexpectedly: %#v", got)
	}
	if !got.Fallback {
		t.Error("резервное чтение должно быть включено по умолчанию")
	}
}

func TestParseSettingsNormalizesChannelAndAPIKey(t *testing.T) {
	got, fields := parseSettings(map[string]any{
		"api_key": "  test-key  ",
		"channel": []any{"https://www.youtube.com/@Paradoxlivechannel/live"},
	})
	if len(fields) != 0 {
		t.Fatalf("unexpected validation errors: %#v", fields)
	}
	if got.APIKey != "test-key" {
		t.Fatalf("API key was not trimmed: %q", got.APIKey)
	}
	if len(got.Targets) != 1 || got.Targets[0].label != "@Paradoxlivechannel" {
		t.Fatalf("channel was not normalized: %#v", got.Targets)
	}
}

func TestParseSettingsRejectsWhitespaceInAPIKey(t *testing.T) {
	_, fields := parseSettings(map[string]any{
		"api_key": "key with space",
		"channel": "@somechannel",
	})
	if fields["api_key"] == "" {
		t.Fatalf("expected API key validation error, got %#v", fields)
	}
}

func TestAPIRequestParsesJSONAndIncludesKey(t *testing.T) {
	oldBase, oldClient := apiBase, httpClient
	defer func() {
		apiBase, httpClient = oldBase, oldClient
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "test-key" {
			t.Errorf("key query parameter = %q", r.URL.Query().Get("key"))
		}
		if r.URL.Query().Get("part") != "id,snippet" {
			t.Errorf("part query parameter = %q", r.URL.Query().Get("part"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"UC123"}]}`))
	}))
	defer server.Close()
	apiBase = server.URL + "/"
	httpClient = server.Client()

	var result struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	err := apiRequest(t.Context(), "test-key", "channels", url.Values{"part": {"id,snippet"}}, &result)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || result.Items[0].ID != "UC123" {
		t.Fatalf("unexpected JSON response: %#v", result)
	}
}

func TestResolveChannelAndFindActiveLives(t *testing.T) {
	oldBase, oldClient := apiBase, httpClient
	defer func() {
		apiBase, httpClient = oldBase, oldClient
	}()

	const channelID = "UC1234567890123456789012"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/channels":
			if r.URL.Query().Get("forHandle") != "Paradoxlivechannel" {
				t.Errorf("forHandle = %q", r.URL.Query().Get("forHandle"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{map[string]any{
					"id":      channelID,
					"snippet": map[string]any{"title": "Paradox Live"},
				}},
			})
		case "/search":
			if r.URL.Query().Get("channelId") != channelID ||
				r.URL.Query().Get("type") != "video" ||
				r.URL.Query().Get("eventType") != "live" {
				t.Errorf("unexpected search query: %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{
					map[string]any{
						"id":      map[string]any{"videoId": "video-with-chat"},
						"snippet": map[string]any{"title": "Live now"},
					},
					map[string]any{
						"id":      map[string]any{"videoId": "video-without-chat"},
						"snippet": map[string]any{"title": "Chat disabled"},
					},
				},
			})
		case "/videos":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{
					map[string]any{
						"id":                   "video-with-chat",
						"snippet":              map[string]any{"title": "Live now"},
						"liveStreamingDetails": map[string]any{"activeLiveChatId": "chat-123"},
					},
					map[string]any{
						"id":                   "video-without-chat",
						"snippet":              map[string]any{"title": "Chat disabled"},
						"liveStreamingDetails": map[string]any{},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	apiBase = server.URL + "/"
	httpClient = server.Client()

	id, title, err := resolveChannel(t.Context(), "test-key", "@Paradoxlivechannel")
	if err != nil {
		t.Fatal(err)
	}
	if id != channelID || title != "Paradox Live" {
		t.Fatalf("resolveChannel() = %q, %q", id, title)
	}

	lives, err := findActiveLives(t.Context(), "test-key", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(lives) != 1 {
		t.Fatalf("found %d active chats, want 1: %#v", len(lives), lives)
	}
	if lives[0].ID != "video-with-chat" || lives[0].Title != "Live now" || lives[0].Chat != "chat-123" {
		t.Fatalf("unexpected active live: %#v", lives[0])
	}
}

func TestAPIRequestReturnsStructuredError(t *testing.T) {
	oldBase, oldClient := apiBase, httpClient
	defer func() {
		apiBase, httpClient = oldBase, oldClient
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    http.StatusForbidden,
				"message": "quota exceeded",
				"errors": []any{
					map[string]any{"reason": "quotaExceeded"},
				},
			},
		})
	}))
	defer server.Close()
	apiBase = server.URL + "/"
	httpClient = server.Client()

	err := apiRequest(t.Context(), "test-key", "search", nil, &struct{}{})
	if err == nil {
		t.Fatal("expected an API error")
	}
	apiErr, ok := err.(*apiError)
	if !ok {
		t.Fatalf("error type = %T, want *apiError", err)
	}
	if apiErr.Reason != "quotaExceeded" || !apiErr.Permanent() {
		t.Fatalf("unexpected API error: %#v", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "quotaExceeded") {
		t.Fatalf("error text doesn't include reason: %v", apiErr)
	}
}

func TestRetryDelayIsBounded(t *testing.T) {
	if got := retryDelay(0); got != 5*time.Second {
		t.Fatalf("retryDelay(0) = %s", got)
	}
	if got := retryDelay(20); got != maxChatBackoff {
		t.Fatalf("retryDelay(20) = %s; want %s", got, maxChatBackoff)
	}
}


func TestUpdateContinuationTokenPreservesCursorWhenMissing(t *testing.T) {
	const cursor = "cursor-123"

	if got, ok := updateContinuationToken(cursor, ""); got != cursor || ok {
		t.Fatalf("missing token = %q, %v; want previous cursor %q, false", got, ok, cursor)
	}
	if got, ok := updateContinuationToken("old", "  next-456  "); got != "next-456" || !ok {
		t.Fatalf("new token = %q, %v; want trimmed token, true", got, ok)
	}
	if got, ok := updateContinuationToken("", ""); got != "" || ok {
		t.Fatalf("empty initial token = %q, %v; want empty, false", got, ok)
	}
}

func TestStreamRetryDelayDoesNotGrowAfterAValidResponse(t *testing.T) {
	for _, attempt := range []int{0, 1, 4, 20} {
		delay, nextAttempt := streamRetryDelay(attempt, true)
		if delay != streamRestartDelay || nextAttempt != 0 {
			t.Errorf("streamRetryDelay(%d, true) = (%s, %d); want (%s, 0)",
				attempt, delay, nextAttempt, streamRestartDelay)
		}
	}

	delay, nextAttempt := streamRetryDelay(0, false)
	if delay != 5*time.Second || nextAttempt != 1 {
		t.Errorf("first failed attempt = (%s, %d); want (5s, 1)", delay, nextAttempt)
	}
	delay, nextAttempt = streamRetryDelay(2, false)
	if delay != 20*time.Second || nextAttempt != 3 {
		t.Errorf("third failed attempt = (%s, %d); want (20s, 3)", delay, nextAttempt)
	}
}


func TestGRPCChatDefinitelyEndedRequiresExplicitSignal(t *testing.T) {
	for _, message := range []string{
		"chat ended",
		"chat has ended",
		"chat is disabled",
		"broadcast has ended",
		"stream is offline",
		"live chat is no longer live",
	} {
		if !grpcChatDefinitelyEnded(errors.New(message)) {
			t.Errorf("grpcChatDefinitelyEnded(%q) = false; want true", message)
		}
	}

	for _, message := range []string{
		"rpc error: code = FailedPrecondition desc = unsupported request",
		"rpc error: code = NotFound desc = endpoint unavailable",
		"connection reset by peer",
	} {
		if grpcChatDefinitelyEnded(errors.New(message)) {
			t.Errorf("grpcChatDefinitelyEnded(%q) = true; want false", message)
		}
	}
}


func TestPermanentGRPCErrorIncludesQuotaAndUnsupported(t *testing.T) {
	for _, code := range []codes.Code{
		codes.InvalidArgument,
		codes.Unauthenticated,
		codes.PermissionDenied,
		codes.ResourceExhausted,
		codes.Unimplemented,
	} {
		if !permanentGRPCError(status.Error(code, "test")) {
			t.Errorf("permanentGRPCError(%s) = false; want true", code)
		}
	}
}
