PLUGIN_MODULE := github.com/algorandecosystem/caddy-x402avm
PLUGIN_SRC    := $(CURDIR)
BIN_DIR       := $(CURDIR)/bin

.PHONY: build clean xcaddy

xcaddy:
	go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest

build: xcaddy
	mkdir -p $(BIN_DIR)
	xcaddy build --output $(BIN_DIR)/caddy \
		--with $(PLUGIN_MODULE)=$(PLUGIN_SRC)

clean:
	rm -f $(BIN_DIR)/caddy
