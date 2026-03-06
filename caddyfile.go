package x402avm

import (
	"strconv"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func init() {
	httpcaddyfile.RegisterHandlerDirective("x402", parseCaddyfile)
}

func parseCaddyfile(h httpcaddyfile.Helper) (caddyhttp.MiddlewareHandler, error) {
	x := &X402{FacilitatorURL: defaultFacilitatorURL}
	if err := x.UnmarshalCaddyfile(h.Dispenser); err != nil {
		return nil, err
	}
	return x, nil
}

// UnmarshalCaddyfile parses the x402 directive block.
//
// Syntax:
//
//	x402 {
//	    accept {
//	        pay_to   <address>
//	        price    <amount>       # e.g. 0.01 or $0.01
//	        network  <network>      # e.g. algorand-mainnet, base, solana-mainnet
//	        scheme   <scheme>       # optional, default: exact
//	    }
//	    accept { ... }             # repeat for each additional accepted network
//
//	    description  <text>        # optional
//	    mime_type    <mime>        # optional, e.g. application/json
//	    facilitator_url <url>      # default: https://facilitator.goplausible.xyz
//	    dry_run      [true|false]  # default: false
//
//	    # except: skip paywall for paths matching a regexp
//	    except  ^/robots\.txt$
//	    except  ^/\.well-known/
//	    except  ^/favicon\.ico$
//	    except  ^/health$
//
//	    # ua_match: only apply paywall when User-Agent matches (regexp)
//	    ua_match  GPTBot|ChatGPT-User|OAI-SearchBot|Google-Extended|Googlebot-Extended
//	    ua_match  anthropic-ai|Claude-Web|ClaudeBot|FacebookBot|Meta-ExternalAgent|meta-externalagent
//	    ua_match  Applebot-Extended|CCBot|PerplexityBot|Amazonbot|cohere-ai|Ai2Bot|Bytespider
//	    ua_match  SemrushBot|AhrefsBot|DataForSeoBot|PetalBot|YouBot|Diffbot|Timpibot
//	    ua_match  ImagesiftBot|Kangaroo.Bot|Sidetrade.indexer.bot|Webz\.io|img2dataset
//	}
//
// Multiple x402 blocks can appear in one Caddyfile, each attached to a
// different Caddy route; path scoping is handled by Caddy's route/matcher
// system, not by this directive.
func (x *X402) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume directive name "x402"

	for d.NextBlock(0) {
		switch d.Val() {

		case "accept":
			opt := PaymentOption{Scheme: "exact"}
			for d.NextBlock(1) {
				switch d.Val() {
				case "pay_to":
					if !d.NextArg() {
						return d.ArgErr()
					}
					opt.PayTo = d.Val()
				case "price":
					if !d.NextArg() {
						return d.ArgErr()
					}
					opt.Price = d.Val()
				case "network":
					if !d.NextArg() {
						return d.ArgErr()
					}
					opt.Network = d.Val()
				case "scheme":
					if !d.NextArg() {
						return d.ArgErr()
					}
					opt.Scheme = d.Val()
				default:
					return d.Errf("unknown accept option: %q", d.Val())
				}
			}
			x.Accepts = append(x.Accepts, opt)

		case "description":
			if !d.NextArg() {
				return d.ArgErr()
			}
			x.Description = d.Val()

		case "mime_type":
			if !d.NextArg() {
				return d.ArgErr()
			}
			x.MimeType = d.Val()

		case "facilitator_url":
			if !d.NextArg() {
				return d.ArgErr()
			}
			x.FacilitatorURL = d.Val()

		case "except":
			if !d.NextArg() {
				return d.ArgErr()
			}
			x.Except = append(x.Except, d.Val())

		case "ua_match":
			if !d.NextArg() {
				return d.ArgErr()
			}
			x.UAMatch = append(x.UAMatch, d.Val())

		case "dry_run":
			if !d.NextArg() {
				x.DryRun = true
				continue
			}
			b, err := strconv.ParseBool(d.Val())
			if err != nil {
				return d.Errf("dry_run must be a boolean, got %q", d.Val())
			}
			x.DryRun = b

		default:
			return d.Errf("unknown x402 option: %q", d.Val())
		}
	}
	return nil
}
