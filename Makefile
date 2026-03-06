PLUGIN_MODULE := github.com/algorandecosystem/caddy-x402avm
PLUGIN_SRC    := $(CURDIR)
BIN_DIR       := $(CURDIR)/bin

.PHONY: build clean xcaddy test

xcaddy:
	go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

build: xcaddy
	mkdir -p $(BIN_DIR)
	xcaddy build --output $(BIN_DIR)/caddy \
		--with $(PLUGIN_MODULE)=$(PLUGIN_SRC)

test: build
	go test ./testscenario/ -v -timeout 3m -run TestWeatherAPIPaywall

clean:
	rm -f $(BIN_DIR)/caddy
