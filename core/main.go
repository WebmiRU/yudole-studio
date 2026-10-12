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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"studio/core/plugin"
	"studio/core/settings"
)

const (
	defaultAddr      = ":1080"
	defaultDevTarget = "http://localhost:1081"

	// sourceDirName holds the unbuilt sources; distDirName is the Vite output.
	sourceDirName = "html"
	distDirName   = "dist"
)

// pluginsRelPath is the chat plugins directory, relative to the repository root.
var pluginsRelPath = filepath.Join("plugins", "chat")

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

// defaultPluginsDir points at plugins/chat in the repository root.
func defaultPluginsDir() string {
	if candidate := filepath.Join(repoRoot(), pluginsRelPath); isDir(candidate) {
		return candidate
	}
	// Standalone checkout without the workspace file: try next to the core.
	if candidate := filepath.Join(baseDir(), pluginsRelPath); isDir(candidate) {
		return candidate
	}
	return filepath.Join(repoRoot(), pluginsRelPath)
}

// repoRoot finds the directory holding go.work, so plugins and the core are
// resolved from one place.
func repoRoot() string {
	base := baseDir()

	for dir := base; ; {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Dir(base)
		}
		dir = parent
	}
}

// defaultConfigPath points at config.json next to the kernel, the way it lives
// in the repository.
func defaultConfigPath() string {
	return filepath.Join(baseDir(), settings.FileName)
}

// relPath shortens a path for logging: relative to the repository root when
// the file lives inside it.
func relPath(path string) string {
	rel, err := filepath.Rel(repoRoot(), path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return filepath.ToSlash(rel)
}

func main() {
	// Лог идёт в stdout: перенаправление ядра в файл забирает и логи, и
	// сообщения об ошибках, в отличие от stderr.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		fmt.Fprintf(os.Stdout, "studio: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", envOr("STUDIO_ADDR", defaultAddr), "address to listen on")
	configPath := flag.String("config", envOr("STUDIO_CONFIG", defaultConfigPath()), "path to the configuration file")
	pluginsDir := flag.String("plugins", envOr("STUDIO_PLUGINS", defaultPluginsDir()), "directory with chat plugins")
	webDir := flag.String("web", envOr("STUDIO_WEB", defaultWebDir()), "directory with the built front-end (npm run build -> dist)")
	shutdownTimeout := flag.Duration("shutdown-timeout", 5*time.Second, "how long to wait for in-flight requests on shutdown")
	flag.Parse()

	config, err := settings.Load(*configPath)
	if err != nil {
		return err
	}
	slog.Info("config loaded",
		"path", relPath(*configPath),
		"version", config.Version,
		"plugins", len(config.ChatPlugins()),
	)

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

	// Занимаем порт до запуска плагинов: иначе при неудачном бинде мы
	// выходим, оставив процессы плагинов сиротами.
	listener, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", srv.Addr, err)
	}

	// Плагины живут на своём контексте: его отменяем только после того, как
	// всем отправлен quit. Иначе CommandContext убил бы их раньше времени и
	// плагин не успел бы завершиться сам.
	pluginCtx, cancelPlugins := context.WithCancel(context.Background())
	defer cancelPlugins()

	plugins := startPlugins(pluginCtx, *pluginsDir, config)
	defer plugins.StopAll()

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
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

	// defers above do the same on early returns; StopAll is idempotent.
	plugins.StopAll()
	cancelPlugins()
	return nil
}

// startPlugins discovers and launches chat plugins, tells them who they are
// configured as and hands them their options. A broken plugin is logged and
// skipped: the kernel and the other plugins keep working.
func startPlugins(ctx context.Context, dir string, config *settings.Config) *plugin.Supervisor {
	supervisor := &plugin.Supervisor{}

	descriptors, err := plugin.Discover(dir)
	if err != nil {
		slog.Error("cannot read plugins", "dir", dir, "err", err)
		return supervisor
	}
	if len(descriptors) == 0 {
		slog.Info("no chat plugins found", "dir", relPath(dir))
		return supervisor
	}

	slog.Info("scanning chat plugins", "dir", relPath(dir), "found", len(descriptors))
	for _, d := range descriptors {
		slog.Info("plugin found", "plugin", d.Name, "path", relPath(d.Path))
	}

	supervisor.StartAll(ctx, descriptors)

	for _, p := range supervisor.Plugins() {
		activate(ctx, p, config)
	}

	slog.Info("chat plugins running", "plugins", strings.Join(supervisor.Names(), ", "))
	return supervisor
}

// activate asks a plugin who it is and gives it the options from the config
// file. Without a description it still runs, just without settings.
func activate(ctx context.Context, p *plugin.Plugin, config *settings.Config) {
	info, err := p.Describe(ctx)
	if err != nil {
		slog.Warn("plugin did not describe itself", "plugin", p.Name, "err", err)
		return
	}
	slog.Info("plugin info",
		"plugin", p.Name,
		"title", info.Name,
		"version", info.Version,
		"options", len(info.Options),
	)

	stored, _ := config.Chat(p.Name)
	values := plugin.ValuesFor(info, stored)

	unknown, err := plugin.CheckValues(info.Options, values)
	if err != nil {
		slog.Warn("stored options do not fit the schema", "plugin", p.Name, "err", err)
		slog.Debug("using the schema defaults instead", "plugin", p.Name)
		values = plugin.Defaults(info.Options)
	}
	if len(unknown) > 0 {
		slog.Info("options the plugin did not declare, keeping them", "plugin", p.Name, "keys", unknown)
	}

	if err := p.Configure(values); err != nil {
		slog.Warn("plugin rejected its options", "plugin", p.Name, "err", err)
	}
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
