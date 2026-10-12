package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVKVideoSmileProxyServesImage(t *testing.T) {
	var gotPath, gotQuery, gotReferer string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("change_time")
		gotReferer = r.Header.Get("Referer")
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write([]byte("GIF89a-test-image"))
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodGet,
		"/api/vkvideo-smile/abc-123/icon/size/large?change_time=12345", nil)
	request.SetPathValue("id", "abc-123")
	request.SetPathValue("size", "large")
	recorder := httptest.NewRecorder()

	proxyVKVideoSmile(recorder, request, upstream.Client(), upstream.URL)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if got, want := recorder.Header().Get("Content-Type"), "image/gif"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := recorder.Header().Get("X-Content-Type-Options"), "nosniff"; got != want {
		t.Errorf("X-Content-Type-Options = %q, want %q", got, want)
	}
	if got, want := recorder.Body.String(), "GIF89a-test-image"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if got, want := gotPath, "/smile/abc-123/icon/size/large"; got != want {
		t.Errorf("upstream path = %q, want %q", got, want)
	}
	if got, want := gotQuery, "12345"; got != want {
		t.Errorf("upstream change_time = %q, want %q", got, want)
	}
	if got, want := gotReferer, "https://live.vkvideo.ru/"; got != want {
		t.Errorf("upstream Referer = %q, want %q", got, want)
	}
}

func TestVKVideoSmileProxyRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name string
		id string
		size string
		query string
	}{
		{name: "path traversal", id: "../admin", size: "large"},
		{name: "unsupported size", id: "abc-123", size: "original"},
		{name: "invalid cache time", id: "abc-123", size: "large", query: "?change_time=1%26host%3Devil"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet,
				"/api/vkvideo-smile/"+test.id+"/icon/size/"+test.size+test.query, nil)
			request.SetPathValue("id", test.id)
			request.SetPathValue("size", test.size)
			recorder := httptest.NewRecorder()

			proxyVKVideoSmile(recorder, request, http.DefaultClient, "http://127.0.0.1:1")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestVKVideoSmileProxyRejectsNonImageResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, "<html>blocked</html>")
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodGet, "/api/vkvideo-smile/abc-123/icon/size/large", nil)
	request.SetPathValue("id", "abc-123")
	request.SetPathValue("size", "large")
	recorder := httptest.NewRecorder()

	proxyVKVideoSmile(recorder, request, upstream.Client(), upstream.URL)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "temporarily unavailable") {
		t.Errorf("unexpected body: %q", recorder.Body.String())
	}
}
