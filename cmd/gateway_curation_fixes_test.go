package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/scheduler"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Regression lock for the tenant fix: the pending store is fail-closed on a nil
// tenant. The sweep MUST inject a tenant before the first store read, otherwise
// the whole feature silently no-ops in production. With a tenant-enforcing fake,
// the sweep must still enumerate + curate (proving the ctx carries a tenant).
func TestSweep_InjectsTenantForStoreReads(t *testing.T) {
	cfg := newTestCfg()
	pending := &fakePendingStore{
		requireTenant: true, // mirror pg/sqlite scopeClause fail-closed behavior
		groups: []store.PendingMessageGroup{
			{ChannelName: "lineworks", HistoryKey: "g1", MessageCount: 5},
		},
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): mkMsgs("g1", 5),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)

	// Caller passes a tenant-less context (as loop() does); the sweep must inject one.
	s.sweep(context.Background())

	if len(sched.captured) != 1 {
		t.Fatalf("expected 1 curation with tenant-enforcing store (sweep must inject tenant), got %d", len(sched.captured))
	}
	// ListByKey (inside curateGroup) and the trim re-list also went through the
	// tenant-scoped ctx — otherwise curateGroup would have errored before scheduling.
	if len(pending.deleted) != 1 {
		t.Fatalf("expected trim to run under tenant-scoped ctx (1 delete), got %d", len(pending.deleted))
	}
}

// Regression lock for the production SIGSEGV: when no curation.* config is set,
// cfg.Channels.GroupCuration is NIL. The *GroupMemoryCurationConfig methods are
// nil-safe but RAW field reads (Provider, Model) are not — curateGroup crashed
// the whole gateway on gc.Provider. The sweeper must normalize gc to non-nil so
// the default-on path runs without panicking even with zero config.
func TestSweep_NilCurationConfigDoesNotPanic(t *testing.T) {
	cfg := &config.Config{} // GroupCuration left nil, exactly like prod with no curation.* keys
	pending := &fakePendingStore{
		groups: []store.PendingMessageGroup{
			{ChannelName: "lineworks", HistoryKey: "g1", MessageCount: 5},
		},
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): mkMsgs("g1", 5),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	s := newTestSweeper(cfg, pending, sched)

	// Must NOT panic on the raw field reads (gc.Provider/gc.Model) in curateGroup.
	s.sweep(context.Background())

	if len(sched.captured) != 1 {
		t.Fatalf("expected default-on curation with nil config (1 run), got %d", len(sched.captured))
	}
	// ModelOverride resolves to "" (zero value) when no curation config — fine.
	if sched.captured[0].ModelOverride != "" {
		t.Fatalf("expected empty ModelOverride with nil config, got %q", sched.captured[0].ModelOverride)
	}
}

// Curation MUST run as the agent that SERVES the channel (so write_file routes to
// that agent's memory backend / vault), NOT the global default agent. Regression
// lock for the prod bug where curation ran as the default agent (field-recorder)
// and the vault was never written.
func TestCurateGroup_UsesChannelServingAgent(t *testing.T) {
	cfg := newTestCfg()
	chanAgentID := uuid.Must(uuid.NewV7())
	pending := &fakePendingStore{
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): mkMsgs("g1", 5),
		},
	}
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	agents := &fakeAgentResolver{
		agent: &store.AgentData{AgentKey: "field-recorder", TenantID: store.MasterTenantID}, // the (wrong) default
		byID: map[uuid.UUID]*store.AgentData{
			chanAgentID: {AgentKey: "e-smith-hub", TenantID: store.MasterTenantID}, // the channel's agent
		},
	}
	channels := &fakeChannelResolver{instances: []store.ChannelInstanceData{
		{ChannelType: "lineworks", AgentID: chanAgentID},
	}}
	s := newCurationSweeper(cfg, pending, sched, agents, channels, &fakeSessionResetter{}, nil)

	s.curateGroup(context.Background(), "lineworks", "g1")

	if len(sched.captured) != 1 {
		t.Fatalf("expected 1 run, got %d", len(sched.captured))
	}
	// The session key embeds the agent key — it MUST be the channel's agent, not the default.
	if !strings.Contains(sched.captured[0].SessionKey, "e-smith-hub") {
		t.Fatalf("curation must run as the channel's agent (e-smith-hub), got SessionKey %q", sched.captured[0].SessionKey)
	}
	if strings.Contains(sched.captured[0].SessionKey, "field-recorder") {
		t.Fatalf("curation wrongly ran as the default agent (field-recorder): %q", sched.captured[0].SessionKey)
	}
}

// cadenceElapsed must fire exactly once per cron boundary, honor the timezone,
// stay idle outside business hours, and not firing-storm on a bad expr.
func TestCadenceElapsed(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Skip("Asia/Taipei tz not available")
	}
	mkSweeper := func(cadence string) *curationSweeper {
		cfg := newTestCfg()
		cfg.Channels.GroupCuration.Cadence = cadence
		cfg.Channels.GroupCuration.Timezone = "Asia/Taipei"
		return newTestSweeper(cfg, &fakePendingStore{}, &fakeScheduler{})
	}

	// 09:00 Taipei is on a "0 8-17 * * *" boundary → first eval fires.
	at0900 := time.Date(2026, 6, 9, 9, 0, 0, 0, loc)
	s := mkSweeper("0 8-17 * * *")
	if !s.cadenceElapsed(at0900) {
		t.Fatal("expected fire at 09:00 business-hours boundary")
	}
	// Same now again → already fired this boundary → no double-fire.
	if s.cadenceElapsed(at0900) {
		t.Fatal("expected NO second fire at the same boundary")
	}

	// Outside business hours (20:00) → never fires for "0 8-17 * * *".
	at2000 := time.Date(2026, 6, 9, 20, 0, 0, 0, loc)
	sOut := mkSweeper("0 8-17 * * *")
	if sOut.cadenceElapsed(at2000) {
		t.Fatal("expected NO fire outside business hours (20:00)")
	}

	// Invalid cron expr → no fire (and no panic / firing storm).
	sBad := mkSweeper("not-a-cron")
	if sBad.cadenceElapsed(at0900) {
		t.Fatal("expected NO fire on invalid cadence expression")
	}
}

// trimAfterSuccess must re-list pending FRESH so messages that arrive DURING the
// curation turn are counted as recent and never deleted (continuity invariant).
func TestCurateGroup_TrimReListsFreshMidRunArrivals(t *testing.T) {
	cfg := newTestCfg() // KeepRecent = 3
	original := mkMsgs("g1", 5)
	pending := &fakePendingStore{
		byKey: map[string][]store.PendingMessage{
			keyOf("lineworks", "g1"): original,
		},
	}
	// Two messages arrive WHILE the curation turn is running.
	newArrivals := mkMsgs("g1", 2)
	sched := &fakeScheduler{outcome: scheduler.RunOutcome{Result: &agent.RunResult{}}}
	sched.onSchedule = func() {
		k := keyOf("lineworks", "g1")
		pending.byKey[k] = append(pending.byKey[k], newArrivals...)
	}
	s := newTestSweeper(cfg, pending, sched)

	s.curateGroup(context.Background(), "lineworks", "g1")

	// After the run there are 7 (5 original + 2 mid-run). KeepRecent=3 → delete 4
	// oldest (all from the original set); the 2 mid-run arrivals must survive.
	if len(pending.deleted) != 1 {
		t.Fatalf("expected 1 trim delete, got %d", len(pending.deleted))
	}
	survived := map[uuid.UUID]bool{}
	for _, m := range pending.byKey[keyOf("lineworks", "g1")] {
		survived[m.ID] = true
	}
	for _, m := range newArrivals {
		if !survived[m.ID] {
			t.Fatalf("mid-run arrival %s was wrongly trimmed (trim must re-list fresh)", m.ID)
		}
	}
	// Deleted set must contain only IDs from the ORIGINAL older messages.
	origIDs := map[uuid.UUID]bool{}
	for _, m := range original {
		origIDs[m.ID] = true
	}
	for _, id := range pending.deleted[0] {
		if !origIDs[id] {
			t.Fatalf("trim deleted a non-original (possibly mid-run) message id %s", id)
		}
	}
}
