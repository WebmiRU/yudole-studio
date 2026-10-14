package plugin

// Живые состояния плагинов: страница настроек узнаёт о них из событий, а не
// опросом. Тесты гоняют настоящий менеджер с настоящим фейковым плагином —
// поддевать тут нечего: смысл как раз в том, что состояние меняется по
//ступенькам, и каждая должна доехать.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"studio/core/settings"
)

// newTestManager builds a manager over a temporary config and plugins dir.
func newTestManager(t *testing.T, config string, script string) *Manager {
	t.Helper()

	dir := t.TempDir()
	pluginsDir := filepath.Join(dir, "chat")
	if err := os.MkdirAll(filepath.Join(pluginsDir, "fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(pluginsDir, "fake", "fake")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewManager(configPath, pluginsDir)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

// enabledConfig switches the fake plugin on, so the manager starts it.
const enabledConfig = `{"kind":"config","version":"1.0","plugins":{"chat":{"fake":{"enabled":true}}}}`

// busyPlugin greets, takes the options, and then says it is busy whenever it is
// asked to: it stands in for a plugin that went to the network.
const busyPlugin = `#!/bin/sh
echo '{"kind":"system.plugin.hello","payload":{"title":"Fake","version":"0.1.0","options":[{"key":"nick","type":"text","label":"Nick"}]}}'
while IFS= read -r line; do
  case "$line" in
    *system.fake.work*)
      echo '{"kind":"system.message.waiting","payload":{"message":{"src":"Спрашиваю сервис","text":"Спрашиваю сервис","html":"Спрашиваю сервис"}}}'
      echo '{"kind":"system.message.ack","payload":{"message":"готово"}}'
      ;;
    *system.plugin.options.set*) echo '{"kind":"system.message.ack"}' ;;
    *system.plugin.quit*)        echo '{"kind":"system.message.ack"}'; exit 0 ;;
  esac
done
`

// waitForView reads the next state the page would receive.
func waitForView(t *testing.T, views <-chan PluginView, want string) PluginView {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case view, ok := <-views:
			if !ok {
				t.Fatalf("поток состояний закрылся, не дождавшись %q", want)
			}
			if view.ID == "fake" && matches(view, want) {
				return view
			}
		case <-deadline:
			t.Fatalf("состояние %q так и не пришло", want)
		}
	}
}

// matches says whether a view is in the state the test is waiting for.
func matches(view PluginView, want string) bool {
	switch want {
	case "running":
		return view.State == StateRunning
	case "off":
		return view.State == StateOff
	case "failed":
		return view.State == StateFailed
	case "busy":
		return view.Waiting && view.WaitingText != ""
	case "idle":
		return !view.Waiting
	default:
		return false
	}
}

// Запуск плагина — событие: страница узнаёт, что он поднялся, сразу, а не
// через две секунды опроса.
func TestWatcherSeesThePluginStart(t *testing.T) {
	captureLog(t)
	manager := newTestManager(t, enabledConfig, busyPlugin)

	views, unwatch := manager.WatchPlugins(8)
	defer unwatch()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Shutdown()

	view := waitForView(t, views, "running")
	if view.Title != "Fake" {
		t.Errorf("заголовок = %q, want Fake", view.Title)
	}
	if len(view.Options) != 1 {
		t.Errorf("форма не доехала: %+v", view.Options)
	}
}

// Занятость приходит от самого плагина и доезжает до страницы: кнопка
// «Обновить» гаснет не потому, что человек её нажал, а потому что плагин
// сказал, что занят.
func TestWatcherSeesThePluginGetBusy(t *testing.T) {
	captureLog(t)
	manager := newTestManager(t, enabledConfig, busyPlugin)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Shutdown()

	// Подписка после старта: иначе мы поймали бы только состояние из запуска.
	views, unwatch := manager.WatchPlugins(8)
	defer unwatch()

	if _, err := manager.Run(ctx, "fake", "system.fake.work"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	busy := waitForView(t, views, "busy")
	if busy.WaitingText != "Спрашиваю сервис" {
		t.Errorf("чем занят = %q, want Спрашиваю сервис", busy.WaitingText)
	}
}

// Смерть процесса — тоже событие. Плагин о ней сообщить не может, а страница
// должна узнать: иначе она до следующего клика считает, что всё в порядке.
func TestWatcherSeesThePluginDie(t *testing.T) {
	// Плагин здоровается, подтверждает настройки, живёт немного и уходит сам — как
	// упавший. Подтверждение обязательно: без него ядро честно ждёт все
	// двадцать секунд configureTimeout, и тест ничего не проверяет.
	quitAfterGreeting := `#!/bin/sh
echo '{"kind":"system.plugin.hello","payload":{"title":"Fake","version":"0.1.0","options":[]}}'
while IFS= read -r line; do
  case "$line" in
    *system.plugin.options.set*) echo '{"kind":"system.message.ack"}' ;;
    *system.plugin.quit*)        exit 0 ;;
  esac
  # Ответили на настройки — и сразу уходим, как упавший процесс.
  exit 0
done
`
	captureLog(t)
	manager := newTestManager(t, enabledConfig, quitAfterGreeting)

	views, unwatch := manager.WatchPlugins(8)
	defer unwatch()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Shutdown()

	// Сначала он поднялся, потом умер — и оба состояния доехали.
	waitForView(t, views, "running")
	waitForView(t, views, "failed")
}

// Выключение — событие тоже: страница гасит переключатель без ожидания.
func TestWatcherSeesThePluginSwitchedOff(t *testing.T) {
	captureLog(t)
	manager := newTestManager(t, enabledConfig, busyPlugin)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Shutdown()

	views, unwatch := manager.WatchPlugins(8)
	defer unwatch()

	if _, err := manager.Disable("fake"); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	view := waitForView(t, views, "off")
	if view.Enabled {
		t.Errorf("плагин всё ещё включён: %+v", view)
	}
}

// Медленная страница теряет состояние, но не тормозит ядро: следующее придёт
// целиком, и от неё ничего не зависит.
func TestSlowWatcherDoesNotBlockTheKernel(t *testing.T) {
	captureLog(t)
	manager := newTestManager(t, enabledConfig, busyPlugin)

	// Буфер на один: всё, что сверх, отбрасывается молча.
	views, unwatch := manager.WatchPlugins(1)
	defer unwatch()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Shutdown()

	// Читать никто не будет: пусть состояния копятся и теряются.
	time.Sleep(200 * time.Millisecond)

	view, ok := <-views
	if !ok {
		t.Fatal("поток закрылся")
	}
	if view.ID != "fake" {
		t.Errorf("состояние = %+v", view)
	}
}

// Страница должна получить и текущие состояния тоже, а не ждать первого
// изменения: иначе страница, открытая на готовом плагине, была бы пустой.
func TestWatcherStartsWithTheCurrentState(t *testing.T) {
	captureLog(t)
	manager := newTestManager(t, enabledConfig, busyPlugin)

	views, unwatch := manager.WatchPlugins(1)
	defer unwatch()

	// Плагин ещё не запущен: состояние должно быть «выключен», но прийти оно
	// может только из подписки — списка страница спрашивать больше не будет.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager.Start(ctx)
	defer manager.Shutdown()

	if _, ok := <-views; !ok {
		t.Fatal("поток закрылся")
	}
}

// Настройки, которые ядро само не знает, должны пережить запись.
func TestManagerKeepsUnknownConfigSections(t *testing.T) {
	captureLog(t)

	const withExtras = `{
	  "kind": "config",
	  "version": "1.0",
	  "plugins": {"chat": {"fake": {"enabled": false}}},
	  "window": {"width": 900}
	}`

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(withExtras), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := settings.Load(configPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := config.Save(configPath); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"window"`) {
		t.Errorf("чужая секция пропала при записи:\n%s", data)
	}
}
