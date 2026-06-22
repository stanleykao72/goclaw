// Package nlmingest implements sub-phase 2.3 of the goclaw × NotebookLM memory
// system: the background INGEST WORKER.
//
// When a LINE WORKS conversation happens, the text is buffered (group messages
// in channel_pending_messages by the channel's GroupHistory.Record; DM messages
// by the channel's recordDirectMessageForIngest, also into
// channel_pending_messages). This worker is the SOLE, background consumer of
// that buffer: on a ticker it reads NEW messages since its own per-scope
// high-water, batches each scope's window into ONE Drive-Doc append plus ONE
// `nlm source sync`, then advances the high-water. The webhook path stays fast
// (capture only enqueues; all Drive/nlm work happens here).
//
// HARD CONSTRAINTS honored:
//   - Webhook returns 200 fast — capture only enqueues (channel side).
//   - SINGLE consumer per Doc — the worker is the only appender, draining each
//     scope sequentially within one goroutine, serialized by the per-scope
//     cursor key (tenant, channel, history_key).
//   - high-water dedup — the cursor survives ticks + restart (durable table).
//   - source-bloat control — one Doc per scope (foundation), one append + one
//     sync per scope per drain.
//   - OPT-IN — GOCLAW_NLM_INGEST_ENABLED (default false); OFF → true no-op
//     (no ticker goroutine, no DB reads).
//   - fail-soft — nlm/Drive errors are logged and retried next tick (cursor not
//     advanced); the worker never crashes goclaw, never blocks the request path.
//
// ISOLATION (per FINAL spec §10): scope is derived from the buffered row's
// VERIFIED fields — DM row → (user, userId); group row → (group, chatId) — never
// from message body content.
package nlmingest

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

const (
	// EnvEnabled is the opt-in flag. Mirrored in the lineworks channel
	// (recordDirectMessageForIngest) so capture and drain share one switch.
	EnvEnabled = "GOCLAW_NLM_INGEST_ENABLED"

	// EnvInterval overrides the drain cadence (a Go duration string, e.g. "10m").
	EnvInterval = "GOCLAW_NLM_INGEST_INTERVAL"

	// EnvMinMessages, when set to a positive integer, requires at least that many
	// NEW messages in a scope's window before it is drained — batches small talk
	// into fewer Doc appends + source syncs. Default 1 (drain any new message).
	EnvMinMessages = "GOCLAW_NLM_INGEST_MIN_MESSAGES"

	// defaultInterval is the drain cadence when EnvInterval is unset/invalid.
	defaultInterval = 10 * time.Minute

	// channelName is the only channel whose buffered messages are ingested. LINE
	// WORKS is the sole supported platform (mirrors curationChannelName).
	channelName = "lineworks"

	// windowLimit caps how many messages a single scope drains per tick, bounding
	// one AppendText's read-modify-write size during a long-idle catch-up. The
	// remainder is drained on subsequent ticks (the cursor advances per batch).
	windowLimit = 500

	// drainSyncTimeout bounds a single scope's nlm source-sync subprocess so a
	// hung nlm base cannot stall the whole sweep.
	drainSyncTimeout = 120 * time.Second
)

// Enabled reports whether NotebookLM ingest is opted in. Default false: any
// unset / unparseable / falsey value disables the worker (and DM capture).
func Enabled() bool {
	v := strings.TrimSpace(os.Getenv(EnvEnabled))
	if v == "" {
		return false
	}
	enabled, err := strconv.ParseBool(v)
	return err == nil && enabled
}

// resolveInterval reads EnvInterval → defaultInterval.
func resolveInterval() time.Duration {
	if v := strings.TrimSpace(os.Getenv(EnvInterval)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
		slog.Warn("nlm_ingest: invalid GOCLAW_NLM_INGEST_INTERVAL, using default",
			"value", v, "default", defaultInterval)
	}
	return defaultInterval
}

// resolveMinMessages reads EnvMinMessages → 1.
func resolveMinMessages() int {
	if v := strings.TrimSpace(os.Getenv(EnvMinMessages)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
		slog.Warn("nlm_ingest: invalid GOCLAW_NLM_INGEST_MIN_MESSAGES, using 1", "value", v)
	}
	return 1
}

// --- Dependency seams (interfaces so the sweep is unit-testable with fakes) ---

// pendingReader is the READ-ONLY slice of store.PendingMessageStore the worker
// needs. It deliberately omits every delete/compact method — the worker NEVER
// mutates the buffer (it coexists with group-memory curation + a possibly
// re-enabled channelmemory worker; advancing its own cursor is its only state).
type pendingReader interface {
	ListGroups(ctx context.Context) ([]store.PendingMessageGroup, error)
	ListSince(ctx context.Context, channelName, historyKey string, afterCreatedAt time.Time, afterID uuid.UUID, limit int) ([]store.PendingMessage, error)
}

// cursorStore is store.IngestCursorStore (re-declared narrowly for the fake).
type cursorStore interface {
	Get(ctx context.Context, tenant uuid.UUID, channelName, historyKey string) (store.IngestCursor, bool, error)
	Upsert(ctx context.Context, c store.IngestCursor) error
}

// provisioner lazily resolves (creating on first write) the notebook + Drive Doc
// for a scope. *tools.NotebookProvisioner satisfies it.
type provisioner interface {
	GetOrCreateScopeNotebook(ctx context.Context, tenant uuid.UUID, scopeKind, scopeID, displayName string) (notebookID, driveDocID string, err error)
}

// docAppender appends a batched text blob to a Drive Doc. nlmdoc.DocLibrary
// satisfies it (the worker only needs AppendText, not folder/create ops).
type docAppender interface {
	AppendText(ctx context.Context, docID, text string) error
}

// sourceSyncer refreshes a NotebookLM notebook from its (just-appended) Drive
// source. Backed by `nlm source sync <notebookId>`.
type sourceSyncer interface {
	SourceSync(ctx context.Context, notebookID string) error
}

// tenantResolver resolves the single tenant the buffered rows live under (the
// e-smith deployment writes pending under the default agent's tenant, falling
// back to MasterTenantID). Mirrors curationSweeper.sweep.
type tenantResolver interface {
	DefaultTenant(ctx context.Context) uuid.UUID
}

// modeResolver resolves the bound deployment agent's memory_mode
// ("notebook" | "vault" | "both"). NotebookLM ingest is skipped for a
// "vault"-mode agent (it does not use NotebookLM). A nil resolver, any miss, or
// an unknown value yields "both" so ingest stays active by default — preserving
// current behavior and keeping the worker startable without this dependency.
//
// Resolution is deployment-global (the single bound lineworks agent, same
// assumption directoryClient already makes): PendingMessage carries no agent_id,
// so per-scope agent binding is not modeled. The worker resolves it ONCE per
// sweep (memoized) and applies it to every drained scope.
type modeResolver interface {
	ResolveMode(ctx context.Context) string
}

// displayNameResolver best-effort resolves a human title for a scope's Doc on
// first create (Chinese name for a user, chat title for a group). Returns "" on
// any miss — the Doc is still created correctly (scope_id is the real key).
type displayNameResolver interface {
	// UserDisplayName resolves a LINE WORKS user's display name (e.g. 高玉明).
	UserDisplayName(ctx context.Context, tenant uuid.UUID, userID string) string
	// GroupDisplayName resolves a group's chat title.
	GroupDisplayName(ctx context.Context, tenant uuid.UUID, chatID string) string
}

// Worker drains buffered LINE WORKS conversation into per-scope NotebookLM Docs.
// All dependencies are injected so the sweep is exercised with fakes (no real
// Drive / nlm / DB / network) in unit tests.
type Worker struct {
	Pending     pendingReader
	Cursors     cursorStore
	Provisioner provisioner
	Docs        docAppender
	Sync        sourceSyncer
	Tenants     tenantResolver
	Names       displayNameResolver
	// Modes resolves the bound deployment agent's memory_mode so ingest is
	// skipped for a "vault"-mode agent. Optional: nil → ingest always active
	// (treated as "both"), preserving current behavior.
	Modes modeResolver

	// Interval / MinMessages override the env-resolved defaults when > 0 (tests
	// set them directly). Zero → resolved from env at Start / per sweep.
	Interval    time.Duration
	MinMessages int
}

// Start launches the background drain loop and returns a cancel func. It is a
// TRUE no-op when the feature is disabled or a required dependency is missing:
// no goroutine, no ticker, no DB reads — so OFF costs nothing pre-enable.
func (w *Worker) Start(ctx context.Context) func() {
	if w == nil || !Enabled() {
		slog.Info("nlm_ingest: worker disabled (GOCLAW_NLM_INGEST_ENABLED not set)")
		return func() {}
	}
	if w.Pending == nil || w.Cursors == nil || w.Provisioner == nil || w.Docs == nil || w.Sync == nil {
		slog.Warn("nlm_ingest: worker not started — missing dependency")
		return func() {}
	}

	interval := w.Interval
	if interval <= 0 {
		interval = resolveInterval()
	}

	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		slog.Info("nlm_ingest: worker started", "interval", interval)
		for {
			select {
			case <-ticker.C:
				// A bug in one sweep must never crash the gateway: recover, log,
				// keep the ticker alive (mirrors curationSweeper.loop).
				func() {
					defer func() {
						if r := recover(); r != nil {
							slog.Error("nlm_ingest: sweep panic recovered", "panic", r)
						}
					}()
					w.sweep(runCtx)
				}()
			case <-runCtx.Done():
				slog.Info("nlm_ingest: worker stopped")
				return
			}
		}
	}()
	return cancel
}

// sweep performs one drain pass over every qualifying LINE WORKS scope.
//
// The pending store is tenant-scoped and fail-closed (a nil tenant makes
// ListGroups/ListSince error). Resolve the deployment tenant and inject it
// before any read so the whole sweep shares one tenant-scoped context (mirrors
// curationSweeper.sweep). Multi-tenant enumeration is a followup; the cursor
// table is already keyed by tenant_id so that extension needs no migration.
func (w *Worker) sweep(ctx context.Context) {
	tenant := store.MasterTenantID
	if w.Tenants != nil {
		if t := w.Tenants.DefaultTenant(ctx); t != uuid.Nil {
			tenant = t
		}
	}
	ctx = store.WithTenantID(ctx, tenant)

	// Memory mode gate: NotebookLM ingest is skipped entirely for a "vault"-mode
	// agent (it does not use NotebookLM). Resolved ONCE per sweep — it is
	// deployment-global. A nil resolver / any miss yields "both" → ingest active,
	// so behavior is unchanged when the resolver is not wired. The cursor is NOT
	// advanced (no reads happen), so a later mode change resumes from where it left.
	if w.Modes != nil && w.Modes.ResolveMode(ctx) == store.MemoryModeVault {
		slog.Debug("nlm_ingest: sweep skipped — bound agent memory_mode is vault")
		return
	}

	groups, err := w.Pending.ListGroups(ctx)
	if err != nil {
		slog.Warn("nlm_ingest: list pending groups failed", "error", err)
		return
	}

	minMsgs := w.MinMessages
	if minMsgs <= 0 {
		minMsgs = resolveMinMessages()
	}

	drained := 0
	for _, g := range groups {
		if g.ChannelName != channelName {
			continue
		}
		if w.drainScope(ctx, tenant, g.HistoryKey, minMsgs) {
			drained++
		}
	}
	if drained > 0 {
		slog.Info("nlm_ingest: sweep drained scopes", "count", drained)
	}
}

// drainScope ingests one scope's NEW messages (since its cursor) into its Doc.
// Returns true iff it appended + synced + advanced the cursor this tick.
//
// fail-soft: any error (read, provision, append, sync) is logged and the cursor
// is NOT advanced, so the window retries next tick. Per-scope errors never abort
// the sweep — other scopes still drain.
func (w *Worker) drainScope(ctx context.Context, tenant uuid.UUID, historyKey string, minMsgs int) bool {
	cur, _, err := w.Cursors.Get(ctx, tenant, channelName, historyKey)
	if err != nil {
		slog.Warn("nlm_ingest: cursor get failed", "history_key", historyKey, "error", err)
		return false
	}

	msgs, err := w.Pending.ListSince(ctx, channelName, historyKey, cur.LastCreatedAt, cur.LastID, windowLimit)
	if err != nil {
		slog.Warn("nlm_ingest: list since cursor failed", "history_key", historyKey, "error", err)
		return false
	}
	if len(msgs) < minMsgs {
		return false // not enough new messages yet — wait for the next tick
	}

	// Classify scope from the buffered rows' VERIFIED fields, never from body.
	scopeKind, scopeID, ok := classifyScope(historyKey, msgs)
	if !ok {
		slog.Warn("nlm_ingest: could not classify scope, skipping", "history_key", historyKey)
		return false
	}

	displayName := w.resolveDisplayName(ctx, tenant, scopeKind, scopeID)

	// Lazy-create (or reuse) the scope's notebook + Drive Doc.
	notebookID, driveDocID, err := w.Provisioner.GetOrCreateScopeNotebook(ctx, tenant, scopeKind, scopeID, displayName)
	if err != nil {
		slog.Warn("nlm_ingest: provision notebook failed",
			"scope_kind", scopeKind, "scope_id", scopeID, "error", err)
		return false
	}

	// ONE batched append for the whole window.
	blob := buildBatchBlob(msgs)
	if err := w.Docs.AppendText(ctx, driveDocID, blob); err != nil {
		slog.Warn("nlm_ingest: append to doc failed",
			"scope_kind", scopeKind, "scope_id", scopeID, "doc_id", driveDocID, "error", err)
		return false
	}

	// ONE source sync to refresh the notebook from the just-appended Doc.
	syncCtx, cancel := context.WithTimeout(ctx, drainSyncTimeout)
	defer cancel()
	if err := w.Sync.SourceSync(syncCtx, notebookID); err != nil {
		// The Doc already has the appended content, but the notebook has NOT
		// re-read it. Advancing the cursor here would be wrong — the notebook would
		// never see this batch until a *later* batch happens to sync. So leave the
		// cursor: next tick re-appends the same window AND re-syncs. The cost is a
		// duplicated block in the Doc (fail-soft, constraint #6), preferred over a
		// batch the notebook never indexes.
		slog.Warn("nlm_ingest: source sync failed (cursor not advanced)",
			"notebook_id", notebookID, "scope_kind", scopeKind, "scope_id", scopeID, "error", err)
		return false
	}

	// SUCCESS — advance the cursor past the drained window's max (created_at, id).
	last := msgs[len(msgs)-1]
	if err := w.Cursors.Upsert(ctx, store.IngestCursor{
		TenantID:      tenant,
		ChannelName:   channelName,
		HistoryKey:    historyKey,
		LastCreatedAt: last.CreatedAt,
		LastID:        last.ID,
	}); err != nil {
		// The append+sync already landed; failing to persist the cursor means the
		// next tick re-appends the same window (duplication) but never data loss.
		slog.Warn("nlm_ingest: cursor advance failed (window may re-ingest next tick)",
			"history_key", historyKey, "error", err)
		return false
	}

	slog.Info("nlm_ingest: scope drained",
		"scope_kind", scopeKind, "scope_id", scopeID,
		"messages", len(msgs), "notebook_id", notebookID)
	return true
}

// resolveDisplayName best-effort resolves a Doc title for the scope. Never
// blocks ingest: a nil resolver or any miss yields "" (the provisioner builds a
// valid id-only Doc name and the pointer table persists "").
func (w *Worker) resolveDisplayName(ctx context.Context, tenant uuid.UUID, scopeKind, scopeID string) string {
	if w.Names == nil {
		return ""
	}
	switch scopeKind {
	case store.ScopeKindUser:
		return w.Names.UserDisplayName(ctx, tenant, scopeID)
	case store.ScopeKindGroup:
		return w.Names.GroupDisplayName(ctx, tenant, scopeID)
	default:
		return ""
	}
}
