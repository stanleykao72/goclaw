package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/adhocore/gronx"
	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/providers"
	"github.com/nextlevelbuilder/goclaw/internal/scheduler"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// curationChannelName is the only channel whose pending group discussion is
// curated into the vault. LINE WORKS is the sole supported platform for this
// feature by design.
const curationChannelName = "lineworks"

// curationPollInterval is how often the sweep wakes to check whether the
// configured cron cadence has elapsed. A coarse poll (1 minute) is enough for an
// hourly business-hours cadence and keeps idle-time CPU negligible.
const curationPollInterval = time.Minute

// curationRunTimeout bounds a single per-group curation turn so a hung agent
// cannot block the sweep (mirrors the cron job timeout discipline).
const curationRunTimeout = 5 * time.Minute

// curationFallbackKeepRecent is the defensive trim floor when config resolves a
// non-positive keep-recent (EffectiveKeepRecent already defaults to 40).
const curationFallbackKeepRecent = 40

// curationVaultMaxLines / curationVaultMaxBytes bound how much of the existing
// LONGTERM.md is fed back into the curator prompt for merge-delta. Generous, but
// capped so a large index does not blow the prompt budget.
const (
	curationVaultMaxLines = 400
	curationVaultMaxBytes = 24000
)

// --- Stage 2 dependency seams (interfaces so curateGroup is unit-testable) ---

// curationScheduler is the subset of *scheduler.Scheduler curateGroup needs.
type curationScheduler interface {
	Schedule(ctx context.Context, lane string, req agent.RunRequest) <-chan scheduler.RunOutcome
}

// curationAgentResolver resolves the agent (key + tenant) used to scope the
// curation session and the curator provider. GetByID maps the channel
// instance's agent_id to the agent that SERVES the channel (its memory backend
// decides where write_file lands — e.g. e-smith-hub uses the vault); GetDefault
// is the fallback when no channel→agent mapping is found.
type curationAgentResolver interface {
	GetDefault(ctx context.Context) (*store.AgentData, error)
	GetByID(ctx context.Context, id uuid.UUID) (*store.AgentData, error)
}

// curationChannelResolver lists channel instances so curation can find the
// agent that serves a given channel type (the pending rows carry the channel
// TYPE, e.g. "lineworks"). Required so curation runs as the channel's own agent
// rather than the global default — otherwise the curator's memory backend is
// wrong and the vault is never written.
type curationChannelResolver interface {
	ListEnabled(ctx context.Context) ([]store.ChannelInstanceData, error)
}

// curationSessionResetter resets + persists the curation session before each run
// so a previous run's context does not leak into the next.
type curationSessionResetter interface {
	Reset(ctx context.Context, key string)
	Save(ctx context.Context, key string) error
}

// curationProviderResolver resolves a curator provider override by name.
type curationProviderResolver interface {
	GetForTenant(tenantID uuid.UUID, name string) (providers.Provider, error)
}

// curationSweeper periodically curates accumulated UNMENTIONED LINE WORKS group
// discussion into each group's vault LONGTERM.md, via an agent turn.
//
// Sweep-vs-cron-service rationale: the cron Service (internal/cron) is a
// user-facing job store (jobs are persisted JSON rows created via AddJob and
// fired by a single shared onJob handler). It has no clean way to register a
// system-owned, non-persisted handler bound to a config-driven cron expr. The
// heartbeat ticker is the established precedent for a system-owned periodic
// sweep, so this mirrors that shape: a standalone goroutine ticker that
// re-evaluates the configured cron cadence (reusing the project's gronx engine)
// on each poll. This keeps the feature additive and avoids polluting the user
// cron job list.
type curationSweeper struct {
	cfg        *config.Config
	pending    store.PendingMessageStore
	sched      curationScheduler
	agents     curationAgentResolver
	channels   curationChannelResolver
	sessions   curationSessionResetter
	provReg    curationProviderResolver
	stopCh     chan struct{}
	wg         sync.WaitGroup
	lastFire   time.Time // last time the cadence fired (zero = never)
	tzWarnOnce sync.Once // warn at most once on an invalid timezone (poll runs every minute)
}

// newCurationSweeper constructs a sweeper. Dependencies mirror how
// startCronAndHeartbeat receives its deps from gateway.go.
func newCurationSweeper(
	cfg *config.Config,
	pending store.PendingMessageStore,
	sched curationScheduler,
	agents curationAgentResolver,
	channels curationChannelResolver,
	sess curationSessionResetter,
	provReg curationProviderResolver,
) *curationSweeper {
	return &curationSweeper{
		cfg:      cfg,
		pending:  pending,
		sched:    sched,
		agents:   agents,
		channels: channels,
		sessions: sess,
		provReg:  provReg,
		stopCh:   make(chan struct{}),
	}
}

// Start launches the background poll loop. No-op if curation is disabled or the
// pending store is unavailable.
func (s *curationSweeper) Start() {
	if s.pending == nil {
		slog.Info("group curation sweep disabled: no pending message store")
		return
	}
	if !s.cfg.Channels.GroupCuration.IsEnabled() {
		slog.Info("group curation sweep disabled by config")
		return
	}
	s.wg.Add(1)
	go s.loop()
	slog.Info("group curation sweep started",
		"cadence", s.cfg.Channels.GroupCuration.EffectiveCadence(),
		"timezone", s.cfg.Channels.GroupCuration.EffectiveTimezone(s.cfg.Cron.DefaultTimezone),
	)
}

// Stop signals the poll loop to exit and waits for completion. Safe to call even
// if Start was a no-op.
func (s *curationSweeper) Stop() {
	select {
	case <-s.stopCh:
		// already closed
	default:
		close(s.stopCh)
	}
	s.wg.Wait()
	slog.Info("group curation sweep stopped")
}

// gc returns the group-curation config, never nil. When no curation.* keys are
// configured the config pointer is nil; the *GroupMemoryCurationConfig methods
// are nil-safe, but RAW field reads (Provider, Model) are not — so normalize to a
// zero-value struct here so every use site (including raw field access) is safe.
func (s *curationSweeper) gc() *config.GroupMemoryCurationConfig {
	if s.cfg != nil && s.cfg.Channels.GroupCuration != nil {
		return s.cfg.Channels.GroupCuration
	}
	return &config.GroupMemoryCurationConfig{}
}

// resolveChannelAgent returns the agent that serves the given channel TYPE
// (e.g. "lineworks"), by matching a channel instance's ChannelType and looking
// up its agent_id. Returns nil when no mapping is found (caller falls back to the
// default agent). This is what makes curation run as the channel's own agent so
// write_file routes to that agent's memory backend (the vault).
func (s *curationSweeper) resolveChannelAgent(ctx context.Context, channelName string) *store.AgentData {
	if s.channels == nil || s.agents == nil {
		return nil
	}
	instances, err := s.channels.ListEnabled(ctx)
	if err != nil {
		slog.Warn("group curation: list channel instances failed", "error", err)
		return nil
	}
	for _, inst := range instances {
		if inst.ChannelType != channelName {
			continue
		}
		ag, aerr := s.agents.GetByID(ctx, inst.AgentID)
		if aerr != nil || ag == nil {
			slog.Warn("group curation: channel agent lookup failed",
				"channel", channelName, "agent_id", inst.AgentID, "error", aerr)
			return nil
		}
		return ag
	}
	return nil
}

func (s *curationSweeper) loop() {
	defer s.wg.Done()
	ticker := time.NewTicker(curationPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case now := <-ticker.C:
			if s.cadenceElapsed(now) {
				// Guard: a bug in a single sweep/curation must never crash the
				// whole gateway. Recover, log, and keep the ticker alive.
				func() {
					defer func() {
						if r := recover(); r != nil {
							slog.Error("group curation: sweep panic recovered", "panic", r)
						}
					}()
					s.sweep(context.Background())
				}()
			}
		}
	}
}

// cadenceElapsed reports whether the configured cron cadence has fired since the
// last successful evaluation. It computes the next scheduled tick after lastFire
// (or process start) in the configured timezone and returns true once now has
// reached it, advancing lastFire to now.
func (s *curationSweeper) cadenceElapsed(now time.Time) bool {
	gc := s.gc()
	expr := gc.EffectiveCadence()
	tz := gc.EffectiveTimezone(s.cfg.Cron.DefaultTimezone)

	loc := time.Local
	if l, err := time.LoadLocation(tz); err == nil {
		loc = l
	} else {
		// Poll runs every minute; warn only once so an invalid tz does not spam.
		s.tzWarnOnce.Do(func() {
			slog.Warn("group curation: invalid timezone, falling back to local", "tz", tz, "error", err)
		})
	}

	anchor := s.lastFire
	if anchor.IsZero() {
		// First evaluation: anchor one poll interval back so a cadence boundary
		// that lands exactly at startup still fires, without back-firing for the
		// whole history.
		anchor = now.Add(-curationPollInterval)
	}

	next, err := gronx.NextTickAfter(expr, anchor.In(loc), false)
	if err != nil {
		slog.Warn("group curation: invalid cadence expression", "expr", expr, "error", err)
		return false
	}
	if !now.In(loc).Before(next) {
		s.lastFire = now
		return true
	}
	return false
}

// sweep enumerates qualifying LINE WORKS groups and curates each. This is the
// OUTER loop only — per-group curation is delegated to curateGroup.
func (s *curationSweeper) sweep(ctx context.Context) {
	gc := s.gc()
	if !gc.IsEnabled() {
		return
	}

	// The pending message store is tenant-scoped and fail-closed: a nil tenant in
	// ctx makes ListGroups/ListByKey/DeleteByIDs error ("tenant_id required").
	// Resolve the default agent's tenant and inject it before any store read so
	// the whole sweep (ListGroups → curateGroup's ListByKey → trim) shares one
	// tenant-scoped context. Fall back to MasterTenantID (the single-tenant
	// e-smith deployment writes pending under Master). Multi-tenant enumeration
	// (sweeping every tenant) is a followup.
	tenantID := store.MasterTenantID
	if s.agents != nil {
		if ag, aerr := s.agents.GetDefault(store.WithTenantID(ctx, store.MasterTenantID)); aerr == nil && ag != nil && ag.TenantID != uuid.Nil {
			tenantID = ag.TenantID
		}
	}
	ctx = store.WithTenantID(ctx, tenantID)

	groups, err := s.pending.ListGroups(ctx)
	if err != nil {
		slog.Warn("group curation: failed to list pending groups", "error", err)
		return
	}

	minPending := gc.EffectiveMinPending()
	curated := 0
	for _, g := range groups {
		if g.ChannelName != curationChannelName {
			continue
		}
		if g.MessageCount < minPending {
			continue
		}
		if gc.IsGroupDisabled(g.HistoryKey) {
			slog.Debug("group curation: skipping disabled group", "history_key", g.HistoryKey)
			continue
		}
		s.curateGroup(ctx, g.ChannelName, g.HistoryKey)
		curated++
	}
	if curated > 0 {
		slog.Info("group curation sweep fired", "groups_curated", curated)
	}
}

// curationExtraSystemPrompt is the curator instruction prompt. It is appended to
// the agent's normal system prompt for the curation turn only. Self-contained;
// in the spirit of hub-memory's curate-cowork prompt but not dependent on it.
const curationExtraSystemPrompt = "[Group Memory Curation]\n" +
	"You are curating the long-term memory of a LINE WORKS group chat. The user " +
	"message contains (1) the EXISTING curated LONGTERM.md for this group (may be " +
	"empty on first run) and (2) the recent un-curated group discussion.\n" +
	"Your task:\n" +
	"- DISCARD greetings, smalltalk, acknowledgements, and ephemeral chatter.\n" +
	"- EXTRACT only durable facts, decisions, preferences, naming rules/conventions, " +
	"commitments, and stable context worth remembering long-term.\n" +
	"- MERGE the extracted items into the existing LONGTERM content WITHOUT " +
	"duplicating anything already present. Refine or correct existing entries when " +
	"the new discussion supersedes them; otherwise leave them intact.\n" +
	"- WRITE the complete, merged result to LONGTERM.md using the write_file tool " +
	"(this is your group's curated index file). Do not write any other file.\n" +
	"- If there is nothing durable to add and nothing to correct, leave LONGTERM.md " +
	"unchanged and simply report that no update was needed.\n" +
	"Keep LONGTERM.md concise, well-organized, and in the language the group uses."

// curateGroup curates a single group's pending discussion into its vault
// LONGTERM.md via an agent turn, then trims pending on success.
//
// The vault write happens THROUGH the agent turn (the curator calls write_file →
// MemoryInterceptor → vault). curateGroup never writes the vault directly. Scope
// is derived by the tool layer from ChannelType+PeerKind+ChatID, identical to the
// scope this function reads from to feed merge-delta.
func (s *curationSweeper) curateGroup(ctx context.Context, channelName, historyKey string) {
	gc := s.gc()

	// [1] Read the group's pending discussion.
	msgs, err := s.pending.ListByKey(ctx, channelName, historyKey)
	if err != nil {
		slog.Warn("group curation: failed to list pending messages",
			"history_key", historyKey, "error", err)
		return
	}
	if len(msgs) == 0 {
		return
	}

	// [2] Resolve the agent that SERVES this channel (its memory backend decides
	// where write_file lands — the lineworks channel's agent uses the vault). Fall
	// back to the default agent only if no channel→agent mapping is found (and warn,
	// since the default's backend may not be the vault → curation would not persist).
	agentKey := s.cfg.ResolveDefaultAgentID()
	var tenantID uuid.UUID
	if ag := s.resolveChannelAgent(ctx, channelName); ag != nil {
		agentKey = ag.AgentKey
		tenantID = ag.TenantID
	} else if s.agents != nil {
		if ag, aerr := s.agents.GetDefault(ctx); aerr == nil && ag != nil {
			agentKey = ag.AgentKey
			tenantID = ag.TenantID
		}
		slog.Warn("group curation: no channel→agent mapping; using default agent — vault may not be written if its memory backend is not the vault",
			"channel", channelName, "agent", agentKey)
	}

	// [3] Read existing LONGTERM.md for the scope so the prompt can merge-delta.
	scope := tools.MemoryVaultSubdirFor(curationChannelName, "group", historyKey, "")
	existingLongterm := ""
	if vaultDir := s.vaultDir(); vaultDir != "" {
		if content, _, rerr := tools.ReadMemoryVaultIndexForScope(
			vaultDir, scope, curationVaultMaxLines, curationVaultMaxBytes,
		); rerr != nil {
			slog.Warn("group curation: failed to read existing LONGTERM",
				"history_key", historyKey, "scope", scope, "error", rerr)
		} else {
			existingLongterm = content
		}
	}

	// [4] Build the run context (tenant-scoped, timeout-bounded).
	runCtx, cancel := context.WithTimeout(ctx, curationRunTimeout)
	defer cancel()
	if tenantID != uuid.Nil {
		runCtx = store.WithTenantID(runCtx, tenantID)
	}

	// [5] Reset the curation session so a prior run does not pollute context.
	sessionKey := sessions.BuildCronSessionKey(agentKey, "curation:"+historyKey)
	if s.sessions != nil {
		s.sessions.Reset(runCtx, sessionKey)
		if serr := s.sessions.Save(runCtx, sessionKey); serr != nil {
			slog.Debug("group curation: session save failed", "error", serr)
		}
	}

	// [6] Resolve the curator provider override (cheap model), like cron/heartbeat.
	var providerOverride providers.Provider
	if gc.Provider != "" && s.provReg != nil {
		if prov, perr := s.provReg.GetForTenant(tenantID, gc.Provider); perr == nil {
			providerOverride = prov
		} else {
			slog.Warn("group curation: curator provider not in registry, using agent default",
				"provider", gc.Provider, "error", perr)
		}
	}

	// [7] Build the curator message: existing LONGTERM + the pending discussion.
	message := buildCurationMessage(existingLongterm, msgs)

	// [8] Schedule the curation turn through the cron lane and wait.
	outCh := s.sched.Schedule(runCtx, scheduler.LaneCron, agent.RunRequest{
		SessionKey:        sessionKey,
		Message:           message,
		Channel:           curationChannelName,
		ChannelType:       curationChannelName,
		ChatID:            historyKey,
		PeerKind:          "group",
		UserID:            "system:curation",
		RunID:             "curation:" + historyKey,
		ExtraSystemPrompt: curationExtraSystemPrompt,
		ModelOverride:     gc.Model,
		ProviderOverride:  providerOverride,
		Stream:            false,
		TraceName:         "Curation [" + historyKey + "]",
		TraceTags:         []string{"curation"},
	})

	var outcome scheduler.RunOutcome
	select {
	case outcome = <-outCh:
	case <-runCtx.Done():
		slog.Warn("group curation: run timed out, not trimming",
			"history_key", historyKey, "timeout", curationRunTimeout)
		return
	}
	if outcome.Err != nil {
		// Do NOT trim on failure — retry on the next cadence.
		slog.Warn("group curation: run failed, not trimming",
			"history_key", historyKey, "error", outcome.Err)
		return
	}

	// [9] Trim pending to the most recent KeepRecent ONLY after a successful run.
	s.trimAfterSuccess(ctx, channelName, historyKey, gc.EffectiveKeepRecent())
}

// trimAfterSuccess deletes all but the most recent keepRecent pending messages
// for the group. The deletion is the watermark — no summary row is inserted (the
// curated content already lives in the vault). Keeping the most recent keepRecent
// preserves continuity so the next mention's BuildContext still has recent turns.
//
// IMPORTANT: it lists the pending set FRESH (rather than trusting the snapshot
// taken before the run) so messages that arrived during the curation turn are
// counted as "recent" and never deleted.
func (s *curationSweeper) trimAfterSuccess(ctx context.Context, channelName, historyKey string, keepRecent int) {
	if keepRecent <= 0 {
		keepRecent = curationFallbackKeepRecent
	}
	current, err := s.pending.ListByKey(ctx, channelName, historyKey)
	if err != nil {
		slog.Warn("group curation: trim re-list failed, leaving pending intact",
			"history_key", historyKey, "error", err)
		return
	}
	if len(current) <= keepRecent {
		return // nothing to trim
	}
	// ListByKey returns ASC by created_at, so the oldest are at the front.
	toDelete := current[:len(current)-keepRecent]
	ids := make([]uuid.UUID, len(toDelete))
	for i, m := range toDelete {
		ids[i] = m.ID
	}
	if derr := s.pending.DeleteByIDs(ctx, ids); derr != nil {
		slog.Warn("group curation: trim delete failed",
			"history_key", historyKey, "error", derr)
		return
	}
	slog.Info("group curation: curated + trimmed",
		"history_key", historyKey,
		"deleted", len(ids),
		"kept", keepRecent,
	)
}

// vaultDir returns the deployment-global memory vault root, or "" if unset.
func (s *curationSweeper) vaultDir() string {
	if s.cfg == nil || s.cfg.Agents.Defaults.Memory == nil {
		return ""
	}
	return config.ExpandHome(s.cfg.Agents.Defaults.Memory.VaultDir)
}

// buildCurationMessage renders the curator user message: the existing curated
// LONGTERM (for merge-delta) followed by the recent un-curated discussion.
func buildCurationMessage(existingLongterm string, msgs []store.PendingMessage) string {
	var sb strings.Builder
	sb.WriteString("## Existing LONGTERM.md (merge into this — do not duplicate)\n")
	if strings.TrimSpace(existingLongterm) == "" {
		sb.WriteString("(empty — this is the first curation for this group)\n")
	} else {
		sb.WriteString(existingLongterm)
		if !strings.HasSuffix(existingLongterm, "\n") {
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n## Recent group discussion to curate\n")
	for _, m := range msgs {
		sender := m.Sender
		if sender == "" {
			sender = m.SenderID
		}
		if m.IsSummary {
			sender = "[previous summary]"
		}
		ts := ""
		if !m.CreatedAt.IsZero() {
			ts = fmt.Sprintf(" [%s]", m.CreatedAt.Format("2006-01-02 15:04"))
		}
		fmt.Fprintf(&sb, "%s%s: %s\n", sender, ts, m.Body)
	}
	return sb.String()
}
