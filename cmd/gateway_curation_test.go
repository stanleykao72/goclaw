package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/scheduler"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// --- fakes ---

// fakePendingStore implements store.PendingMessageStore. Only ListGroups,
// ListByKey and DeleteByIDs carry behaviour; the rest are no-ops to satisfy the
// interface.
type fakePendingStore struct {
	groups    []store.PendingMessageGroup
	byKey     map[string][]store.PendingMessage // key = channel + "|" + historyKey
	deleted   [][]uuid.UUID                     // recorded DeleteByIDs calls
	listErr   error
	deleteErr error
	// requireTenant mirrors the real pg/sqlite stores' fail-closed scopeClause:
	// ListGroups/ListByKey error when the ctx carries no tenant. Used to prove the
	// sweep injects a tenant before any store read.
	requireTenant bool
}

func keyOf(channel, historyKey string) string { return channel + "|" + historyKey }

// tenantErr returns the fail-closed error the real store raises on a nil tenant.
func (f *fakePendingStore) tenantErr(ctx context.Context) error {
	if f.requireTenant && store.TenantIDFromContext(ctx) == uuid.Nil {
		return errors.New("tenant_id required")
	}
	return nil
}

func (f *fakePendingStore) AppendBatch(ctx context.Context, msgs []store.PendingMessage) error {
	return nil
}

func (f *fakePendingStore) ListByKey(ctx context.Context, channel, historyKey string) ([]store.PendingMessage, error) {
	if err := f.tenantErr(ctx); err != nil {
		return nil, err
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byKey[keyOf(channel, historyKey)], nil
}

func (f *fakePendingStore) ListSince(ctx context.Context, channel, historyKey string, afterCreatedAt time.Time, afterID uuid.UUID, limit int) ([]store.PendingMessage, error) {
	return nil, nil
}

func (f *fakePendingStore) DeleteByKey(ctx context.Context, channel, historyKey string) error {
	return nil
}

func (f *fakePendingStore) Compact(ctx context.Context, deleteIDs []uuid.UUID, summary *store.PendingMessage) error {
	return nil
}

func (f *fakePendingStore) DeleteByIDs(ctx context.Context, ids []uuid.UUID) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, ids)
	// Reflect the deletion in byKey so a subsequent ListByKey is consistent.
	for k, msgs := range f.byKey {
		var kept []store.PendingMessage
		for _, m := range msgs {
			drop := false
			for _, id := range ids {
				if m.ID == id {
					drop = true
					break
				}
			}
			if !drop {
				kept = append(kept, m)
			}
		}
		f.byKey[k] = kept
	}
	return nil
}

func (f *fakePendingStore) DeleteStale(ctx context.Context, olderThan time.Duration) (int64, error) {
	return 0, nil
}

func (f *fakePendingStore) ListGroups(ctx context.Context) ([]store.PendingMessageGroup, error) {
	if err := f.tenantErr(ctx); err != nil {
		return nil, err
	}
	return f.groups, nil
}

func (f *fakePendingStore) CountAll(ctx context.Context) (int64, error) { return 0, nil }

func (f *fakePendingStore) CountByKey(ctx context.Context, channel, historyKey string) (int, error) {
	return len(f.byKey[keyOf(channel, historyKey)]), nil
}

func (f *fakePendingStore) ResolveGroupTitles(ctx context.Context, groups []store.PendingMessageGroup) (map[string]string, error) {
	return map[string]string{}, nil
}

// fakeScheduler captures the RunRequest and returns a preset outcome.
type fakeScheduler struct {
	captured   []agent.RunRequest
	outcome    scheduler.RunOutcome
	onSchedule func() // optional: runs inside Schedule before the outcome is returned (simulate mid-run state changes)
}

func (f *fakeScheduler) Schedule(ctx context.Context, lane string, req agent.RunRequest) <-chan scheduler.RunOutcome {
	f.captured = append(f.captured, req)
	if f.onSchedule != nil {
		f.onSchedule()
	}
	ch := make(chan scheduler.RunOutcome, 1)
	ch <- f.outcome
	close(ch)
	return ch
}

// fakeAgentResolver returns a fixed default agent, and (for GetByID) any agent
// registered in byID, else the default agent.
type fakeAgentResolver struct {
	agent *store.AgentData
	err   error
	byID  map[uuid.UUID]*store.AgentData
}

func (f *fakeAgentResolver) GetDefault(ctx context.Context) (*store.AgentData, error) {
	return f.agent, f.err
}

func (f *fakeAgentResolver) GetByID(ctx context.Context, id uuid.UUID) (*store.AgentData, error) {
	if f.byID != nil {
		if a, ok := f.byID[id]; ok {
			return a, nil
		}
	}
	return f.agent, f.err
}

// fakeChannelResolver returns a fixed channel-instance list for channel→agent mapping.
type fakeChannelResolver struct {
	instances []store.ChannelInstanceData
	err       error
}

func (f *fakeChannelResolver) ListEnabled(ctx context.Context) ([]store.ChannelInstanceData, error) {
	return f.instances, f.err
}

// fakeSessionResetter records reset/save calls.
type fakeSessionResetter struct {
	resets int
	saves  int
}

func (f *fakeSessionResetter) Reset(ctx context.Context, key string) { f.resets++ }
func (f *fakeSessionResetter) Save(ctx context.Context, key string) error {
	f.saves++
	return nil
}

// --- helpers ---

func newTestCfg() *config.Config {
	enabled := true
	cfg := &config.Config{}
	cfg.Channels.GroupCuration = &config.GroupMemoryCurationConfig{
		Enabled:    &enabled,
		MinPending: 2,
		KeepRecent: 3,
		Model:      "cheap-model",
	}
	return cfg
}

func newTestSweeper(cfg *config.Config, pending store.PendingMessageStore, sched curationScheduler) *curationSweeper {
	return newCurationSweeper(cfg, pending, sched,
		&fakeAgentResolver{agent: &store.AgentData{AgentKey: "default", TenantID: store.MasterTenantID}},
		&fakeChannelResolver{}, // empty → resolveChannelAgent nil → default agent (existing behavior)
		&fakeSessionResetter{},
		nil, // provReg unused unless Provider set
	)
}

func mkMsgs(historyKey string, n int) []store.PendingMessage {
	msgs := make([]store.PendingMessage, n)
	base := time.Now().Add(-time.Duration(n) * time.Minute)
	for i := range msgs {
		msgs[i] = store.PendingMessage{
			ID:         uuid.Must(uuid.NewV7()),
			HistoryKey: historyKey,
			Sender:     "user",
			Body:       "msg",
			CreatedAt:  base.Add(time.Duration(i) * time.Minute),
		}
	}
	return msgs
}

// --- sweep tests ---

func TestSweep_DisabledSkipsCuration(t *testing.T) {
	cfg := newTestCfg()
	disabled := false
	cfg.Channels.GroupCuration.Enabled = &disabled

	pending := &fakePendingStore{
		groups: []store.PendingMessageGroup{{ChannelName: "lineworks", HistoryKey: "g1", MessageCount: 10}},
	}
	sched := &fakeScheduler{}
	s := newTestSweeper(cfg, pending, sched)
	s.sweep(context.Background())

	if len(sched.captured) != 0 {
		t.Fatalf("expected no curation when disabled, got %d runs", len(sched.captured))
	}
}

func TestSweep_MinPendingThreshold(t *testing.T) {
	cfg := newTestCfg() // MinPending = 2
	pending := &fakePendingStore{
		groups: []store.PendingMessageGroup{
			{ChannelName: "lineworks", HistoryKey: "low", MessageCount: 1},  // below threshold
			{ChannelName: "lineworks", HistoryKey: "high", MessageCount: 5}, // at/above
		},
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "high"): mkMsgs("high", 5),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)
	s.sweep(context.Background())

	if len(sched.captured) != 1 {
		t.Fatalf("expected 1 group curated (threshold), got %d", len(sched.captured))
	}
	if sched.captured[0].ChatID != "high" {
		t.Fatalf("expected 'high' group curated, got %q", sched.captured[0].ChatID)
	}
}

func TestSweep_OnlyLineworksChannel(t *testing.T) {
	cfg := newTestCfg()
	pending := &fakePendingStore{
		groups: []store.PendingMessageGroup{
			{ChannelName: "telegram", HistoryKey: "tg", MessageCount: 10},
			{ChannelName: "lineworks", HistoryKey: "lw", MessageCount: 10},
			{ChannelName: "slack", HistoryKey: "sl", MessageCount: 10},
		},
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "lw"): mkMsgs("lw", 10),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)
	s.sweep(context.Background())

	if len(sched.captured) != 1 {
		t.Fatalf("expected only lineworks curated, got %d runs", len(sched.captured))
	}
	if sched.captured[0].ChannelType != "lineworks" {
		t.Fatalf("expected lineworks channel, got %q", sched.captured[0].ChannelType)
	}
}

func TestSweep_PerGroupDisabledSkipped(t *testing.T) {
	cfg := newTestCfg()
	cfg.Channels.GroupCuration.DisabledGroups = []string{"blocked"}
	pending := &fakePendingStore{
		groups: []store.PendingMessageGroup{
			{ChannelName: "lineworks", HistoryKey: "blocked", MessageCount: 10},
			{ChannelName: "lineworks", HistoryKey: "ok", MessageCount: 10},
		},
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "ok"): mkMsgs("ok", 10),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)
	s.sweep(context.Background())

	if len(sched.captured) != 1 {
		t.Fatalf("expected disabled group skipped (1 run), got %d", len(sched.captured))
	}
	if sched.captured[0].ChatID != "ok" {
		t.Fatalf("expected 'ok' curated, got %q", sched.captured[0].ChatID)
	}
}

// --- curateGroup RunRequest mapping test ---

func TestCurateGroup_BuildsRunRequest(t *testing.T) {
	cfg := newTestCfg()
	pending := &fakePendingStore{
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): mkMsgs("g1", 5),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)

	s.curateGroup(context.Background(), "lineworks", "g1")

	if len(sched.captured) != 1 {
		t.Fatalf("expected 1 run request, got %d", len(sched.captured))
	}
	req := sched.captured[0]
	if req.ChannelType != "lineworks" {
		t.Errorf("ChannelType = %q, want lineworks", req.ChannelType)
	}
	if req.PeerKind != "group" {
		t.Errorf("PeerKind = %q, want group", req.PeerKind)
	}
	if req.ChatID != "g1" {
		t.Errorf("ChatID = %q, want g1", req.ChatID)
	}
	if req.ModelOverride != "cheap-model" {
		t.Errorf("ModelOverride = %q, want cheap-model", req.ModelOverride)
	}
	if !strings.Contains(req.ExtraSystemPrompt, "MERGE") {
		t.Errorf("ExtraSystemPrompt missing merge-delta instruction: %q", req.ExtraSystemPrompt)
	}
	if !strings.Contains(req.ExtraSystemPrompt, "write_file") {
		t.Errorf("ExtraSystemPrompt missing write_file instruction")
	}
	if !strings.Contains(req.RunID, "curation:g1") {
		t.Errorf("RunID = %q, want curation:g1", req.RunID)
	}
	foundTag := false
	for _, tg := range req.TraceTags {
		if tg == "curation" {
			foundTag = true
		}
	}
	if !foundTag {
		t.Errorf("TraceTags missing 'curation': %v", req.TraceTags)
	}
}

func TestCurateGroup_ReadsExistingLongterm(t *testing.T) {
	cfg := newTestCfg()
	// Point the vault at a temp dir with a pre-seeded LONGTERM.md for the scope.
	vaultDir := t.TempDir()
	scope := "lineworks/group-g1"
	if err := writeTempLongterm(t, vaultDir, scope, "EXISTING_FACT: bot is named Claw"); err != nil {
		t.Fatal(err)
	}
	cfg.Agents.Defaults.Memory = &config.MemoryConfig{VaultDir: vaultDir}

	pending := &fakePendingStore{
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): mkMsgs("g1", 5),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)

	s.curateGroup(context.Background(), "lineworks", "g1")

	if len(sched.captured) != 1 {
		t.Fatalf("expected 1 run, got %d", len(sched.captured))
	}
	if !strings.Contains(sched.captured[0].Message, "EXISTING_FACT: bot is named Claw") {
		t.Errorf("existing LONGTERM not included in curator message:\n%s", sched.captured[0].Message)
	}
}

// --- trim tests ---

func TestCurateGroup_TrimsOnSuccess(t *testing.T) {
	cfg := newTestCfg() // KeepRecent = 3
	msgs := mkMsgs("g1", 10)
	pending := &fakePendingStore{
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): msgs,
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)

	s.curateGroup(context.Background(), "lineworks", "g1")

	if len(pending.deleted) != 1 {
		t.Fatalf("expected exactly 1 DeleteByIDs call, got %d", len(pending.deleted))
	}
	// 10 total, keep 3 -> delete 7 oldest.
	if got := len(pending.deleted[0]); got != 7 {
		t.Fatalf("expected 7 deleted (keep recent 3 of 10), got %d", got)
	}
	// Remaining should be the 3 most recent (last in ASC order).
	remaining := pending.byKey[keyOf("lineworks", "g1")]
	if len(remaining) != 3 {
		t.Fatalf("expected 3 remaining, got %d", len(remaining))
	}
	if remaining[0].ID != msgs[7].ID || remaining[2].ID != msgs[9].ID {
		t.Fatalf("trim kept wrong messages: continuity broken for next BuildContext")
	}
}

func TestCurateGroup_NoTrimOnFailure(t *testing.T) {
	cfg := newTestCfg()
	pending := &fakePendingStore{
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): mkMsgs("g1", 10),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Err: errors.New("agent boom")}}
	s := newTestSweeper(cfg, pending, sched)

	s.curateGroup(context.Background(), "lineworks", "g1")

	if len(pending.deleted) != 0 {
		t.Fatalf("expected NO trim on scheduler error, got %d delete calls", len(pending.deleted))
	}
	if len(pending.byKey[keyOf("lineworks", "g1")]) != 10 {
		t.Fatalf("pending must remain intact on failure for retry")
	}
}

func TestCurateGroup_NoTrimWhenBelowKeepRecent(t *testing.T) {
	cfg := newTestCfg() // KeepRecent = 3
	pending := &fakePendingStore{
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): mkMsgs("g1", 3),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)

	s.curateGroup(context.Background(), "lineworks", "g1")

	if len(pending.deleted) != 0 {
		t.Fatalf("expected no trim when count <= keepRecent, got %d delete calls", len(pending.deleted))
	}
}

// provider override resolution
func TestCurateGroup_ProviderOverrideResolved(t *testing.T) {
	cfg := newTestCfg()
	cfg.Channels.GroupCuration.Provider = "cheap-provider"
	pending := &fakePendingStore{
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): mkMsgs("g1", 5),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	provReg := &fakeProviderResolver{}
	s := newCurationSweeper(cfg, pending, sched,
		&fakeAgentResolver{agent: &store.AgentData{AgentKey: "default", TenantID: store.MasterTenantID}},
		&fakeChannelResolver{},
		&fakeSessionResetter{},
		provReg,
	)

	s.curateGroup(context.Background(), "lineworks", "g1")

	if provReg.lastName != "cheap-provider" {
		t.Fatalf("expected provider 'cheap-provider' resolved, got %q", provReg.lastName)
	}
	if len(sched.captured) != 1 || sched.captured[0].ProviderOverride == nil {
		t.Fatalf("expected ProviderOverride set on RunRequest")
	}
}

// fakeProviderResolver records the resolved name and returns a stub provider.
type fakeProviderResolver struct {
	lastName string
}

func (f *fakeProviderResolver) GetForTenant(tenantID uuid.UUID, name string) (providers.Provider, error) {
	f.lastName = name
	return stubProvider{}, nil
}

type stubProvider struct{ providers.Provider }

// writeTempLongterm seeds <vaultDir>/<scope>/LONGTERM.md for read tests.
func writeTempLongterm(t *testing.T, vaultDir, scope, content string) error {
	t.Helper()
	dir := filepath.Join(vaultDir, scope)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "LONGTERM.md"), []byte(content), 0o644)
}
