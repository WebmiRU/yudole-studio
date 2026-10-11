# Сборка ядра и плагинов. По умолчанию — под текущую ОС.
#
#   make            ядро и все плагины для текущей ОС
#   make windows    то же для windows
#   make linux      то же для linux
#   make release    ядро и плагины под обе платформы в dist/
#   make test       тесты
#   make clean      убрать собранные бинарники

GO      ?= go
CORE_DIR := core
PLUGINS  := plugins/chat
CORE_BIN := studio
LDFLAGS := -s -w

.PHONY: all core plugins test vet fmt windows linux release clean

all: core plugins

core:
	cd $(CORE_DIR) && $(GO) build -ldflags "$(LDFLAGS)" -o $(CORE_BIN) .
	@echo "==> $(CORE_DIR)/$(CORE_BIN)$(if $(GOOS),$(if $(filter windows,$(GOOS)),.exe,))"

# Каждый плагин собирается в бинарник с именем плагина рядом с исходниками.
plugins:
	@for dir in $(PLUGINS)/*/; do \
		[ -f "$$dir/go.mod" ] || continue; \
		name=$$(basename "$$dir"); \
		out="$$name"; \
		if [ "$(GOOS)" = "windows" ]; then out="$$name.exe"; fi; \
		echo "==> $$dir$$out"; \
		(cd "$$dir" && $(GO) build -ldflags "$(LDFLAGS)" -o "$$out" .) || exit 1; \
	done

windows:
	$(MAKE) all GOOS=windows GOARCH=amd64

linux:
	$(MAKE) all GOOS=linux GOARCH=amd64

# Обе платформы в dist/<goos>-<goarch>/, чтобы можно было раздать архивом.
release: windows linux
	@for os in windows linux; do \
		mkdir -p dist/$$os-amd64/plugins/chat; \
		cp -f $(CORE_DIR)/$(CORE_BIN)$([ $$os = windows ] && echo .exe) dist/$$os-amd64/$(CORE_BIN)$([ $$os = windows ] && echo .exe); \
		for dir in $(PLUGINS)/*/; do \
			name=$$(basename "$$dir"); \
			cp -f "$$dir$$name$$([ $$os = windows ] && echo .exe)" dist/$$os-amd64/plugins/chat/$$name$$([ $$os = windows ] && echo .exe); \
		done; \
		echo "==> dist/$$os-amd64"; \
	done

test:
	cd $(CORE_DIR) && $(GO) test ./...
	@for dir in $(PLUGINS)/*/; do \
		[ -f "$$dir/go.mod" ] || continue; \
		(cd "$$dir" && $(GO) test ./...) || exit 1; \
	done

vet:
	cd $(CORE_DIR) && $(GO) vet ./...
	@for dir in $(PLUGINS)/*/; do \
		[ -f "$$dir/go.mod" ] || continue; \
		(cd "$$dir" && $(GO) vet ./...) || exit 1; \
	done

fmt:
	gofmt -l -w $(CORE_DIR) $(PLUGINS)

clean:
	rm -f $(CORE_DIR)/$(CORE_BIN) $(CORE_DIR)/$(CORE_BIN).exe
	@for dir in $(PLUGINS)/*/; do \
		[ -d "$$dir" ] || continue; \
		name=$$(basename "$$dir"); \
		rm -f "$$dir$$name" "$$dir$$name.exe"; \
	done
	rm -rf dist