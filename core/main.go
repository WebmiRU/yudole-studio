// Command studio runs the studio core: an HTTP server for the front-end.
//
// Development: `npm run dev` serves the sources with HMR on :1081.
// Production:   `npm run build` + this server serving dist/ on :1080.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	defaultAddr      = ":1080"
	defaultDevTarget = "http://localhost:1081"

	// sourceDirName holds the unbuilt sources; distDirName is the Vite output.
	sourceDirName = "html"
	distDirName   = "dist"
)

// defaultWebDir prefers the build output and falls back to the sources, so the
// server works before the first `npm run build`.
func defaultWebDir() string {
	base := baseDir()
	for _, name := range []string{distDirName, sourceDirName} {
		if candidate := filepath.Join(base, name); isDir(candidate) {
			return candidate
		}
	}
	return filepath.Join(base, sourceDirName)
}

// baseDir finds the project directory: next to the executable when it holds a
// front-end, otherwise the working directory (how `go run` executes).
func baseDir() string {
	if exe, err := os.Executable(); err == nil {
		if base := filepath.Dir(exe); isWebRoot(base) {
			return base
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "."
}

func isWebRoot(dir string) bool {
	return isDir(filepath.Join(dir, distDirName)) || isDir(filepath.Join(dir, sourceDirName))
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "studio: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("STUDIO_ADDR", defaultAddr), "address to listen on")
	webDir := flag.String("web", envOr("STUDIO_WEB", defaultWebDir()), "directory with the built front-end (npm run build -> dist)")
	shutdownTimeout := flag.Duration("shutdown-timeout", 5*time.Second, "how long to wait for in-flight requests on shutdown")
	flag.Parse()

	page, err := newWeb(*webDir)
	if err != nil {
		return err
	}
	// Исходники без сборки отдаются как есть: стили и модульный скрипт в
	// браузере не заработают. Говорим об этом сразу, а не молча ломаем страницу.
	if filepath.Base(page.dir) == sourceDirName {
		slog.Warn("serving html/ sources without a build: the page will look broken",
			"hint", "run `npm run build`")
	}

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", healthHandler(page))
	mux.Handle("/", page)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listenErr := make(chan error, 1)
	go func() {
		slog.Info("studio: serving front-end",
			"addr", srv.Addr,
			"web", page.dir,
			"url", "http://"+displayAddr(srv.Addr)+"/",
		)
		// HMR живёт только в дев-сервере: собранная страница обновляется
		// лишь после npm run build. Напоминаем, где включается разработка.
		slog.Info("studio: для разработки с HMR — npm run dev", "url", defaultDevTarget)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			listenErr <- err
		}
	}()

	select {
	case err := <-listenErr:
		return err
	case <-ctx.Done():
		slog.Info("studio: shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// displayAddr turns ":1080" into "localhost:1080" for logging.
func displayAddr(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "localhost" + addr
	}
	return addr
}
