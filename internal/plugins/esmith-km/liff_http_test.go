package esmithkm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/channels/line"
)

// fakeVerifier stands in for *line.Channel. Handler tests do not exercise
// the real HMAC path (that is covered by id_token_test.go); here we only
// care that the handler handles the verifier's return values correctly.
type fakeVerifier struct {
	claims *line.IDTokenClaims
	err    error
}

func (f *fakeVerifier) VerifyIDToken(_ string) (*line.IDTokenClaims, error) {
	return f.claims, f.err
}

// writeDraft writes a minimal draft JSON for a test and returns the
// absolute path.
func writeDraft(t *testing.T, dir, ref, lineUID, subject string, partnerIDs []int) string {
	t.Helper()
	lu := lineUID
	d := map[string]any{
		"source_ref":    ref,
		"state":         "awaiting_attendees",
		"line_chat_id":  "C1",
		"line_user_id":  &lu,
		"subject":       subject,
		"meeting_date":  "2026-04-16",
		"answers": map[string]any{
			"project_id":       42,
			"meeting_location": "office",
			"partner_ids":      partnerIDs,
		},
	}
	path := filepath.Join(dir, ref+".json")
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		t.Fatalf("marshal draft: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write draft: %v", err)
	}
	return path
}

func newTestHandler(t *testing.T, verifier IDTokenVerifier, dir string, mcpFn func(context.Context, string, string, string, map[string]any, any) error) *LiffHandler {
	t.Helper()
	h := NewLiffHandler(verifier, dir, "mcp://stub", "stub-token", []string{"https://odoo-esmith*.odoo.com"})
	if mcpFn != nil {
		h.mcp = mcpFn
	}
	h.now = func() time.Time { return time.Date(2026, 4, 16, 10, 0, 0, 0, time.UTC) }
	return h
}

// partnersMCP builds an mcp stub that returns the given partner ids with a
// predictable name. Matches the [id, name] many2one shape the real MCP emits.
func partnersMCP(ids ...int) func(context.Context, string, string, string, map[string]any, any) error {
	return func(_ context.Context, _ string, _ string, tool string, _ map[string]any, dst any) error {
		if tool != "search_records" {
			return errors.New("unexpected tool")
		}
		type row struct {
			ID        int   `json:"id"`
			PartnerID []any `json:"partner_id"`
		}
		rows := make([]row, 0, len(ids))
		for i, id := range ids {
			rows = append(rows, row{ID: i + 1, PartnerID: []any{float64(id), "User" + string(rune('A'+i))}})
		}
		data, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, dst)
	}
}

// --- Bootstrap -------------------------------------------------------------

func TestBootstrapEndpoint_ReturnPartners(t *testing.T) {
	dir := t.TempDir()
	writeDraft(t, dir, "ref1", "Uabc", "Subject 1", []int{101})

	verifier := &fakeVerifier{claims: &line.IDTokenClaims{Sub: "Uabc"}}
	h := newTestHandler(t, verifier, dir, partnersMCP(101, 102, 103))

	req := httptest.NewRequest(http.MethodGet, "/km/meeting/attendees/bootstrap?ref=ref1", nil)
	req.Header.Set("Authorization", "Bearer fake")
	rec := httptest.NewRecorder()
	h.handleBootstrap(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp bootstrapResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success {
		t.Fatal("success = false")
	}
	if len(resp.Partners) != 3 {
		t.Fatalf("partners len = %d, want 3", len(resp.Partners))
	}
	if len(resp.Selected) != 1 || resp.Selected[0] != 101 {
		t.Errorf("selected = %v, want [101]", resp.Selected)
	}
	if resp.Subject != "Subject 1" {
		t.Errorf("subject = %q", resp.Subject)
	}
}

func TestBootstrapEndpoint_RejectInvalidToken(t *testing.T) {
	dir := t.TempDir()
	writeDraft(t, dir, "ref1", "Uabc", "Subject 1", nil)

	verifier := &fakeVerifier{err: line.ErrIDTokenSignature}
	h := newTestHandler(t, verifier, dir, partnersMCP(101))

	req := httptest.NewRequest(http.MethodGet, "/km/meeting/attendees/bootstrap?ref=ref1", nil)
	req.Header.Set("Authorization", "Bearer tampered")
	rec := httptest.NewRecorder()
	h.handleBootstrap(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid token") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestBootstrapEndpoint_404MissingDraft(t *testing.T) {
	dir := t.TempDir()
	verifier := &fakeVerifier{claims: &line.IDTokenClaims{Sub: "Uabc"}}
	h := newTestHandler(t, verifier, dir, partnersMCP(101))

	req := httptest.NewRequest(http.MethodGet, "/km/meeting/attendees/bootstrap?ref=nope", nil)
	req.Header.Set("Authorization", "Bearer fake")
	rec := httptest.NewRecorder()
	h.handleBootstrap(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

// --- Submit ----------------------------------------------------------------

func TestSubmitEndpoint_PatchesDraft(t *testing.T) {
	dir := t.TempDir()
	path := writeDraft(t, dir, "ref1", "Uabc", "Subject 1", nil)

	verifier := &fakeVerifier{claims: &line.IDTokenClaims{Sub: "Uabc"}}
	h := newTestHandler(t, verifier, dir, partnersMCP(101, 102, 103))

	body, _ := json.Marshal(submitRequest{Ref: "ref1", LiffIDToken: "fake", PartnerIDs: []int{101, 103}})
	req := httptest.NewRequest(http.MethodPost, "/km/meeting/attendees/submit", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.handleSubmit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read patched: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse patched: %v", err)
	}
	if got := m["state"]; got != "awaiting_confirm" {
		t.Errorf("state = %v, want awaiting_confirm", got)
	}
	ans, _ := m["answers"].(map[string]any)
	if ans == nil {
		t.Fatal("answers missing")
	}
	pids, _ := ans["partner_ids"].([]any)
	if len(pids) != 2 {
		t.Fatalf("partner_ids len = %d, want 2", len(pids))
	}
	if _, ok := m["updated_at"].(string); !ok {
		t.Errorf("updated_at not a string: %v", m["updated_at"])
	}
}

func TestSubmitEndpoint_RejectMismatchedUser(t *testing.T) {
	dir := t.TempDir()
	path := writeDraft(t, dir, "ref1", "Uabc", "Subject 1", []int{101})
	orig, _ := os.ReadFile(path)

	verifier := &fakeVerifier{claims: &line.IDTokenClaims{Sub: "Uother"}}
	h := newTestHandler(t, verifier, dir, partnersMCP(101))

	body, _ := json.Marshal(submitRequest{Ref: "ref1", LiffIDToken: "fake", PartnerIDs: []int{101}})
	req := httptest.NewRequest(http.MethodPost, "/km/meeting/attendees/submit", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.handleSubmit(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "authentication mismatch") {
		t.Errorf("body = %s", rec.Body.String())
	}
	// Draft must be unchanged on reject.
	after, _ := os.ReadFile(path)
	if string(orig) != string(after) {
		t.Error("draft mutated on reject")
	}
}

func TestSubmitEndpoint_RejectEmptyPartners(t *testing.T) {
	dir := t.TempDir()
	path := writeDraft(t, dir, "ref1", "Uabc", "Subject 1", []int{101})
	orig, _ := os.ReadFile(path)

	verifier := &fakeVerifier{claims: &line.IDTokenClaims{Sub: "Uabc"}}
	h := newTestHandler(t, verifier, dir, partnersMCP(101))

	body, _ := json.Marshal(submitRequest{Ref: "ref1", LiffIDToken: "fake", PartnerIDs: []int{}})
	req := httptest.NewRequest(http.MethodPost, "/km/meeting/attendees/submit", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.handleSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least one attendee") {
		t.Errorf("body = %s", rec.Body.String())
	}
	after, _ := os.ReadFile(path)
	if string(orig) != string(after) {
		t.Error("draft mutated on empty-partners reject")
	}
}

func TestSubmitEndpoint_AtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	path := writeDraft(t, dir, "ref1", "Uabc", "Subject 1", nil)

	verifier := &fakeVerifier{claims: &line.IDTokenClaims{Sub: "Uabc"}}
	h := newTestHandler(t, verifier, dir, partnersMCP(101, 102))

	body, _ := json.Marshal(submitRequest{Ref: "ref1", LiffIDToken: "fake", PartnerIDs: []int{101}})
	req := httptest.NewRequest(http.MethodPost, "/km/meeting/attendees/submit", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.handleSubmit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	// .tmp must not leak after successful rename.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp leaked: err = %v", err)
	}
	// Final file must be valid JSON end-to-end.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("final not valid JSON: %v", err)
	}
	if m["state"] != "awaiting_confirm" {
		t.Errorf("state = %v", m["state"])
	}
}

// --- CORS ------------------------------------------------------------------

func TestCORS_AllowsOdooStagingSubdomain(t *testing.T) {
	h := NewLiffHandler(&fakeVerifier{}, "/tmp", "mcp://stub", "stub", []string{"https://odoo-esmith*.odoo.com"})
	tests := []struct {
		origin string
		want   bool
	}{
		{"https://odoo-esmith-v18-stage35-29554478.dev.odoo.com", false}, // host tail differs
		{"https://odoo-esmith.odoo.com", true},
		{"https://odoo-esmith-prod.odoo.com", true},
		{"https://evil.example.com", false},
		{"", false},
	}
	for _, tc := range tests {
		got := h.originAllowed(tc.origin)
		if got != tc.want {
			t.Errorf("origin %q: got %v, want %v", tc.origin, got, tc.want)
		}
	}
}
