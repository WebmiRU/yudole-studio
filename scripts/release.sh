#!/usr/bin/env bash
#
# Сборка релизных архивов: studio-{версия}-{платформа}.zip
#
# В архиве ядро со собранным интерфейсом, все чат-плагины под его платформу и
# настройки по умолчанию. Разархивировал — запустил — работает.
#
#   ./scripts/release.sh              собрать под linux и windows
#   ./scripts/release.sh linux        только под linux
#   VERSION=0.3.0 ./scripts/release.sh   с указанием версии
#   SKIP_TESTS=1 ./scripts/release.sh    без прогона тестов
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_DIR="$ROOT/release"
STAGE="$RELEASE_DIR/.stage"
OUT="$RELEASE_DIR/out"

CORE_DIR="$ROOT/core"
PLUGINS_DIR="$ROOT/plugins/chat"

# Версия: из окружения, иначе «альфа» с датой и коротким хешем. Имя тега для
# этого не годится: теги в проекте называются по площадкам, а версия должна быть
# короткой и без посторонних символов — она попадает в имя файла архива.
if [ -n "${VERSION:-}" ]; then
    VERSION="$VERSION"
else
    VERSION="alpha-$(date +%Y%m%d)-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo nogit)"
fi
VERSION="$(printf '%s' "$VERSION" | sed 's/[^A-Za-z0-9._-]/-/g')"
DATE="$(date +%Y-%m-%d)"

# Имя раздаваемого архива: префикс, дата, время, платформа. Час и минута — чтобы
# две сборки в один день не назывались одинаково и тестировщику было видно,
# какой архив свежее.
STAMP="$(date +%Y-%m-%d_%H-%M)"
ARCHIVE_PREFIX="yudole_studio_light"

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31mОШИБКА:\033[0m %s\n' "$*" >&2; exit 1; }

# --- Проверки --------------------------------------------------------------

command -v go >/dev/null || die "не найден go"
command -v npm >/dev/null || die "не найден npm: без него не собрать интерфейс"

case "${1:-}" in
    "")      TARGETS=(linux windows) ;;
    linux)   TARGETS=(linux) ;;
    windows) TARGETS=(windows) ;;
    *)       die "неизвестная платформа: $1 (доступно: linux, windows)" ;;
esac

PLUGINS=()
for dir in "$PLUGINS_DIR"/*/; do
    [ -f "$dir/go.mod" ] || continue
    PLUGINS+=("$(basename "$dir")")
done
[ "${#PLUGINS[@]}" -gt 0 ] || die "не найдено ни одного плагина в $PLUGINS_DIR"
say "плагинов: ${PLUGINS[*]}"

# --- Тесты -----------------------------------------------------------------

if [ -z "${SKIP_TESTS:-}" ]; then
    say "тесты"
    (cd "$CORE_DIR" && go test ./... >/dev/null) || die "тесты ядра не прошли"
    for plugin in "${PLUGINS[@]}"; do
        (cd "$PLUGINS_DIR/$plugin" && go test ./... >/dev/null) || die "тесты $plugin не прошли"
    done
fi

# --- Интерфейс -------------------------------------------------------------
#
# Собирается один раз: интерфейс от платформы не зависит и уезжает в оба архива.

say "интерфейс"
if [ ! -d "$CORE_DIR/node_modules" ]; then
    (cd "$CORE_DIR" && npm install --no-audit --no-fund) || die "не поставились зависимости"
fi
(cd "$CORE_DIR" && npm run build) || die "не собрался интерфейс"
[ -d "$CORE_DIR/dist" ] || die "после сборки нет каталога dist"

# --- Подготовка ------------------------------------------------------------

rm -rf "$STAGE" "$OUT"
mkdir -p "$OUT"

build_core() {
    local goos="$1" binary="$2" dir="$3"
    mkdir -p "$dir"
    (cd "$CORE_DIR" && CGO_ENABLED=0 GOOS="$goos" GOARCH=amd64 \
        go build -trimpath -ldflags "-s -w" -o "$dir/$binary" .)
}

build_plugin() {
    local goos="$1" name="$2" dir="$3"
    local out="$name"
    [ "$goos" = "windows" ] && out="$name.exe"
    mkdir -p "$dir/plugins/chat/$name"
    (cd "$PLUGINS_DIR/$name" && CGO_ENABLED=0 GOOS="$goos" GOARCH=amd64 \
        go build -trimpath -ldflags "-s -w" -o "$dir/plugins/chat/$name/$out" .)
    chmod +x "$dir/plugins/chat/$name/$out" 2>/dev/null || true
}

stage_files() {
    local dir="$1"
    cp -r "$CORE_DIR/dist" "$dir/dist"
    cp "$RELEASE_DIR/config.default.json" "$dir/config.json"
    sed -e "s/{{VERSION}}/$VERSION/g" -e "s/{{DATE}}/$DATE/g" \
        "$RELEASE_DIR/README.txt" > "$dir/README.txt"
}

# --- Сборка ----------------------------------------------------------------

for target in "${TARGETS[@]}"; do
    case "$target" in
        linux)
            bundle="linux64"
            stage="$STAGE/${ARCHIVE_PREFIX}_$bundle"
            say "сборка linux"
            rm -rf "$stage"; mkdir -p "$stage"
            build_core linux studio "$stage"
            for plugin in "${PLUGINS[@]}"; do build_plugin linux "$plugin" "$stage"; done
            stage_files "$stage"
            archive="$OUT/${ARCHIVE_PREFIX}_${STAMP}_$bundle.zip"
            ;;
        windows)
            bundle="win64"
            stage="$STAGE/${ARCHIVE_PREFIX}_$bundle"
            say "сборка windows"
            rm -rf "$stage"; mkdir -p "$stage"
            build_core windows studio.exe "$stage"
            for plugin in "${PLUGINS[@]}"; do build_plugin windows "$plugin" "$stage"; done
            stage_files "$stage"
            archive="$OUT/${ARCHIVE_PREFIX}_${STAMP}_$bundle.zip"
            ;;
    esac

    command -v zip >/dev/null || die "не найден zip: поставь zip либо используй другую архиваторку"
    rm -f "$archive"
    # -9 — максимальное сжатие zip, больше некуда: это потолок deflate.
    # -X убирает из архива лишние метаданные (uid, gid, лишние временные метки):
    # немного меньше и не тянем в архив мусор, который тестировщику не нужен.
    #
    # Путь к архиву абсолютный: иначе zip создал бы его внутри staging, а тот
    # удаляется в конце сборки, и скрипт рапортовал бы об успехе с пустым
    # списком архивов.
    (cd "$STAGE" && zip -q -r -9 -X "$archive" "$(basename "$stage")") \
        || die "не собрался архив $(basename "$archive")"
    [ -f "$archive" ] || die "архив не появился: $(basename "$archive")"
    say "$(basename "$archive"): $(du -h "$archive" | cut -f1)"
done

rm -rf "$STAGE"

say "готово:"
ls -1 "$OUT" | sed 's/^/  /'