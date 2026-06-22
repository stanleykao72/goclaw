package tools

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakePointerStore is an in-memory store.NotebookPointerStore for unit tests.
// Keyed by (tenant, kind, id). Create is race-safe (mutex + first-writer-wins),
// mirroring the PG ON CONFLICT DO NOTHING + re-SELECT semantics.
type fakePointerStore struct {
	mu       sync.Mutex
	rows     map[string]store.NotebookPointer
	creates  int // total Create calls (including losers)
	inserted int // distinct rows actually materialised
}

func newFakePointerStore() *fakePointerStore {
	return &fakePointerStore{rows: make(map[string]store.NotebookPointer)}
}

func ptrKey(tenant uuid.UUID, kind, id string) string {
	return tenant.String() + "|" + kind + "|" + id
}

func (f *fakePointerStore) Get(ctx context.Context, tenant uuid.UUID, kind, id string) (*store.NotebookPointer, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.rows[ptrKey(tenant, kind, id)]
	if !ok {
		return nil, false, nil
	}
	cp := p
	return &cp, true, nil
}

func (f *fakePointerStore) Create(ctx context.Context, p store.NotebookPointer) (*store.NotebookPointer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	k := ptrKey(p.TenantID, p.ScopeKind, p.ScopeID)
	if existing, ok := f.rows[k]; ok {
		cp := existing
		return &cp, nil // conflict — winner already present
	}
	f.rows[k] = p
	f.inserted++
	cp := p
	return &cp, nil
}

func (f *fakePointerStore) List(ctx context.Context, tenant uuid.UUID) ([]store.NotebookPointer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.NotebookPointer
	for _, p := range f.rows {
		if p.TenantID == tenant {
			out = append(out, p)
		}
	}
	return out, nil
}

const testTenantUUID = "11111111-1111-1111-1111-111111111111"

func testTenant() uuid.UUID { return uuid.MustParse(testTenantUUID) }

// dmCtx builds a verified DM context (tenant + user + agentKey, no group).
func dmCtx() context.Context {
	ctx := store.WithTenantID(context.Background(), testTenant())
	ctx = store.WithUserID(ctx, "user-777")
	ctx = store.WithAgentKey(ctx, "e-smith-hub")
	return ctx
}

// groupCtx builds a verified group context (adds peerKind=group + chatId).
func groupCtx() context.Context {
	ctx := dmCtx()
	ctx = WithToolPeerKind(ctx, "group")
	ctx = WithToolChatID(ctx, "chat-999")
	return ctx
}

func TestScopeKeyForCtx_DM(t *testing.T) {
	ctx := dmCtx()

	if sk, ok := scopeKeyForCtx(ctx, store.ScopeKindShared); !ok || sk.ID != "" {
		t.Fatalf("shared = %+v ok=%v", sk, ok)
	}
	if sk, ok := scopeKeyForCtx(ctx, store.ScopeKindUser); !ok || sk.ID != "user-777" {
		t.Fatalf("user = %+v ok=%v", sk, ok)
	}
	// agent → AgentKey (string), NOT the agent UUID.
	if sk, ok := scopeKeyForCtx(ctx, store.ScopeKindAgent); !ok || sk.ID != "e-smith-hub" {
		t.Fatalf("agent = %+v ok=%v", sk, ok)
	}
	// group must be skipped outside a group chat.
	if _, ok := scopeKeyForCtx(ctx, store.ScopeKindGroup); ok {
		t.Fatal("group scope must NOT resolve in a DM context")
	}
}

func TestScopeKeyForCtx_Group(t *testing.T) {
	ctx := groupCtx()
	sk, ok := scopeKeyForCtx(ctx, store.ScopeKindGroup)
	if !ok || sk.ID != "chat-999" {
		t.Fatalf("group = %+v ok=%v", sk, ok)
	}
}

func TestScopeKeyForCtx_MissingIdentitySkips(t *testing.T) {
	// Bare ctx: no user, no agentKey → user/agent scopes do not resolve.
	bare := store.WithTenantID(context.Background(), testTenant())
	if _, ok := scopeKeyForCtx(bare, store.ScopeKindUser); ok {
		t.Fatal("user scope must not resolve without a user id")
	}
	if _, ok := scopeKeyForCtx(bare, store.ScopeKindAgent); ok {
		t.Fatal("agent scope must not resolve without an agent key")
	}
	// shared always resolves.
	if _, ok := scopeKeyForCtx(bare, store.ScopeKindShared); !ok {
		t.Fatal("shared scope must always resolve")
	}
}

func TestScopeKeyForCtx_GroupWithoutChatID(t *testing.T) {
	ctx := store.WithTenantID(context.Background(), testTenant())
	ctx = WithToolPeerKind(ctx, "group") // group peer but no chat id
	if _, ok := scopeKeyForCtx(ctx, store.ScopeKindGroup); ok {
		t.Fatal("group scope must not resolve without a chat id")
	}
}

func TestCandidateScopesForCtx_Order(t *testing.T) {
	dm := candidateScopesForCtx(dmCtx())
	wantDM := []string{store.ScopeKindShared, store.ScopeKindUser, store.ScopeKindAgent}
	if len(dm) != len(wantDM) {
		t.Fatalf("DM candidates = %+v, want %v", dm, wantDM)
	}
	for i, w := range wantDM {
		if dm[i].Kind != w {
			t.Fatalf("DM candidate[%d] = %s, want %s", i, dm[i].Kind, w)
		}
	}

	grp := candidateScopesForCtx(groupCtx())
	wantGrp := []string{store.ScopeKindShared, store.ScopeKindUser, store.ScopeKindAgent, store.ScopeKindGroup}
	if len(grp) != len(wantGrp) {
		t.Fatalf("group candidates = %+v, want %v", grp, wantGrp)
	}
	for i, w := range wantGrp {
		if grp[i].Kind != w {
			t.Fatalf("group candidate[%d] = %s, want %s", i, grp[i].Kind, w)
		}
	}
}

func TestResolveNotebookSet_ExistingOnlyOrdered(t *testing.T) {
	st := newFakePointerStore()
	tenant := testTenant()

	// Seed shared + agent pointers (but NOT user) → resolve skips user.
	st.rows[ptrKey(tenant, store.ScopeKindShared, "")] = store.NotebookPointer{
		TenantID: tenant, ScopeKind: store.ScopeKindShared, NotebookID: "nb-shared", DriveDocID: "doc-shared",
	}
	st.rows[ptrKey(tenant, store.ScopeKindAgent, "e-smith-hub")] = store.NotebookPointer{
		TenantID: tenant, ScopeKind: store.ScopeKindAgent, ScopeID: "e-smith-hub", NotebookID: "nb-agent", DriveDocID: "doc-agent",
	}

	got, err := resolveNotebookSet(dmCtx(), st, tenant)
	if err != nil {
		t.Fatalf("resolveNotebookSet: %v", err)
	}
	// Candidate order is [shared, user, agent]; user has no pointer → omitted.
	if len(got) != 2 {
		t.Fatalf("got %d notebooks, want 2 (shared, agent)", len(got))
	}
	if got[0].Scope.Kind != store.ScopeKindShared || got[0].NotebookID != "nb-shared" {
		t.Fatalf("notebook[0] = %+v, want shared/nb-shared", got[0])
	}
	if got[1].Scope.Kind != store.ScopeKindAgent || got[1].NotebookID != "nb-agent" {
		t.Fatalf("notebook[1] = %+v, want agent/nb-agent", got[1])
	}
}

func TestResolveNotebookSet_GroupIncluded(t *testing.T) {
	st := newFakePointerStore()
	tenant := testTenant()
	st.rows[ptrKey(tenant, store.ScopeKindGroup, "chat-999")] = store.NotebookPointer{
		TenantID: tenant, ScopeKind: store.ScopeKindGroup, ScopeID: "chat-999", NotebookID: "nb-group", DriveDocID: "doc-group",
	}

	got, err := resolveNotebookSet(groupCtx(), st, tenant)
	if err != nil {
		t.Fatalf("resolveNotebookSet: %v", err)
	}
	// Only the group pointer exists → exactly one result, the group.
	if len(got) != 1 || got[0].Scope.Kind != store.ScopeKindGroup {
		t.Fatalf("got %+v, want single group notebook", got)
	}
}

func TestResolveNotebookSet_EmptyWhenNoPointers(t *testing.T) {
	st := newFakePointerStore()
	got, err := resolveNotebookSet(dmCtx(), st, testTenant())
	if err != nil {
		t.Fatalf("resolveNotebookSet: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d, want 0 (no pointers → fail-soft upstream)", len(got))
	}
}
