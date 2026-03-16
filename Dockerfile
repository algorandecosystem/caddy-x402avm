FROM caddy:builder AS builder

WORKDIR /src
COPY . .

# Install xcaddy, then build caddy with the plugin pointed at local source
RUN go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest && \
    xcaddy build \
        --output /usr/bin/caddy \
        --with github.com/algorandecosystem/caddy-x402avm=/src

FROM caddy:latest

COPY --from=builder /usr/bin/caddy /usr/bin/caddy

COPY Caddyfile /etc/caddy/Caddyfile
   CMD ["caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"]