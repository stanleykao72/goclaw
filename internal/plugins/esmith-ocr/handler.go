// Package esmithocr exposes the /fr/ocr/extract HTTP endpoint consumed by the
// job_field_recorder WOFF expense wizard. The browser POSTs a base64 receipt
// image plus a short-lived HMAC token minted by Odoo (fr_expense_api
// ocr_token); the handler verifies the token locally with the shared secret,
// writes the image to a temp file, and runs a one-shot vision CLI extraction
// (provider-pluggable: agy / claude / codex / grok / gemini / …) bounded by a
// concurrency semaphore and a hard timeout.
//
// Trust boundary: the token is `uid.exp.hexsig` where hexsig =
// hex(hmac_sha256(secret, uid+"."+exp)). Verification failures always return
// a generic 401 and never log any token material.
package esmithocr

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxImageBytes is the decoded-image size cap. Anything larger is rejected
// before touching disk or the vision CLI.
const maxImageBytes = 10 * 1024 * 1024

// runnerTimeout is the hard limit for a single vision CLI invocation. On expiry
// the process is killed (via context) and the request resolves to result:null.
const runnerTimeout = 60 * time.Second

// maxConcurrentRunners caps simultaneous vision CLI processes.
const maxConcurrentRunners = 2

// Result is the structured invoice extraction returned by the vision runner.
// All fields are nullable: the model returns null for anything it cannot read.
type Result struct {
	InvoiceNo   *string  `json:"invoice_no"`
	InvoiceDate *string  `json:"invoice_date"`
	SellerVat   *string  `json:"seller_vat"`
	SellerName  *string  `json:"seller_name"`
	SalesAmount *float64 `json:"sales_amount"`
	TaxAmount   *float64 `json:"tax_amount"`
	Total       *float64 `json:"total"`
	Confidence  *float64 `json:"confidence"`
}

// Handler serves POST /fr/ocr/extract. Construct with NewHandler.
type Handler struct {
	Secret       string
	AllowOrigins []string

	// runner executes the vision extraction; tests inject a fake.
	runner func(ctx context.Context, imagePath string) (*Result, error)
	// sem bounds concurrent runner invocations (blocking queue).
	sem chan struct{}
	// now is injected for deterministic exp checks in tests.
	now func() time.Time
}

// NewHandler wires a production handler using the pluggable vision CLI runner
// and a semaphore of maxConcurrentRunners.
func NewHandler(secret string, allowOrigins []string) *Handler {
	return &Handler{
		Secret:       secret,
		AllowOrigins: allowOrigins,
		runner:       defaultRunner,
		sem:          make(chan struct{}, maxConcurrentRunners),
		now:          time.Now,
	}
}

// RegisterRoutes mounts the extract endpoint on mux. Implements the
// gateway.HTTPRoutes contract.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/fr/ocr/extract", h.cors(h.handleExtract))
}

// --- CORS (mirrors esmith-km LiffHandler) -----------------------------------

func (h *Handler) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, pattern := range h.AllowOrigins {
		if pattern == "*" || pattern == origin {
			return true
		}
		if strings.Contains(pattern, "*") {
			// Convert `https://odoo-esmith*.odoo.com` into a regex anchored
			// to the full origin string.
			escaped := regexp.QuoteMeta(pattern)
			escaped = strings.ReplaceAll(escaped, `\*`, `[a-zA-Z0-9-]*`)
			re, err := regexp.Compile("^" + escaped + "$")
			if err == nil && re.MatchString(origin) {
				return true
			}
		}
	}
	return false
}

func (h *Handler) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if h.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			if !h.originAllowed(origin) {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// --- Token verification ------------------------------------------------------

// verifyToken checks the stateless `uid.exp.hexsig` token. Any structural or
// cryptographic failure returns false; callers must respond with a generic
// 401 and must not log the token.
func (h *Handler) verifyToken(token string) bool {
	if h.Secret == "" || token == "" {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	uid, expStr, sigHex := parts[0], parts[1], parts[2]
	if uid == "" {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil || exp <= h.now().Unix() {
		return false
	}
	mac := hmac.New(sha256.New, []byte(h.Secret))
	mac.Write([]byte(uid + "." + expStr))
	expected := mac.Sum(nil)
	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}
	return hmac.Equal(expected, got)
}

// --- Extract endpoint ---------------------------------------------------------

type extractRequest struct {
	Image string `json:"image"`
	Token string `json:"token"`
}

type extractResponse struct {
	Success bool    `json:"success"`
	Result  *Result `json:"result"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func unauthorized(w http.ResponseWriter, reason string) {
	// Log an event only — never the token or signature material.
	slog.Warn("esmith-ocr: token verify failed", "reason", reason)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}

func (h *Handler) handleExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req extractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		badRequest(w, "invalid body")
		return
	}

	if req.Token == "" {
		unauthorized(w, "missing token")
		return
	}
	if !h.verifyToken(req.Token) {
		unauthorized(w, "invalid or expired token")
		return
	}

	// Strip an optional data-URI prefix (`data:image/jpeg;base64,....`).
	imageB64 := req.Image
	if strings.HasPrefix(imageB64, "data:") {
		if idx := strings.Index(imageB64, ","); idx >= 0 {
			imageB64 = imageB64[idx+1:]
		}
	}
	// Cheap pre-decode guard: base64 expands ~4/3, so anything whose encoded
	// length cannot decode under the cap is rejected without allocating.
	if len(imageB64) > (maxImageBytes/3+1)*4+4 {
		badRequest(w, "image too large")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(imageB64)
	if err != nil {
		badRequest(w, "invalid image encoding")
		return
	}
	if len(raw) > maxImageBytes {
		badRequest(w, "image too large")
		return
	}
	switch http.DetectContentType(raw) {
	case "image/jpeg", "image/png", "image/webp":
		// ok
	default:
		badRequest(w, "unsupported image type")
		return
	}

	tmp, err := os.CreateTemp("", "esmith-ocr-*.jpg")
	if err != nil {
		slog.Warn("esmith-ocr: temp file create failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	tmpPath := tmp.Name()
	// Always remove the temp file — on every exit path, error or not.
	defer func() {
		if rmErr := os.Remove(tmpPath); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Warn("esmith-ocr: temp file cleanup failed", "err", rmErr)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		slog.Warn("esmith-ocr: temp file write failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err := tmp.Close(); err != nil {
		slog.Warn("esmith-ocr: temp file close failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Acquire the runner semaphore (blocking queue, bounded by client ctx).
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-r.Context().Done():
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "canceled"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), runnerTimeout)
	defer cancel()

	result, err := h.runner(ctx, tmpPath)
	if err != nil {
		// Runner infrastructure failure (e.g. vision CLI binary missing). Degrade
		// to result:null — the frontend falls back to manual entry.
		slog.Warn("esmith-ocr: runner failed", "err", err)
		result = nil
	}
	sellerVat := ""
	if result != nil && result.SellerVat != nil {
		sellerVat = *result.SellerVat
	}
	invNo := ""
	if result != nil && result.InvoiceNo != nil {
		invNo = *result.InvoiceNo
	}
	slog.Info("esmith-ocr: extract ok", "invoice_no", invNo, "seller_vat", sellerVat, "has_result", result != nil)
	writeJSON(w, http.StatusOK, extractResponse{Success: true, Result: result})
}
