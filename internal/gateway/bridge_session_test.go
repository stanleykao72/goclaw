package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// TestInjectBridgeIdentity_AllFields asserts the shared helper injects every
// identity field — including the agent key + memory backend recovered from the
// (fake) agent store — so the claude and agy paths produce identical ctx.
func TestInjectBridgeIdentity_AllFields(t *testing.T) {
	agentID := uuid.New()
	tenantID := uuid.New()
	agentStore := &stubAgentKeyStore{ag: &store.AgentData{
		AgentKey:    "vault-keeper",
		OtherConfig: json.RawMessage(`{"memory_backend":"vault"}`),
	}}

	id := ResolvedIdentity{
		AgentID:        agentID,
		UserID:         "user-42",
		TenantID:       tenantID.String(),
		Channel:        "lineworks",
		ChatID:         "chat-7",
		PeerKind:       "group",
		Workspace:      "/ws/abc",
		LocalKey:       "lk-1",
		SessionKey:     "sk-1",
		ChannelType:    "lineworks",
		SenderID:       "sender-9",
		TenantVerified: true,
		SenderVerified: true,
	}

	ctx := injectBridgeIdentity(context.Background(), id, agentStore)

	if got := store.AgentIDFromContext(ctx); got != agentID {
		t.Errorf("AgentID = %v, want %v", got, agentID)
	}
	if got := tools.ToolAgentKeyFromCtx(ctx); got != "vault-keeper" {
		t.Errorf("ToolAgentKey = %q, want %q", got, "vault-keeper")
	}
	if got := store.MemoryBackendFromCtx(ctx); got != "vault" {
		t.Errorf("MemoryBackend = %q, want %q", got, "vault")
	}
	if got := store.UserIDFromContext(ctx); got != "user-42" {
		t.Errorf("UserID = %q, want %q", got, "user-42")
	}
	if got := store.TenantIDFromContext(ctx); got != tenantID {
		t.Errorf("TenantID = %v, want %v", got, tenantID)
	}
	if got := store.SenderIDFromContext(ctx); got != "sender-9" {
		t.Errorf("SenderID = %q, want %q", got, "sender-9")
	}
	if got := tools.ToolChannelTypeFromCtx(ctx); got != "lineworks" {
		t.Errorf("ToolChannelType = %q, want %q", got, "lineworks")
	}
	if got := tools.ToolChannelFromCtx(ctx); got != "lineworks" {
		t.Errorf("ToolChannel = %q, want %q", got, "lineworks")
	}
	if got := tools.ToolChatIDFromCtx(ctx); got != "chat-7" {
		t.Errorf("ToolChatID = %q, want %q", got, "chat-7")
	}
	if got := tools.ToolPeerKindFromCtx(ctx); got != "group" {
		t.Errorf("ToolPeerKind = %q, want %q", got, "group")
	}
	if got := tools.ToolWorkspaceFromCtx(ctx); got != "/ws/abc" {
		t.Errorf("ToolWorkspace = %q, want %q", got, "/ws/abc")
	}
	if got := tools.ToolLocalKeyFromCtx(ctx); got != "lk-1" {
		t.Errorf("ToolLocalKey = %q, want %q", got, "lk-1")
	}
	if got := tools.ToolSessionKeyFromCtx(ctx); got != "sk-1" {
		t.Errorf("ToolSessionKey = %q, want %q", got, "sk-1")
	}
}

// TestInjectBridgeIdentity_VerificationGates asserts tenant/sender are gated by
// the *Verified flags and that workspace requires an agent/user identity.
func TestInjectBridgeIdentity_VerificationGates(t *testing.T) {
	tenantID := uuid.New()

	// Unverified tenant + sender: must NOT be injected.
	id := ResolvedIdentity{
		UserID:         "u",
		TenantID:       tenantID.String(),
		SenderID:       "s",
		ChannelType:    "c",
		TenantVerified: false,
		SenderVerified: false,
	}
	ctx := injectBridgeIdentity(context.Background(), id, nil)
	if got := store.TenantIDFromContext(ctx); got != uuid.Nil {
		t.Errorf("unverified TenantID injected: %v", got)
	}
	if got := store.SenderIDFromContext(ctx); got != "" {
		t.Errorf("unverified SenderID injected: %q", got)
	}
	if got := tools.ToolChannelTypeFromCtx(ctx); got != "" {
		t.Errorf("unverified ChannelType injected: %q", got)
	}

	// Workspace without any identity: must NOT be injected.
	noIdentity := ResolvedIdentity{Workspace: "/ws/secret"}
	ctx = injectBridgeIdentity(context.Background(), noIdentity, nil)
	if got := tools.ToolWorkspaceFromCtx(ctx); got != "" {
		t.Errorf("workspace injected without identity: %q", got)
	}
}

// NOTE: BridgeSessionStore (token→identity in-memory map) was deleted: variant 2
// of the bridge design uses port-as-identity exclusively (the per-session
// loopback port IS the credential), so the parallel token-store mechanism was
// unwired and removed for clarity. See bridge_session_listener.go.

// TestBridgeSessionListeners_Start binds a per-session loopback listener and
// asserts: a bearer request reaches the bridge handler with the injected
// identity; a wrong/missing bearer is rejected; the URL is 127.0.0.1; Close
// stops the listener.
func TestBridgeSessionListeners_Start(t *testing.T) {
	const token = "gw-token"
	agentID := uuid.New()
	agentStore := &stubAgentKeyStore{ag: &store.AgentData{AgentKey: "k", OtherConfig: json.RawMessage(`{"memory_backend":"vault"}`)}}

	// Stub bridge handler records the ctx it was called with.
	var gotAgentID uuid.UUID
	var gotKey, gotBackend, gotUser string
	stub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgentID = store.AgentIDFromContext(r.Context())
		gotKey = tools.ToolAgentKeyFromCtx(r.Context())
		gotBackend = store.MemoryBackendFromCtx(r.Context())
		gotUser = store.UserIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	mgr := NewBridgeSessionListeners(stub, token, agentStore)
	id := ResolvedIdentity{AgentID: agentID, UserID: "u-99", TenantVerified: true, SenderVerified: true}

	url, closer, err := mgr.Start(id)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer closer()

	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("URL not loopback: %q", url)
	}
	if !strings.HasSuffix(url, "/mcp/bridge") {
		t.Fatalf("URL missing bridge path: %q", url)
	}

	// Correct bearer -> reaches handler with injected identity.
	resp := doReq(t, http.MethodPost, url, "Bearer "+token)
	if resp != http.StatusOK {
		t.Fatalf("authorized request status = %d, want 200", resp)
	}
	if gotAgentID != agentID {
		t.Errorf("handler AgentID = %v, want %v", gotAgentID, agentID)
	}
	if gotKey != "k" {
		t.Errorf("handler agent key = %q, want %q", gotKey, "k")
	}
	if gotBackend != "vault" {
		t.Errorf("handler memory backend = %q, want %q", gotBackend, "vault")
	}
	if gotUser != "u-99" {
		t.Errorf("handler user = %q, want %q", gotUser, "u-99")
	}

	// Wrong bearer -> 401, handler not reached.
	gotAgentID = uuid.Nil
	if s := doReq(t, http.MethodPost, url, "Bearer nope"); s != http.StatusUnauthorized {
		t.Errorf("wrong-bearer status = %d, want 401", s)
	}
	if gotAgentID != uuid.Nil {
		t.Error("handler reached with wrong bearer")
	}

	// No bearer -> 401.
	if s := doReq(t, http.MethodPost, url, ""); s != http.StatusUnauthorized {
		t.Errorf("no-bearer status = %d, want 401", s)
	}

	// Close stops the listener: subsequent requests must fail to connect.
	if err := closer(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	client := &http.Client{Timeout: time.Second}
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if _, err := client.Do(req); err == nil {
		t.Error("request succeeded after Close; listener still up")
	}
}

// TestBridgeSessionListeners_CloseAll asserts that CloseAll tears down every
// still-tracked per-session listener (gateway-shutdown reclaim of leaked ports),
// that a normally-closed listener is already removed from the tracking map so
// CloseAll does not touch it, and that CloseAll is safe to call twice.
func TestBridgeSessionListeners_CloseAll(t *testing.T) {
	const token = "gw-token"
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mgr := NewBridgeSessionListeners(stub, token, nil)

	// Two listeners left live (simulating sessions whose closers were never
	// called — e.g. a crashed provider).
	url1, _, err := mgr.Start(ResolvedIdentity{UserID: "a"})
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}
	url2, _, err := mgr.Start(ResolvedIdentity{UserID: "b"})
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}

	// A third listener closed normally: its closer must remove it from tracking
	// so CloseAll has nothing extra to do for it.
	url3, closer3, err := mgr.Start(ResolvedIdentity{UserID: "c"})
	if err != nil {
		t.Fatalf("Start 3: %v", err)
	}
	if err := closer3(); err != nil {
		t.Fatalf("closer3: %v", err)
	}
	mgr.mu.Lock()
	tracked := len(mgr.listeners)
	mgr.mu.Unlock()
	if tracked != 2 {
		t.Fatalf("after normal close, tracked listeners = %d, want 2", tracked)
	}

	// All three URLs must now be reachable/unreachable as expected: 1 and 2 up,
	// 3 down.
	if s := doReq(t, http.MethodPost, url1, "Bearer "+token); s != http.StatusOK {
		t.Errorf("url1 pre-CloseAll status = %d, want 200", s)
	}
	if s := doReq(t, http.MethodPost, url2, "Bearer "+token); s != http.StatusOK {
		t.Errorf("url2 pre-CloseAll status = %d, want 200", s)
	}
	assertUnreachable(t, url3, token)

	// CloseAll reclaims the two leaked listeners.
	mgr.CloseAll()
	mgr.mu.Lock()
	tracked = len(mgr.listeners)
	mgr.mu.Unlock()
	if tracked != 0 {
		t.Errorf("after CloseAll, tracked listeners = %d, want 0", tracked)
	}
	assertUnreachable(t, url1, token)
	assertUnreachable(t, url2, token)

	// Idempotent: second CloseAll must not panic.
	mgr.CloseAll()
}

func assertUnreachable(t *testing.T, url, token string) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Errorf("url %q still reachable; listener not shut down", url)
	}
}

func doReq(t *testing.T, method, url, auth string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
