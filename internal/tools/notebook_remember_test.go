package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// --- fakes for the three curate write seams (no real Drive / nlm / network) ---
//
// Distinct from nlm_provision_test.go's fakeDocLibrary/fakeNotebookRunner: those
// don't capture AppendText text and can't fail AppendText/SourceSync, which the
// curate tests need. The Doc name encodes the scope, so the provisioned doc id
// (returned by curateDocs.CreateDoc) carries the scope for isolation assertions.

// curateDocs is an in-memory nlmdoc.DocLibrary that records every AppendText
// (docID + text) and serves EnsureFolder/CreateDoc so the real
// NotebookProvisioner runs end-to-end against it. CreateDoc returns "doc-<title>"
// so the scope-derived Doc name is visible in append assertions. appendErr
// injects an append failure.
type curateDocs struct {
	mu        sync.Mutex
	appends   []docAppendCall
	appendErr error
}

type docAppendCall struct {
	docID string
	text  string
}

func (f *curateDocs) EnsureFolder(ctx context.Context, path []string) (string, error) {
	return "folder-" + strings.Join(path, "/"), nil
}

func (f *curateDocs) CreateDoc(ctx context.Context, folderID, title, initialText string) (string, error) {
	return "doc-" + title, nil
}

func (f *curateDocs) AppendText(ctx context.Context, docID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.appendErr != nil {
		return f.appendErr
	}
	f.appends = append(f.appends, docAppendCall{docID, text})
	return nil
}

func (f *curateDocs) appendCalls() []docAppendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]docAppendCall, len(f.appends))
	copy(out, f.appends)
	return out
}

// curateRunner is an in-memory NLMNotebookRunner recording create / source add /
// sync so the curate path (create-on-first-write + sync-after-append) is
// asserted; syncErr injects a SourceSync failure (partial-success path).
type curateRunner struct {
	mu          sync.Mutex
	created     int
	addedSource int
	synced      []string
	createErr   error
	syncErr     error
}

func (f *curateRunner) CreateNotebook(ctx context.Context, title string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return "", f.createErr
	}
	f.created++
	return "nb-" + title, nil
}

func (f *curateRunner) AddDriveSource(ctx context.Context, notebookID, driveDocID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addedSource++
	return nil
}

func (f *curateRunner) DeleteNotebook(ctx context.Context, notebookID string) error {
	return nil
}

func (f *curateRunner) SourceSync(ctx context.Context, notebookID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.syncErr != nil {
		return f.syncErr
	}
	f.synced = append(f.synced, notebookID)
	return nil
}

func (f *curateRunner) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.synced)
}

// newRememberTool wires a curate tool (by scopeKind) to a REAL NotebookProvisioner
// backed by fake Doc library + fake nlm runner + the in-memory pointer store. The
// same fake doc library is set on both the provisioner (for create) and the tool
// (for append), so an append lands on the provisioned Doc id.
func newRememberTool(scopeKind string, docs *curateDocs, runner *curateRunner) *NotebookRememberTool {
	var tool *NotebookRememberTool
	switch scopeKind {
	case store.ScopeKindAgent:
		tool = NewRememberAgentTool()
	default:
		tool = NewRememberSharedTool()
	}
	ps := newFakePointerStore()
	prov := NewNotebookProvisioner(ps, docs, runner, "test-root")
	tool.SetProvisioner(prov)
	tool.SetDocs(docs)
	tool.SetSync(runner)
	return tool
}

// (a) content empty → ErrorResult, and NOTHING is written (no provision/append).
func TestNotebookRemember_EmptyContentErrors(t *testing.T) {
	docs := &curateDocs{}
	runner := &curateRunner{}
	tool := newRememberTool(store.ScopeKindShared, docs, runner)

	for _, content := range []any{"", "   ", nil} {
		res := tool.Execute(identityCtx(), map[string]any{"content": content})
		if !res.IsError {
			t.Fatalf("empty content %#v must be an error result", content)
		}
		if res.ForLLM != rememberContentRequired {
			t.Fatalf("expected %q, got %q", rememberContentRequired, res.ForLLM)
		}
	}
	if len(docs.appendCalls()) != 0 || runner.created != 0 {
		t.Fatal("empty content must not provision or append anything")
	}
}

// (b) vault mode → graceful no-op: no provision, no append, no sync.
func TestNotebookRemember_VaultModeNoOps(t *testing.T) {
	docs := &curateDocs{}
	runner := &curateRunner{}
	tool := newRememberTool(store.ScopeKindShared, docs, runner)

	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeVault)
	res := tool.Execute(ctx, map[string]any{"content": "remember this"})
	if res.IsError {
		t.Fatalf("vault mode must no-op gracefully, not error: %q", res.ForLLM)
	}
	if res.ForLLM != rememberNoMemory {
		t.Fatalf("expected %q, got %q", rememberNoMemory, res.ForLLM)
	}
	if runner.created != 0 || len(docs.appendCalls()) != 0 || runner.syncCount() != 0 {
		t.Fatal("vault mode must not provision/append/sync")
	}
}

// (c) shared scope, notebook mode → provision+append+sync with scope ("shared","")
// regardless of any bogus scope-ish arg the LLM smuggles.
func TestNotebookRemember_SharedScopeWritesAndIgnoresArgs(t *testing.T) {
	docs := &curateDocs{}
	runner := &curateRunner{}
	tool := newRememberTool(store.ScopeKindShared, docs, runner)

	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeNotebook)
	// Smuggled scope-redirect args MUST be ignored.
	res := tool.Execute(ctx, map[string]any{
		"content":  "the sky is blue",
		"title":    "Sky",
		"scope":    "agent",
		"agent":    "attacker",
		"scope_id": "evil",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}
	if res.ForLLM != rememberSavedShared {
		t.Fatalf("expected %q, got %q", rememberSavedShared, res.ForLLM)
	}
	if runner.created != 1 || runner.addedSource != 1 {
		t.Fatalf("shared scope must create exactly one notebook + source, got created=%d addedSource=%d", runner.created, runner.addedSource)
	}
	appends := docs.appendCalls()
	if len(appends) != 1 {
		t.Fatalf("expected exactly one append, got %d", len(appends))
	}
	// SECURITY: the doc id is derived from the SHARED scope provision, never from
	// the smuggled args. The provisioner names the Doc from DocName(scopeKind,...);
	// assert the append targeted a shared-derived doc, not an agent one.
	if strings.Contains(appends[0].docID, "agent") || strings.Contains(appends[0].docID, "attacker") {
		t.Fatalf("append must target the SHARED doc, not an arg-redirected one: %q", appends[0].docID)
	}
	if runner.syncCount() != 1 {
		t.Fatalf("expected one source sync, got %d", runner.syncCount())
	}
}

// (d) agent scope → provision targets ("agent", ctx agentKey); a content/arg
// claiming a different scope cannot redirect it (isolation).
func TestNotebookRemember_AgentScopeUsesCtxAgentKey(t *testing.T) {
	docs := &curateDocs{}
	runner := &curateRunner{}
	tool := newRememberTool(store.ScopeKindAgent, docs, runner)

	// identityCtx sets agentKey "e-smith-hub".
	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeBoth)
	res := tool.Execute(ctx, map[string]any{
		"content": "agent-only fact",
		// Attempt to redirect to shared / another agent — must be ignored.
		"scope": "shared",
		"agent": "other-agent",
	})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}
	if res.ForLLM != rememberSavedAgent {
		t.Fatalf("expected %q, got %q", rememberSavedAgent, res.ForLLM)
	}
	appends := docs.appendCalls()
	if len(appends) != 1 {
		t.Fatalf("expected one append, got %d", len(appends))
	}
	// The Doc name encodes the agent scope + the ctx agentKey ("e-smith-hub").
	// A redirect to "shared" / "other-agent" must NOT appear.
	if !strings.Contains(appends[0].docID, "e-smith-hub") {
		t.Fatalf("agent scope must use ctx agentKey in the doc, got %q", appends[0].docID)
	}
	if strings.Contains(appends[0].docID, "other-agent") {
		t.Fatalf("arg must not redirect the agent scope: %q", appends[0].docID)
	}
}

// (e) agent scope with NO agentKey in ctx → graceful no-scope result, NO write.
func TestNotebookRemember_AgentScopeMissingKeyNoWrite(t *testing.T) {
	docs := &curateDocs{}
	runner := &curateRunner{}
	tool := newRememberTool(store.ScopeKindAgent, docs, runner)

	// Tenant + user but deliberately NO agentKey.
	ctx := store.WithTenantID(context.Background(), testTenant())
	ctx = store.WithUserID(ctx, "user-42")
	ctx = store.WithMemoryMode(ctx, store.MemoryModeNotebook)

	res := tool.Execute(ctx, map[string]any{"content": "x"})
	if res.IsError {
		t.Fatalf("missing agentKey must fail soft, not error: %q", res.ForLLM)
	}
	if res.ForLLM != rememberNoScope {
		t.Fatalf("expected %q, got %q", rememberNoScope, res.ForLLM)
	}
	if runner.created != 0 || len(docs.appendCalls()) != 0 {
		t.Fatal("missing agentKey must not provision or append")
	}
}

// (f) provenance header: append text begins with "\n\n## <title> — <ts> (by <user>)"
// and ends with the content + trailing newline. The curating user is the ctx
// sender name when present.
func TestNotebookRemember_ProvenanceHeader(t *testing.T) {
	docs := &curateDocs{}
	runner := &curateRunner{}
	tool := newRememberTool(store.ScopeKindShared, docs, runner)

	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeNotebook)
	ctx = store.WithSenderName(ctx, "高玉明")
	res := tool.Execute(ctx, map[string]any{"content": "the fact body", "title": "My Title"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}
	appends := docs.appendCalls()
	if len(appends) != 1 {
		t.Fatalf("expected one append, got %d", len(appends))
	}
	got := appends[0].text
	if !strings.HasPrefix(got, "\n\n## My Title — ") {
		t.Fatalf("header prefix missing, got: %q", got)
	}
	if !strings.Contains(got, "(by 高玉明)") {
		t.Fatalf("curating user missing from header, got: %q", got)
	}
	if !strings.HasSuffix(got, "the fact body\n") {
		t.Fatalf("body + trailing newline missing, got: %q", got)
	}
	// The timestamp must be RFC3339-parseable (between the em-dash and " (by").
	tsStart := strings.Index(got, " — ") + len(" — ")
	tsEnd := strings.Index(got, " (by ")
	if tsStart <= 0 || tsEnd <= tsStart {
		t.Fatalf("could not locate timestamp in header: %q", got)
	}
	if _, err := time.Parse(time.RFC3339, got[tsStart:tsEnd]); err != nil {
		t.Fatalf("header timestamp not RFC3339: %q (%v)", got[tsStart:tsEnd], err)
	}
}

// (f2) provenance user falls back to the bare (prefix-stripped) user id when no
// sender name is set.
func TestNotebookRemember_ProvenanceUserFallback(t *testing.T) {
	docs := &curateDocs{}
	runner := &curateRunner{}
	tool := newRememberTool(store.ScopeKindShared, docs, runner)

	ctx := store.WithTenantID(context.Background(), testTenant())
	ctx = store.WithUserID(ctx, "lineworks:abc123")
	ctx = store.WithAgentKey(ctx, "e-smith-hub")
	ctx = store.WithMemoryMode(ctx, store.MemoryModeNotebook)

	tool.Execute(ctx, map[string]any{"content": "x"})
	appends := docs.appendCalls()
	if len(appends) != 1 {
		t.Fatalf("expected one append, got %d", len(appends))
	}
	if !strings.Contains(appends[0].text, "(by abc123)") {
		t.Fatalf("expected bare user id fallback, got: %q", appends[0].text)
	}
}

// (g) AppendText failure → fail-soft "could not save"; provisioner WAS called,
// SourceSync was NOT.
func TestNotebookRemember_AppendFailureFailsSoft(t *testing.T) {
	docs := &curateDocs{appendErr: errors.New("drive 500")}
	runner := &curateRunner{}
	tool := newRememberTool(store.ScopeKindShared, docs, runner)

	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeNotebook)
	res := tool.Execute(ctx, map[string]any{"content": "x"})
	if res.IsError {
		t.Fatalf("append failure must fail soft, not error: %q", res.ForLLM)
	}
	if res.ForLLM != rememberFailSoft {
		t.Fatalf("expected %q, got %q", rememberFailSoft, res.ForLLM)
	}
	if runner.created != 1 {
		t.Fatalf("provisioner must have been called (notebook created) before append, got created=%d", runner.created)
	}
	if runner.syncCount() != 0 {
		t.Fatal("source sync must NOT run after an append failure")
	}
}

// (h) SourceSync failure AFTER a successful append → PARTIAL success (content
// saved). The append DID land.
func TestNotebookRemember_SyncFailurePartialSuccess(t *testing.T) {
	docs := &curateDocs{}
	runner := &curateRunner{syncErr: errors.New("nlm sync timeout")}
	tool := newRememberTool(store.ScopeKindShared, docs, runner)

	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeNotebook)
	res := tool.Execute(ctx, map[string]any{"content": "x"})
	if res.IsError {
		t.Fatalf("sync failure must be partial success, not error: %q", res.ForLLM)
	}
	if res.ForLLM != rememberPartialShared {
		t.Fatalf("expected %q, got %q", rememberPartialShared, res.ForLLM)
	}
	if len(docs.appendCalls()) != 1 {
		t.Fatal("append MUST have landed before the sync failure (partial success)")
	}
}

// (i) unwired tool (no provisioner/docs injected) → fail-soft, no panic.
func TestNotebookRemember_UnwiredFailsSoft(t *testing.T) {
	tool := NewRememberSharedTool() // SetProvisioner/SetDocs/SetSync NOT called

	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeNotebook)
	res := tool.Execute(ctx, map[string]any{"content": "x"})
	if res.IsError {
		t.Fatalf("unwired tool must fail soft, not error: %q", res.ForLLM)
	}
	if res.ForLLM != rememberFailSoft {
		t.Fatalf("expected %q, got %q", rememberFailSoft, res.ForLLM)
	}
}

// (j) schema exposes ONLY {content, title}; content required; no scope/agent field.
func TestNotebookRemember_SchemaContentTitleOnly(t *testing.T) {
	for _, tool := range []*NotebookRememberTool{NewRememberSharedTool(), NewRememberAgentTool()} {
		params := tool.Parameters()
		props, ok := params["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s: properties not a map", tool.Name())
		}
		if _, ok := props["content"]; !ok {
			t.Fatalf("%s: schema missing 'content'", tool.Name())
		}
		if _, ok := props["title"]; !ok {
			t.Fatalf("%s: schema missing 'title'", tool.Name())
		}
		if len(props) != 2 {
			t.Fatalf("%s: schema must expose ONLY content+title, got %d: %#v", tool.Name(), len(props), props)
		}
		for _, forbidden := range []string{"scope", "scope_id", "agent", "notebook", "notebook_id", "kind"} {
			if _, bad := props[forbidden]; bad {
				t.Fatalf("%s: schema must NOT expose %q (scope redirect)", tool.Name(), forbidden)
			}
		}
		required, _ := params["required"].([]string)
		if len(required) != 1 || required[0] != "content" {
			t.Fatalf("%s: required must be exactly [content], got %#v", tool.Name(), required)
		}
	}
}

// (k) tool names are stable (registration sites depend on these literals).
func TestNotebookRemember_Names(t *testing.T) {
	if got := NewRememberSharedTool().Name(); got != "remember_shared" {
		t.Fatalf("shared Name() = %q, want remember_shared", got)
	}
	if got := NewRememberAgentTool().Name(); got != "remember_agent" {
		t.Fatalf("agent Name() = %q, want remember_agent", got)
	}
}
