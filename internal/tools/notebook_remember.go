package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/nlmdoc"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// Curate (write) tool constants. The curate tools persist an LLM-supplied fact
// into the SHARED or AGENT scope notebook, mirroring the ingest worker's proven
// GetOrCreateScopeNotebook → AppendText → SourceSync sequence. SECURITY: the
// scope is ALWAYS derived from the verified injected identity (scopeKeyForCtx),
// never from tool args / message content — remember_agent can only ever write to
// the CALLING agent's OWN notebook, and remember_shared to the single shared one.
const (
	// rememberSyncTimeout bounds the post-append `nlm source sync` subprocess so a
	// hung nlm base cannot stall the agent turn. Mirrors nlmingest.drainSyncTimeout.
	rememberSyncTimeout = 120 * time.Second

	// rememberSavedShared / rememberSavedAgent are the success confirmations.
	rememberSavedShared = "Saved to shared memory."
	rememberSavedAgent  = "Saved to agent memory."

	// rememberPartialShared / rememberPartialAgent are returned when the content
	// was appended to the Drive Doc but the follow-up source sync failed: the fact
	// is durably saved and will be indexed on the next sync. Reported as a
	// (non-error) partial success, matching the worker's tolerance.
	rememberPartialShared = "Saved to shared memory (will sync into NotebookLM shortly)."
	rememberPartialAgent  = "Saved to agent memory (will sync into NotebookLM shortly)."

	// rememberFailSoft is returned on a provision/append failure (transient
	// outage). Graceful, never an error — the turn is never broken.
	rememberFailSoft = "Could not save to memory right now."

	// rememberNoMemory is returned when the agent's memory_mode is "vault": the
	// NotebookLM subsystem is off for this agent, so the curate tool no-ops
	// gracefully (mirrors notebook_recall's vault gate).
	rememberNoMemory = "NotebookLM memory is not enabled for this agent."

	// rememberNoScope is returned when the required identity dimension is absent
	// (e.g. agent scope with no agentKey in ctx) so no mis-keyed write happens.
	rememberNoScope = "Could not determine which memory to save to."

	// rememberContentRequired is the validation error for an empty content arg.
	rememberContentRequired = "content is required"

	// rememberUnknownUser labels a curated block whose curating user is not
	// resolvable from ctx (no sender name, no user id).
	rememberUnknownUser = "unknown"

	// rememberDefaultTitle labels a curated block when no title is supplied.
	rememberDefaultTitle = "Note"
)

// NotebookRememberTool persists an LLM-curated fact into ONE memory scope's
// NotebookLM notebook (shared or agent). It is a general builtin available to
// ALL agent types. One tool type is parameterized by scopeKind so the shared and
// agent variants share all logic and differ only in their fixed scope + name.
//
// SECURITY / ISOLATION (load-bearing):
//   - The scope is resolved INSIDE Execute via scopeKeyForCtx(ctx, scopeKind),
//     keyed PURELY off the verified injected identity (shared → ("shared",""),
//     agent → ("agent", AgentKeyFromContext)). No tool arg can redirect it:
//     a smuggled {"scope":...} / {"agent":...} field is never read.
//   - remember_agent therefore can only ever write to the CALLING agent's OWN
//     notebook; remember_shared always targets the single shared notebook. No
//     cross-user / cross-agent write is expressible.
//   - The tenant comes from recallTenant(ctx) — IDENTICAL to ingest + recall — so
//     the write keys the same notebook recall later reads (see commit 197c2f44).
type NotebookRememberTool struct {
	// scopeKind is the FIXED write scope kind for this tool instance
	// (store.ScopeKindShared or store.ScopeKindAgent). It is set at construction
	// and is NEVER taken from args.
	scopeKind string
	// toolName / savedMsg / partialMsg are the per-scope user-facing strings.
	toolName   string
	savedMsg   string
	partialMsg string

	// provisioner lazily resolves (creating on first write) the scope's notebook +
	// Drive Doc. Injected post-construction via SetProvisioner (mirrors
	// notebook_recall.SetPointerStore). nil → fail-soft (no write).
	provisioner *NotebookProvisioner
	// docs appends the curated block to the scope's Drive Doc. Injected via
	// SetDocs. nil → fail-soft.
	docs nlmdoc.DocLibrary
	// sync refreshes the notebook from its just-appended Drive source. Injected
	// via SetSync. nil → the append still counts (partial success).
	sync NLMNotebookRunner
}

// NewRememberSharedTool constructs the SHARED-scope curate tool. Dependencies are
// injected later via SetProvisioner / SetDocs / SetSync, so a zero-value
// constructor is valid for every wiring path; unwired, it fails soft.
func NewRememberSharedTool() *NotebookRememberTool {
	return &NotebookRememberTool{
		scopeKind:  store.ScopeKindShared,
		toolName:   "remember_shared",
		savedMsg:   rememberSavedShared,
		partialMsg: rememberPartialShared,
	}
}

// NewRememberAgentTool constructs the AGENT-scope curate tool (writes only to the
// calling agent's own notebook).
func NewRememberAgentTool() *NotebookRememberTool {
	return &NotebookRememberTool{
		scopeKind:  store.ScopeKindAgent,
		toolName:   "remember_agent",
		savedMsg:   rememberSavedAgent,
		partialMsg: rememberPartialAgent,
	}
}

// SetProvisioner injects the lazy notebook provisioner (shared with the ingest
// worker + recall's pointer store). Wired in wireExtraTools after the PG stores
// are ready. A nil provisioner is tolerated: the tool then fails soft.
func (t *NotebookRememberTool) SetProvisioner(p *NotebookProvisioner) { t.provisioner = p }

// SetDocs injects the Drive-Doc library used to append the curated block.
func (t *NotebookRememberTool) SetDocs(d nlmdoc.DocLibrary) { t.docs = d }

// SetSync injects the runner whose SourceSync refreshes the notebook after the
// append. nil → the append is reported as a (durable) partial success.
func (t *NotebookRememberTool) SetSync(s NLMNotebookRunner) { t.sync = s }

func (t *NotebookRememberTool) Name() string { return t.toolName }

func (t *NotebookRememberTool) Description() string {
	switch t.scopeKind {
	case store.ScopeKindAgent:
		return "Save an important fact to THIS agent's own long-term NotebookLM memory. " +
			"Use to remember agent-specific knowledge, preferences, or context for future turns."
	default:
		return "Save an important fact to the SHARED long-term NotebookLM memory visible to all agents. " +
			"Use to remember team-wide knowledge, decisions, or context for future turns."
	}
}

func (t *NotebookRememberTool) Parameters() map[string]any {
	// SECURITY: {content, title} ONLY — no scope/agent/notebook field. The LLM
	// must not be able to choose which scope/notebook is written.
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"content": map[string]any{
				"type":        "string",
				"description": "The fact to remember (will be appended to long-term memory).",
			},
			"title": map[string]any{
				"type":        "string",
				"description": "Optional short label for this note.",
			},
		},
		"required": []string{"content"},
	}
}

func (t *NotebookRememberTool) Execute(ctx context.Context, args map[string]any) *Result {
	content, _ := args["content"].(string)
	content = strings.TrimSpace(content)
	if content == "" {
		return ErrorResult(rememberContentRequired)
	}
	title, _ := args["title"].(string)
	title = strings.TrimSpace(title)

	// Memory mode gate: a "vault"-only agent does not use NotebookLM. The tool
	// stays registered for every agent but no-ops here (graceful, never an error)
	// so the turn is unbroken and no Drive/nlm work runs. Mirrors notebook_recall.
	if store.MemoryModeFromCtx(ctx) == store.MemoryModeVault {
		return NewResult(rememberNoMemory)
	}

	// Resolve the SINGLE write scope PURELY from the injected identity. The LLM's
	// args play no role — they are never read past content/title above.
	scope, ok := scopeKeyForCtx(ctx, t.scopeKind)
	if !ok {
		// Required identity dimension absent (e.g. agent scope, no agentKey). Do
		// NOT write a mis-keyed pointer — fail soft with a clear message.
		slog.Warn("notebook_remember.scope_unresolved", "tool", t.toolName, "scope_kind", t.scopeKind)
		return NewResult(rememberNoScope)
	}

	if t.provisioner == nil || t.docs == nil {
		// Unwired (sqlite stub / zero-value path). Nothing to write to — fail soft.
		slog.Warn("notebook_remember.unwired", "tool", t.toolName)
		return NewResult(rememberFailSoft)
	}

	tenant := recallTenant(ctx)

	// Server-side plumbing log (notebook ids deliberately NOT echoed to the LLM).
	slog.Info("notebook_remember.write",
		"tool", t.toolName,
		"agent_id", store.AgentIDFromContext(ctx),
		"user_id", store.UserIDFromContext(ctx),
		"tenant_id", tenant,
		"scope_kind", scope.Kind,
	)

	// displayName: human label for the Doc on first create. shared → "shared";
	// agent → the agentKey string (== scope.ID for the agent scope).
	displayName := t.displayName(scope)

	// 1. Lazy-create (or reuse) the scope's notebook + Drive Doc. On a fresh scope
	// this does the full create (Drive folder + Doc + nlm notebook + source); it
	// is bounded by the inherited ctx and fails soft on any error.
	notebookID, driveDocID, err := t.provisioner.GetOrCreateScopeNotebook(ctx, tenant, scope.Kind, scope.ID, displayName)
	if err != nil {
		slog.Warn("notebook_remember.provision_failed",
			"tool", t.toolName, "scope_kind", scope.Kind, "error", err)
		return NewResult(rememberFailSoft)
	}

	// 2. Append the provenance-headed block to the scope's Drive Doc. The Doc
	// library inserts NO separator between appends, so we own the leading "\n\n"
	// + header (matches the spec format).
	block := t.buildBlock(ctx, title, content)
	if err := t.docs.AppendText(ctx, driveDocID, block); err != nil {
		slog.Warn("notebook_remember.append_failed",
			"tool", t.toolName, "scope_kind", scope.Kind, "doc_id", driveDocID, "error", err)
		return NewResult(rememberFailSoft)
	}

	// 3. Refresh the notebook from the just-appended Doc. A sync failure AFTER a
	// successful append is PARTIAL success: the content is durably in the Doc and
	// will be indexed on a later sync (matches the worker's tolerance). We do NOT
	// retry — curate is one-shot, unlike the worker's cursor loop.
	if t.sync == nil {
		slog.Warn("notebook_remember.no_sync_runner", "tool", t.toolName)
		return NewResult(t.partialMsg)
	}
	syncCtx, cancel := context.WithTimeout(ctx, rememberSyncTimeout)
	defer cancel()
	if err := t.sync.SourceSync(syncCtx, notebookID); err != nil {
		slog.Warn("notebook_remember.source_sync_failed (content saved, not yet indexed)",
			"tool", t.toolName, "notebook_id", notebookID, "scope_kind", scope.Kind, "error", err)
		return NewResult(t.partialMsg)
	}

	return NewResult(t.savedMsg)
}

// displayName returns the Doc title for first-create. shared → "shared"; agent →
// the agentKey string (the agent scope id IS the agentKey).
func (t *NotebookRememberTool) displayName(scope ScopeKey) string {
	switch scope.Kind {
	case store.ScopeKindAgent:
		return scope.ID // agentKey
	default:
		return store.ScopeKindShared // "shared"
	}
}

// buildBlock formats the appended, auditable provenance block:
//
//	\n\n## <title or 'Note'> — <RFC3339 ts> (by <user>)\n<content>\n
//
// The leading blank line separates this block from the previous append (the Doc
// library inserts no separator). The curating user is the ctx sender name, else
// the bare (channel-prefix-stripped) user id, else "unknown".
func (t *NotebookRememberTool) buildBlock(ctx context.Context, title, content string) string {
	if title == "" {
		title = rememberDefaultTitle
	}
	user := curatingUser(ctx)
	ts := time.Now().UTC().Format(time.RFC3339)
	return fmt.Sprintf("\n\n## %s — %s (by %s)\n%s\n", title, ts, user, content)
}

// curatingUser resolves a human label for the curator from ctx: sender display
// name if present, else the bare user id (channel prefix stripped), else
// "unknown". Used only for the audit header — never to choose the scope.
func curatingUser(ctx context.Context) string {
	if name := strings.TrimSpace(store.SenderNameFromContext(ctx)); name != "" {
		return name
	}
	if uid := normalizeScopeUserID(store.UserIDFromContext(ctx)); uid != "" {
		return uid
	}
	return rememberUnknownUser
}
