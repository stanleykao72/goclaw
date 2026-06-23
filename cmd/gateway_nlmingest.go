package cmd

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	lw "github.com/nextlevelbuilder/goclaw/internal/lineworks"
	"github.com/nextlevelbuilder/goclaw/internal/nlmdoc"
	"github.com/nextlevelbuilder/goclaw/internal/nlmingest"
	"github.com/nextlevelbuilder/goclaw/internal/store"
	"github.com/nextlevelbuilder/goclaw/internal/tools"
)

// startNLMIngestWorker constructs and starts the NotebookLM ingest worker
// (sub-phase 2.3), returning a cleanup func. It is a TRUE no-op when the feature
// is disabled (nlmingest.Worker.Start short-circuits on GOCLAW_NLM_INGEST_ENABLED)
// or when a required store is unavailable — so OFF costs nothing pre-enable.
//
// The worker drains buffered LINE WORKS conversation (group rows from
// GroupHistory.Record, DM rows from the channel's recordDirectMessageForIngest)
// into per-scope NotebookLM Docs via the same Drive-Doc library + nlm runner +
// pointer-store provisioner the recall path (2.2/2.4) shares.
func startNLMIngestWorker(pgStores *store.Stores) func() {
	if !nlmingest.Enabled() {
		// Skip building any dependency (rclone token source, directory client) when
		// the feature is off — Start would no-op anyway, but this avoids the work.
		return (&nlmingest.Worker{}).Start(context.Background())
	}
	if pgStores == nil || pgStores.PendingMessages == nil ||
		pgStores.IngestCursors == nil || pgStores.NotebookPointers == nil {
		slog.Warn("nlm_ingest: worker not started — pending/cursor/pointer store unavailable")
		return func() {}
	}

	// Drive Doc library (rclone-brokered token) + nlm runner + provisioner — the
	// shared NLM write stack used by BOTH this ingest worker AND the curate tools
	// (remember_shared / remember_agent). Built once here and re-built identically
	// in wireExtraTools when ingest is off (the curate tools must NOT be gated by
	// GOCLAW_NLM_INGEST_ENABLED).
	stack := buildNLMWriteStack(pgStores)
	docs := stack.docs
	runner := stack.runner
	provisioner := stack.provisioner

	worker := &nlmingest.Worker{
		Pending:     pgStores.PendingMessages,
		Cursors:     pgStores.IngestCursors,
		Provisioner: provisioner,
		Docs:        docs,
		Sync:        runner, // execNLMNotebookRunner.SourceSync shells `nlm source sync`
		Tenants:     &defaultTenantResolver{agents: pgStores.Agents},
		Names: &nlmIngestNameResolver{
			channels: pgStores.ChannelInstances,
			pending:  pgStores.PendingMessages,
		},
		Modes: &nlmIngestModeResolver{
			channels: pgStores.ChannelInstances,
			agents:   pgStores.Agents,
		},
	}
	cleanup := worker.Start(context.Background())
	slog.Info("nlm_ingest: ingest worker wired")
	return cleanup
}

// nlmWriteStack bundles the constructed NotebookLM write dependencies: the
// Drive-Doc library, the nlm CLI runner, and the pointer-store-backed
// provisioner. It is the single source of truth shared by the ingest worker
// (startNLMIngestWorker) and the curate tools (wireNotebookRememberTools), so a
// curate call writes to the SAME notebook the worker / recall use.
type nlmWriteStack struct {
	docs        *nlmdoc.DriveDocLibrary
	runner      tools.NLMNotebookRunner
	provisioner *tools.NotebookProvisioner
}

// buildNLMWriteStack constructs the Drive Doc library + nlm runner + provisioner.
// The Drive root + nlm binary are env-resolved inside the provisioner / runner
// (GOCLAW_NLM_DRIVE_ROOT / GOCLAW_NLM_BINARY). The pointer store is the same one
// recall + the provisioner use (pgStores.NotebookPointers). Cheap + stateless
// (folder cache aside), so re-constructing for a second caller is harmless.
func buildNLMWriteStack(pgStores *store.Stores) nlmWriteStack {
	tokens := nlmdoc.NewRcloneTokenSource(strings.TrimSpace(os.Getenv("GOCLAW_NLM_RCLONE_REMOTE")))
	docs := nlmdoc.NewDriveDocLibrary(tokens, nil)
	runner := tools.NewExecNLMNotebookRunner("")
	provisioner := tools.NewNotebookProvisioner(pgStores.NotebookPointers, docs, runner, "")
	return nlmWriteStack{docs: docs, runner: runner, provisioner: provisioner}
}

// wireNotebookRememberTools injects the NLM write stack into the curate tools
// (remember_shared / remember_agent). They are registered zero-dep in
// setupToolRegistry; this upgrades them from the fail-soft unwired state to live
// writes against the caller's resolved scope notebook. Guarded on a non-nil
// pointer store (the sqlite stub leaves them unwired → they fail soft). This runs
// regardless of GOCLAW_NLM_INGEST_ENABLED — the curate tools are agent-driven,
// not part of the background ingest worker (the per-agent memory_mode gate is
// their off-switch). The shared stack keeps one source of truth with ingest.
func wireNotebookRememberTools(pgStores *store.Stores, toolsReg *tools.Registry) {
	if pgStores == nil || pgStores.NotebookPointers == nil {
		slog.Info("notebook curate tools left unwired (no pointer store) — they fail soft")
		return
	}
	stack := buildNLMWriteStack(pgStores)
	for _, name := range []string{"remember_shared", "remember_agent"} {
		t, ok := toolsReg.Get(name)
		if !ok {
			continue
		}
		if rt, ok := t.(interface {
			SetProvisioner(*tools.NotebookProvisioner)
			SetDocs(nlmdoc.DocLibrary)
			SetSync(tools.NLMNotebookRunner)
		}); ok {
			rt.SetProvisioner(stack.provisioner)
			rt.SetDocs(stack.docs)
			rt.SetSync(stack.runner)
		}
	}
	slog.Info("notebook curate tools wired (remember_shared + remember_agent → NLM write stack)")
}

// defaultTenantResolver resolves the deployment's single tenant (the default
// agent's tenant, falling back to MasterTenantID) the buffered pending rows live
// under — mirrors curationSweeper.sweep's tenant resolution.
type defaultTenantResolver struct {
	agents store.AgentStore
}

func (r *defaultTenantResolver) DefaultTenant(ctx context.Context) uuid.UUID {
	if r == nil || r.agents == nil {
		return store.MasterTenantID
	}
	if ag, err := r.agents.GetDefault(store.WithTenantID(ctx, store.MasterTenantID)); err == nil && ag != nil && ag.TenantID != uuid.Nil {
		return ag.TenantID
	}
	return store.MasterTenantID
}

// nlmIngestModeResolver resolves the bound deployment agent's memory_mode so the
// ingest worker can skip ingest for a "vault"-mode agent (it does not use
// NotebookLM). It mirrors directoryClient's single-instance assumption: the agent
// of the first enabled lineworks channel instance. Resolution is memoized — the
// deployment binding does not change at runtime — and any miss yields "both" so
// ingest stays active by default.
type nlmIngestModeResolver struct {
	channels store.ChannelInstanceStore
	agents   store.AgentStore

	once sync.Once
	mode string
}

// ResolveMode returns the bound lineworks agent's memory_mode ("notebook" |
// "vault" | "both"). Falls back to "both" on any miss (no channels store, no
// enabled lineworks instance, agent lookup failure) so ingest is never disabled
// by an unresolved binding.
func (r *nlmIngestModeResolver) ResolveMode(ctx context.Context) string {
	r.once.Do(func() {
		r.mode = store.MemoryModeBoth
		if r.channels == nil || r.agents == nil {
			return
		}
		instances, err := r.channels.ListEnabled(store.WithCrossTenant(ctx))
		if err != nil {
			slog.Warn("nlm_ingest: list channel instances failed; ingest mode gate disabled (defaulting to both)", "err", err)
			return
		}
		for _, inst := range instances {
			if inst.ChannelType != "lineworks" || inst.AgentID == uuid.Nil {
				continue
			}
			ag, aerr := r.agents.GetByIDUnscoped(ctx, inst.AgentID)
			if aerr != nil || ag == nil {
				slog.Debug("nlm_ingest: bound agent lookup failed for mode gate (defaulting to both)", "agent_id", inst.AgentID, "err", aerr)
				return
			}
			r.mode = ag.ParseMemoryMode()
			slog.Info("nlm_ingest: ingest mode gate resolved", "memory_mode", r.mode)
			return
		}
		slog.Info("nlm_ingest: no enabled lineworks instance for mode gate (defaulting to both)")
	})
	return r.mode
}

// nlmIngestNameResolver best-effort resolves Doc titles for new scope notebooks:
// a user's Chinese name via the LINE WORKS directory, a group's chat title via
// session metadata. Both are cosmetic (the scope_id is the real key) and never
// block ingest — any miss returns "".
type nlmIngestNameResolver struct {
	channels store.ChannelInstanceStore
	pending  store.PendingMessageStore

	once   sync.Once
	client *lw.Client // lazily built from the first enabled lineworks instance's creds; nil on failure
}

// UserDisplayName resolves a LINE WORKS user's display name (e.g. 高玉明) via the
// directory. Requires directory.read on the bot token; on any error (incl. the
// scope not being granted) it returns "" so the Doc falls back to an id-only
// name. The result is naturally cached by the pointer table — the worker only
// needs the name once, at Doc-create time.
func (r *nlmIngestNameResolver) UserDisplayName(ctx context.Context, _ uuid.UUID, userID string) string {
	c := r.directoryClient(ctx)
	if c == nil || userID == "" {
		return ""
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u, err := c.GetUser(callCtx, userID)
	if err != nil || u == nil {
		slog.Debug("nlm_ingest: user display-name lookup failed (best-effort)", "user", userID, "err", err)
		return ""
	}
	return u.DisplayName()
}

// GroupDisplayName resolves a group's chat title from session metadata via the
// pending store's ResolveGroupTitles. "" on any miss (no clean directory name
// for a group exists; the Doc falls back to a group-<id8> name).
func (r *nlmIngestNameResolver) GroupDisplayName(ctx context.Context, _ uuid.UUID, chatID string) string {
	if r.pending == nil || chatID == "" {
		return ""
	}
	titles, err := r.pending.ResolveGroupTitles(ctx, []store.PendingMessageGroup{
		{ChannelName: "lineworks", HistoryKey: chatID},
	})
	if err != nil {
		slog.Debug("nlm_ingest: group title lookup failed (best-effort)", "chat", chatID, "err", err)
		return ""
	}
	return titles["lineworks:"+chatID]
}

// directoryClient lazily builds a LINE WORKS directory client from the first
// enabled lineworks channel instance's credentials, ensuring directory.read is
// in scope. Built at most once; nil on any failure (display-name resolution then
// degrades to id-only Doc names — ingest is unaffected).
func (r *nlmIngestNameResolver) directoryClient(ctx context.Context) *lw.Client {
	r.once.Do(func() {
		if r.channels == nil {
			return
		}
		instances, err := r.channels.ListEnabled(store.WithCrossTenant(ctx))
		if err != nil {
			slog.Warn("nlm_ingest: list channel instances failed; display names disabled", "err", err)
			return
		}
		for _, inst := range instances {
			if inst.ChannelType != "lineworks" || len(inst.Credentials) == 0 {
				continue
			}
			var cr lineWorksFactoryCreds
			if err := json.Unmarshal(inst.Credentials, &cr); err != nil {
				continue
			}
			scopes := ensureScope(cr.Scopes, "directory.read")
			ts, terr := lw.NewTokenSource(lw.AuthConfig{
				ClientID:       cr.ClientID,
				ClientSecret:   cr.ClientSecret,
				ServiceAccount: cr.ServiceAccount,
				PrivateKeyPEM:  cr.PrivateKey,
				Scopes:         scopes,
			})
			if terr != nil {
				continue
			}
			client, cerr := lw.NewClient(lw.ClientConfig{BotID: cr.BotID, Tokens: ts})
			if cerr != nil {
				continue
			}
			r.client = client
			slog.Info("nlm_ingest: directory client built for display-name resolution")
			return
		}
		slog.Info("nlm_ingest: no lineworks instance creds; display names disabled (id-only Doc titles)")
	})
	return r.client
}
