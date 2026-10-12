package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const vkVideoSmileOrigin = "https://images.live.vkvideo.ru"

var (
	vkVideoSmileIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,80}$`)
	vkVideoSmileClient    = &http.Client{
		Timeout: 12 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// The proxy is intentionally limited to the VK smile CDN, even if
			// that CDN ever responds with a redirect to another host.
			if req.URL.Scheme != "https" || req.URL.Host != "images.live.vkvideo.ru" {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
)

// vkVideoSmileHandler serves smile images through the studio origin. Some
// browsers or CDN policies block the original image when it is embedded in a
// third-party page, even though opening its URL directly works.
func vkVideoSmileHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyVKVideoSmile(w, r, vkVideoSmileClient, vkVideoSmileOrigin)
	})
}

// proxyVKVideoSmile only builds requests to a fixed CDN host and a constrained
// path. It is deliberately not a general-purpose URL proxy.
func proxyVKVideoSmile(w http.ResponseWriter, r *http.Request, client *http.Client, origin string) {
	id := r.PathValue("id")
	size := r.PathValue("size")
	if !vkVideoSmileIDPattern.MatchString(id) {
		http.Error(w, "invalid VK smile id", http.StatusBadRequest)
		return
	}
	switch size {
	case "small", "medium", "large":
	default:
		http.Error(w, "invalid VK smile size", http.StatusBadRequest)
		return
	}

	query := url.Values{}
	if changeTime := r.URL.Query().Get("change_time"); changeTime != "" {
		if len(changeTime) > 20 {
			http.Error(w, "invalid change_time", http.StatusBadRequest)
			return
		}
		for _, c := range changeTime {
			if c < '0' || c > '9' {
				http.Error(w, "invalid change_time", http.StatusBadRequest)
				return
			}
		}
		query.Set("change_time", changeTime)
	}

	target, err := url.Parse(origin)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" {
		http.Error(w, "VK smile proxy is misconfigured", http.StatusInternalServerError)
		return
	}
	target.Path = strings.TrimRight(target.Path, "/") + fmt.Sprintf("/smile/%s/icon/size/%s", id, size)
	target.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		http.Error(w, "failed to prepare VK smile request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	req.Header.Set("Referer", "https://live.vkvideo.ru/")

	response, err := client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		slog.Warn("VK smile proxy: upstream request failed", "smile", id, "error", err)
		http.Error(w, "VK smile image is temporarily unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(contentType, "image/") {
		slog.Warn("VK smile proxy: upstream returned an unexpected response",
			"smile", id, "status", response.StatusCode, "content_type", contentType)
		http.Error(w, "VK smile image is temporarily unavailable", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if length := response.Header.Get("Content-Length"); length != "" {
		w.Header().Set("Content-Length", length)
	}
	if _, err := io.Copy(w, response.Body); err != nil {
		slog.Warn("VK smile proxy: writing image failed", "smile", id, "error", err)
	}
}
