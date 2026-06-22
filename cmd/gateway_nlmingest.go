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

	// Drive Doc library (rclone-brokered token) + nlm runner — same wiring the
	// recall path uses. The Drive root + nlm binary are env-resolved inside the
	// provisioner / runner (GOCLAW_NLM_DRIVE_ROOT / GOCLAW_NLM_BINARY).
	tokens := nlmdoc.NewRcloneTokenSource(strings.TrimSpace(os.Getenv("GOCLAW_NLM_RCLONE_REMOTE")))
	docs := nlmdoc.NewDriveDocLibrary(tokens, nil)
	runner := tools.NewExecNLMNotebookRunner("")
	provisioner := tools.NewNotebookProvisioner(pgStores.NotebookPointers, docs, runner, "")

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
	}
	cleanup := worker.Start(context.Background())
	slog.Info("nlm_ingest: ingest worker wired")
	return cleanup
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
