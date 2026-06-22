package nlmingest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// --- fakes ---

// fakePending is a READ-ONLY in-memory pending buffer keyed by historyKey. It
// records every ListSince cursor it was asked for so tests can assert the
// high-water is honored, and it NEVER mutates (the worker must not delete).
type fakePending struct {
	groups   []store.PendingMessageGroup
	byKey    map[string][]store.PendingMessage
	listErr  error
	groupErr error

	mu        sync.Mutex
	sinceArgs []sinceCall
}

type sinceCall struct {
	historyKey string
	afterTime  time.Time
	afterID    uuid.UUID
}

func (f *fakePending) ListGroups(ctx context.Context) ([]store.PendingMessageGroup, error) {
	if f.groupErr != nil {
		return nil, f.groupErr
	}
	return f.groups, nil
}

func (f *fakePending) ListSince(ctx context.Context, channelName, historyKey string, afterCreatedAt time.Time, afterID uuid.UUID, limit int) ([]store.PendingMessage, error) {
	f.mu.Lock()
	f.sinceArgs = append(f.sinceArgs, sinceCall{historyKey, afterCreatedAt, afterID})
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	all := f.byKey[historyKey]
	var out []store.PendingMessage
	for _, m := range all {
		if m.IsSummary {
			continue // mirror the real store: ingest worker never sees summaries
		}
		if after(m.CreatedAt, m.ID, afterCreatedAt, afterID) {
			out = append(out, m)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// after reports whether (ca, id) > (afterCa, afterID) on the composite key.
func after(ca time.Time, id uuid.UUID, afterCa time.Time, afterID uuid.UUID) bool {
	if ca.After(afterCa) {
		return true
	}
	if ca.Equal(afterCa) {
		return strings.Compare(id.String(), afterID.String()) > 0
	}
	return false
}

// fakeCursor is an in-memory cursor store.
type fakeCursor struct {
	mu        sync.Mutex
	cursors   map[string]store.IngestCursor
	getErr    error
	upsertErr error
	upserts   []store.IngestCursor
}

func newFakeCursor() *fakeCursor { return &fakeCursor{cursors: map[string]store.IngestCursor{}} }

func cursorKey(tenant uuid.UUID, ch, hk string) string { return tenant.String() + "|" + ch + "|" + hk }

func (f *fakeCursor) Get(ctx context.Context, tenant uuid.UUID, channelName, historyKey string) (store.IngestCursor, bool, error) {
	if f.getErr != nil {
		return store.IngestCursor{}, false, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.cursors[cursorKey(tenant, channelName, historyKey)]
	if !ok {
		return store.IngestCursor{TenantID: tenant, ChannelName: channelName, HistoryKey: historyKey}, false, nil
	}
	return c, true, nil
}

func (f *fakeCursor) Upsert(ctx context.Context, c store.IngestCursor) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursors[cursorKey(c.TenantID, c.ChannelName, c.HistoryKey)] = c
	f.upserts = append(f.upserts, c)
	return nil
}

// fakeProvisioner records each (scopeKind, scopeID) it provisioned.
type fakeProvisioner struct {
	mu    sync.Mutex
	calls []provCall
	err   error
}

type provCall struct {
	scopeKind   string
	scopeID     string
	displayName string
}

func (f *fakeProvisioner) GetOrCreateScopeNotebook(ctx context.Context, tenant uuid.UUID, scopeKind, scopeID, displayName string) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, provCall{scopeKind, scopeID, displayName})
	f.mu.Unlock()
	if f.err != nil {
		return "", "", f.err
	}
	return "nb-" + scopeKind + "-" + scopeID, "doc-" + scopeKind + "-" + scopeID, nil
}

// fakeDocs records each AppendText (docID + text).
type fakeDocs struct {
	mu      sync.Mutex
	appends []appendCall
	err     error
}

type appendCall struct {
	docID string
	text  string
}

func (f *fakeDocs) AppendText(ctx context.Context, docID, text string) error {
	f.mu.Lock()
	f.appends = append(f.appends, appendCall{docID, text})
	f.mu.Unlock()
	return f.err
}

// fakeSync records each SourceSync (notebookID).
type fakeSync struct {
	mu     sync.Mutex
	synced []string
	err    error
}

func (f *fakeSync) SourceSync(ctx context.Context, notebookID string) error {
	f.mu.Lock()
	f.synced = append(f.synced, notebookID)
	f.mu.Unlock()
	return f.err
}

type fakeTenant struct{ t uuid.UUID }

func (f *fakeTenant) DefaultTenant(ctx context.Context) uuid.UUID { return f.t }

type fakeNames struct {
	user  string
	group string
}

func (f *fakeNames) UserDisplayName(ctx context.Context, tenant uuid.UUID, userID string) string {
	return f.user
}
func (f *fakeNames) GroupDisplayName(ctx context.Context, tenant uuid.UUID, chatID string) string {
	return f.group
}

// --- helpers ---

const lwPrefix = "lineworks:"

func dmMsg(userID, body string, ts time.Time) store.PendingMessage {
	return store.PendingMessage{
		ID:          uuid.Must(uuid.NewV7()),
		ChannelName: channelName,
		HistoryKey:  userID,
		Sender:      userID,
		SenderID:    lwPrefix + userID,
		Body:        body,
		CreatedAt:   ts,
	}
}

func groupMsg(chatID, senderUID, body string, ts time.Time) store.PendingMessage {
	return store.PendingMessage{
		ID:          uuid.Must(uuid.NewV7()),
		ChannelName: channelName,
		HistoryKey:  chatID,
		Sender:      senderUID,
		SenderID:    lwPrefix + senderUID,
		Body:        body,
		CreatedAt:   ts,
	}
}

// fakeModes returns a fixed memory_mode and records whether ResolveMode ran.
type fakeModes struct {
	mode   string
	called bool
}

func (f *fakeModes) ResolveMode(context.Context) string {
	f.called = true
	return f.mode
}

func newWorker(p *fakePending, c *fakeCursor, prov *fakeProvisioner, d *fakeDocs, s *fakeSync, n *fakeNames) *Worker {
	return &Worker{
		Pending:     p,
		Cursors:     c,
		Provisioner: prov,
		Docs:        d,
		Sync:        s,
		Tenants:     &fakeTenant{t: store.MasterTenantID},
		Names:       n,
		MinMessages: 1,
	}
}

// --- tests ---

// TestSweep_DMRoutesToUserScope_GroupToGroupScope verifies routing isolation:
// a DM key lands in a USER scope and a group key in a GROUP scope, NEVER agent
// or shared, and each scope batches its window into ONE append + ONE sync.
func TestSweep_DMRoutesToUserScope_GroupToGroupScope(t *testing.T) {
	t0 := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	p := &fakePending{
		groups: []store.PendingMessageGroup{
			{ChannelName: channelName, HistoryKey: "userA"},
			{ChannelName: channelName, HistoryKey: "chat123"},
		},
		byKey: map[string][]store.PendingMessage{
			"userA": {
				dmMsg("userA", "hello", t0),
				dmMsg("userA", "world", t0.Add(time.Minute)),
			},
			"chat123": {
				groupMsg("chat123", "userA", "in group", t0),
				groupMsg("chat123", "userB", "me too", t0.Add(time.Minute)),
			},
		},
	}
	c := newFakeCursor()
	prov := &fakeProvisioner{}
	docs := &fakeDocs{}
	sync := &fakeSync{}
	w := newWorker(p, c, prov, docs, sync, &fakeNames{user: "高玉明", group: "Project Room"})

	w.sweep(context.Background())

	// Exactly two provisions: one user, one group; no agent/shared.
	if len(prov.calls) != 2 {
		t.Fatalf("expected 2 provisions, got %d: %+v", len(prov.calls), prov.calls)
	}
	gotKinds := map[string]string{} // scopeKind -> scopeID
	for _, call := range prov.calls {
		if call.scopeKind == store.ScopeKindAgent || call.scopeKind == store.ScopeKindShared {
			t.Fatalf("ingest must never write agent/shared scope, got %q", call.scopeKind)
		}
		gotKinds[call.scopeKind] = call.scopeID
	}
	if gotKinds[store.ScopeKindUser] != "userA" {
		t.Errorf("DM should map to (user, userA), got user=%q", gotKinds[store.ScopeKindUser])
	}
	if gotKinds[store.ScopeKindGroup] != "chat123" {
		t.Errorf("group should map to (group, chat123), got group=%q", gotKinds[store.ScopeKindGroup])
	}

	// ONE append + ONE sync per scope (2 scopes → 2 each).
	if len(docs.appends) != 2 {
		t.Fatalf("expected 2 appends (one per scope), got %d", len(docs.appends))
	}
	if len(sync.synced) != 2 {
		t.Fatalf("expected 2 syncs (one per scope), got %d", len(sync.synced))
	}

	// The user batch must contain BOTH messages in one append.
	var userAppend string
	for _, a := range docs.appends {
		if a.docID == "doc-user-userA" {
			userAppend = a.text
		}
	}
	if !strings.Contains(userAppend, "hello") || !strings.Contains(userAppend, "world") {
		t.Errorf("user append should batch both messages, got %q", userAppend)
	}

	// Display name forwarded to provision.
	for _, call := range prov.calls {
		if call.scopeKind == store.ScopeKindUser && call.displayName != "高玉明" {
			t.Errorf("user displayName not forwarded, got %q", call.displayName)
		}
		if call.scopeKind == store.ScopeKindGroup && call.displayName != "Project Room" {
			t.Errorf("group displayName not forwarded, got %q", call.displayName)
		}
	}
}

// TestSweep_VaultMode_SkipsIngest verifies that a "vault"-mode deployment agent
// disables NotebookLM ingest entirely: no pending reads, no provision, no append,
// no sync, no cursor change.
func TestSweep_VaultMode_SkipsIngest(t *testing.T) {
	t0 := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	p := &fakePending{
		groups: []store.PendingMessageGroup{
			{ChannelName: channelName, HistoryKey: "userA"},
		},
		byKey: map[string][]store.PendingMessage{
			"userA": {dmMsg("userA", "hello", t0)},
		},
	}
	c := newFakeCursor()
	prov := &fakeProvisioner{}
	docs := &fakeDocs{}
	sync := &fakeSync{}
	w := newWorker(p, c, prov, docs, sync, &fakeNames{})
	modes := &fakeModes{mode: store.MemoryModeVault}
	w.Modes = modes

	w.sweep(context.Background())

	if !modes.called {
		t.Fatal("ResolveMode should have been consulted")
	}
	if len(prov.calls) != 0 || len(docs.appends) != 0 || len(sync.synced) != 0 {
		t.Fatalf("vault mode must skip ingest entirely, got prov=%d append=%d sync=%d",
			len(prov.calls), len(docs.appends), len(sync.synced))
	}
}

// TestSweep_BothMode_StillIngests verifies that a "both"-mode resolver does NOT
// disable ingest (the gate only fires for vault).
func TestSweep_BothMode_StillIngests(t *testing.T) {
	t0 := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	p := &fakePending{
		groups: []store.PendingMessageGroup{
			{ChannelName: channelName, HistoryKey: "userA"},
		},
		byKey: map[string][]store.PendingMessage{
			"userA": {dmMsg("userA", "hello", t0)},
		},
	}
	c := newFakeCursor()
	prov := &fakeProvisioner{}
	docs := &fakeDocs{}
	sync := &fakeSync{}
	w := newWorker(p, c, prov, docs, sync, &fakeNames{})
	w.Modes = &fakeModes{mode: store.MemoryModeBoth}

	w.sweep(context.Background())

	if len(prov.calls) != 1 || len(docs.appends) != 1 || len(sync.synced) != 1 {
		t.Fatalf("both mode must ingest, got prov=%d append=%d sync=%d",
			len(prov.calls), len(docs.appends), len(sync.synced))
	}
}

// TestSweep_NilModeResolver_StillIngests verifies the worker keeps ingesting when
// no mode resolver is wired (nil → treated as "both"), preserving current
// behavior and the existing tests that pass no resolver.
func TestSweep_NilModeResolver_StillIngests(t *testing.T) {
	t0 := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	p := &fakePending{
		groups: []store.PendingMessageGroup{
			{ChannelName: channelName, HistoryKey: "userA"},
		},
		byKey: map[string][]store.PendingMessage{
			"userA": {dmMsg("userA", "hello", t0)},
		},
	}
	c := newFakeCursor()
	prov := &fakeProvisioner{}
	docs := &fakeDocs{}
	sync := &fakeSync{}
	w := newWorker(p, c, prov, docs, sync, &fakeNames{}) // Modes left nil

	w.sweep(context.Background())

	if len(prov.calls) != 1 {
		t.Fatalf("nil resolver must default to active ingest, got prov=%d", len(prov.calls))
	}
}

// TestSweep_HighWaterAdvancesOnSuccess_PreventsReIngest verifies the cursor
// advances past the drained window and a second sweep re-ingests nothing.
func TestSweep_HighWaterAdvancesOnSuccess_PreventsReIngest(t *testing.T) {
	t0 := time.Date(2026, 6, 22, 10, 0, 0, 0, time.UTC)
	m1 := dmMsg("userA", "first", t0)
	m2 := dmMsg("userA", "second", t0.Add(time.Minute))
	p := &fakePending{
		groups: []store.PendingMessageGroup{{ChannelName: channelName, HistoryKey: "userA"}},
		byKey:  map[string][]store.PendingMessage{"userA": {m1, m2}},
	}
	c := newFakeCursor()
	docs := &fakeDocs{}
	sync := &fakeSync{}
	w := newWorker(p, c, &fakeProvisioner{}, docs, sync, &fakeNames{})

	// First sweep drains both, advances cursor.
	w.sweep(context.Background())
	if len(docs.appends) != 1 {
		t.Fatalf("first sweep: expected 1 append, got %d", len(docs.appends))
	}
	cur, ok, _ := c.Get(context.Background(), store.MasterTenantID, channelName, "userA")
	if !ok || cur.LastID != m2.ID || !cur.LastCreatedAt.Equal(m2.CreatedAt) {
		t.Fatalf("cursor should be at m2 after first sweep, got %+v", cur)
	}

	// Second sweep: nothing new → no append, no sync.
	w.sweep(context.Background())
	if len(docs.appends) != 1 {
		t.Errorf("second sweep must not re-ingest: appends=%d", len(docs.appends))
	}
	if len(sync.synced) != 1 {
		t.Errorf("second sweep must not re-sync: syncs=%d", len(sync.synced))
	}
}

// TestSweep_AppendFailure_LeavesCursor verifies an AppendText error does NOT
// advance the cursor (so the window retries next tick) and does not sync.
func TestSweep_AppendFailure_LeavesCursor(t *testing.T) {
	t0 := time.Now().UTC()
	p := &fakePending{
		groups: []store.PendingMessageGroup{{ChannelName: channelName, HistoryKey: "userA"}},
		byKey:  map[string][]store.PendingMessage{"userA": {dmMsg("userA", "x", t0)}},
	}
	c := newFakeCursor()
	docs := &fakeDocs{err: errors.New("drive down")}
	sync := &fakeSync{}
	w := newWorker(p, c, &fakeProvisioner{}, docs, sync, &fakeNames{})

	w.sweep(context.Background())

	if _, ok, _ := c.Get(context.Background(), store.MasterTenantID, channelName, "userA"); ok {
		t.Error("cursor must NOT advance when AppendText fails")
	}
	if len(sync.synced) != 0 {
		t.Error("must not sync when append failed")
	}
}

// TestSweep_SyncFailure_LeavesCursor verifies a SourceSync error does NOT
// advance the cursor (the notebook would otherwise never see the batch).
func TestSweep_SyncFailure_LeavesCursor(t *testing.T) {
	t0 := time.Now().UTC()
	p := &fakePending{
		groups: []store.PendingMessageGroup{{ChannelName: channelName, HistoryKey: "userA"}},
		byKey:  map[string][]store.PendingMessage{"userA": {dmMsg("userA", "x", t0)}},
	}
	c := newFakeCursor()
	docs := &fakeDocs{}
	sync := &fakeSync{err: errors.New("nlm down")}
	w := newWorker(p, c, &fakeProvisioner{}, docs, sync, &fakeNames{})

	w.sweep(context.Background())

	if len(docs.appends) != 1 {
		t.Fatalf("append should have happened before sync, got %d", len(docs.appends))
	}
	if _, ok, _ := c.Get(context.Background(), store.MasterTenantID, channelName, "userA"); ok {
		t.Error("cursor must NOT advance when SourceSync fails")
	}
}

// TestSweep_OneScopeFails_OthersContinue verifies fail-soft isolation: a failing
// scope does not abort the sweep — other scopes still drain.
func TestSweep_OneScopeFails_OthersContinue(t *testing.T) {
	t0 := time.Now().UTC()
	p := &fakePending{
		groups: []store.PendingMessageGroup{
			{ChannelName: channelName, HistoryKey: "userA"},
			{ChannelName: channelName, HistoryKey: "userB"},
		},
		byKey: map[string][]store.PendingMessage{
			"userA": {dmMsg("userA", "a", t0)},
			"userB": {dmMsg("userB", "b", t0)},
		},
	}
	c := newFakeCursor()
	// Provisioner fails ONLY for userA (scopeID == userA).
	prov := &failingProvisioner{failScopeID: "userA"}
	docs := &fakeDocs{}
	sync := &fakeSync{}
	w := &Worker{Pending: p, Cursors: c, Provisioner: prov, Docs: docs, Sync: sync,
		Tenants: &fakeTenant{t: store.MasterTenantID}, MinMessages: 1}

	w.sweep(context.Background())

	// userB drained despite userA failing.
	if _, ok, _ := c.Get(context.Background(), store.MasterTenantID, channelName, "userB"); !ok {
		t.Error("userB should have drained even though userA failed")
	}
	if _, ok, _ := c.Get(context.Background(), store.MasterTenantID, channelName, "userA"); ok {
		t.Error("userA cursor must not advance (it failed)")
	}
	if len(docs.appends) != 1 {
		t.Errorf("only userB should have appended, got %d appends", len(docs.appends))
	}
}

type failingProvisioner struct {
	failScopeID string
}

func (f *failingProvisioner) GetOrCreateScopeNotebook(ctx context.Context, tenant uuid.UUID, scopeKind, scopeID, displayName string) (string, string, error) {
	if scopeID == f.failScopeID {
		return "", "", errors.New("provision failed")
	}
	return "nb-" + scopeID, "doc-" + scopeID, nil
}

// TestSweep_MinMessages_Holds verifies a scope below the flush threshold is not
// drained until it accumulates enough new messages.
func TestSweep_MinMessages_Holds(t *testing.T) {
	t0 := time.Now().UTC()
	p := &fakePending{
		groups: []store.PendingMessageGroup{{ChannelName: channelName, HistoryKey: "userA"}},
		byKey:  map[string][]store.PendingMessage{"userA": {dmMsg("userA", "x", t0)}},
	}
	c := newFakeCursor()
	docs := &fakeDocs{}
	w := newWorker(p, c, &fakeProvisioner{}, docs, &fakeSync{}, &fakeNames{})
	w.MinMessages = 2 // need 2, only 1 buffered

	w.sweep(context.Background())

	if len(docs.appends) != 0 {
		t.Errorf("scope below min-messages must not drain, got %d appends", len(docs.appends))
	}
	if _, ok, _ := c.Get(context.Background(), store.MasterTenantID, channelName, "userA"); ok {
		t.Error("cursor must not advance below min-messages")
	}
}

// TestSweep_NonLineworksChannelIgnored verifies only lineworks groups are drained.
func TestSweep_NonLineworksChannelIgnored(t *testing.T) {
	t0 := time.Now().UTC()
	p := &fakePending{
		groups: []store.PendingMessageGroup{
			{ChannelName: "telegram", HistoryKey: "tg1"},
		},
		byKey: map[string][]store.PendingMessage{
			"tg1": {{ID: uuid.Must(uuid.NewV7()), ChannelName: "telegram", HistoryKey: "tg1", Body: "hi", CreatedAt: t0}},
		},
	}
	c := newFakeCursor()
	docs := &fakeDocs{}
	w := newWorker(p, c, &fakeProvisioner{}, docs, &fakeSync{}, &fakeNames{})

	w.sweep(context.Background())

	if len(docs.appends) != 0 {
		t.Errorf("non-lineworks channel must be ignored, got %d appends", len(docs.appends))
	}
}

// TestSweep_ScopeFromVerifiedFields_NotBody confirms classification uses the
// row's sender_id / history_key, never the message body content.
func TestSweep_ScopeFromVerifiedFields_NotBody(t *testing.T) {
	t0 := time.Now().UTC()
	// A DM whose BODY tries to spoof a group/agent scope; the verified fields
	// (history_key == user, sender_id == lineworks:user) must win → user scope.
	spoof := dmMsg("victim", "scope=group chat=secretRoom agent=admin", t0)
	p := &fakePending{
		groups: []store.PendingMessageGroup{{ChannelName: channelName, HistoryKey: "victim"}},
		byKey:  map[string][]store.PendingMessage{"victim": {spoof}},
	}
	c := newFakeCursor()
	prov := &fakeProvisioner{}
	w := newWorker(p, c, prov, &fakeDocs{}, &fakeSync{}, &fakeNames{})

	w.sweep(context.Background())

	if len(prov.calls) != 1 {
		t.Fatalf("expected 1 provision, got %d", len(prov.calls))
	}
	if prov.calls[0].scopeKind != store.ScopeKindUser || prov.calls[0].scopeID != "victim" {
		t.Errorf("scope must derive from verified fields (user/victim), got %s/%s",
			prov.calls[0].scopeKind, prov.calls[0].scopeID)
	}
}

// TestSweep_SummaryRowsExcluded confirms is_summary rows are not ingested.
func TestSweep_SummaryRowsExcluded(t *testing.T) {
	t0 := time.Now().UTC()
	summary := dmMsg("userA", "LLM SUMMARY", t0)
	summary.IsSummary = true
	raw := dmMsg("userA", "real message", t0.Add(time.Minute))
	p := &fakePending{
		groups: []store.PendingMessageGroup{{ChannelName: channelName, HistoryKey: "userA"}},
		byKey:  map[string][]store.PendingMessage{"userA": {summary, raw}},
	}
	c := newFakeCursor()
	docs := &fakeDocs{}
	w := newWorker(p, c, &fakeProvisioner{}, docs, &fakeSync{}, &fakeNames{})

	w.sweep(context.Background())

	if len(docs.appends) != 1 {
		t.Fatalf("expected 1 append, got %d", len(docs.appends))
	}
	if strings.Contains(docs.appends[0].text, "LLM SUMMARY") {
		t.Error("summary rows must NOT be ingested")
	}
	if !strings.Contains(docs.appends[0].text, "real message") {
		t.Error("raw message should be ingested")
	}
}

// TestStart_DisabledIsNoOp verifies the worker does NOT tick when the feature is
// off: no provision/append/sync ever happens, and Start returns a usable cancel.
func TestStart_DisabledIsNoOp(t *testing.T) {
	t.Setenv(EnvEnabled, "false")
	p := &fakePending{groupErr: errors.New("must not be called")}
	w := newWorker(p, newFakeCursor(), &fakeProvisioner{}, &fakeDocs{}, &fakeSync{}, &fakeNames{})
	w.Interval = time.Millisecond

	cancel := w.Start(context.Background())
	defer cancel()
	time.Sleep(20 * time.Millisecond)

	p.mu.Lock()
	called := len(p.sinceArgs)
	p.mu.Unlock()
	if called != 0 {
		t.Errorf("disabled worker must not read the buffer, got %d ListSince calls", called)
	}
}

// TestStart_EnabledTicks verifies the worker drains on its ticker when enabled.
func TestStart_EnabledTicks(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	t0 := time.Now().UTC()
	p := &fakePending{
		groups: []store.PendingMessageGroup{{ChannelName: channelName, HistoryKey: "userA"}},
		byKey:  map[string][]store.PendingMessage{"userA": {dmMsg("userA", "tick", t0)}},
	}
	docs := &fakeDocs{}
	w := newWorker(p, newFakeCursor(), &fakeProvisioner{}, docs, &fakeSync{}, &fakeNames{})
	w.Interval = 5 * time.Millisecond

	cancel := w.Start(context.Background())
	defer cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		docs.mu.Lock()
		n := len(docs.appends)
		docs.mu.Unlock()
		if n >= 1 {
			return // drained
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("enabled worker did not drain within deadline")
}

// TestStart_MissingDepIsNoOp verifies Start no-ops (returns harmless cancel) when
// a required dependency is nil, even with the feature enabled.
func TestStart_MissingDepIsNoOp(t *testing.T) {
	t.Setenv(EnvEnabled, "true")
	w := &Worker{Pending: nil} // missing everything
	cancel := w.Start(context.Background())
	cancel() // must not panic
}
