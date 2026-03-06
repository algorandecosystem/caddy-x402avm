// Package testscenario_test is an end-to-end integration test for the caddy-x402avm plugin.
//
// What it tests:
//   - A dummy weather JSON API runs in-process on :19082.
//   - Caddy (built with the x402 plugin) proxies it on :19081, gating /weather/*
//     behind a $0.001 USDC paywall on Algorand testnet.
//   - A payment-aware HTTP client, loaded from the project-root .env file,
//     automatically handles the 402 challenge and submits a real Algorand transaction.
//
// Prerequisites:
//   1. Copy .env.example → .env and fill in CLIENT_MNEMONIC + PAY_TO.
//   2. CLIENT_MNEMONIC account must hold testnet USDC (ASA 10458941) and be
//      opted into that asset.
//   3. PAY_TO address must be opted into testnet USDC (ASA 10458941).
//   4. Run `make build` once to produce bin/caddy with the x402 plugin.
//
// Run:
//
//	go test ./testscenario/ -v -timeout 3m -run TestWeatherAPIPaywall
package testscenario_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	algomnemonic "github.com/algorand/go-algorand-sdk/v2/mnemonic"

	x402 "github.com/GoPlausible/x402-avm/go"
	x402http "github.com/GoPlausible/x402-avm/go/http"
	avm "github.com/GoPlausible/x402-avm/go/mechanisms/avm"
	avmclient "github.com/GoPlausible/x402-avm/go/mechanisms/avm/exact/client"
	avmsigners "github.com/GoPlausible/x402-avm/go/signers/avm"
)

// ─── Port assignments ─────────────────────────────────────────────────────────

const (
	weatherPort = 19082 // dummy weather API (in-process)
	caddyPort   = 19081 // caddy reverse-proxy with x402 paywall
	adminPort   = 19180 // caddy admin API (avoids conflict with default :2019)
)

// ─── Weather API types ────────────────────────────────────────────────────────

type weatherResponse struct {
	City      string  `json:"city"`
	TempC     float64 `json:"temp_c"`
	Condition string  `json:"condition"`
	Humidity  int     `json:"humidity"`
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// loadDotEnv reads KEY=VALUE pairs from a .env file.
// Lines starting with # and blank lines are ignored.
// Values may optionally be quoted with " or '.
func loadDotEnv(path string) map[string]string {
	env := make(map[string]string)
	f, err := os.Open(path)
	if err != nil {
		return env
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		// Strip optional surrounding quotes.
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') ||
			(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		env[key] = val
	}
	return env
}

// startWeatherServer launches a minimal in-process HTTP server that responds
// to GET /weather/current with a static JSON payload.
// The server is stopped automatically when the test ends.
func startWeatherServer(t *testing.T) {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("/weather/current", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(weatherResponse{ //nolint:errcheck
			City:      "Testville",
			TempC:     22.5,
			Condition: "Sunny",
			Humidity:  45,
		})
	})

	// /health is kept outside the paywall so Caddy can reach it for readiness checks.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok")) //nolint:errcheck
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", weatherPort),
		Handler: mux,
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
	})

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.Logf("weather server error: %v", err)
		}
	}()

	waitForPort(t, weatherPort, 5*time.Second)
}

// writeCaddyfile writes the test Caddyfile into tmpDir and returns its path.
// payTo is the Algorand address that will receive USDC micropayments.
func writeCaddyfile(t *testing.T, tmpDir, payTo string) string {
	t.Helper()

	content := fmt.Sprintf(`{
	admin localhost:%d
	auto_https off
}

:%d {
	# Paywalled weather endpoint.
	route /weather/* {
		x402 {
			description "Weather API"
			mime_type   application/json
			accept {
				pay_to  %s
				price   0.001
				network algorand-testnet
			}
		}
		reverse_proxy localhost:%d
	}

	# Health check bypasses the paywall entirely.
	route /health {
		reverse_proxy localhost:%d
	}
}
`, adminPort, caddyPort, payTo, weatherPort, weatherPort)

	path := filepath.Join(tmpDir, "Caddyfile")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write Caddyfile: %v", err)
	}
	return path
}

// findCaddyBin locates the caddy binary produced by `make build`.
// When running `go test ./testscenario/` the working directory is testscenario/,
// so the binary is one level up at ../bin/caddy.
func findCaddyBin(t *testing.T) string {
	t.Helper()
	for _, rel := range []string{"../bin/caddy", "bin/caddy"} {
		if _, err := os.Stat(rel); err == nil {
			abs, _ := filepath.Abs(rel)
			return abs
		}
	}
	t.Fatal("caddy binary not found; run `make build` from the project root first")
	return ""
}

// startCaddy starts the caddy binary with the given Caddyfile and registers
// a cleanup function that kills the process when the test ends.
func startCaddy(t *testing.T, caddyBin, caddyfilePath string) {
	t.Helper()

	cmd := exec.Command(caddyBin, "run",
		"--config", caddyfilePath,
		"--adapter", "caddyfile",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start caddy: %v", err)
	}

	t.Cleanup(func() {
		cmd.Process.Kill()   //nolint:errcheck
		cmd.Wait()           //nolint:errcheck
	})
}

// waitForPort dials addr:port repeatedly until it succeeds or timeout expires.
func waitForPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := fmt.Sprintf("localhost:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("port %d not ready after %s", port, timeout)
}

// buildX402Client creates an *http.Client that transparently handles x402 402
// responses: it reads the PAYMENT-REQUIRED header, builds an Algorand USDC
// payment transaction signed with the key derived from mnemonic, and retries
// the request with the PAYMENT-SIGNATURE header.
func buildX402Client(t *testing.T, mnemonicWords string) *http.Client {
	t.Helper()

	// Derive ed25519 private key from the 25-word Algorand mnemonic.
	sk, err := algomnemonic.ToPrivateKey(mnemonicWords)
	if err != nil {
		t.Fatalf("mnemonic → private key: %v", err)
	}

	// The SDK signer helper expects a standard base64-encoded 64-byte key.
	signer, err := avmsigners.NewClientSignerFromPrivateKey(
		base64.StdEncoding.EncodeToString(sk),
	)
	if err != nil {
		t.Fatalf("create AVM signer: %v", err)
	}
	t.Logf("client address: %s", signer.Address())

	// Build the x402 core client and register the AVM exact-payment scheme.
	avmScheme := avmclient.NewExactAvmScheme(signer, &avm.ClientConfig{
		AlgodURL: "https://testnet-api.algonode.cloud",
	})
	x402Core := x402.Newx402Client()
	x402Core.Register(x402.Network(avm.AlgorandTestnetCAIP2), avmScheme)

	// Wrap a standard http.Client so it handles 402 responses automatically.
	base := &http.Client{Timeout: 90 * time.Second}
	return x402http.WrapHTTPClientWithPayment(base, x402http.Newx402HTTPClient(x402Core))
}

// ─── Test ─────────────────────────────────────────────────────────────────────

// TestWeatherAPIPaywall is the full end-to-end scenario.
func TestWeatherAPIPaywall(t *testing.T) {
	// ── Load .env ──────────────────────────────────────────────────────────────
	env := loadDotEnv("../.env")
	clientMnemonic := env["CLIENT_MNEMONIC"]
	payTo := env["PAY_TO"]

	if clientMnemonic == "" || payTo == "" {
		t.Skip("copy .env.example → .env and set CLIENT_MNEMONIC + PAY_TO to run this test")
	}

	// ── Start dummy weather API ────────────────────────────────────────────────
	startWeatherServer(t)
	t.Logf("weather API started on :%d", weatherPort)

	// ── Start Caddy with x402 paywall ──────────────────────────────────────────
	tmpDir := t.TempDir()
	caddyfilePath := writeCaddyfile(t, tmpDir, payTo)
	caddyBin := findCaddyBin(t)
	startCaddy(t, caddyBin, caddyfilePath)
	waitForPort(t, caddyPort, 30*time.Second)
	t.Logf("caddy proxy started on :%d", caddyPort)

	target := fmt.Sprintf("http://localhost:%d/weather/current", caddyPort)

	// ── Sub-test 1: bare request returns 402 with PAYMENT-REQUIRED header ─────
	t.Run("returns_402_without_payment", func(t *testing.T) {
		resp, err := http.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPaymentRequired {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 402, got %d: %s", resp.StatusCode, body)
		}

		if resp.Header.Get("PAYMENT-REQUIRED") == "" {
			t.Error("expected PAYMENT-REQUIRED header in 402 response")
		}
		t.Log("received 402 with PAYMENT-REQUIRED header")
	})

	// ── Sub-test 2: payment-enabled client receives 200 + weather data ─────────
	t.Run("pays_and_receives_weather_data", func(t *testing.T) {
		client := buildX402Client(t, clientMnemonic)

		t.Log("submitting Algorand testnet USDC payment (may take ~5 s for block confirmation)...")
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("paid GET %s: %v", target, err)
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		// Caddy should attach the settlement receipt.
		if resp.Header.Get("PAYMENT-RESPONSE") == "" {
			t.Error("expected PAYMENT-RESPONSE header after successful payment")
		}
		t.Log("PAYMENT-RESPONSE header present")

		var weather weatherResponse
		if err := json.Unmarshal(body, &weather); err != nil {
			t.Fatalf("parse weather JSON: %v\nraw: %s", err, body)
		}
		t.Logf("weather data received: city=%s temp=%.1f°C condition=%s humidity=%d%%",
			weather.City, weather.TempC, weather.Condition, weather.Humidity)
	})

	// ── Sub-test 3: health endpoint is not paywalled ───────────────────────────
	t.Run("health_endpoint_bypasses_paywall", func(t *testing.T) {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%d/health", caddyPort))
		if err != nil {
			t.Fatalf("GET /health: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200 for /health, got %d: %s", resp.StatusCode, body)
		}
		t.Log("/health bypasses the paywall as expected")
	})
}
