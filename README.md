# caddy-x402avm

A [Caddy](https://caddyserver.com) HTTP middleware plugin that gates requests behind [x402](https://github.com/GoPlausible/x402-avm) micropayments, powered by the [GoPlausible facilitator](https://facilitator.goplausible.xyz).

Built on the **GoPlausible x402-avm Go SDK**. Each `x402` block is self-contained and attaches to whichever Caddy route wraps it. Multiple `x402` blocks can appear in one Caddyfile — each protecting a different path with different payment settings and networks.

## Supported networks

| Short name | Chain | Network |
|---|---|---|
| `algorand-mainnet` | AVM | Algorand Mainnet (USDC ASA 31566704) |
| `algorand-testnet` | AVM | Algorand Testnet (USDC ASA 10458941) |
| `solana-mainnet` | SVM | Solana Mainnet |
| `solana-devnet` | SVM | Solana Devnet |
| `base` | EVM | Base Mainnet |
| `base-sepolia` | EVM | Base Sepolia |

Raw CAIP-2 strings are also accepted (e.g. `eip155:8453`).

## Build

```sh
xcaddy build --with github.com/goplausible/caddy-x402avm
```

## Caddyfile syntax

```caddyfile
x402 {
    accept {
        pay_to   <address>       # Required: recipient address (network-specific format)
        price    <amount>        # Required: e.g. 0.01 or $0.01
        network  <network>       # Required: short name or CAIP-2
        scheme   <scheme>        # Optional: default "exact"
    }
    accept { ... }              # Repeat for each additional accepted network

    description  <text>         # Optional: shown in 402 body
    mime_type    <mime>         # Optional: e.g. application/json
    facilitator_url <url>       # Default: https://facilitator.goplausible.xyz
    dry_run      [true|false]   # Skip on-chain settlement (default: false)

    except  <regexp>            # Skip paywall for paths matching regexp; repeat for multiple
    ua_match <regexp>           # Only apply paywall when User-Agent matches regexp; repeat for multiple
}
```

Path scoping is handled by Caddy's `route` / `@matcher` system, not by `x402` itself.

## Examples

### Parametrized snippet — all networks, price per route

Define a reusable snippet that accepts the price as a parameter, then import it for each route with a different price.

```caddyfile
# Snippet: (x402_pay <price> <description>)
# {args[0]} = price   e.g. 0.01
# {args[1]} = description shown in 402 body
(x402_pay) {
    x402 {
        accept {
            pay_to   YOURALGOADDRHERE
            price    {args[0]}
            network  algorand-mainnet
        }
        accept {
            pay_to   YourSolanaPublicKeyBase58
            price    {args[0]}
            network  solana-mainnet
        }
        accept {
            pay_to   0xYourEVMAddress
            price    {args[0]}
            network  base
        }
        description {args[1]}
        mime_type   application/json
    }
}

api.example.com {

    # /api  — $0.01 per request
    route /api {
        import x402_pay 0.01 "API access – $0.01"
        reverse_proxy restapisrv:8080
    }

    # /graphql  — $0.10 per request
    route /graphql {
        import x402_pay 0.10 "GraphQL access – $0.10"
        reverse_proxy graphqlsrv:8080
    }

    # Everything else is public
    reverse_proxy localhost:8080
}
```

### Two paths, different networks each

```caddyfile
example.com {

    # Premium API — clients can pay on Algorand or Base
    @premium path /api/premium*
    route @premium {
        x402 {
            accept {
                pay_to  YOURALGOADDRHERE
                price   0.01
                network algorand-mainnet
            }
            accept {
                pay_to  0xYourEVMAddress
                price   0.01
                network base
            }
            description "Premium API – pay on Algorand or Base"
            mime_type   application/json
        }
        reverse_proxy localhost:8080
    }

    # Paid content — Solana only
    @content path /content/*
    route @content {
        x402 {
            accept {
                pay_to  YourSolanaPublicKeyBase58
                price   0.001
                network solana-mainnet
            }
            description "Exclusive content"
        }
        file_server
    }

    # Everything else is public
    reverse_proxy localhost:8080
}
```

### Single route, one network

```caddyfile
api.example.com {
    route {
        x402 {
            accept {
                pay_to  YOURALGOADDRHERE
                price   0.05
                network algorand-mainnet
            }
        }
        reverse_proxy localhost:9000
    }
}
```

### Paywalling /docs for AI crawlers, with exceptions

Gate the `/docs` path only for known AI crawlers (`ua_match`), while letting humans through freely. A few sub-paths are always exempted (`except`).

```caddyfile
example.com {

    route /docs {
        x402 {
            accept {
                pay_to  YOURALGOADDRHERE
                price   0.001
                network algorand-mainnet
            }
            description "Documentation access for AI agents"

            # Never paywall these sub-paths even for bots
            except  ^/docs/robots\.txt$
            except  ^/docs/sitemap\.xml$
            except  ^openapi\.json$

            # Only apply paywall when the User-Agent matches a known AI crawler
            ua_match  GPTBot|ChatGPT-User|OAI-SearchBot|Google-Extended|Googlebot-Extended
            ua_match  anthropic-ai|Claude-Web|ClaudeBot|FacebookBot|Meta-ExternalAgent|meta-externalagent
            ua_match  Applebot-Extended|CCBot|PerplexityBot|Amazonbot|cohere-ai|Ai2Bot|Bytespider
            ua_match  SemrushBot|AhrefsBot|DataForSeoBot|PetalBot|YouBot|Diffbot|Timpibot
            ua_match  ImagesiftBot|Kangaroo.Bot|Sidetrade.indexer.bot|Webz\.io|img2dataset            
        }
        file_server
    }

    # Everything else is public
    reverse_proxy localhost:8080
}
```

Human visitors hit `/docs` normally; crawler User-Agents receive a 402 and must pay. The three `except` patterns bypass the paywall unconditionally so crawlers can still discover the sitemap and API schema.

### Testnet / dry run

```caddyfile
dev.example.com {
    route /paid/* {
        x402 {
            accept {
                pay_to   0xYourEVMAddress
                price    0.01
                network  base-sepolia
            }
            facilitator_url https://facilitator.goplausible.xyz
            dry_run         true
        }
        reverse_proxy localhost:9000
    }
}
```

## JSON config equivalent

```json
{
  "handler": "x402",
  "accepts": [
    {
      "pay_to": "YOURALGOADDRHERE",
      "price": "0.01",
      "network": "algorand-mainnet"
    },
    {
      "pay_to": "0xYourEVMAddress",
      "price": "0.01",
      "network": "base"
    }
  ],
  "description": "Premium API access",
  "facilitator_url": "https://facilitator.goplausible.xyz",
  "dry_run": false
}
```

## Protocol flow

```
Client                        Caddy (x402avm)              GoPlausible Facilitator       Backend
  │                                 │                                │                      │
  │── GET /api/premium ────────────>│                                │                      │
  │<─ 402 + PAYMENT-REQUIRED ───────│                                │                      │
  │   (accepts: algorand OR base)   │                                │                      │
  │                                 │                                │                      │
  │   [Client picks network,        │                                │                      │
  │    signs payment]               │                                │                      │
  │                                 │                                │                      │
  │── GET /api/premium ────────────>│                                │                      │
  │   PAYMENT-SIGNATURE: <b64>      │── POST /verify ───────────────>│                      │
  │                                 │<─ {isValid: true} ─────────────│                      │
  │                                 │                                │                      │
  │                                 │── GET /api/premium ────────────────────────────────>  │
  │                                 │<─ 200 + response body ──────────────────────────────  │
  │                                 │                                │                      │
  │                                 │── POST /settle ───────────────>│                      │
  │                                 │<─ {success:true, tx:...} ──────│                      │
  │<─ 200 + PAYMENT-RESPONSE ───────│                                │                      │
```

## License

Apache 2.0
