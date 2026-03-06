// Package x402avm implements a Caddy HTTP middleware that enforces x402 micropayments
// for all requests that reach it, using the GoPlausible x402-avm SDK.
//
// Path scoping is handled by Caddy's own route/matcher directives; each x402
// block in the Caddyfile is self-contained with its own accepted payment
// networks. Multiple x402 blocks can appear in the same server, each attached
// to a different Caddy route.
//
// Build with xcaddy:
//
//	xcaddy build --with github.com/algorandecosystem/caddy-x402avm
package x402avm

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	x402 "github.com/GoPlausible/x402-avm/go"
	x402http "github.com/GoPlausible/x402-avm/go/http"
	avmserver "github.com/GoPlausible/x402-avm/go/mechanisms/avm/exact/server"
	evmserver "github.com/GoPlausible/x402-avm/go/mechanisms/evm/exact/server"
	svmserver "github.com/GoPlausible/x402-avm/go/mechanisms/svm/exact/server"
)

func init() {
	caddy.RegisterModule(X402{})
}

// ─── Config types ─────────────────────────────────────────────────────────────

// PaymentOption describes one accepted payment method.
// Multiple options enable multi-network support: the client picks one.
type PaymentOption struct {
	// Scheme is the x402 payment scheme. Defaults to "exact".
	Scheme string `json:"scheme,omitempty"`

	// PayTo is the recipient address in the format required by the network
	// (Algorand address, Solana public key, or 0x EVM address).
	PayTo string `json:"pay_to"`

	// Price is the USDC cost per request, e.g. "0.01" or "$0.01".
	Price string `json:"price"`

	// Network selects the blockchain network. Accepts either a short name
	// (algorand-mainnet, algorand-testnet, solana-mainnet, solana-devnet,
	// base, base-sepolia) or a raw CAIP-2 string.
	Network string `json:"network"`
}

// ─── Middleware ───────────────────────────────────────────────────────────────

// X402 is a Caddy HTTP middleware that gates every request that reaches it
// behind an x402 micropayment. Path scoping is left to Caddy's route/matcher
// system, so different x402 blocks can protect different paths with different
// payment settings.
type X402 struct {
	// Accepts lists the payment options offered to clients.
	// At least one entry is required; list multiple to support several networks
	// simultaneously (the client chooses which chain to pay on).
	Accepts []PaymentOption `json:"accepts"`

	// Description is a human-readable label for the protected resource,
	// included in the 402 response body.
	Description string `json:"description,omitempty"`

	// MimeType is the MIME type of the protected resource,
	// e.g. "application/json".
	MimeType string `json:"mime_type,omitempty"`

	// FacilitatorURL is the base URL of the x402 facilitator.
	// Defaults to https://facilitator.goplausible.xyz
	FacilitatorURL string `json:"facilitator_url,omitempty"`

	// DryRun disables on-chain settlement; payments are verified but not
	// settled. Useful for development and testing.
	DryRun bool `json:"dry_run,omitempty"`

	// Except is a list of regular expressions matched against the request
	// path. If any pattern matches, the request bypasses x402 processing
	// entirely and is passed to the next handler without requiring payment.
	// Patterns use regexp.MatchString semantics (not full-line anchored
	// unless you add ^ / $).
	Except []string `json:"except,omitempty"`

	// UAMatch is a list of regular expressions matched against the
	// User-Agent header. When at least one pattern is configured, only
	// requests whose User-Agent matches at least one pattern are subject
	// to x402 processing; all other User-Agents are passed through freely.
	// If UAMatch is empty every request is subject to x402 (default).
	UAMatch []string `json:"ua_match,omitempty"`

	// ─── internal ──────────────────────────────────────────────────────
	httpServer      *x402http.HTTPServer
	exceptCompiled  []*regexp.Regexp
	uaMatchCompiled *regexp.Regexp
	logger          *zap.Logger
}

// ─── Caddy interface guards ───────────────────────────────────────────────────

var (
	_ caddy.Module                = (*X402)(nil)
	_ caddy.Provisioner           = (*X402)(nil)
	_ caddy.Validator             = (*X402)(nil)
	_ caddyhttp.MiddlewareHandler = (*X402)(nil)
)

// CaddyModule returns module metadata.
func (X402) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.x402",
		New: func() caddy.Module { return new(X402) },
	}
}

// Provision initialises the module after configuration is loaded.
func (x *X402) Provision(ctx caddy.Context) error {
	x.logger = ctx.Logger(x)

	if x.FacilitatorURL == "" {
		x.FacilitatorURL = defaultFacilitatorURL
	}

	// ── Compile except patterns ────────────────────────────────────────
	x.exceptCompiled = make([]*regexp.Regexp, 0, len(x.Except))
	for _, pattern := range x.Except {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("x402: invalid except pattern %q: %w", pattern, err)
		}
		x.exceptCompiled = append(x.exceptCompiled, re)
	}

	// ── Compile ua_match patterns into a single case-insensitive regexp ─
	if len(x.UAMatch) > 0 {
		combined := "(?i)(?:" + strings.Join(x.UAMatch, "|") + ")"
		re, err := regexp.Compile(combined)
		if err != nil {
			return fmt.Errorf("x402: invalid ua_match pattern(s): %w", err)
		}
		x.uaMatchCompiled = re
	}

	// ── Convert accepts to SDK PaymentOptions ──────────────────────────
	sdkOpts := make(x402http.PaymentOptions, 0, len(x.Accepts))
	registeredNetworks := map[x402.Network]bool{}
	var schemeOpts []x402.ResourceServerOption

	for _, a := range x.Accepts {
		network, family, err := resolveNetwork(a.Network)
		if err != nil {
			return err
		}

		scheme := a.Scheme
		if scheme == "" {
			scheme = "exact"
		}

		sdkOpts = append(sdkOpts, x402http.PaymentOption{
			Scheme:  scheme,
			PayTo:   a.PayTo,
			Price:   a.Price,
			Network: network,
		})

		// Register one scheme server per unique network.
		if !registeredNetworks[network] {
			registeredNetworks[network] = true
			ss, err := newSchemeServer(family)
			if err != nil {
				return fmt.Errorf("network %q: %w", a.Network, err)
			}
			schemeOpts = append(schemeOpts, x402.WithSchemeServer(network, ss))
		}
	}

	// ── Build SDK RoutesConfig ─────────────────────────────────────────
	// Use a single catch-all route: Caddy's own routing already narrowed
	// which requests reach this middleware instance.
	routes := x402http.RoutesConfig{
		"/*": {
			Accepts:     sdkOpts,
			Description: x.Description,
			MimeType:    x.MimeType,
		},
	}

	// ── Assemble and initialize the SDK HTTP server ────────────────────
	facilitatorClient := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{
		URL: x.FacilitatorURL,
	})
	allOpts := append(schemeOpts, x402.WithFacilitatorClient(facilitatorClient))
	x.httpServer = x402http.Newx402HTTPResourceServer(routes, allOpts...)

	initCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := x.httpServer.Initialize(initCtx); err != nil {
		x.logger.Warn("x402 server initialization warning (will retry at request time)",
			zap.Error(err))
	}

	x.logger.Info("x402 middleware provisioned",
		zap.Int("networks", len(x.Accepts)),
		zap.String("facilitator", x.FacilitatorURL),
		zap.Bool("dry_run", x.DryRun),
	)
	return nil
}

// Validate checks that all required configuration values are present and valid.
func (x *X402) Validate() error {
	if len(x.Accepts) == 0 {
		return fmt.Errorf("x402: at least one accept block is required")
	}
	for i, a := range x.Accepts {
		if a.PayTo == "" {
			return fmt.Errorf("x402: accepts[%d]: pay_to is required", i)
		}
		if a.Price == "" {
			return fmt.Errorf("x402: accepts[%d]: price is required", i)
		}
		if a.Network == "" {
			return fmt.Errorf("x402: accepts[%d]: network is required", i)
		}
		if _, _, err := resolveNetwork(a.Network); err != nil {
			return fmt.Errorf("x402: accepts[%d]: %w", i, err)
		}
	}

	u, err := url.Parse(x.FacilitatorURL)
	if err != nil {
		return fmt.Errorf("x402: invalid facilitator_url: %w", err)
	}
	if u.Scheme != "https" && !isLocalhost(u.Host) {
		return fmt.Errorf("x402: facilitator_url must use HTTPS (got %q)", x.FacilitatorURL)
	}

	return nil
}

// ─── Scheme server factory ────────────────────────────────────────────────────

func newSchemeServer(family string) (x402.SchemeNetworkServer, error) {
	switch family {
	case "avm":
		return avmserver.NewExactAvmScheme(), nil
	case "evm":
		return evmserver.NewExactEvmScheme(), nil
	case "svm":
		return svmserver.NewExactSvmScheme(), nil
	default:
		return nil, fmt.Errorf("unsupported chain family %q", family)
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

const defaultFacilitatorURL = "https://facilitator.goplausible.xyz"

func isLocalhost(host string) bool {
	h := host
	if idx := strings.LastIndex(h, ":"); idx != -1 {
		h = h[:idx]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}
