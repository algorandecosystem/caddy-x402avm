package x402avm

import (
	"bytes"
	"fmt"
	"net/http"
	"sync"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	x402http "github.com/GoPlausible/x402-avm/go/http"
)

// ServeHTTP is the core middleware handler.
//
// Request flow:
//  1. Delegate to the SDK's ProcessHTTPRequest to match routes and verify payment.
//  2. ResultNoPaymentRequired → pass through to next handler.
//  3. ResultPaymentError     → write 402 response with payment requirements.
//  4. ResultPaymentVerified  → buffer the downstream response, settle on-chain,
//     attach the PAYMENT-RESPONSE header, then flush to the client.
func (x *X402) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	// If ua_match patterns are configured, only proceed for matching UAs.
	if x.uaMatchCompiled != nil && !x.uaMatchCompiled.MatchString(r.UserAgent()) {
		return next.ServeHTTP(w, r)
	}

	// Check except path patterns.
	for _, re := range x.exceptCompiled {
		if re.MatchString(r.URL.Path) {
			return next.ServeHTTP(w, r)
		}
	}

	ctx := r.Context()

	adapter := &stdHTTPAdapter{r: r}
	reqCtx := x402http.HTTPRequestContext{
		Adapter: adapter,
		Path:    r.URL.Path,
		Method:  r.Method,
	}

	result := x.httpServer.ProcessHTTPRequest(ctx, reqCtx, nil)

	switch result.Type {

	case x402http.ResultNoPaymentRequired:
		return next.ServeHTTP(w, r)

	case x402http.ResultPaymentError:
		writeSDKResponse(w, result.Response)
		return nil

	case x402http.ResultPaymentVerified:
		// Buffer the downstream handler's output so we can settle before
		// committing bytes to the client.  If settlement fails we can still
		// return a clean error instead of a partial response.
		rc := newResponseCapture(w)
		if err := next.ServeHTTP(rc, r); err != nil {
			return err
		}

		// Don't settle if the handler itself reported an error.
		if rc.statusCode >= 400 {
			rc.flush(w, nil)
			return nil
		}

		if x.DryRun {
			x.logger.Info("dry_run: skipping settlement",
				zap.String("path", r.URL.Path),
				zap.String("pay_to", result.PaymentPayload.Accepted.PayTo),
			)
			rc.flush(w, nil)
			return nil
		}

		settle := x.httpServer.ProcessSettlement(ctx, *result.PaymentPayload, *result.PaymentRequirements)
		if !settle.Success {
			x.logger.Error("settlement failed",
				zap.String("reason", settle.ErrorReason),
				zap.String("path", r.URL.Path),
			)
			http.Error(w, fmt.Sprintf("payment settlement failed: %s", settle.ErrorReason),
				http.StatusInternalServerError)
			return nil
		}

		x.logger.Info("payment settled",
			zap.String("tx", settle.Transaction),
			zap.String("network", string(settle.Network)),
			zap.String("payer", settle.Payer),
			zap.String("path", r.URL.Path),
		)

		rc.flush(w, settle.Headers)
		return nil
	}

	return nil
}

// ─── net/http → x402http.HTTPAdapter ─────────────────────────────────────────

type stdHTTPAdapter struct{ r *http.Request }

func (a *stdHTTPAdapter) GetHeader(name string) string { return a.r.Header.Get(name) }
func (a *stdHTTPAdapter) GetMethod() string            { return a.r.Method }
func (a *stdHTTPAdapter) GetPath() string              { return a.r.URL.Path }
func (a *stdHTTPAdapter) GetAcceptHeader() string      { return a.r.Header.Get("Accept") }
func (a *stdHTTPAdapter) GetUserAgent() string         { return a.r.Header.Get("User-Agent") }

func (a *stdHTTPAdapter) GetURL() string {
	scheme := "https"
	if a.r.TLS == nil {
		if proto := a.r.Header.Get("X-Forwarded-Proto"); proto != "" {
			scheme = proto
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + a.r.Host + a.r.RequestURI
}

// ─── Response buffering ───────────────────────────────────────────────────────

type responseCapture struct {
	wrapped    http.ResponseWriter
	buf        bytes.Buffer
	mu         sync.Mutex
	statusCode int
	headerSent bool
}

func newResponseCapture(w http.ResponseWriter) *responseCapture {
	return &responseCapture{wrapped: w, statusCode: http.StatusOK}
}

// Header forwards to the wrapped writer so handlers can set response headers
// normally; they will be emitted when flush() is called.
func (rc *responseCapture) Header() http.Header {
	return rc.wrapped.Header()
}

func (rc *responseCapture) WriteHeader(code int) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if !rc.headerSent {
		rc.statusCode = code
		rc.headerSent = true
	}
}

func (rc *responseCapture) Write(b []byte) (int, error) {
	rc.mu.Lock()
	if !rc.headerSent {
		rc.statusCode = http.StatusOK
		rc.headerSent = true
	}
	rc.mu.Unlock()
	return rc.buf.Write(b)
}

// flush writes any extra headers then the captured status + body to w.
func (rc *responseCapture) flush(w http.ResponseWriter, extraHeaders map[string]string) {
	for k, v := range extraHeaders {
		w.Header().Set(k, v)
	}
	w.WriteHeader(rc.statusCode)
	w.Write(rc.buf.Bytes()) //nolint:errcheck
}

// ─── SDK response writer ──────────────────────────────────────────────────────

// writeSDKResponse translates an SDK HTTPResponseInstructions into a real HTTP response.
func writeSDKResponse(w http.ResponseWriter, resp *x402http.HTTPResponseInstructions) {
	if resp == nil {
		http.Error(w, "payment required", http.StatusPaymentRequired)
		return
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if resp.IsHTML {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(resp.Status)
		if s, ok := resp.Body.(string); ok {
			w.Write([]byte(s)) //nolint:errcheck
		}
		return
	}
	if resp.Body == nil {
		w.WriteHeader(resp.Status)
		return
	}
	// Content-Type already set by SDK in resp.Headers; just write the status.
	w.WriteHeader(resp.Status)
}
