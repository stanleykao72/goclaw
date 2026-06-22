package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// notebookRecall constants.
const (
	// defaultNLMBinary is the nlm CLI binary name resolved from PATH when
	// GOCLAW_NLM_BINARY / config does not override it.
	defaultNLMBinary = "nlm"

	// nlmExecTimeout bounds a SINGLE nlm subprocess. nlm's own -t flag is set
	// slightly below this so the binary returns a clean error before we SIGKILL.
	// The fan-out across the scope set runs these concurrently (bounded), so the
	// wall-clock cost of the whole recall stays close to one query, not N.
	nlmExecTimeout      = 120 * time.Second
	nlmInternalTimeoutS = "110"

	// nlmRecallMaxParallel bounds concurrent per-notebook queries during the
	// scope-set fan-out. The set is at most 4 (shared+user+agent+group); a small
	// cap keeps NotebookLM from being hammered while still collapsing the latency
	// of N sequential 110s queries into roughly one.
	nlmRecallMaxParallel = 4

	// nlmSourceNote is appended ONCE to the merged answer so the user/LLM knows
	// the grounding came from NotebookLM.
	nlmSourceNote = "\n\n(來源: NotebookLM)"

	// nlmFailSoftMessage is returned on a TRANSIENT failure (nlm error, parse
	// failure, store error) so the turn is never broken — it reads as an outage.
	nlmFailSoftMessage = "NotebookLM 記憶暫時無法存取"

	// nlmNoMemoryMessage is returned when the caller's resolved scope set is
	// EMPTY (no notebook has been provisioned for any of their scopes yet). It is
	// DISTINCT from nlmFailSoftMessage so an empty set is never mistaken for an
	// outage — there simply is no memory to recall yet.
	nlmNoMemoryMessage = "尚無相關記憶"
)

// nlmEnvBinary is the env var name mirrored in config_load.go applyEnvOverrides;
// the tool also reads it directly so the builtin works in every process path
// (native loop, ACP, CLI bridge) without threading extra wiring.
//
// NOTE: GOCLAW_NLM_NOTEBOOK (the single-notebook override) is intentionally NOT
// consulted on the recall path anymore — Phase 2.4 recalls over the caller's
// resolved scope SET (resolveNotebookSet), so a single global notebook would
// mix scopes across users. The config field still exists for the ingest path.
const nlmEnvBinary = "GOCLAW_NLM_BINARY"

// nlmRunner executes the nlm CLI and returns its raw stdout. It is a struct
// field so tests can inject a fake (no real nlm / network). The real
// implementation uses exec.CommandContext with an argv slice — the question is
// NEVER interpolated into a shell string, so shell metacharacters cannot inject.
type nlmRunner func(ctx context.Context, binary string, args []string) ([]byte, error)

// defaultNLMRunner shells out to the nlm binary with a hard context timeout.
func defaultNLMRunner(ctx context.Context, binary string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	return cmd.Output()
}

// nlmResponse is the JSON shape returned by `nlm query notebook ...`.
// nlm wraps the result under a "value" object: {"value":{"answer":"..."}}.
// Older/flat shapes ({"answer":"..."}) are tolerated via the top-level Answer.
// Errors arrive as {"status":"error","error":"..."}.
type nlmResponse struct {
	Answer string `json:"answer"`
	Value  *struct {
		Answer string `json:"answer"`
	} `json:"value"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// NotebookRecallTool answers a question grounded in the caller's accumulated
// NotebookLM memory. It is a general builtin available to ALL agent types.
//
// SECURITY (single NotebookLM account, no per-notebook ACL — trust boundary is
// goclaw resolving the notebook SET server-side):
//   - The InputSchema is {question} ONLY. The LLM cannot choose the notebook(s).
//   - The notebook SET is resolved INSIDE Execute via resolveNotebookSet, keyed
//     PURELY off the injected identity (tenant + user + agentKey [+ group chat]),
//     never from tool args. This is why nlm's own MCP server (whose notebook_query
//     takes `notebook` as a caller arg) is NOT registered — the bridge forwards
//     LLM args verbatim and would let user A read user B's notebook.
//   - The fan-out queries EACH resolved notebook id explicitly; no id ever comes
//     from the LLM, and the resolved ids are never echoed back to the LLM.
//
// Phase 2.4: recall queries the caller's ORDERED scope set
// [shared, user, agent (, group)] — only scopes that already have a provisioned
// notebook (recall never creates). An empty set fails soft as "no memory yet"
// (nlmNoMemoryMessage), DISTINCT from a transient outage (nlmFailSoftMessage).
type NotebookRecallTool struct {
	// binary is the configured nlm binary path/name. Empty → env → PATH default.
	binary string
	// pointerStore drives resolveNotebookSet. Injected post-construction via
	// SetPointerStore (mirrors SetSessionStore / SetAgentStore). When nil (e.g.
	// the zero-value test/CLI path, or the sqlite stub), recall degrades to the
	// empty-set "no memory yet" fail-soft rather than panicking.
	pointerStore store.NotebookPointerStore
	// runner is injectable so tests never touch real nlm.
	runner nlmRunner
}

// NewNotebookRecallTool constructs the tool. binary may be empty (resolved at
// execution time: config → env → default). The pointer store is injected later
// via SetPointerStore, so a zero-value constructor is valid for every wiring
// path; without a store, recall safely yields the "no memory yet" message.
//
// The second positional arg is retained for call-site compatibility but is no
// longer used on the recall path (the single-notebook fallback was dropped in
// 2.4 — recall is scope-set driven). Callers may pass "".
func NewNotebookRecallTool(binary, _ string) *NotebookRecallTool {
	return &NotebookRecallTool{
		binary: binary,
		runner: defaultNLMRunner,
	}
}

// SetPointerStore injects the scope→notebook pointer store used by
// resolveNotebookSet. Wired in wireExtraTools after the PG stores are ready
// (mirrors SessionStoreAware.SetSessionStore). A nil store is tolerated: recall
// then resolves an empty set and fails soft with the "no memory yet" message.
func (t *NotebookRecallTool) SetPointerStore(s store.NotebookPointerStore) {
	t.pointerStore = s
}

func (t *NotebookRecallTool) Name() string { return "notebook_recall" }

func (t *NotebookRecallTool) Description() string {
	return "Answer a question grounded in the user's accumulated NotebookLM memory/knowledge. " +
		"Use when the user asks about previously shared information, documents, or past context."
}

func (t *NotebookRecallTool) Parameters() map[string]any {
	// SECURITY: question ONLY — no notebook/notebookId/scope. The LLM must not
	// be able to choose which notebook is queried.
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "The question to answer from the user's NotebookLM knowledge.",
			},
		},
		"required": []string{"question"},
	}
}

// resolveBinary picks the nlm binary: configured value → GOCLAW_NLM_BINARY → "nlm".
func (t *NotebookRecallTool) resolveBinary() string {
	if t.binary != "" {
		return t.binary
	}
	if v := strings.TrimSpace(os.Getenv(nlmEnvBinary)); v != "" {
		return v
	}
	return defaultNLMBinary
}

// recallTenant resolves the tenant the caller's pointers live under. Recall runs
// per-turn with an injected identity, so it prefers the ctx tenant; when that is
// uuid.Nil (single-deployment / unset) it falls back to MasterTenantID — the
// tenant the ingest path's defaultTenantResolver wrote pointers under — so
// List/Get actually find them instead of silently returning an empty set.
func recallTenant(ctx context.Context) uuid.UUID {
	if tid := store.TenantIDFromContext(ctx); tid != uuid.Nil {
		return tid
	}
	return store.MasterTenantID
}

func (t *NotebookRecallTool) Execute(ctx context.Context, args map[string]any) *Result {
	question, _ := args["question"].(string)
	question = strings.TrimSpace(question)
	if question == "" {
		return ErrorResult("question is required")
	}

	// Resolve the caller's ORDERED scope set PURELY from the injected identity.
	// The LLM's args (including any smuggled "notebook"/"scope" field) play no
	// role here — they are never read past "question" above.
	tenant := recallTenant(ctx)
	set, err := t.resolveSet(ctx, tenant)
	if err != nil {
		// A store error is a transient outage, not "no memory" — fail soft as such.
		slog.Warn("notebook_recall.resolve_set_failed", "tenant", tenant, "error", err)
		return t.failSoft()
	}

	// Identity plumbing log (server-side only — notebook ids are deliberately
	// NOT echoed back to the LLM).
	slog.Info("notebook_recall.resolve",
		"agent_id", store.AgentIDFromContext(ctx),
		"user_id", store.UserIDFromContext(ctx),
		"tenant_id", tenant,
		"peer_kind", ToolPeerKindFromCtx(ctx),
		"scopes", len(set),
	)

	// Empty set → no notebook provisioned for ANY of the caller's scopes yet.
	// Per spec §6 this is a distinct "no memory yet" outcome, NOT an outage, and
	// MUST NOT fall back to a global/test notebook (that would mix scopes).
	if len(set) == 0 {
		return t.noMemory()
	}

	binary := t.resolveBinary()

	answers := t.queryAll(ctx, binary, question, set)
	if len(answers) == 0 {
		// Every notebook either errored or returned nothing. We had notebooks to
		// query, so this is a transient failure, not "no memory".
		slog.Warn("notebook_recall.no_answers", "tenant", tenant, "scopes", len(set))
		return t.failSoft()
	}

	return NewResult(mergeAnswers(answers) + nlmSourceNote)
}

// resolveSet returns the caller's ordered existing-notebook set. A nil pointer
// store (zero-value / sqlite stub) yields an empty set with no error so recall
// degrades to the "no memory yet" message instead of panicking.
func (t *NotebookRecallTool) resolveSet(ctx context.Context, tenant uuid.UUID) ([]ResolvedNotebook, error) {
	if t.pointerStore == nil {
		return nil, nil
	}
	return resolveNotebookSet(ctx, t.pointerStore, tenant)
}

// scopeAnswer is one notebook's grounded answer, tagged with its scope so the
// merge can label it. NotebookID is kept server-side only (for logging) and is
// NEVER placed into the merged text returned to the LLM.
type scopeAnswer struct {
	scope  ScopeKey
	answer string
}

// queryAll fans the question out over the resolved set, querying each notebook
// with the EXISTING Phase-1 command (`nlm query notebook <id> <q> -t 110`) so the
// clean {value:{answer}} JSON contract + parseNLMAnswer are reused unchanged.
// Queries run concurrently (bounded) so N notebooks cost ~one query of wall
// time, not N. Per-notebook failures are logged and skipped (fail-soft); results
// are returned in the canonical scope order regardless of completion order.
func (t *NotebookRecallTool) queryAll(ctx context.Context, binary, question string, set []ResolvedNotebook) []scopeAnswer {
	results := make([]string, len(set))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(nlmRecallMaxParallel)

	var mu sync.Mutex
	for i, nb := range set {
		i, nb := i, nb
		g.Go(func() error {
			ans, ok := t.queryOne(gctx, binary, question, nb.NotebookID)
			if !ok {
				return nil // skip — fail-soft per notebook
			}
			mu.Lock()
			results[i] = ans
			mu.Unlock()
			return nil
		})
	}
	// queryOne never returns an error (failures are reported via ok=false), so
	// Wait is purely a barrier; ignore its (always-nil) error.
	_ = g.Wait()

	out := make([]scopeAnswer, 0, len(set))
	for i, ans := range results {
		if strings.TrimSpace(ans) == "" {
			continue
		}
		out = append(out, scopeAnswer{scope: set[i].Scope, answer: ans})
	}
	return out
}

// queryOne runs a single per-notebook query and parses the answer. It never
// panics and never returns an error; ok=false signals "skip this notebook"
// (exec failure, parse failure, or empty answer). Each call names exactly ONE of
// OUR resolved notebook ids — there is zero surface for a cross-user leak.
func (t *NotebookRecallTool) queryOne(ctx context.Context, binary, question, notebookID string) (string, bool) {
	if notebookID == "" {
		return "", false
	}

	// Hard timeout PER subprocess; nlm's own -t is set just under it.
	execCtx, cancel := context.WithTimeout(ctx, nlmExecTimeout)
	defer cancel()

	// argv slice — question is a SEPARATE element, never shell-interpolated.
	// Command: nlm query notebook <NB_ID> <question> -t 110
	//
	// IMPORTANT: -t here is `nlm query notebook`'s --timeout (a float seconds
	// value, used as the Phase-1 contract). Do NOT migrate this fan-out to
	// `nlm cross query`: on that subcommand -t means --tags, and worse, v0.5.16
	// `nlm cross query` emits only Rich/ANSI TUI panels (no --json), which are
	// not machine-parseable. Per-notebook query is the only scoped + parseable
	// path. (See the change Rejected note for the cross-query deviation.)
	cmdArgs := []string{
		"query", "notebook",
		notebookID,
		question,
		"-t", nlmInternalTimeoutS,
	}

	out, err := t.runner(execCtx, binary, cmdArgs)
	if err != nil {
		slog.Warn("notebook_recall.exec_failed",
			"binary", binary,
			"notebook", notebookID,
			"error", err,
			"stderr", stderrSnippet(err),
		)
		return "", false
	}

	answer, ok := parseNLMAnswer(out)
	if !ok || strings.TrimSpace(answer) == "" {
		slog.Warn("notebook_recall.parse_failed", "notebook", notebookID, "stdout_len", len(out))
		return "", false
	}
	return strings.TrimSpace(answer), true
}

// mergeAnswers combines per-scope answers into one grounded response. A single
// answer is returned bare; multiple are labelled per scope (in canonical scope
// order) so the LLM can attribute each. Scope LABELS are human-readable kind
// names only — the raw notebook/scope IDS are NEVER included (isolation: the LLM
// must not learn which notebook ids exist).
func mergeAnswers(answers []scopeAnswer) string {
	if len(answers) == 1 {
		return answers[0].answer
	}
	var b strings.Builder
	for i, a := range answers {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("【")
		b.WriteString(scopeLabel(a.scope.Kind))
		b.WriteString("】\n")
		b.WriteString(a.answer)
	}
	return b.String()
}

// scopeLabel maps a scope kind to a user-facing label. It deliberately returns
// only the KIND (never the scope id) so no identity/notebook id leaks to the LLM.
func scopeLabel(kind string) string {
	switch kind {
	case store.ScopeKindShared:
		return "共享記憶"
	case store.ScopeKindUser:
		return "個人記憶"
	case store.ScopeKindAgent:
		return "助理記憶"
	case store.ScopeKindGroup:
		return "群組記憶"
	default:
		return "記憶"
	}
}

// failSoft returns a graceful, non-error result for a TRANSIENT failure so the
// turn is never broken.
func (t *NotebookRecallTool) failSoft() *Result {
	return NewResult(nlmFailSoftMessage)
}

// noMemory returns a graceful, non-error result when the caller has no notebook
// provisioned for any scope yet. Distinct from failSoft so an empty set is not
// mistaken for an outage.
func (t *NotebookRecallTool) noMemory() *Result {
	return NewResult(nlmNoMemoryMessage)
}

// parseNLMAnswer extracts the answer from nlm's JSON stdout. nlm returns
// {"value":{"answer":"..."}} on success and {"status":"error","error":"..."}
// on failure; a flat {"answer":"..."} is also tolerated. Any non-JSON prefix
// before the first '{' is skipped.
func parseNLMAnswer(out []byte) (string, bool) {
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	var resp nlmResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", false
	}
	if resp.Status == "error" {
		return "", false
	}
	if resp.Value != nil && strings.TrimSpace(resp.Value.Answer) != "" {
		return strings.TrimSpace(resp.Value.Answer), true
	}
	return strings.TrimSpace(resp.Answer), true
}

// stderrSnippet pulls captured stderr from an *exec.ExitError for logging only.
func stderrSnippet(err error) string {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		s := string(exitErr.Stderr)
		if len(s) > 500 {
			s = s[:500]
		}
		return s
	}
	return ""
}
