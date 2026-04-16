package esmithkm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// IDTokenVerifier is the narrow contract LiffHandler needs from the LINE
// channel. *line.Channel implements this; tests can pass a fake.
type IDTokenVerifier interface {
	VerifyIDToken(idToken string) (*line.IDTokenClaims, error)
}

// LiffHandler exposes HTTP endpoints consumed by the km-meeting attendees
// LIFF webview. Mounted on the goclaw gateway mux at /km/meeting/attendees/*.
//
// Trust boundary: every request carries a LINE-issued ID token in the
// Authorization header (bootstrap) or body (submit). The handler verifies it
// via IDTokenVerifier before touching any draft file or MCP endpoint.
type LiffHandler struct {
	Verifier     IDTokenVerifier
	DraftsDir    string
	MCPURL       string
	MCPToken     string
	AllowOrigins []string

	// mcp is the MCP call hook; tests override it.
	mcp func(ctx context.Context, url, token, tool string, args map[string]any, dst any) error
	// now is injected for deterministic updated_at in tests.
	now func() time.Time
}

// NewLiffHandler wires a handler using the package's default mcpToolCall and
// real wall clock. Production callers pass AllowOrigins as the list of
// LIFF-page origins allowed to XHR into goclaw (Odoo staging/prod hosts).
func NewLiffHandler(v IDTokenVerifier, draftsDir, mcpURL, mcpToken string, allowOrigins []string) *LiffHandler {
	return &LiffHandler{
		Verifier:     v,
		DraftsDir:    draftsDir,
		MCPURL:       mcpURL,
		MCPToken:     mcpToken,
		AllowOrigins: allowOrigins,
		mcp: func(ctx context.Context, url, token, tool string, args map[string]any, dst any) error {
			return mcpToolCall(ctx, url, token, tool, args, dst)
		},
		now: time.Now,
	}
}

// RegisterRoutes mounts bootstrap + submit endpoints on mux. Implements the
// gateway.routeRegistrar contract.
func (h *LiffHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/km/meeting/attendees/bootstrap", h.cors(h.handleBootstrap))
	mux.HandleFunc("/km/meeting/attendees/submit", h.cors(h.handleSubmit))
}

// --- CORS ------------------------------------------------------------------

// originAllowPatterns is compiled from the allow list at first use. A pattern
// may contain a single leading `*` as a subdomain wildcard (e.g.
// `https://odoo-esmith*.odoo.com` matches the rotating stage build subdomain).
var originPatternRE = regexp.MustCompile(`^\*`)

func (h *LiffHandler) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, pattern := range h.AllowOrigins {
		if pattern == "*" || pattern == origin {
			return true
		}
		if strings.Contains(pattern, "*") {
			// Convert `https://odoo-esmith*.odoo.com` into a regex
			// anchored to the full origin string.
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

func (h *LiffHandler) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if h.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
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

// --- Helpers ---------------------------------------------------------------

var refSafeRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (h *LiffHandler) draftPath(ref string) string {
	return filepath.Join(h.DraftsDir, ref+".json")
}

type bootstrapResponse struct {
	Success  bool      `json:"success"`
	Partners []partner `json:"partners"`
	Selected []int     `json:"selected"`
	Subject  string    `json:"subject"`
	Error    string    `json:"error,omitempty"`
}

type submitRequest struct {
	Ref         string `json:"ref"`
	LiffIDToken string `json:"liff_id_token"`
	PartnerIDs  []int  `json:"partner_ids"`
}

type submitResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *LiffHandler) verifyAuthHeader(r *http.Request) (*line.IDTokenClaims, error) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return nil, errors.New("missing bearer token")
	}
	return h.Verifier.VerifyIDToken(strings.TrimPrefix(auth, prefix))
}

// --- Bootstrap -------------------------------------------------------------

func (h *LiffHandler) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" || !refSafeRE.MatchString(ref) {
		writeJSON(w, http.StatusBadRequest, bootstrapResponse{Error: "invalid ref"})
		return
	}

	claims, err := h.verifyAuthHeader(r)
	if err != nil {
		writeJSON(w, http.StatusForbidden, bootstrapResponse{Error: "invalid token"})
		return
	}

	draft, err := readDraft(h.draftPath(ref))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
			writeJSON(w, http.StatusNotFound, bootstrapResponse{Error: "draft not found"})
			return
		}
		slog.Warn("liff bootstrap: draft read failed", "ref", ref, "err", err)
		writeJSON(w, http.StatusInternalServerError, bootstrapResponse{Error: "draft read failed"})
		return
	}
	if draft.LineUserID == nil || *draft.LineUserID != claims.Sub {
		writeJSON(w, http.StatusForbidden, bootstrapResponse{Error: "ownership mismatch"})
		return
	}

	partners, err := h.fetchPartners(r.Context())
	if err != nil {
		slog.Warn("liff bootstrap: mcp fetch failed", "ref", ref, "err", err)
		writeJSON(w, http.StatusBadGateway, bootstrapResponse{Error: "mcp fetch failed"})
		return
	}

	writeJSON(w, http.StatusOK, bootstrapResponse{
		Success:  true,
		Partners: partners,
		Selected: append([]int(nil), draft.Answers.PartnerIDs...),
		Subject:  draft.Subject,
	})
}

// fetchPartners returns internal company users (res.users, active=true,
// share=false) as partner id + name. Mirrors fetchPartnersFallback.
func (h *LiffHandler) fetchPartners(ctx context.Context) ([]partner, error) {
	var rows []struct {
		ID        int   `json:"id"`
		PartnerID []any `json:"partner_id"`
	}
	if err := h.mcp(ctx, h.MCPURL, h.MCPToken, "search_records", map[string]any{
		"model":  "res.users",
		"domain": [][]any{{"active", "=", true}, {"share", "=", false}},
		"fields": []string{"id", "partner_id"},
		"order":  "name asc",
		"limit":  maxAttendeesPerPicker,
	}, &rows); err != nil {
		return nil, err
	}
	out := make([]partner, 0, len(rows))
	for _, r := range rows {
		if len(r.PartnerID) >= 2 {
			id, _ := r.PartnerID[0].(float64)
			name, _ := r.PartnerID[1].(string)
			if int(id) > 0 {
				out = append(out, partner{ID: int(id), Name: name})
			}
		}
	}
	return out, nil
}

// --- Submit ----------------------------------------------------------------

func (h *LiffHandler) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req submitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, submitResponse{Error: "invalid body"})
		return
	}
	if req.Ref == "" || !refSafeRE.MatchString(req.Ref) {
		writeJSON(w, http.StatusBadRequest, submitResponse{Error: "invalid ref"})
		return
	}
	if len(req.PartnerIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, submitResponse{Error: "at least one attendee required"})
		return
	}

	claims, err := h.Verifier.VerifyIDToken(req.LiffIDToken)
	if err != nil {
		writeJSON(w, http.StatusForbidden, submitResponse{Error: "invalid token"})
		return
	}

	path := h.draftPath(req.Ref)
	draft, err := readDraft(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
			writeJSON(w, http.StatusNotFound, submitResponse{Error: "draft not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, submitResponse{Error: "draft read failed"})
		return
	}
	if draft.LineUserID == nil || *draft.LineUserID != claims.Sub {
		writeJSON(w, http.StatusForbidden, submitResponse{Error: "authentication mismatch"})
		return
	}

	partners, err := h.fetchPartners(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, submitResponse{Error: "mcp fetch failed"})
		return
	}
	pool := make(map[int]bool, len(partners))
	for _, p := range partners {
		pool[p.ID] = true
	}
	for _, pid := range req.PartnerIDs {
		if !pool[pid] {
			writeJSON(w, http.StatusBadRequest, submitResponse{
				Error: fmt.Sprintf("partner_id %d not in pool", pid),
			})
			return
		}
	}

	if err := h.patchDraft(path, req.PartnerIDs); err != nil {
		slog.Warn("liff submit: patch failed", "ref", req.Ref, "err", err)
		writeJSON(w, http.StatusInternalServerError, submitResponse{Error: "patch failed"})
		return
	}
	writeJSON(w, http.StatusOK, submitResponse{Success: true})
}

// patchDraft atomically updates answers.partner_ids + state + updated_at.
// Uses tmp+rename so concurrent readers never see a half-written file.
func (h *LiffHandler) patchDraft(path string, partnerIDs []int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse draft: %w", err)
	}
	answers, _ := m["answers"].(map[string]any)
	if answers == nil {
		answers = map[string]any{}
	}
	ids := make([]any, len(partnerIDs))
	for i, id := range partnerIDs {
		ids[i] = id
	}
	answers["partner_ids"] = ids
	m["answers"] = answers
	m["state"] = "awaiting_confirm"
	m["updated_at"] = h.now().UTC().Format(time.RFC3339)

	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
