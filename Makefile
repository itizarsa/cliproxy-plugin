PLUGIN_DIR := $(HOME)/.config/cliproxyapi/plugins
# The SDK must match the running hub: the plugin ABI and schema are versioned.
HUB := $(shell cliproxyapi -h 2>&1 | sed -n 's/^CLIProxyAPI Version: \([^,]*\),.*/\1/p')

.PHONY: test build install sdk

test:
	go test ./...

build: test
	go build -buildmode=c-shared -o build/cliproxy-plugin.dylib .

install: build
	mkdir -p $(PLUGIN_DIR)
	cp build/cliproxy-plugin.dylib $(PLUGIN_DIR)/
	brew services restart cliproxyapi

# After `brew upgrade cliproxyapi`: move the SDK to the hub's version, then `make install`.
sdk:
	go get github.com/router-for-me/CLIProxyAPI/v8@v$(HUB)
	go mod tidy
