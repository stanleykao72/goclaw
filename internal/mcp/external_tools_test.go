package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// TestResolveActorUserID is the docs/26 §11 condition-1 / §11.T B1 matrix.
// The lineworks branch is the actual B1 fix; it ALSO fixes a latent native-loop
// bug where LINE WORKS DM-after-contact-merge users lost per-user MCP creds.
func TestResolveActorUserID(t *testing.T) {
	cases := []struct {
		name        string
		userID      string
		senderID    string
		peerKind    string
		channelType string
		want        string
	}{
		// B1 core: lineworks DM-after-merge — userID is the rewritten tenant_user
		// UUID, the actor MUST resolve to lineworks:<uid> (the cred key).
		{"lineworks_dm_merged", "uuid-x", "lineworks:42", "direct", "lineworks", "lineworks:42"},
		{"lineworks_group", "group:lw:c1", "lineworks:42", "group", "lineworks", "lineworks:42"},
		// Non-provisioning channel with the SAME shape must be UNCHANGED (no
		// lineworks regression onto telegram et al.).
		{"telegram_dm_unchanged", "uuid-x", "tg:42", "direct", "telegram", "uuid-x"},
		// bitrix24 special-case still works (not regressed by the lineworks add).
		{"bitrix24_dm", "uuid-x", "bx:7", "direct", "bitrix24", "bx:7"},
		{"bitrix24_group", "group:bx:c", "bx:7", "group", "bitrix24", "bx:7"},
		// Group fallback (no channel special-case) keeps SenderID.
		{"generic_group_keeps_sender", "group:tg:c", "tg:9", "group", "telegram", "tg:9"},
		// DM on a generic channel keeps UserID.
		{"generic_dm_keeps_user", "uuid-y", "tg:9", "direct", "telegram", "uuid-y"},
		// Synthetic sender (empty SenderID) → UserID regardless of channel.
		{"lineworks_empty_sender", "uuid-z", "", "direct", "lineworks", "uuid-z"},
		{"bitrix24_empty_sender", "uuid-z", "", "group", "bitrix24", "uuid-z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveActorUserID(tc.userID, tc.senderID, tc.peerKind, tc.channelType)
			if got != tc.want {
				t.Fatalf("ResolveActorUserID(%q,%q,%q,%q) = %q, want %q",
					tc.userID, tc.senderID, tc.peerKind, tc.channelType, got, tc.want)
			}
		})
	}
}

func TestCredsPresent(t *testing.T) {
	if credsPresent(nil) {
		t.Fatal("nil creds must be absent")
	}
	if credsPresent(&store.MCPUserCredentials{}) {
		t.Fatal("empty creds must be absent")
	}
	if !credsPresent(&store.MCPUserCredentials{APIKey: "k"}) {
		t.Fatal("APIKey creds must be present")
	}
	if !credsPresent(&store.MCPUserCredentials{Headers: map[string]string{"a": "b"}}) {
		t.Fatal("header creds must be present")
	}
	if !credsPresent(&store.MCPUserCredentials{Env: map[string]string{"a": "b"}}) {
		t.Fatal("env creds must be present")
	}
}

func TestCredHasNonEmpty(t *testing.T) {
	if credHasNonEmpty(nil, "k") {
		t.Fatal("nil map")
	}
	if credHasNonEmpty(map[string]string{"k": "  "}, "k") {
		t.Fatal("blank value must be empty")
	}
	if !credHasNonEmpty(map[string]string{"k": "v"}, "k") {
		t.Fatal("present value")
	}
}

// fakeBridgeStore embeds the package's mockMCPStore (full interface stubs) and
// overrides the few methods ResolveExternalBridgeTools exercises.
type fakeBridgeStore struct {
	*mockMCPStore
	listErr      error
	lastListUser string
	listCalls    int32
	creds        map[string]*store.MCPUserCredentials // serverID|userID → creds
	deleteCalls  int32
}

func newFakeBridgeStore(accessible []store.MCPAccessInfo) *fakeBridgeStore {
	return &fakeBridgeStore{
		mockMCPStore: &mockMCPStore{accessible: accessible},
		creds:        map[string]*store.MCPUserCredentials{},
	}
}

func (f *fakeBridgeStore) ListAccessible(ctx context.Context, agentID uuid.UUID, userID string) ([]store.MCPAccessInfo, error) {
	atomic.AddInt32(&f.listCalls, 1)
	f.lastListUser = userID
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.mockMCPStore.accessible, nil
}

func (f *fakeBridgeStore) GetUserCredentials(ctx context.Context, serverID uuid.UUID, userID string) (*store.MCPUserCredentials, error) {
	return f.creds[serverID.String()+"|"+userID], nil
}

func (f *fakeBridgeStore) DeleteUserCredentials(ctx context.Context, serverID uuid.UUID, userID string) error {
	atomic.AddInt32(&f.deleteCalls, 1)
	return nil
}

func userCredServer(name string) store.MCPAccessInfo {
	return store.MCPAccessInfo{
		Server: store.MCPServerData{
			BaseModel: store.BaseModel{ID: uuid.New()},
			Name:      name,
			Enabled:   true,
			Settings:  json.RawMessage(`{"require_user_credentials":true}`),
		},
	}
}

// C5 / §11.T B1 S4: a ListAccessible error (tenant-less / legacy-HMAC ctx)
// MUST fail closed — zero tools, the error surfaced, never a fallback
// enumeration.
func TestResolveExternalBridgeTools_FailClosed(t *testing.T) {
	st := newFakeBridgeStore(nil)
	st.listErr = errors.New("scope: tenant required")
	pool := NewPool(PoolConfig{})

	out, actor, err := ResolveExternalBridgeTools(context.Background(), st, pool, nil,
		uuid.New(), uuid.New(), "uuid-x", "lineworks:42", "direct", "lineworks")

	if err == nil {
		t.Fatal("expected error to surface (fail-closed)")
	}
	if out != nil {
		t.Fatalf("expected zero tools on fail-closed, got %d", len(out))
	}
	if actor != "lineworks:42" {
		t.Fatalf("actor should still resolve, got %q", actor)
	}
}

func TestResolveExternalBridgeTools_NilDeps(t *testing.T) {
	out, actor, err := ResolveExternalBridgeTools(context.Background(), nil, nil, nil,
		uuid.New(), uuid.New(), "uuid-x", "lineworks:42", "direct", "lineworks")
	if err != nil || out != nil {
		t.Fatalf("nil deps must return (nil,nil); got out=%d err=%v", len(out), err)
	}
	if actor != "lineworks:42" {
		t.Fatalf("actor should resolve even with nil deps, got %q", actor)
	}
}

// §11 §2.7 / condition 3: ListAccessible MUST be scoped by the RESOLVED actor
// id, not the raw header userID.
func TestResolveExternalBridgeTools_ScopesByActor(t *testing.T) {
	st := newFakeBridgeStore([]store.MCPAccessInfo{})
	pool := NewPool(PoolConfig{})

	_, actor, err := ResolveExternalBridgeTools(context.Background(), st, pool, nil,
		uuid.New(), uuid.New(), "uuid-rewritten", "lineworks:42", "direct", "lineworks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if actor != "lineworks:42" {
		t.Fatalf("actor = %q, want lineworks:42", actor)
	}
	if st.lastListUser != "lineworks:42" {
		t.Fatalf("ListAccessible scoped by %q, want resolved actor lineworks:42", st.lastListUser)
	}
}

// A per-user server for which the actor has NO credentials must be skipped
// BEFORE any pool acquire (matches native getUserMCPTools / resolveServerCredentials
// skip). With no creds the server is dropped → empty result, no error, no
// DeleteUserCredentials.
func TestResolveExternalBridgeTools_SkipRequireUserCredsNoCreds(t *testing.T) {
	srv := userCredServer("odoo-prod")
	st := newFakeBridgeStore([]store.MCPAccessInfo{srv})
	// intentionally NO creds in st.creds
	pool := NewPool(PoolConfig{})

	out, _, err := ResolveExternalBridgeTools(context.Background(), st, pool, nil,
		uuid.New(), uuid.New(), "uuid-x", "lineworks:42", "direct", "lineworks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("require_user_credentials server with no creds must be skipped, got %d tools", len(out))
	}
	if atomic.LoadInt32(&st.deleteCalls) != 0 {
		t.Fatal("no 401, must not purge")
	}
}

// A disabled server is never surfaced.
func TestResolveExternalBridgeTools_SkipDisabled(t *testing.T) {
	srv := userCredServer("odoo-prod")
	srv.Server.Enabled = false
	st := newFakeBridgeStore([]store.MCPAccessInfo{srv})
	st.creds[srv.Server.ID.String()+"|lineworks:42"] = &store.MCPUserCredentials{APIKey: "k"}
	pool := NewPool(PoolConfig{})

	out, _, err := ResolveExternalBridgeTools(context.Background(), st, pool, nil,
		uuid.New(), uuid.New(), "uuid-x", "lineworks:42", "direct", "lineworks")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("disabled server must be skipped, got %d tools", len(out))
	}
}
