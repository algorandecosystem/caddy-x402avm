package x402avm

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"

	x402http "github.com/GoPlausible/x402-avm/go/http"
)

type facilitatorVerifyResponse struct {
	IsValid        bool   `json:"isValid"`
	InvalidReason  string `json:"invalidReason,omitempty"`
	InvalidMessage string `json:"invalidMessage,omitempty"`
	Payer          string `json:"payer,omitempty"`
}

type facilitatorSettleResponse struct {
	Success      bool   `json:"success"`
	ErrorReason  string `json:"errorReason,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
	Payer        string `json:"payer,omitempty"`
	Transaction  string `json:"transaction,omitempty"`
	Network      string `json:"network,omitempty"`
}

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

	if x.QuoteURL != "" && r.Header.Get("PAYMENT-SIGNATURE") == "" {
		if handled, err := x.maybeWriteDynamicQuoteResponse(w, r); err != nil {
			return err
		} else if handled {
			return nil
		}
	}
	if x.isSettlementGatedPath(r.URL.Path) && r.Header.Get("PAYMENT-SIGNATURE") != "" {
		return x.handleSettlementGatedRequest(w, r, next)
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
		if x.isSettlementGatedPath(r.URL.Path) {
			if x.DryRun {
				http.Error(w, "dry_run is not allowed for settlement-gated routes", http.StatusServiceUnavailable)
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

			quote, err := x.parseQuoteFromPaymentSignature(r.Header.Get("PAYMENT-SIGNATURE"))
			if err != nil {
				x.logger.Error("missing quote-bound payload on verified paid route", zap.Error(err))
				http.Error(w, "missing quote-bound payment payload", http.StatusBadRequest)
				return nil
			}

			proof := settlementProof{
				Version:           "aq-settlement-proof-v1",
				QuoteID:           quoteStringField(quote, "quoteId"),
				ActionFingerprint: quoteStringField(quote, "actionFingerprint"),
				Network:           quoteStringField(quote, "network"),
				AssetID:           quoteStringField(quote, "assetId"),
				Payer:             settle.Payer,
				Transaction:       settle.Transaction,
				Split:             quoteSplitField(quote),
				Nonce:             quoteStringField(quote, "nonce"),
				ExpiresAt:         quoteStringField(quote, "expiresAt"),
				SettledAt:         time.Now().UTC().Format(time.RFC3339),
			}
			proof.Signature = x.signSettlementProof(proof)
			rawProof, err := json.Marshal(proof)
			if err != nil {
				return fmt.Errorf("marshal settlement proof: %w", err)
			}

			for k, v := range settle.Headers {
				w.Header().Set(k, v)
			}
			r.Header.Set("X-AQ-Settlement-Proof", base64.StdEncoding.EncodeToString(rawProof))
			return next.ServeHTTP(w, r)
		}

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

func (x *X402) handleSettlementGatedRequest(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	signature := r.Header.Get("PAYMENT-SIGNATURE")
	quote, err := x.parseQuoteFromPaymentSignature(signature)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid payment signature quote payload: %v", err), http.StatusBadRequest)
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		http.Error(w, "invalid PAYMENT-SIGNATURE encoding", http.StatusBadRequest)
		return nil
	}
	var paymentPayload map[string]any
	if err := json.Unmarshal(decoded, &paymentPayload); err != nil {
		http.Error(w, "invalid PAYMENT-SIGNATURE payload", http.StatusBadRequest)
		return nil
	}

	verifyReq := map[string]any{
		"x402Version":         paymentPayload["x402Version"],
		"paymentPayload":      paymentPayload,
		"paymentRequirements": quote,
	}
	verifyBody, _ := json.Marshal(verifyReq)
	verifyResp, err := http.Post(x.FacilitatorURL+"/verify", "application/json", bytes.NewReader(verifyBody))
	if err != nil {
		http.Error(w, fmt.Sprintf("facilitator verify failed: %v", err), http.StatusBadGateway)
		return nil
	}
	defer verifyResp.Body.Close()
	rawVerify, _ := io.ReadAll(verifyResp.Body)
	if verifyResp.StatusCode < 200 || verifyResp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("facilitator verify status=%d body=%s", verifyResp.StatusCode, string(rawVerify)), http.StatusBadGateway)
		return nil
	}
	var verify facilitatorVerifyResponse
	if err := json.Unmarshal(rawVerify, &verify); err != nil {
		http.Error(w, "facilitator verify response parse failed", http.StatusBadGateway)
		return nil
	}
	if !verify.IsValid {
		required := map[string]any{
			"x402Version": 1,
			"accepts":     []map[string]any{quote},
			"error":       verify.InvalidReason,
		}
		requiredBytes, _ := json.Marshal(required)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(requiredBytes))
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":"%s","message":"%s"}`, verify.InvalidReason, verify.InvalidMessage)))
		return nil
	}

	settleReq := map[string]any{
		"x402Version":         paymentPayload["x402Version"],
		"paymentPayload":      paymentPayload,
		"paymentRequirements": quote,
	}
	settleBody, _ := json.Marshal(settleReq)
	settleResp, err := http.Post(x.FacilitatorURL+"/settle", "application/json", bytes.NewReader(settleBody))
	if err != nil {
		http.Error(w, fmt.Sprintf("facilitator settle failed: %v", err), http.StatusBadGateway)
		return nil
	}
	defer settleResp.Body.Close()
	rawSettle, _ := io.ReadAll(settleResp.Body)
	if settleResp.StatusCode < 200 || settleResp.StatusCode >= 300 {
		http.Error(w, fmt.Sprintf("facilitator settle status=%d body=%s", settleResp.StatusCode, string(rawSettle)), http.StatusBadGateway)
		return nil
	}
	var settle facilitatorSettleResponse
	if err := json.Unmarshal(rawSettle, &settle); err != nil {
		http.Error(w, "facilitator settle response parse failed", http.StatusBadGateway)
		return nil
	}
	if !settle.Success {
		required := map[string]any{
			"x402Version": 1,
			"accepts":     []map[string]any{quote},
			"error":       settle.ErrorReason,
		}
		requiredBytes, _ := json.Marshal(required)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(requiredBytes))
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":"%s","message":"%s"}`, settle.ErrorReason, settle.ErrorMessage)))
		return nil
	}

	proof := settlementProof{
		Version:           "aq-settlement-proof-v1",
		QuoteID:           quoteStringField(quote, "quoteId"),
		ActionFingerprint: quoteStringField(quote, "actionFingerprint"),
		Network:           quoteStringField(quote, "network"),
		AssetID:           quoteStringField(quote, "assetId"),
		Payer:             settle.Payer,
		Transaction:       settle.Transaction,
		Split:             quoteSplitField(quote),
		Nonce:             quoteStringField(quote, "nonce"),
		ExpiresAt:         quoteStringField(quote, "expiresAt"),
		SettledAt:         time.Now().UTC().Format(time.RFC3339),
	}
	if proof.Payer == "" {
		if payer, ok := paymentPayload["payer"].(string); ok {
			proof.Payer = payer
		}
	}
	proof.Signature = x.signSettlementProof(proof)
	rawProof, err := json.Marshal(proof)
	if err != nil {
		return fmt.Errorf("marshal settlement proof: %w", err)
	}
	r.Header.Set("X-AQ-Settlement-Proof", base64.StdEncoding.EncodeToString(rawProof))
	return next.ServeHTTP(w, r)
}

func (x *X402) maybeWriteDynamicQuoteResponse(w http.ResponseWriter, r *http.Request) (bool, error) {
	timeout := 3 * time.Second
	if x.QuoteTimeoutMS > 0 {
		timeout = time.Duration(x.QuoteTimeoutMS) * time.Millisecond
	}
	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, x.QuoteURL, nil)
	if err != nil {
		return false, fmt.Errorf("x402 quote request build failed: %w", err)
	}
	if x.QuoteAuthHeader != "" {
		req.Header.Set("Authorization", x.QuoteAuthHeader)
	}
	req.Header.Set("X-Forwarded-Method", r.Method)
	req.Header.Set("X-Forwarded-Path", r.URL.Path)
	req.Header.Set("X-Forwarded-Query", r.URL.RawQuery)

	resp, err := client.Do(req)
	if err != nil {
		x.logger.Warn("quote fetch failed, falling back to static payment requirements", zap.Error(err))
		return false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		x.logger.Warn("quote endpoint non-2xx, falling back to static payment requirements", zap.Int("status", resp.StatusCode))
		return false, nil
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("quote read failed: %w", err)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		x.logger.Warn("quote endpoint returned empty body, falling back to static payment requirements")
		return false, nil
	}

	var quote map[string]any
	if err := json.Unmarshal(body, &quote); err != nil {
		x.logger.Warn("quote endpoint returned invalid json, falling back to static payment requirements", zap.Error(err))
		return false, nil
	}
	required := map[string]any{
		"x402Version": 1,
		"accepts":     []map[string]any{quote},
		"error":       "X402 payment required",
	}
	encoded, _ := json.Marshal(required)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(encoded))
	w.WriteHeader(http.StatusPaymentRequired)
	_, _ = w.Write(encoded)
	return true, nil
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

type settlementProof struct {
	Version           string              `json:"version"`
	QuoteID           string              `json:"quoteId"`
	ActionFingerprint string              `json:"actionFingerprint"`
	Network           string              `json:"network"`
	AssetID           string              `json:"assetId"`
	Payer             string              `json:"payer"`
	Transaction       string              `json:"transaction"`
	Split             []map[string]string `json:"split"`
	Nonce             string              `json:"nonce"`
	ExpiresAt         string              `json:"expiresAt"`
	SettledAt         string              `json:"settledAt"`
	Signature         string              `json:"signature"`
}

func (x *X402) isSettlementGatedPath(path string) bool {
	return x.settlementGate != nil && x.settlementGate.MatchString(path)
}

func (x *X402) parseQuoteFromPaymentSignature(encoded string) (map[string]any, error) {
	if encoded == "" {
		return nil, fmt.Errorf("missing PAYMENT-SIGNATURE header")
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid PAYMENT-SIGNATURE encoding: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, fmt.Errorf("invalid PAYMENT-SIGNATURE json: %w", err)
	}
	required, ok := payload["paymentRequired"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("paymentRequired missing from PAYMENT-SIGNATURE")
	}
	accepts, ok := required["accepts"].([]any)
	if !ok || len(accepts) == 0 {
		return nil, fmt.Errorf("accepts missing from PAYMENT-SIGNATURE paymentRequired")
	}
	quote, ok := accepts[0].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("first accepts entry is not an object")
	}
	return quote, nil
}

func quoteStringField(quote map[string]any, key string) string {
	if v, ok := quote[key].(string); ok {
		return v
	}
	return ""
}

func quoteSplitField(quote map[string]any) []map[string]string {
	rawSplit, ok := quote["split"].([]any)
	if !ok {
		return nil
	}
	result := make([]map[string]string, 0, len(rawSplit))
	for _, item := range rawSplit {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		result = append(result, map[string]string{
			"role":    quoteStringField(entry, "role"),
			"address": quoteStringField(entry, "address"),
			"amount":  quoteStringField(entry, "amount"),
		})
	}
	return result
}

func (x *X402) signSettlementProof(payload settlementProof) string {
	canonicalSplit := make([]map[string]string, len(payload.Split))
	copy(canonicalSplit, payload.Split)
	sort.Slice(canonicalSplit, func(i, j int) bool {
		left := canonicalSplit[i]["role"] + ":" + canonicalSplit[i]["address"]
		right := canonicalSplit[j]["role"] + ":" + canonicalSplit[j]["address"]
		return left < right
	})
	canonicalPayload := struct {
		Version           string              `json:"version"`
		QuoteID           string              `json:"quoteId"`
		ActionFingerprint string              `json:"actionFingerprint"`
		Network           string              `json:"network"`
		AssetID           string              `json:"assetId"`
		Payer             string              `json:"payer"`
		Transaction       string              `json:"transaction"`
		Split             []map[string]string `json:"split"`
		Nonce             string              `json:"nonce"`
		ExpiresAt         string              `json:"expiresAt"`
		SettledAt         string              `json:"settledAt"`
	}{
		Version:           payload.Version,
		QuoteID:           payload.QuoteID,
		ActionFingerprint: payload.ActionFingerprint,
		Network:           payload.Network,
		AssetID:           payload.AssetID,
		Payer:             payload.Payer,
		Transaction:       payload.Transaction,
		Split:             canonicalSplit,
		Nonce:             payload.Nonce,
		ExpiresAt:         payload.ExpiresAt,
		SettledAt:         payload.SettledAt,
	}
	canonicalBytes, _ := json.Marshal(canonicalPayload)
	mac := hmac.New(sha256.New, []byte(x.ProofSharedSecret))
	mac.Write(canonicalBytes)
	return hex.EncodeToString(mac.Sum(nil))
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
	// Write JSON/plain body when present.
	if _, already := resp.Headers["Content-Type"]; !already {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.Status)
	switch body := resp.Body.(type) {
	case string:
		_, _ = w.Write([]byte(body))
	default:
		if encoded, err := json.Marshal(body); err == nil {
			_, _ = w.Write(encoded)
		}
	}
}
